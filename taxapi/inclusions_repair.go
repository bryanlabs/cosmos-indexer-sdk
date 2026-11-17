package taxapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RepeatedInclusionsOptions defaults to a read-only dry run: the plan is
// checked through the ledger's own reads with the planned rows standing in
// for the new tables, and nothing is written. Apply requires a new backup
// file on durable storage. Backup records name wallets and must never be
// logged or committed.
type RepeatedInclusionsOptions struct {
	Apply      bool
	BackupPath string
	// Heights limits the run to these recorded holes; empty means every
	// recorded hole not yet cleared.
	Heights []int64
	// Compare names holes whose accounts are also rebuilt the way the API
	// answers them, walking back from the ledger tip, and compared with the
	// chain's state at the block and the one before it.
	Compare         []int64
	CompareAccounts int
	Workers         int
}

// Kinds of misplaced inclusion.
const (
	inclusionRanBefore = "A" // the copy that ran is dated at an earlier copy turned away
	inclusionRanAfter  = "B" // the copy that ran is dated at a later copy turned away
	inclusionPaidFee   = "C" // a failed copy that paid its fee is dated at a copy turned away
	inclusionPrePaid   = "P" // as C, before Gaia v7, settled from the fees the block collected
)

// inclusionRow is the txes row that holds a hash, as the index has it.
type inclusionRow struct {
	ID       uint
	Hash     string
	Code     int64
	BlockID  uint
	Height   int64
	Messages bool
	Events   bool
	Resolved bool
	Charged  string
}

// accountCheck is one account rebuilt across one block and compared with the
// chain's state before it.
type accountCheck struct {
	Height  int64  `json:"height"`
	Account string `json:"account"`
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
}

// inclusionHole is the plan for one hole's block.
type inclusionHole struct {
	Version   int       `json:"version"`
	PlannedAt time.Time `json:"planned_at"`
	Apply     bool      `json:"apply"`
	Chain     string    `json:"chain"`
	Height    int64     `json:"height"`
	ChainTxs  int       `json:"chain_txs"`
	Named     []string  `json:"named"`
	// Effective is the inclusions this block's transactions are dated by;
	// Held is the copies the index row was dated at, turned away.
	Effective []TxInclusion  `json:"effective"`
	Held      []TxInclusion  `json:"held"`
	Kinds     map[string]int `json:"kinds"`
	// Moved is the accounts each moved transaction names on chain, checked
	// across the block the index dated it at.
	Moved map[string][]string `json:"moved"`
	// PlaceBlockers stop the placements from being written; Blockers stop
	// only the clearance.
	PlaceBlockers []string       `json:"place_blockers,omitempty"`
	Blockers      []string       `json:"blockers,omitempty"`
	Checks        []accountCheck `json:"checks"`
	Accepted      bool           `json:"accepted"`
	Proven        bool           `json:"proven"`
}

// RepeatedInclusionsSummary counts what a run planned, without addresses.
type RepeatedInclusionsSummary struct {
	Apply           bool           `json:"apply"`
	Holes           int            `json:"holes"`
	Proven          int            `json:"proven"`
	Accepted        int            `json:"accepted_blocks"`
	Placements      int            `json:"placements"`
	Kinds           map[string]int `json:"kinds"`
	AccountChecks   int            `json:"account_checks"`
	AccountFailures int            `json:"account_failures"`
	// NamedAccounts is the distinct accounts the run's holes name, and
	// Unrefused those left with no uncleared hole anywhere once the plan
	// applies.
	NamedAccounts int            `json:"named_accounts"`
	Unrefused     int            `json:"unrefused_accounts"`
	Blockers      map[string]int `json:"blockers,omitempty"`
	Compared      []compareRow   `json:"compared,omitempty"`
}

// compareRow is one account of a compared hole, rebuilt the way the API
// answers, without the account or its amounts.
type compareRow struct {
	Height  int64  `json:"height"`
	Account int    `json:"account"`
	Before  string `json:"before"`
	At      string `json:"at"`
}

func openInclusionBackup(opts RepeatedInclusionsOptions) (*os.File, error) {
	if opts.BackupPath == "" {
		if opts.Apply {
			return nil, errors.New("applying repeated inclusions requires --backup on durable storage")
		}
		return nil, nil
	}
	f, err := os.OpenFile(opts.BackupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create repeated-inclusion backup: %w", err)
	}
	dir, err := os.Open(filepath.Dir(opts.BackupPath))
	if err == nil {
		err = dir.Sync()
		if cerr := dir.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("persist repeated-inclusion backup directory: %w", err)
	}
	return f, nil
}

// passedAnte reports whether a copy of a transaction got past the ante
// handler: it ran (code 0) or, from Gaia v7, failed after the fee deduction,
// which leaves events. Before v7 a failure is settled from the block's fees.
func passedAnte(t nodeTx) bool { return t.Code == 0 || !t.Rejected }

// RepairRepeatedInclusions dates the index rows of transactions the chain
// included more than once at the inclusion that took effect, and clears the
// recorded holes whose blocks are then proven complete. A block is proven
// when every transaction the chain let past the ante handler in it has an
// index row effective there with the chain's outcome, and every account the
// hole names, rebuilt from the chain's state at the block back across it,
// equals the chain's state before it. The blocks the moved rows were dated at
// are checked the same way for the accounts the moved transactions name.
// Nothing is deleted or rewritten: inclusions and clearances are new rows.
func (s *Server) RepairRepeatedInclusions(chain string, node *ledgerNode, opts RepeatedInclusionsOptions, logf func(string, ...any)) (RepeatedInclusionsSummary, error) {
	sum := RepeatedInclusionsSummary{Apply: opts.Apply, Kinds: map[string]int{}, Blockers: map[string]int{}}
	if s.preview != nil {
		return sum, errors.New("a repair run needs its own Server")
	}
	backup, err := openInclusionBackup(opts)
	if err != nil {
		return sum, err
	}
	if backup != nil {
		defer backup.Close()
	}
	workers := max(opts.Workers, 1)
	ids, err := s.chainRowIDs(chain)
	if err != nil {
		return sum, err
	}
	preEvents := func(h int64) bool { return chainParam(chain) == apiMainnet && h < hubFailedTxEventsFrom }

	// The holes to work on, with the accounts each names.
	var tableExists bool
	if err := s.db.Raw("SELECT to_regclass('tax_index_hole_clearances') IS NOT NULL").Scan(&tableExists).Error; err != nil {
		return sum, err
	}
	var holes []IndexHole
	q := s.db.Where("chain = ?", chain)
	if len(opts.Heights) > 0 {
		q = q.Where("height IN ?", opts.Heights)
	}
	if err := q.Order("height").Find(&holes).Error; err != nil {
		return sum, fmt.Errorf("holes: %w", err)
	}
	cleared := map[int64]bool{}
	if tableExists {
		var hs []int64
		if err := s.db.Model(&IndexHoleClearance{}).Where("chain = ?", chain).Pluck("height", &hs).Error; err != nil {
			return sum, err
		}
		for _, h := range hs {
			cleared[h] = true
		}
	}
	plans := make([]*inclusionHole, 0, len(holes))
	for _, h := range holes {
		if cleared[h.Height] {
			continue
		}
		p := &inclusionHole{Version: 1, PlannedAt: time.Now().UTC(), Apply: opts.Apply, Chain: chain, Height: h.Height, Kinds: map[string]int{}, Moved: map[string][]string{}}
		if !h.AddressesKnown {
			p.Blockers = append(p.Blockers, "the hole does not list the accounts it names")
		}
		if err := s.db.Model(&IndexHoleAddress{}).Where("chain = ? AND height = ?", chain, h.Height).Order("address").Pluck("address", &p.Named).Error; err != nil {
			return sum, err
		}
		plans = append(plans, p)
	}
	sum.Holes = len(plans)
	if len(plans) == 0 {
		return sum, nil
	}

	// Read the hole blocks, then the blocks the index dates their
	// transactions at instead.
	scans := map[int64]scannedBlock{}
	nexts := map[int64]feeCollectorBegin{}
	var mu sync.Mutex
	read := func(heights []int64) error {
		var want []int64
		mu.Lock()
		for _, h := range heights {
			if _, ok := scans[h]; !ok {
				want = append(want, h)
			}
		}
		mu.Unlock()
		errs := make([]error, len(want))
		ch := make(chan int, len(want))
		for i := range want {
			ch <- i
		}
		close(ch)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range ch {
					h := want[i]
					b, err := node.blockScan(h, preEvents(h))
					if err != nil {
						errs[i] = fmt.Errorf("block %d: %w", h, err)
						continue
					}
					var next scannedBlock
					if preEvents(h) {
						if next, err = node.blockScan(h+1, false); err != nil {
							errs[i] = fmt.Errorf("block %d, read for the fees block %d collected: %w", h+1, h, err)
							continue
						}
					}
					mu.Lock()
					scans[h] = b
					nexts[h] = next.begin
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		return errors.Join(errs...)
	}
	holeHeights := make([]int64, len(plans))
	for i, p := range plans {
		holeHeights[i] = p.Height
	}
	if err := read(holeHeights); err != nil {
		return sum, err
	}
	hashes := map[string]bool{}
	for _, p := range plans {
		for i, t := range scans[p.Height].txs {
			if t.Code == 0 || !t.Rejected || (preEvents(p.Height) && !anteOnlyFailure(t.Code, scans[p.Height].raw[i].Codespace)) {
				hashes[t.Hash] = true
			}
		}
	}
	rows, err := s.inclusionRows(ids, hashes)
	if err != nil {
		return sum, err
	}
	var held []int64
	for _, r := range rows {
		held = append(held, r.Height)
	}
	if err := read(held); err != nil {
		return sum, err
	}

	// A hash one copy of which got past the ante handler: every other copy
	// was turned away. Before v7 that is known only for a copy that ran, or
	// from the index row holding the messages' events, which only a copy
	// that ran emits.
	passed := map[string]bool{}
	for _, b := range scans {
		for _, t := range b.txs {
			if t.Code == 0 || !t.Rejected {
				passed[t.Hash] = true
			}
		}
	}
	for h, r := range rows {
		if r.Events {
			passed[h] = true
		}
	}
	for h, b := range scans {
		if !preEvents(h) {
			continue
		}
		for i, t := range b.txs {
			if t.Code != 0 && passed[t.Hash] {
				b.raw[i].Reincluded = true
			}
		}
		settlePreEventBlock(h, b, nexts[h])
	}

	heightsNeeded := map[int64]bool{}
	for _, p := range plans {
		heightsNeeded[p.Height] = true
	}
	blockIDs, err := s.blockIDs(ids, heightsNeeded)
	if err != nil {
		return sum, err
	}

	// Plan each block's placements.
	claimed := map[uint]int64{}
	now := time.Now().UTC()
	for _, p := range plans {
		b := scans[p.Height]
		distinct := map[string]bool{}
		for i, t := range b.txs {
			if distinct[t.Hash] {
				continue
			}
			distinct[t.Hash] = true
			if t.Rejected {
				continue
			}
			p.ChainTxs++
			if t.ChargeUnknown {
				p.Blockers = append(p.Blockers, "a failed transaction's fee outcome could not be settled")
				continue
			}
			r, ok := rows[t.Hash]
			if !ok {
				p.Blockers = append(p.Blockers, "a transaction the chain ran is not in the index")
				continue
			}
			if t.Code != 0 && r.Events {
				p.PlaceBlockers = append(p.PlaceBlockers, "the index row holds message events for a copy the chain failed")
				continue
			}
			if r.Height == p.Height {
				switch {
				case (r.Code == 0) != (t.Code == 0) || (t.Code != 0 && r.Code != int64(t.Code)):
					p.Blockers = append(p.Blockers, "a transaction is indexed at its block with another outcome")
				case t.Code == 0 && !r.Messages:
					p.Blockers = append(p.Blockers, "a transaction is indexed without its messages")
				case t.Code != 0 && !r.Resolved:
					p.Blockers = append(p.Blockers, "a failed transaction has no recorded fee outcome")
				case t.Code != 0 && r.Charged != t.Charged:
					p.Blockers = append(p.Blockers, "a failed transaction's recorded fee outcome differs from the chain")
				}
				continue
			}
			hb, ok := scans[r.Height]
			j := -1
			for k, c := range hb.txs {
				if ok && c.Hash == t.Hash {
					j = k
					break
				}
			}
			switch {
			case j < 0:
				p.PlaceBlockers = append(p.PlaceBlockers, "the index dates a transaction at a block that does not include it")
				continue
			case passedAnte(hb.txs[j]) || hb.txs[j].ChargeUnknown:
				p.PlaceBlockers = append(p.PlaceBlockers, "the block the index dates a transaction at also let it past the ante handler")
				continue
			case t.Code == 0 && !r.Events:
				p.PlaceBlockers = append(p.PlaceBlockers, "the index row holds no message events for the copy that ran")
				continue
			}
			if other, dup := claimed[r.ID]; dup && other != p.Height {
				p.PlaceBlockers = append(p.PlaceBlockers, "two blocks claim the same index row")
				continue
			}
			claimed[r.ID] = p.Height
			eff := TxInclusion{Chain: chain, Height: p.Height, TxIndex: i, Hash: t.Hash, TxID: r.ID, BlockID: blockIDs[p.Height], Code: int64(t.Code), Effective: true, Granter: t.Granter, RecordedAt: now}
			if t.Code != 0 {
				eff.Codespace, eff.Charged = b.raw[i].Codespace, t.Charged
			}
			ht := hb.txs[j]
			old := TxInclusion{Chain: chain, Height: r.Height, TxIndex: j, Hash: t.Hash, TxID: r.ID, BlockID: r.BlockID, Code: int64(ht.Code), Codespace: hb.raw[j].Codespace, RecordedAt: now}
			p.Effective = append(p.Effective, eff)
			p.Moved[t.Hash] = t.Accounts
			p.Held = append(p.Held, old)
			switch {
			case t.Code == 0 && r.Height < p.Height:
				p.Kinds[inclusionRanBefore]++
			case t.Code == 0:
				p.Kinds[inclusionRanAfter]++
			case preEvents(p.Height):
				p.Kinds[inclusionPrePaid]++
			default:
				p.Kinds[inclusionPaidFee]++
			}
		}
		p.Accepted = len(p.PlaceBlockers) == 0
	}

	// Check every account, with the accepted placements standing in for
	// the inclusion table. A block whose moved rows do not check out at
	// their old blocks is not accepted, which changes what the others read,
	// so the checks repeat until the accepted set holds.
	// A block leaves the accepted set when its moved rows fail at their old
	// blocks and never comes back, so the rounds end.
	var rates rateCache
	for round := 0; ; round++ {
		preview := &inclusionPreview{cleared: map[int64]bool{}}
		for _, p := range plans {
			if p.Accepted {
				preview.effective = append(preview.effective, p.Effective...)
			}
		}
		sv := &Server{db: s.db, oracle: s.oracle, native: s.native, preview: preview}
		if err := sv.checkInclusionPlans(chain, ids, node, plans, workers, &rates); err != nil {
			return sum, err
		}
		changed := false
		for _, p := range plans {
			if !p.Accepted {
				continue
			}
			for _, c := range p.Checks {
				if c.Height != p.Height && !c.OK {
					p.Accepted, changed = false, true
					p.PlaceBlockers = append(p.PlaceBlockers, "a moved transaction's accounts do not check out at the block the index dated it at")
					break
				}
			}
		}
		logf("repeated inclusions: check round %d done, accepted set changed=%v", round+1, changed)
		if !changed {
			break
		}
	}
	for _, p := range plans {
		p.Proven = p.Accepted && len(p.Blockers) == 0 && len(p.Checks) > 0
		for _, c := range p.Checks {
			if !c.OK {
				p.Proven = false
			}
		}
	}

	// Who would still be refused somewhere.
	named := map[string]bool{}
	for _, p := range plans {
		for _, a := range p.Named {
			named[a] = true
		}
	}
	stillHeld := map[string]bool{}
	var allHoles []IndexHoleAddress
	if err := s.db.Where("chain = ?", chain).Find(&allHoles).Error; err != nil {
		return sum, err
	}
	provenAt := map[int64]bool{}
	for _, p := range plans {
		if p.Proven {
			provenAt[p.Height] = true
		}
	}
	for _, a := range allHoles {
		if named[a.Address] && !provenAt[a.Height] && !cleared[a.Height] {
			stillHeld[a.Address] = true
		}
	}
	sum.NamedAccounts = len(named)
	sum.Unrefused = len(named) - len(stillHeld)

	for _, p := range plans {
		failed := 0
		for _, c := range p.Checks {
			sum.AccountChecks++
			if !c.OK {
				sum.AccountFailures++
				failed++
				sum.Blockers["account check: "+c.Reason]++
			}
		}
		for _, b := range append(append([]string{}, p.PlaceBlockers...), p.Blockers...) {
			sum.Blockers[b]++
		}
		if p.Accepted {
			sum.Accepted++
			sum.Placements += len(p.Effective)
			for k, v := range p.Kinds {
				sum.Kinds[k] += v
			}
		}
		if p.Proven {
			sum.Proven++
		}
		var heldAt []string
		for _, h := range p.Held {
			heldAt = append(heldAt, fmt.Sprint(h.Height))
		}
		logf("block %d: %d chain txs, %d placements %v (held at %s), %d named accounts, %d account checks failed, accepted=%v, clears=%v %s",
			p.Height, p.ChainTxs, len(p.Effective), p.Kinds, strings.Join(heldAt, ","), len(p.Named), failed, p.Accepted, p.Proven, strings.Join(append(append([]string{}, p.PlaceBlockers...), p.Blockers...), "; "))
		for i, e := range p.Effective {
			logf("block %d: would add inclusion tx %d hash %s code %d effective at %d (index %d), turned away at %d (index %d, code %d)",
				p.Height, e.TxID, e.Hash, e.Code, e.Height, e.TxIndex, p.Held[i].Height, p.Held[i].TxIndex, p.Held[i].Code)
		}
	}

	if backup != nil {
		enc := json.NewEncoder(backup)
		for _, p := range plans {
			if err := enc.Encode(p); err != nil {
				return sum, fmt.Errorf("write backup: %w", err)
			}
		}
		if err := backup.Sync(); err != nil {
			return sum, fmt.Errorf("persist backup: %w", err)
		}
	}

	// Rebuild named accounts of the compared holes the way the API answers
	// them, with the plan applied.
	if len(opts.Compare) > 0 {
		preview := &inclusionPreview{cleared: map[int64]bool{}}
		for h := range cleared {
			preview.cleared[h] = true
		}
		for _, p := range plans {
			if p.Accepted {
				preview.effective = append(preview.effective, p.Effective...)
			}
			if p.Proven {
				preview.cleared[p.Height] = true
			}
		}
		sv := &Server{db: s.db, oracle: s.oracle, native: s.native, preview: preview}
		rows, err := sv.compareHoles(chain, ids, node, plans, opts.Compare, max(opts.CompareAccounts, 1), logf)
		if err != nil {
			return sum, err
		}
		sum.Compared = rows
	}

	if !opts.Apply {
		return sum, nil
	}
	if err := inclusionsMigrate(s.db); err != nil {
		return sum, fmt.Errorf("migrate: %w", err)
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var writes []TxInclusion
		var clears []IndexHoleClearance
		for _, p := range plans {
			if !p.Accepted {
				continue
			}
			writes = append(writes, p.Effective...)
			writes = append(writes, p.Held...)
			if p.Proven {
				clears = append(clears, IndexHoleClearance{Chain: chain, Height: p.Height, ChainTxs: p.ChainTxs, Placed: len(p.Effective), Accounts: len(p.Named), ClearedAt: now})
			}
		}
		if len(writes) > 0 {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(writes, 500).Error; err != nil {
				return fmt.Errorf("record inclusions: %w", err)
			}
			for _, w := range writes {
				var got TxInclusion
				if err := tx.Where("chain = ? AND height = ? AND tx_index = ?", w.Chain, w.Height, w.TxIndex).Take(&got).Error; err != nil {
					return fmt.Errorf("read back inclusion at %d: %w", w.Height, err)
				}
				if got.Hash != w.Hash || got.TxID != w.TxID || got.Effective != w.Effective || got.Code != w.Code || got.Charged != w.Charged || got.Granter != w.Granter {
					return fmt.Errorf("an inclusion already recorded at %d index %d differs from the plan", w.Height, w.TxIndex)
				}
			}
		}
		if len(clears) > 0 {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&clears).Error; err != nil {
				return fmt.Errorf("record clearances: %w", err)
			}
		}
		// Prove again from the rows just written, inside the transaction,
		// before anything commits.
		sv := &Server{db: tx, oracle: s.oracle, native: s.native}
		var proven []*inclusionHole
		for _, p := range plans {
			if p.Proven {
				q := *p
				q.Checks = nil
				proven = append(proven, &q)
			}
		}
		// One worker: a transaction holds a single connection.
		if err := sv.checkInclusionPlans(chain, ids, node, proven, 1, &rates); err != nil {
			return err
		}
		for _, p := range proven {
			for _, c := range p.Checks {
				if !c.OK {
					return fmt.Errorf("block %d no longer checks out once written (%s); nothing committed", p.Height, c.Reason)
				}
			}
		}
		logf("repeated inclusions: recorded %d inclusions and %d clearances", len(writes), len(clears))
		return nil
	})
	return sum, err
}

// inclusionRows reads the txes rows holding these hashes.
func (s *Server) inclusionRows(ids []int64, hashes map[string]bool) (map[string]inclusionRow, error) {
	list := make([]string, 0, len(hashes))
	for h := range hashes {
		list = append(list, h)
	}
	sort.Strings(list)
	out := map[string]inclusionRow{}
	for lo := 0; lo < len(list); lo += 500 {
		var rows []inclusionRow
		if err := s.db.Raw(`
SELECT x.id, x.hash, x.code, x.block_id, b.height,
       EXISTS (SELECT 1 FROM messages m WHERE m.tx_id = x.id) AS messages,
       EXISTS (SELECT 1 FROM messages m JOIN message_events me ON me.message_id = m.id WHERE m.tx_id = x.id) AS events,
       o.tx_id IS NOT NULL AS resolved, COALESCE(o.charged, '') AS charged
FROM txes x
JOIN blocks b ON b.id = x.block_id
LEFT JOIN tax_failed_tx_fees o ON o.tx_id = x.id
WHERE x.hash IN @hashes AND b.chain_id IN @ids`,
			map[string]any{"hashes": list[lo:min(lo+500, len(list))], "ids": ids}).Scan(&rows).Error; err != nil {
			return nil, fmt.Errorf("index rows: %w", err)
		}
		for _, r := range rows {
			out[strings.ToUpper(r.Hash)] = r
		}
	}
	return out, nil
}

func (s *Server) blockIDs(ids []int64, heights map[int64]bool) (map[int64]uint, error) {
	list := make([]int64, 0, len(heights))
	for h := range heights {
		list = append(list, h)
	}
	var rows []struct {
		ID     uint
		Height int64
	}
	if err := s.db.Raw("SELECT id, height FROM blocks WHERE chain_id IN @ids AND height IN @hs", map[string]any{"ids": ids, "hs": list}).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := map[int64]uint{}
	for _, r := range rows {
		out[r.Height] = r.ID
	}
	for _, h := range list {
		if out[h] == 0 {
			return nil, fmt.Errorf("block %d is not in the index", h)
		}
	}
	return out, nil
}

// rateCache keeps validator rates and account state read from the node per
// height, since the check rounds read the same ones again.
type rateCache struct {
	mu      sync.Mutex
	m       map[int64]map[string]validatorRate
	anchors map[string]ledgerAnchor
}

func (c *rateCache) anchor(node *ledgerNode, addr string, h int64) (ledgerAnchor, error) {
	k := fmt.Sprintf("%s@%d", addr, h)
	c.mu.Lock()
	a, ok := c.anchors[k]
	c.mu.Unlock()
	if ok {
		return a, nil
	}
	a, err := node.anchor(addr, h)
	if err != nil {
		return a, err
	}
	c.mu.Lock()
	if c.anchors == nil {
		c.anchors = map[string]ledgerAnchor{}
	}
	c.anchors[k] = a
	c.mu.Unlock()
	return a, nil
}

func (c *rateCache) at(node *ledgerNode, h int64) (map[string]validatorRate, error) {
	c.mu.Lock()
	r, ok := c.m[h]
	c.mu.Unlock()
	if ok {
		return r, nil
	}
	r, err := node.validatorRates(h)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.m == nil {
		c.m = map[int64]map[string]validatorRate{}
	}
	c.m[h] = r
	c.mu.Unlock()
	return r, nil
}

// checkInclusionPlans runs each plan's account checks: the hole's named
// accounts across its block, and the accounts each moved transaction names
// across the block it was dated at.
func (s *Server) checkInclusionPlans(chain string, ids []int64, node *ledgerNode, plans []*inclusionHole, workers int, rates *rateCache) error {
	type job struct {
		plan *inclusionHole
		h    int64
		addr string
	}
	var jobs []job
	for _, p := range plans {
		p.Checks = nil
		for _, a := range p.Named {
			jobs = append(jobs, job{p, p.Height, a})
		}
		if !p.Accepted {
			continue
		}
		for _, old := range p.Held {
			seen := map[string]bool{}
			for _, t := range p.Effective {
				if t.TxID != old.TxID {
					continue
				}
				for _, a := range p.Moved[t.Hash] {
					if !seen[a] {
						seen[a] = true
						jobs = append(jobs, job{p, old.Height, a})
					}
				}
			}
		}
	}
	results := make([]accountCheck, len(jobs))
	errs := make([]error, len(jobs))
	ch := make(chan int, len(jobs))
	for i := range jobs {
		ch <- i
	}
	close(ch)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				j := jobs[i]
				reason, err := s.checkAccountAcross(chain, ids, node, j.addr, j.h, rates)
				results[i] = accountCheck{Height: j.h, Account: j.addr, OK: reason == "" && err == nil, Reason: reason}
				errs[i] = err
			}
		}()
	}
	wg.Wait()
	for i, j := range jobs {
		if errs[i] != nil {
			results[i].Reason = "node read failed: " + errs[i].Error()
		}
		j.plan.Checks = append(j.plan.Checks, results[i])
	}
	return nil
}

// checkAccountAcross rebuilds addr from the chain's state at h back across
// block h and compares the result with the chain's state at h-1. It returns
// a reason when they differ.
func (s *Server) checkAccountAcross(chain string, ids []int64, node *ledgerNode, addr string, h int64, rates *rateCache) (string, error) {
	at, err := rates.anchor(node, addr, h)
	if err != nil {
		return "", err
	}
	if err := s.db.Raw("SELECT time_stamp FROM blocks WHERE chain_id IN @ids AND height = @h", map[string]any{"ids": ids, "h": h}).Scan(&at.Time).Error; err != nil {
		return "", err
	}
	if at.Time.IsZero() {
		return "block time unknown", nil
	}
	before, err := rates.anchor(node, addr, h-1)
	if err != nil {
		return "", err
	}
	held, reason, err := s.walkAcross(chain, ids, node, addr, at, []int64{h - 1}, rates)
	if err != nil || reason != "" {
		return reason, err
	}
	return holdingsDiffer(held[h-1], before), nil
}

// walkAcross walks addr back from anchor to the targets through the
// ledger's own reads, as rebuildHoldings does after its coverage checks.
func (s *Server) walkAcross(chain string, ids []int64, node *ledgerNode, addr string, anchor ledgerAnchor, targets []int64, rates *rateCache) (map[int64]ledgerHoldings, string, error) {
	operator, _ := AccountOperator(addr)
	lowest := anchor.Height
	for _, t := range targets {
		lowest = min(lowest, t)
	}
	events, unattributed, err := s.ledgerEvents(ids, addr, operator, lowest, anchor.Height)
	if err != nil {
		return nil, "", err
	}
	if unattributed > 0 {
		return nil, "a redelegation cannot be attributed", nil
	}
	fees, unknown, err := s.ledgerFees(ids, addr, lowest, anchor.Height, nil)
	if err != nil {
		return nil, "", err
	}
	if unknown > 0 {
		return nil, "a failed transaction has no recorded fee outcome", nil
	}
	var slashes []ledgerSlash
	if needsSlashes(anchor, events) {
		if anchor.Rates, err = rates.at(node, anchor.Height); err != nil {
			return nil, "", err
		}
		if slashes, err = s.ledgerSlashes(ids, lowest, anchor.Height, node, anchor.Height, touchedValidators(anchor, events)); err != nil {
			var inc balanceIncomplete
			if errors.As(err, &inc) {
				return nil, "slashes: " + inc.reason, nil
			}
			return nil, "", err
		}
	}
	samples, err := s.rateSamples(chain, sampledValidators(anchor, events, slashes), lowest, anchor.Height)
	if err != nil {
		return nil, "", err
	}
	held, err := walkBack(ledgerInput{Address: addr, Operator: operator, BondDenom: s.native.Denom, Anchor: anchor, Events: events, Fees: fees, Slashes: slashes, Samples: samples}, targets)
	if err != nil {
		var inc ledgerInconsistent
		var unk ledgerUnknown
		switch {
		case errors.As(err, &inc):
			return nil, "the rebuilt history goes negative", nil
		case errors.As(err, &unk):
			return nil, "a delegation's rate is unknown", nil
		}
		return nil, "", err
	}
	return held, "", nil
}

// holdingsDiffer compares rebuilt holdings with the chain's state at the same
// block: liquid coins exactly, delegations and unbonding entries within the
// bound the rebuild gives. It names what differs, without amounts.
func holdingsDiffer(got ledgerHoldings, want ledgerAnchor) string {
	bank := pruneZero(copyInts(want.Bank))
	if len(bank) != len(got.Liquid) {
		return "liquid denoms differ"
	}
	for d, v := range bank {
		if g, ok := got.Liquid[d]; !ok || g.Cmp(v) != 0 {
			return "a liquid balance differs"
		}
	}
	within := func(a, b, bound *big.Int) bool {
		d := new(big.Int).Sub(a, b)
		d.Abs(d)
		if bound == nil {
			return d.Sign() == 0
		}
		return d.Cmp(bound) <= 0
	}
	zero := big.NewInt(0)
	or0 := func(v *big.Int) *big.Int {
		if v == nil {
			return zero
		}
		return v
	}
	vals := map[string]bool{}
	for v := range want.Delegations {
		vals[v] = true
	}
	for v := range got.Delegations {
		vals[v] = true
	}
	for v := range vals {
		if !within(or0(got.Delegations[v]), or0(want.Delegations[v]), got.DelegationError[v]) {
			return "a delegation differs"
		}
	}
	unb := map[string]*big.Int{}
	for _, e := range want.Unbonding {
		addInt(unb, e.Validator, e.Balance)
	}
	vals = map[string]bool{}
	for v := range unb {
		vals[v] = true
	}
	for v := range got.Unbonding {
		vals[v] = true
	}
	for v := range vals {
		if !within(or0(got.Unbonding[v]), or0(unb[v]), got.UnbondingError[v]) {
			return "an unbonding entry differs"
		}
	}
	return ""
}

// compareHoles rebuilds up to n named accounts of each compared hole at the
// block before it and at it, through rebuildHoldings as the API does, and
// compares each with the chain's state there. Accounts with the fewest
// indexed fee payments are tried first, since the walk starts at the tip.
func (s *Server) compareHoles(chain string, ids []int64, node *ledgerNode, plans []*inclusionHole, heights []int64, n int, logf func(string, ...any)) ([]compareRow, error) {
	byHeight := map[int64]*inclusionHole{}
	for _, p := range plans {
		byHeight[p.Height] = p
	}
	var out []compareRow
	for _, h := range heights {
		p, ok := byHeight[h]
		if !ok {
			logf("compare %d: not a hole in this run", h)
			continue
		}
		type cand struct {
			addr string
			fees int64
		}
		var cands []cand
		for _, a := range p.Named {
			var c int64
			if err := s.db.Raw("SELECT count(*) FROM fees f JOIN addresses ad ON ad.id = f.payer_address_id WHERE ad.address = ?", a).Scan(&c).Error; err != nil {
				return nil, err
			}
			cands = append(cands, cand{a, c})
		}
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].fees < cands[j].fees })
		var times []heightTime
		if err := s.db.Raw("SELECT height, time_stamp FROM blocks WHERE chain_id IN @ids AND height IN @hs ORDER BY height", map[string]any{"ids": ids, "hs": []int64{h - 1, h}}).Scan(&times).Error; err != nil {
			return nil, err
		}
		if len(times) != 2 {
			return nil, fmt.Errorf("compare %d: blocks %d and %d are not both indexed", h, h-1, h)
		}
		answered := 0
		for i, c := range cands {
			if answered >= n {
				break
			}
			got, _, err := s.rebuildHoldings(chain, ids, node, c.addr, times)
			if err != nil {
				return nil, fmt.Errorf("compare %d account %d: %w", h, i, err)
			}
			row := compareRow{Height: h, Account: i}
			for k, ht := range times {
				res := "match"
				switch {
				case got[k].Status != "":
					res = "refused: " + strings.ReplaceAll(got[k].Reason, c.addr, "<addr>")
				default:
					want, err := node.anchor(c.addr, ht.Height)
					if err != nil {
						return nil, err
					}
					if d := holdingsDiffer(got[k].Holdings, want); d != "" {
						res = "differs: " + d
					}
				}
				if k == 0 {
					row.Before = res
				} else {
					row.At = res
				}
			}
			if got[0].Status == "" && got[1].Status == "" {
				answered++
			}
			logf("compare %d account %d of %d: at %d %s; at %d %s", h, i+1, len(cands), h-1, row.Before, h, row.At)
			out = append(out, row)
		}
	}
	return out, nil
}
