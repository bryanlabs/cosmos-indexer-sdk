package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/core"
	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	"github.com/DefiantLabs/cosmos-indexer/legacy/genesis"
	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Full-chain staging. One sealed component is one unit of import. A component
// whose heights are all stored is checked by its counts and bound source
// digest and is not read again. A component that is absent or partly stored is
// streamed through the comparing writer, which inserts what is missing and
// refuses any stored height that differs. The stored rows of a component
// depend only on that component, never on the manifest that listed it, so
// adding components one at a time gives the same database as one run.

const chainID = "cosmoshub-2"

var stagingName = regexp.MustCompile(`^legacy_replay_rehearsal_hub2_[a-z0-9_]{4,40}$`)

// guardTarget admits only a private loopback staging name and port. 15432 and
// 5432 are the customary forwards to the serving database.
func guardTarget(name string, p int) error {
	if !stagingName.MatchString(name) || p < 1024 || p > 65535 || p == 15432 || p == 5432 {
		return refused
	}
	return nil
}

// why prints the refused check to the private log. It never prints an
// address, an amount or a path.
func why(format string, a ...any) error {
	fmt.Fprintf(os.Stderr, "refused: "+format+"\n", a...)
	return refused
}

type stored struct{ Blocks, Rows, Transactions, Events, Bound int64 }

func (c Component) width() int64 { return c.Last - c.First + 1 }

// wantEvents is one source event and one receipt per height, plus the pinned
// export records on the canonical height.
func wantEvents(m Manifest, c Component) int64 {
	n := 2 * c.width()
	if m.CanonicalExport != nil && c.Last == canonicalHeight {
		n++
	}
	return n
}

func componentStored(ctx context.Context, q *sql.Conn, c Component) (stored, error) {
	var s stored
	e := q.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM legacy.blocks WHERE chain_id=$1 AND height BETWEEN $2 AND $3),
 (SELECT count(*) FROM legacy.taxable_events WHERE chain_id=$1 AND height BETWEEN $2 AND $3 AND phase<>4),
 (SELECT count(*) FROM legacy.tx_inclusions WHERE chain_id=$1 AND height BETWEEN $2 AND $3),
 (SELECT count(*) FROM legacy.block_events WHERE chain_id=$1 AND height BETWEEN $2 AND $3),
 (SELECT count(*) FROM legacy.block_events WHERE chain_id=$1 AND height BETWEEN $2 AND $3 AND phase=1 AND event_index=2 AND type='pinned_replay_receipt' AND position($4 in attributes::text)>0)`,
		chainID, c.First, c.Last, c.RecordsSHA).Scan(&s.Blocks, &s.Rows, &s.Transactions, &s.Events, &s.Bound)
	return s, e
}

func complete(m Manifest, c Component, s stored) bool {
	return s.Blocks == c.width() && s.Rows == c.HistoricalRows && s.Transactions == c.Transactions && s.Events == wantEvents(m, c) && s.Bound == c.width()
}

func componentReceipt(m Manifest, c Component, opening string) string {
	raw, _ := json.Marshal(map[string]any{"component_first": c.First, "component_last": c.Last, "records_sha256": c.RecordsSHA, "status_sha256": m.Pins[c.Status], "cache_manifest_sha256": m.Pins[c.CacheManifest], "source_commit": m.SourceCommit, "native_zeroheight_exports": "validation_only_excluded_from_historical_rows", "canonical_restart_payout_height": canonicalHeight, "genuine_opening_sha256": opening})
	return string(raw)
}

type componentResult struct {
	First          int64   `json:"first"`
	Last           int64   `json:"last"`
	Heights        int64   `json:"heights"`
	HistoricalRows int64   `json:"historical_rows"`
	Transactions   int64   `json:"transactions"`
	Events         int64   `json:"events"`
	PayoutRows     int64   `json:"canonical_restart_payout_rows"`
	NewHeights     int64   `json:"new_heights"`
	Seconds        float64 `json:"seconds"`
	Readback       bool    `json:"every_batch_exact_readback"`
	firstPrevious  string
}

// loadExport reads the canonical export once. Stream checks the whole digest
// before its callback.
func loadExport(x *CanonicalExport) (*replayrecords.Height, error) {
	f, e := os.Open(x.Records)
	if e != nil {
		return nil, refused
	}
	defer f.Close()
	var out *replayrecords.Height
	if e = replayrecords.Stream(f, x.Proof, func(h replayrecords.Height) error { out = &h; return nil }); e != nil || out == nil || out.Number != canonicalHeight {
		return nil, refused
	}
	var movements int64
	for _, r := range out.Records {
		if r.Kind == "movement" {
			movements++
		}
	}
	if movements != x.Rows {
		return nil, refused
	}
	return out, nil
}

func importComponent(ctx context.Context, conn *sql.Conn, m Manifest, c Component, opening string, export *replayrecords.Height, fresh, check bool, started time.Time) (componentResult, error) {
	began := time.Now()
	out := componentResult{First: c.First, Last: c.Last}
	p, gas, e := admission(c)
	if e != nil {
		return out, why("component %d admission", c.Last)
	}
	expected, e := cachePins(c)
	if e != nil {
		return out, why("component %d cache manifest", c.Last)
	}
	f, e := os.Open(c.Records)
	if e != nil {
		return out, refused
	}
	defer f.Close()
	var reader io.ReadSeeker = f
	if c.Prefix {
		reader = io.NewSectionReader(f, 0, c.RecordBytes)
	}
	receipt := componentReceipt(m, c, opening)
	batch := []ledger.Block{}
	parent := ""
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		n, e := ledger.Write(ctx, conn, batch)
		if check {
			// Reading back only: every height must already be stored and
			// equal to what the sealed files give. The session is read-only.
			if e != nil || n != 0 {
				return why("component %d height %d and up: stored rows differ from the sealed source", c.Last, batch[0].Height)
			}
			batch = nil
			return nil
		}
		if e != nil || (fresh && n != len(batch)) {
			return why("component %d write at height %d: stored differs or short insert", c.Last, batch[0].Height)
		}
		out.NewHeights += int64(n)
		// The second pass inserts nothing and compares every field read back.
		n, e = ledger.Write(ctx, conn, batch)
		if e != nil || n != 0 {
			return why("component %d readback at height %d", c.Last, batch[0].Height)
		}
		batch = nil
		return nil
	}
	e = replayrecords.Stream(reader, p, func(r replayrecords.Height) error {
		if checkHeight(c, r.Number, expected) != nil {
			return why("height %d cache pin", r.Number)
		}
		if gas != nil && gas.CheckCachedFiles(c.Cache, r.Number) != nil {
			return refused
		}
		h, e := capture.LoadHeight(c.Cache, chainID, r.Number, 64<<20)
		if e != nil || parent != "" && h.Previous != parent {
			return why("height %d capture or parent link", r.Number)
		}
		if r.Number == c.First {
			out.firstPrevious = h.Previous
		}
		if gas != nil {
			if h.Results.Legacy == nil {
				return refused
			}
			raw := make([][]byte, len(h.Block.Block.Txs))
			for i, t := range h.Block.Block.Txs {
				raw[i] = t
			}
			if gas.CheckCachedInputs(r.Number, raw, h.Results.Legacy.RawJSON) != nil {
				return refused
			}
		}
		items, e := core.DecodeLegacyBlock(h.Block, h.Results)
		if e != nil {
			return why("height %d decode", r.Number)
		}
		b, e := core.LegacyReplayBlock(h, items, r)
		if e != nil {
			return why("height %d reconcile", r.Number)
		}
		for _, row := range b.Rows {
			if row.Phase == ledger.PhaseExport || row.Source == "export_payout" || row.Trigger == "zero_height_export" {
				return why("height %d export row in historical records", r.Number)
			}
		}
		out.HistoricalRows += int64(len(b.Rows))
		attributes := [][2]string{{"json", receipt}}
		if export != nil && r.Number == canonicalHeight {
			// The restart payout enters here and nowhere else. The ledger key
			// (chain, height, phase 4, position) cannot hold it twice.
			before := len(b.Rows)
			if b, e = core.LegacyReplayExport(b, *export); e != nil {
				return why("canonical export held")
			}
			out.PayoutRows = int64(len(b.Rows) - before)
			exportReceipt, _ := json.Marshal(m.CanonicalExport.Proof)
			attributes = append(attributes, [2]string{"export_json", string(exportReceipt)})
		}
		b.Events = append(b.Events, ledger.Event{Phase: ledger.PhaseBegin, Index: 2, Type: "pinned_replay_receipt", Attributes: attributes})
		if checkHeight(c, r.Number, expected) != nil {
			return why("height %d cache pin after use", r.Number)
		}
		parent = h.Hash
		out.Heights++
		out.Transactions += int64(len(b.Inclusions))
		out.Events += int64(len(b.Events))
		batch = append(batch, b)
		if len(batch) == 500 {
			if flush() != nil {
				return refused
			}
			if r.Number%20000 == 0 {
				fmt.Printf("progress component=%d height=%d elapsed_seconds=%.0f\n", c.Last, r.Number, time.Since(started).Seconds())
			}
		}
		return nil
	})
	if e != nil || flush() != nil {
		return out, why("component %d stream", c.Last)
	}
	wantPayout := int64(0)
	if export != nil && c.Last == canonicalHeight {
		wantPayout = m.CanonicalExport.Rows
	}
	if out.Heights != c.width() || out.HistoricalRows != c.HistoricalRows || out.Transactions != c.Transactions || out.PayoutRows != wantPayout {
		return out, why("component %d totals differ from its seal", c.Last)
	}
	out.Seconds = time.Since(began).Seconds()
	out.Readback = true
	return out, nil
}

func blockHash(ctx context.Context, conn *sql.Conn, height int64) (string, error) {
	var h string
	e := conn.QueryRowContext(ctx, `SELECT block_hash FROM legacy.blocks WHERE chain_id=$1 AND height=$2`, chainID, height).Scan(&h)
	return h, e
}

// importTail stores the two heights after the export as headers and
// transactions. They carry no replay records and produce no tax row.
func importTail(ctx context.Context, conn *sql.Conn, m Manifest, check bool) (int64, error) {
	t := m.Tail
	expected, e := cachePins(Component{CacheManifest: t.CacheManifest})
	if e != nil {
		return 0, why("tail cache manifest")
	}
	parent, e := blockHash(ctx, conn, canonicalHeight)
	if e != nil {
		return 0, why("tail without stored canonical height")
	}
	receipt, _ := json.Marshal(map[string]any{"historical_tail": "2902001..2902002", "tax_rows": "none_tail_is_after_the_canonical_export", "cache_manifest_sha256": m.Pins[t.CacheManifest], "tail_seal_sha256": m.Pins[t.Seal], "source_commit": m.SourceCommit})
	var blocks []ledger.Block
	for n := t.First; n <= t.Last; n++ {
		c := Component{Cache: t.Cache}
		if checkHeight(c, n, expected) != nil {
			return 0, why("tail height %d cache pin", n)
		}
		h, e := capture.LoadHeight(t.Cache, chainID, n, 64<<20)
		if e != nil || h.Previous != parent || h.Block.Block.ChainID != chainID || h.Block.Block.Height != n {
			return 0, why("tail height %d capture or parent link", n)
		}
		items, e := core.DecodeLegacyBlock(h.Block, h.Results)
		if e != nil || len(items) != len(h.Block.Block.Txs) {
			return 0, why("tail height %d decode", n)
		}
		b := ledger.Block{Chain: chainID, Height: n, Time: h.Block.Block.Time, Hash: h.Hash, Proposer: h.Proposer}
		for i, item := range items {
			if item.Tx == nil || item.Result == nil || len(item.Tx.Issues) != 0 || !item.Tx.SignersComplete || len(item.Tx.Signers) == 0 || (item.Fee.Charge != "charged" && item.Fee.Charge != "not charged") {
				return 0, why("tail height %d transaction %d not decidable without replay records", n, i)
			}
			inc := ledger.Inclusion{Index: i, Hash: item.Hash, Code: item.Result.Code, Codespace: item.Result.Codespace, CodeEvidence: item.CodeEvidence, GasWanted: item.Result.GasWanted, GasUsed: item.Result.GasUsed, Fee: item.Fee.Declared.String(), FeePayer: item.Fee.Payer.String(), FeeCharged: item.Fee.Charge == "charged", FeeEvidence: item.Fee.Evidence, Memo: ledger.Text(item.Tx.Memo), Raw: bytes.Clone(h.Block.Block.Txs[i])}
			for _, s := range item.Tx.Signers {
				inc.Signers = append(inc.Signers, s.String())
			}
			b.Inclusions = append(b.Inclusions, inc)
		}
		b.Events = []ledger.Event{{Phase: ledger.PhaseBegin, Index: 2, Type: "pinned_tail_receipt", Attributes: [][2]string{{"json", string(receipt)}}}}
		parent = h.Hash
		blocks = append(blocks, b)
	}
	if n, e := ledger.Write(ctx, conn, blocks); e != nil || (check && n != 0) {
		return 0, why("tail write or stored tail differs from its captures")
	}
	if n, e := ledger.Write(ctx, conn, blocks); e != nil || n != 0 {
		return 0, why("tail readback")
	}
	return int64(len(blocks)), nil
}

// runAdd imports every listed component that is not yet completely stored.
// With incremental false the destination must be empty, as the rehearsal was.
func runAdd(m Manifest, out string, incremental bool, parallel int) error {
	executable, e := os.Executable()
	if e != nil || fileSHA(executable) != m.BinarySHA {
		return why("binary is not the one the manifest pins")
	}
	if pins(m) != nil {
		return why("manifest layout or a pinned file changed")
	}
	if parallel < 1 || parallel > 4 {
		return refused
	}
	ctx := context.Background()
	for _, c := range m.Components {
		if _, _, e := admission(c); e != nil {
			return why("component %d admission", c.Last)
		}
	}
	var export *replayrecords.Height
	if m.CanonicalExport != nil {
		if export, e = loadExport(m.CanonicalExport); e != nil {
			return why("canonical export records")
		}
	}
	if os.Mkdir(out, 0700) != nil {
		return why("receipt directory must be new")
	}
	d, e := openStagingDestination(ctx, dsn(importerRole), database, incremental)
	if e != nil {
		return why("destination identity, confinement, lock or emptiness")
	}
	defer d.close()
	var beyond, foreign int64
	limit := m.Last
	if m.Tail != nil {
		limit = m.Tail.Last
	}
	if d.conn.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE chain_id=$1 AND height>$2), count(*) FILTER (WHERE chain_id<>$1) FROM legacy.blocks`, chainID, limit).Scan(&beyond, &foreign) != nil || beyond != 0 || foreign != 0 {
		return why("destination holds heights this manifest does not list")
	}
	guard, e := gorm.Open(postgres.New(postgres.Config{Conn: d.conn}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		return refused
	}
	gf, e := os.Open(m.Genesis)
	if e != nil {
		return refused
	}
	opening, e := genesis.Read(gf)
	gf.Close()
	if e != nil || opening.Chain != chainID || opening.SHA256 != m.Pins[m.Genesis] {
		return why("genuine genesis")
	}
	// Load inserts the genesis once and otherwise compares the stored copy
	// with the file, row for row.
	gr, e := genesis.Load(guard, opening)
	if e != nil || (!incremental && gr.AlreadyLoaded) || !reflect.DeepEqual(gr.File, gr.Stored) {
		return why("genesis load or stored genesis differs")
	}
	started := time.Now()
	type job struct {
		index int
		fresh bool
	}
	var jobs []job
	var kept []int64
	for i, c := range m.Components {
		s, e := componentStored(ctx, d.conn, c)
		if e != nil {
			return refused
		}
		if complete(m, c, s) {
			kept = append(kept, c.Last)
			continue
		}
		jobs = append(jobs, job{i, s.Blocks == 0})
	}
	results := make([]*componentResult, len(m.Components))
	work := make(chan job)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed error
	for w := 0; w < parallel; w++ {
		// The advisory lock stays on the admission session. Each worker
		// writes on its own session of the same guarded private pool.
		conn := d.conn
		if parallel > 1 {
			if conn, e = d.pool.Conn(ctx); e != nil {
				return refused
			}
			defer conn.Close()
		}
		wg.Add(1)
		go func(conn *sql.Conn) {
			defer wg.Done()
			for j := range work {
				mu.Lock()
				stop := failed != nil
				mu.Unlock()
				if stop {
					continue
				}
				c := m.Components[j.index]
				var x *replayrecords.Height
				if c.Last == canonicalHeight {
					x = export
				}
				r, e := importComponent(ctx, conn, m, c, opening.SHA256, x, j.fresh, false, started)
				mu.Lock()
				if e != nil {
					failed = e
				} else {
					results[j.index] = &r
					if save(filepath.Join(out, fmt.Sprintf("component-%d.json", c.Last)), r) != nil {
						failed = refused
					}
				}
				mu.Unlock()
			}
		}(conn)
	}
	for _, j := range jobs {
		work <- j
	}
	close(work)
	wg.Wait()
	if failed != nil {
		return why("a component failed; completed components stay stored and are checked again on the next run")
	}
	// Seams: the first height of each newly read component must name the
	// stored last block of the component before it.
	var added []componentResult
	for i, r := range results {
		if r == nil {
			continue
		}
		if c := m.Components[i]; c.First > 1 {
			h, e := blockHash(ctx, d.conn, c.First-1)
			if e != nil || h == "" || h != r.firstPrevious {
				return why("seam before component %d", c.Last)
			}
		}
		added = append(added, *r)
	}
	var tail int64
	if m.Tail != nil {
		if tail, e = importTail(ctx, d.conn, m, false); e != nil {
			return refused
		}
	}
	if pins(m) != nil {
		return why("a pinned file changed during the import")
	}
	check, e := verifyStaged(ctx, d.conn, m)
	if e != nil {
		return e
	}
	check["components_already_stored_and_rechecked"] = kept
	check["components_read_this_run"] = added
	check["tail_heights_written_or_compared"] = tail
	check["seconds"] = time.Since(started).Seconds()
	check["genesis_exact_readback"] = gr
	check["parallel_components"] = parallel
	check["manifest_sha256"] = fileSHA(manifestPath)
	check["production_access"] = false
	check["whole_chain_books_tax_accepted"] = false
	return save(filepath.Join(out, "import-result.json"), check)
}

// verifyStaged is the whole-database acceptance of the staged prefix: every
// component complete and bound, no gap or duplicate, the restart payout only
// at the canonical height and with the sealed count, and no tax row after it.
func verifyStaged(ctx context.Context, conn *sql.Conn, m Manifest) (map[string]any, error) {
	var heights, rows, txs, events int64
	for _, c := range m.Components {
		s, e := componentStored(ctx, conn, c)
		if e != nil || !complete(m, c, s) {
			return nil, why("component %d stored counts or source binding", c.Last)
		}
		heights += s.Blocks
		rows += s.Rows
		txs += s.Transactions
		events += s.Events
	}
	want, last := m.Last, m.Last
	if m.Tail != nil {
		want, last = m.Tail.Last, m.Tail.Last
	}
	var n, distinct, low, high, bad int64
	if conn.QueryRowContext(ctx, `SELECT count(*),count(DISTINCT height),coalesce(min(height),0),coalesce(max(height),0) FROM legacy.blocks WHERE chain_id=$1`, chainID).Scan(&n, &distinct, &low, &high) != nil || n != want || distinct != n || low != 1 || high != last {
		return nil, why("header gap, duplicate or unexpected height")
	}
	if conn.QueryRowContext(ctx, `SELECT count(*) FROM legacy.blocks b WHERE b.chain_id=$1 AND b.tx_count<>(SELECT count(*) FROM legacy.tx_inclusions t WHERE t.chain_id=b.chain_id AND t.height=b.height)`, chainID).Scan(&bad) != nil || bad != 0 {
		return nil, why("transaction count differs from header")
	}
	var misplaced, payout, afterExport int64
	if conn.QueryRowContext(ctx, `SELECT
 count(*) FILTER (WHERE (height<=0 OR phase=4 OR amount_source='export_payout' OR reward_trigger='zero_height_export') AND NOT (height=$2 AND phase=4 AND amount_source='export_payout' AND reward_trigger='zero_height_export')),
 count(*) FILTER (WHERE height=$2 AND phase=4 AND amount_source='export_payout' AND reward_trigger='zero_height_export'),
 count(*) FILTER (WHERE height>$2)
 FROM legacy.taxable_events WHERE chain_id=$1`, chainID, canonicalHeight).Scan(&misplaced, &payout, &afterExport) != nil {
		return nil, refused
	}
	wantPayout := int64(0)
	if m.CanonicalExport != nil {
		wantPayout = m.CanonicalExport.Rows
	}
	if misplaced != 0 || payout != wantPayout || afterExport != 0 {
		return nil, why("export payout misplaced, repeated or missing, or a tax row after the export")
	}
	var other int64
	if conn.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM legacy.import_ranges)+(SELECT count(*) FROM legacy.reconciliation)+(SELECT count(*) FROM legacy.balance_days)+(SELECT count(*) FROM legacy.eras WHERE published)`).Scan(&other) != nil || other != 0 {
		return nil, why("derived tables or a published era present")
	}
	return map[string]any{"first": 1, "last": last, "canonical_through": m.Last, "heights": n, "historical_rows": rows, "transactions": txs, "events": events, "canonical_restart_payout_rows": payout, "intermediate_zeroheight_rows": misplaced, "tax_rows_after_export": afterExport, "components": len(m.Components), "complete_chain": m.Last == canonicalHeight && m.Tail != nil}, nil
}

// fingerprint hashes every legacy table in primary key order. loaded_at is
// the wall clock of the genesis insert and is the only column left out.
// through limits the height-keyed tables so a longer database can be
// compared with a shorter one.
func fingerprint(out string, through int64) error {
	db, e := owner()
	if e != nil {
		return refused
	}
	pool, e := db.DB()
	if e != nil {
		return refused
	}
	defer pool.Close()
	ctx := context.Background()
	tx, e := pool.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if e != nil {
		return refused
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO, YMD'`); e != nil {
		return refused
	}
	keys := [][2]string{{"balance_days", "chain_id,address,day"}, {"block_events", "chain_id,height,phase,event_index"}, {"blocks", "chain_id,height"}, {"eras", "chain_id"}, {"genesis_accounts", "chain_id,address"}, {"genesis_delegations", "chain_id,delegator,validator"}, {"genesis_files", "chain_id"}, {"genesis_unbondings", "chain_id,delegator,validator,entry"}, {"genesis_validators", "chain_id,operator"}, {"genesis_withdraw_addresses", "chain_id,delegator"}, {"import_ranges", "chain_id,from_height"}, {"reconciliation", "chain_id,address,component"}, {"taxable_events", "chain_id,height,phase,tx_index,msg_index,sub_index"}, {"tx_inclusions", "chain_id,height,tx_index"}}
	result := map[string]any{}
	all := sha256.New()
	for _, k := range keys {
		where := ""
		if through > 0 && strings.Contains(k[1], ",height") {
			where = fmt.Sprintf(" WHERE height<=%d", through)
		}
		rows, e := tx.QueryContext(ctx, "SELECT (to_jsonb(r)-'loaded_at')::text FROM legacy."+k[0]+" r"+where+" ORDER BY "+k[1])
		if e != nil {
			return refused
		}
		h := sha256.New()
		w := bufio.NewWriterSize(h, 1<<20)
		var n int64
		for rows.Next() {
			var line string
			if rows.Scan(&line) != nil {
				rows.Close()
				return refused
			}
			w.WriteString(line)
			w.WriteByte('\n')
			n++
		}
		e = rows.Err()
		rows.Close()
		if e != nil || w.Flush() != nil {
			return refused
		}
		sum := hex.EncodeToString(h.Sum(nil))
		fmt.Fprintf(all, "%s %d %s\n", k[0], n, sum)
		result[k[0]] = map[string]any{"rows": n, "key_ordered_row_sha256": sum}
	}
	result["all_tables_sha256"] = hex.EncodeToString(all.Sum(nil))
	result["through_height"] = through
	result["database"] = database
	return save(out, result)
}

type acceptance struct {
	Accepted     bool   `json:"component_accepted"`
	FullChain    bool   `json:"full_chain_books_tax_acceptance"`
	First        int64  `json:"first"`
	Last         int64  `json:"last"`
	Held         int64  `json:"held"`
	Rows         int64  `json:"historical_financial_rows"`
	Transactions int64  `json:"transactions"`
	Prior        string `json:"prior_acceptance_sha256"`
}

func loose(path string, v any) error {
	raw, e := os.ReadFile(path)
	if e != nil || json.Unmarshal(raw, v) != nil {
		return refused
	}
	return nil
}

// extendManifest writes a NEW manifest that lists one more sealed component.
// It only reads the sealed component directory. base may be the retained
// 1.4M manifest or any manifest this function wrote.
func extendManifest(base Manifest, root, tailCache, tailManifest, tailSeal, out string) error {
	if validateLayout(base) != nil {
		return why("base manifest layout")
	}
	m := base
	m.Version = 2
	m.Pins = map[string]string{}
	for k, v := range base.Pins {
		m.Pins[k] = v
	}
	m.Components = append([]Component{}, base.Components...)
	pin := func(p string) (string, error) {
		h := fileSHA(p)
		if len(h) != 64 {
			return "", why("sealed component file missing")
		}
		m.Pins[p] = h
		return h, nil
	}
	sealPath := filepath.Join(root, "final-component-acceptance.json")
	var a acceptance
	if loose(sealPath, &a) != nil || !a.Accepted || a.Held != 0 || a.First != base.Last+1 || a.Rows <= 0 || a.Transactions <= 0 {
		return why("component is not sealed and accepted, or does not continue the manifest")
	}
	// The finalizer's rule: a root that was claimed needs its own accepted
	// exit. An abandoned attempt beside the accepted one is evidence, not a part.
	if _, e := os.Lstat(filepath.Join(filepath.Dir(root), "execution-claim.json")); e == nil {
		var exit struct {
			Accepted bool  `json:"component_accepted"`
			Code     int64 `json:"exit_code"`
		}
		if loose(filepath.Join(filepath.Dir(root), "execution-exit.json"), &exit) != nil || !exit.Accepted || exit.Code != 0 {
			return why("this root holds a claim with no accepted exit")
		}
	}
	if a.Prior != "" {
		found := false
		for p, h := range base.Pins {
			if h == a.Prior && filepath.Base(p) == "final-component-acceptance.json" {
				found = true
			}
		}
		if !found {
			return why("the seal names a previous seal this manifest does not pin")
		}
	}
	if _, e := pin(sealPath); e != nil {
		return e
	}
	var command struct {
		Argv []string `json:"argv"`
	}
	if loose(filepath.Join(root, "financial", "command.json"), &command) != nil {
		return why("sealed validation command")
	}
	arg := map[string]string{}
	for i := 1; i+1 < len(command.Argv); i += 2 {
		arg[command.Argv[i]] = command.Argv[i+1]
	}
	c := Component{First: a.First, Last: a.Last, Records: arg["--records"], Cache: arg["--cache"], Status: arg["--status"], Differences: arg["--result-differences"], CacheManifest: filepath.Join(root, "financial", "cache-source-after.sha256"), HistoricalRows: a.Rows, Transactions: a.Transactions}
	if loose(arg["--proof"], &c.Proof) != nil || c.Cache == "" {
		return why("sealed proof")
	}
	c.RecordsSHA = c.Proof.SHA256
	for _, p := range []string{c.Records, c.Status, c.Differences, c.CacheManifest, arg["--proof"], arg["--export-proof"], arg["--export-records"], filepath.Join(root, "financial", "component-result.json"), filepath.Join(root, "financial", "source-binding.json")} {
		if !strings.HasPrefix(p, root+string(filepath.Separator)) {
			return why("sealed validation command names a file outside the component")
		}
		if _, e := pin(p); e != nil {
			return e
		}
	}
	if m.Pins[c.Records] != c.RecordsSHA || m.Pins[c.Differences] != c.Proof.ResultDifferencesSHA256 {
		return why("records or differences digest differs from the sealed proof")
	}
	var x replayrecords.Proof
	if loose(arg["--export-proof"], &x) != nil || !x.Export || x.From != c.Last || x.Through != c.Last || m.Pins[arg["--export-records"]] != x.SHA256 {
		return why("sealed export proof")
	}
	m.Components = append(m.Components, c)
	m.Last = c.Last
	if c.Last == canonicalHeight {
		// Only the export at the canonical end is the restart payout. Every
		// earlier export stays pinned as validation evidence.
		m.CanonicalExport = &CanonicalExport{Records: arg["--export-records"], Proof: x}
		f, e := os.Open(m.CanonicalExport.Records)
		if e != nil {
			return refused
		}
		var h *replayrecords.Height
		e = replayrecords.Stream(f, x, func(v replayrecords.Height) error { h = &v; return nil })
		f.Close()
		if e != nil || h == nil {
			return why("canonical export records")
		}
		for _, r := range h.Records {
			if r.Kind == "movement" {
				m.CanonicalExport.Rows++
			}
		}
		if tailCache != "" {
			m.Tail = &Tail{First: canonicalHeight + 1, Last: tailLast, Cache: tailCache, CacheManifest: tailManifest}
			if _, e := pin(tailManifest); e != nil {
				return e
			}
			if tailSeal != "" {
				m.Tail.Seal = tailSeal
				if _, e := pin(tailSeal); e != nil {
					return e
				}
			}
		}
	} else if tailCache != "" {
		return why("tail before the canonical end")
	}
	executable, e := os.Executable()
	if e != nil {
		return refused
	}
	m.BinarySHA = fileSHA(executable)
	if pins(m) != nil {
		return why("extended manifest does not validate")
	}
	f, e := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return why("manifest output must be new")
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", " ")
	if enc.Encode(m) != nil {
		return refused
	}
	fmt.Printf("manifest components=%d last=%d canonical_export=%t tail=%t\n", len(m.Components), m.Last, m.CanonicalExport != nil, m.Tail != nil)
	return nil
}
