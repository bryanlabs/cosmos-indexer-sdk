//go:build ops818harness

package taxapi

import (
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/shopspring/decimal"
)

type currentReplayWindow struct {
	Key, Address, OldInput string
	Since, AsOf            time.Time
}
type currentOpeningEvidence struct {
	Address              string
	Day                  time.Time
	Holdings             flowHoldings
	Known                bool
	Error                string
	SourcePath           string
	SourceSHA256         string
	OriginalSource       string
	IndependentlyDecoded bool
}

type currentLotLoader struct {
	*dbFlowLoader
	root, producer, dir string
	windows             map[string]currentReplayWindow
	openings            map[string]currentOpeningEvidence
	loaded              int
}

func currentHoldingsMoneyEqual(a, b flowHoldings) bool {
	if !a.Liquid.Equal(b.Liquid) {
		return false
	}
	for _, pair := range [][2]map[string]decimal.Decimal{{a.Staked, b.Staked}, {a.Unbonding, b.Unbonding}} {
		for k, v := range pair[0] {
			if !v.Equal(pair[1][k]) {
				return false
			}
		}
		for k, v := range pair[1] {
			if !v.Equal(pair[0][k]) {
				return false
			}
		}
	}
	return true
}

func currentWindowID(addr string, start, end time.Time) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(addr+"|"+start.UTC().Format(time.RFC3339Nano)+"|"+end.UTC().Format(time.RFC3339Nano))))
}
func currentAppend(path string, value any) error {
	b, e := json.Marshal(value)
	if e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(append(b, '\n'))
	ce := f.Close()
	if e != nil {
		return e
	}
	return ce
}
func (l *currentLotLoader) opening(addr string, day time.Time) (flowHoldings, bool, error) {
	var h flowHoldings
	var k bool
	var e error
	evidence, reused := l.openings[currentWindowID(addr, day, day)]
	if reused {
		if !evidence.Day.Equal(day) || evidence.Address != addr || !evidence.Known || evidence.Error != "<nil>" {
			return h, false, fmt.Errorf("retained opening source/window invalid")
		}
		h, k = evidence.Holdings, evidence.Known
	} else {
		ctx, cancel := context.WithTimeout(l.s.db.Statement.Context, 30*time.Second)
		reader := NewServer(l.s.db.WithContext(ctx), l.s.oracle, l.s.native)
		reader.readOnlyLedger = true
		reader.privateReadSnapshotTimeout = time.Minute
		facade := *l.dbFlowLoader
		facade.s = reader
		h, k, e = facade.opening(addr, day)
		cancel()
	}
	if x := currentAppend(filepath.Join(l.dir, "openings.private.jsonl"), map[string]any{"address": addr, "day": day, "holdings": h, "known": k, "error": fmt.Sprint(e), "retained_opening_reused": reused, "retained_opening_evidence": evidence}); x != nil {
		return h, false, x
	}
	return h, k, e
}
func (l *currentLotLoader) events(addr string, start, end time.Time) ([]flowEvent, error) {
	key := currentWindowID(addr, start, end)
	eventpath := filepath.Join(l.root, "windows", key+".events.private.json")
	if raw, e := os.ReadFile(eventpath); e == nil {
		var events []flowEvent
		if json.Unmarshal(raw, &events) != nil {
			return nil, fmt.Errorf("current converted cache invalid")
		}
		if e = currentAppend(filepath.Join(l.dir, "loaded.private.jsonl"), map[string]any{"address": addr, "since": start, "asof_exclusive": end, "window": key, "converted_cache_reused": true, "at": time.Now().UTC()}); e != nil {
			return nil, e
		}
		l.loaded++
		return events, nil
	}
	var input reportInput
	var inputBytes []byte
	sourcePath := ""
	reused := false
	if w, ok := l.windows[key]; ok {
		sourcePath = filepath.Join(l.producer, "captures", w.Key+".input.private.json")
		receiptPath := filepath.Join(l.producer, "captures", w.Key+".receipt.private.json")
		for {
			b, e := os.ReadFile(receiptPath)
			if e == nil {
				var receipt struct {
					Source, ReadError, InputSHA256 string
					Since, End                     time.Time
				}
				var r map[string]json.RawMessage
				if json.Unmarshal(b, &r) != nil || json.Unmarshal(r["source"], &receipt.Source) != nil || json.Unmarshal(r["read_error"], &receipt.ReadError) != nil || json.Unmarshal(r["current_input_sha256"], &receipt.InputSHA256) != nil {
					return nil, fmt.Errorf("current native receipt invalid")
				}
				if json.Unmarshal(r["since"], &receipt.Since) != nil || json.Unmarshal(r["asof_exclusive"], &receipt.End) != nil || !receipt.Since.Equal(start) || !receipt.End.Equal(end) {
					return nil, fmt.Errorf("retained current window binding failed")
				}
				if receipt.Source != "6754a0d7d7986e48e3e3467b2448aae84d2c1b92" || receipt.ReadError != "<nil>" {
					return nil, fmt.Errorf("retained current native snapshot failed")
				}
				inputBytes, e = os.ReadFile(sourcePath)
				if e != nil {
					return nil, e
				}
				if fmt.Sprintf("%x", sha256.Sum256(inputBytes)) != receipt.InputSHA256 || json.Unmarshal(inputBytes, &input) != nil {
					return nil, fmt.Errorf("current native input hash mismatch")
				}
				reused = true
				break
			}
			if _, e = os.Stat(filepath.Join(l.producer, "exit-state-safe.json")); e == nil {
				return nil, fmt.Errorf("native producer exited without required window")
			}
			time.Sleep(250 * time.Millisecond)
		}
	} else {
		var e error
		input, e = l.s.readReportInput(l.chain, []string{addr}, start, end, false)
		if e != nil {
			_ = routePrivate(filepath.Join(l.root, "windows", key+".error.private.json"), map[string]any{"address": addr, "since": start, "asof_exclusive": end, "error": e.Error(), "source": "6754a0d7d7986e48e3e3467b2448aae84d2c1b92"})
			return nil, e
		}
		sourcePath = filepath.Join(l.root, "windows", key+".input.private.json")
		if e = routePrivate(sourcePath, input); e != nil {
			return nil, e
		}
		inputBytes, e = os.ReadFile(sourcePath)
		if e != nil {
			return nil, e
		}
	}
	if len(input.Wallets) != 1 {
		return nil, fmt.Errorf("current input wallet coverage invalid")
	}
	rows := input.Wallets[0]
	creditContext, cancelCredit := context.WithTimeout(l.s.db.Statement.Context, 30*time.Second)
	creditReader := &Server{db: l.s.db.WithContext(creditContext), native: l.s.native, readOnlyLedger: true}
	credited, e := creditReader.flowReturnCredits(addr, rows.Events)
	cancelCredit()
	if e != nil {
		return nil, e
	}
	creditRows := []map[string]any{}
	for key, ok := range credited {
		if ok {
			creditRows = append(creditRows, map[string]any{"message": key.message, "coin": key.coin})
		}
	}
	events := make([]flowEvent, 0, len(rows.Events)+len(rows.Fees))
	for _, row := range rows.Events {
		if row.Timestamp.Before(start) || !row.Timestamp.Before(end) {
			return nil, fmt.Errorf("current source event outside exact window")
		}
		if flowSingleHopNative(row, l.s.native.Denom) && !credited[flowCredit{row.MessageID, row.Amount + l.s.native.Denom}] {
			continue
		}
		if event, ok := flowEventFromRow(row, addr, l.s.native.Denom); ok {
			events = append(events, event)
		}
	}
	for _, fee := range rows.Fees {
		if fee.Ts.Before(start) || !fee.Ts.Before(end) {
			return nil, fmt.Errorf("current fee outside exact window")
		}
		if fee.Denom != l.s.native.Denom {
			continue
		}
		amount, e := decimal.NewFromString(fee.Amount)
		if e != nil {
			return nil, fmt.Errorf("current fee invalid")
		}
		events = append(events, flowEvent{Key: flowKey{Time: fee.Ts.UTC()}, Category: apiFee, Dir: apiOut, Amount: amount, From: addr})
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Key.before(events[j].Key) })
	if e = routePrivate(eventpath, events); e != nil {
		return nil, e
	}
	if e = routePrivate(filepath.Join(l.root, "windows", key+".binding.private.json"), map[string]any{"address": addr, "since": start, "asof_exclusive": end, "source": "6754a0d7d7986e48e3e3467b2448aae84d2c1b92", "source_input": sourcePath, "source_input_sha256": fmt.Sprintf("%x", sha256.Sum256(inputBytes)), "native_input_cache_reused": reused, "actual_return_credit_rows": creditRows, "allcoins_source_events": len(rows.Events), "allcoins_source_fees": len(rows.Fees), "converted_native_flow_events": len(events), "financially_accepted": false}); e != nil {
		return nil, e
	}
	l.loaded++
	if e = currentAppend(filepath.Join(l.dir, "loaded.private.jsonl"), map[string]any{"address": addr, "since": start, "asof_exclusive": end, "window": key, "native_input_cache_reused": reused, "at": time.Now().UTC()}); e != nil {
		return nil, e
	}
	return events, nil
}
func currentPoolState(w *flowWallet) any {
	rules := make([]map[string]any, 0, flowRules)
	for r := 0; r < flowRules; r++ {
		p := w.pools[r]
		staked := map[string][]flowPart{}
		unbonding := map[string][]flowPart{}
		bids := map[string][]flowPart{}
		for k, v := range p.staked {
			staked[k] = append([]flowPart(nil), v.parts...)
		}
		for k, v := range p.unbonding {
			unbonding[k] = append([]flowPart(nil), v.parts...)
		}
		for k, v := range p.bids {
			bids[k] = append([]flowPart(nil), v.parts...)
		}
		rules = append(rules, map[string]any{"rule": flowRuleNames[r], "liquid": append([]flowPart(nil), p.liquid.parts...), "staked": staked, "unbonding": unbonding, "locked": append([]flowPart(nil), p.locked.parts...), "bids": bids, "total": p.total(), "overdraw": w.Overdraw[r]})
	}
	return map[string]any{"address": w.Addr, "since": w.Since, "hop": w.Hop, "opening_known": w.Known, "opening_source": w.OpeningSource, "rules": rules}
}
func runCurrentLots(tr *flowTrace, group []string, dir string) error {
	for _, addr := range group {
		if e := tr.discover(addr, 0, tr.From, nil); e != nil {
			return e
		}
	}
	for _, w := range tr.order {
		if e := currentAppend(filepath.Join(dir, "initial-states.private.jsonl"), currentPoolState(w)); e != nil {
			return e
		}
	}
	ordinal := 0
	for tr.queue.Len() > 0 {
		q := heap.Pop(&tr.queue).(flowQueued)
		w, event := q.wallet, q.event
		before := map[string]any{w.Addr: currentPoolState(w)}
		var routed [flowRules][]flowPart
		if event.Category == apiTransfer && event.Dir == apiOut && event.From != event.To {
			for r := 0; r < flowRules; r++ {
				pool := &flowPool{rule: r, parts: append([]flowPart(nil), w.pools[r].liquid.parts...)}
				routed[r], _ = pool.take(event.Amount)
			}
			if target := tr.Wallets[event.To]; target != nil {
				before[target.Addr] = currentPoolState(target)
			}
		}
		priorWallets := len(tr.order)
		e := tr.apply(w, event, true)
		after := map[string]any{w.Addr: currentPoolState(w)}
		if target := tr.Wallets[event.To]; target != nil && event.Category == apiTransfer && event.Dir == apiOut {
			after[target.Addr] = currentPoolState(target)
		}
		prefix := []any{}
		for _, added := range tr.order[priorWallets:] {
			prefix = append(prefix, currentPoolState(added))
		}
		if x := currentAppend(filepath.Join(dir, "applied.private.jsonl"), map[string]any{"ordinal": ordinal + 1, "from_wallet": w.Addr, "event": event, "before": before, "after": after, "new_discovery_prefix_states": prefix, "prefix_replay_source": "complete converted window plus exact opening; production discover replays pre-cursor other-origin events", "error": fmt.Sprint(e)}); x != nil {
			return x
		}
		if tagged(routed) {
			if x := currentAppend(filepath.Join(dir, "tagged-routes.private.jsonl"), map[string]any{"from": w.Addr, "hop": w.Hop, "event": event, "parts": routed, "destination_loaded": tr.Wallets[event.To] != nil, "stop": tr.stops[event.To], "error": fmt.Sprint(e)}); x != nil {
				return x
			}
		}
		ordinal++
		if ordinal%1024 == 0 {
			if x := currentAppend(filepath.Join(dir, "progress.private.jsonl"), map[string]any{"applied": ordinal, "wallets": len(tr.order), "attempted": len(tr.attempted), "pending_events": tr.queue.Len(), "chronological_frontier": event.Key.Time, "at": time.Now().UTC()}); x != nil {
				return x
			}
			var disk syscall.Statfs_t
			if syscall.Statfs(dir, &disk) != nil || uint64(disk.Bavail)*uint64(disk.Bsize) < 4*1024*1024*1024 {
				return fmt.Errorf("private persistent capacity exhausted; exact queue evidence retained, not accepted")
			}
		}
		if e != nil {
			return e
		}
	}
	tr.settle()
	return nil
}
func TestOPS818CurrentLotReplay(t *testing.T) {
	root := os.Getenv("H_CURRENT_LOT_DIR")
	if os.Mkdir(root, 0700) != nil {
		t.Fatal("exclusive current lot output required")
	}
	if os.Mkdir(filepath.Join(root, "windows"), 0700) != nil {
		t.Fatal("window cache creation failed")
	}
	var scope struct {
		Source  string
		Windows []currentReplayWindow
	}
	b, e := os.ReadFile(os.Getenv("H_CURRENT_ALL_SCOPE"))
	if e != nil || json.Unmarshal(b, &scope) != nil || scope.Source != "6754a0d7d7986e48e3e3467b2448aae84d2c1b92" {
		t.Fatal("current source manifest invalid")
	}
	var cohort struct {
		Validators      []string
		From, AsOf      string
		OpeningEvidence []currentOpeningEvidence
	}
	b, e = os.ReadFile(os.Getenv("H_CURRENT_LOT_SCOPE"))
	if e != nil || json.Unmarshal(b, &cohort) != nil || len(cohort.Validators) != 7 {
		t.Fatal("entire original cohort scope required")
	}
	from, e := time.Parse(time.DateOnly, cohort.From)
	if e != nil {
		t.Fatal("from invalid")
	}
	asof, e := time.Parse(time.DateOnly, cohort.AsOf)
	if e != nil {
		t.Fatal("end invalid")
	}
	db, e := indexerDB.PostgresDbConnect("127.0.0.1", "15438", "cosmostax", "postgres", os.Getenv("H_PASS"), "silent", "default_transaction_read_only=on", "application_name=ops818-current6754-lot-replay-readonly", "jit=off", "plan_cache_mode=force_custom_plan", "statement_timeout=0", "lock_timeout=2s")
	if e != nil {
		t.Fatal("readonly source connection failed")
	}
	sqlDB, e := db.DB()
	if e != nil {
		t.Fatal("DB handle failed")
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	s := NewServer(db, NewOracle("http://127.0.0.1:1", os.Getenv("NODE_REST_API")), DefaultNativeAsset)
	s.readOnlyLedger = true
	s.privateReadSnapshotTimeout = time.Minute
	node, e := NewLedgerNode()
	if e != nil {
		t.Fatal("node configuration failed")
	}
	archive, e := newFlowArchive(os.Getenv("H_ARCHIVE_RPC"))
	if e != nil {
		t.Fatal("archive configuration failed")
	}
	base := &dbFlowLoader{s: s, chain: apiMainnet, node: node, private: true, archive: archive, mu: &sync.Mutex{}, labels: map[string]AddressLabel{}, chans: map[string]string{}}
	var labels []AddressLabel
	if e = db.Where("chain = ?", apiMainnet).Find(&labels).Error; e != nil {
		t.Fatal("labels read failed")
	}
	for _, x := range labels {
		base.labels[x.Address] = x
	}
	vals, e := s.commissionValidators(apiMainnet)
	if e != nil {
		t.Fatal("validator metadata failed")
	}
	for _, x := range derivedFlowLabels(apiMainnet, vals) {
		if _, ok := base.labels[x.Address]; !ok {
			base.labels[x.Address] = x
		}
	}
	windows := map[string]currentReplayWindow{}
	for _, w := range scope.Windows {
		windows[currentWindowID(w.Address, w.Since, w.AsOf)] = w
	}
	openings := map[string]currentOpeningEvidence{}
	verifiedOpeningFiles := map[string]string{}
	for _, o := range cohort.OpeningEvidence {
		if previous, ok := verifiedOpeningFiles[o.SourcePath]; ok {
			if previous != o.SourceSHA256 {
				t.Fatal("opening source hash conflict")
			}
		} else {
			raw, readErr := os.ReadFile(o.SourcePath)
			if readErr != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != o.SourceSHA256 {
				t.Fatal("opening immutable source hash invalid")
			}
			verifiedOpeningFiles[o.SourcePath] = o.SourceSHA256
		}
		key := currentWindowID(o.Address, o.Day, o.Day)
		if previous, ok := openings[key]; ok && !currentHoldingsMoneyEqual(previous.Holdings, o.Holdings) {
			t.Fatal("retained opening money conflict")
		}
		openings[key] = o
	}
	for i, validator := range cohort.Validators {
		dir := filepath.Join(root, fmt.Sprintf("validator-%02d", i+1))
		if os.Mkdir(dir, 0700) != nil {
			t.Fatal("exclusive cohort required")
		}
		loader := &currentLotLoader{dbFlowLoader: base, root: root, producer: os.Getenv("H_CURRENT_PRODUCER_DIR"), dir: dir, windows: windows, openings: openings}
		receivers, e := s.commissionReceivers(apiMainnet, validator)
		if e != nil {
			t.Fatal("current roots unavailable")
		}
		group := []string{}
		seen := map[string]bool{}
		for _, r := range receivers {
			if r.Address != "" && !seen[r.Address] {
				seen[r.Address] = true
				group = append(group, r.Address)
			}
		}
		if op := operatorAccount(validator); op != "" && !seen[op] {
			group = append(group, op)
		}
		if e = routePrivate(filepath.Join(dir, "identity.private.json"), map[string]any{"validator": validator, "from": from, "asof_exclusive": asof, "source": scope.Source, "roots": group, "financially_accepted": false}); e != nil {
			t.Fatal("identity save failed")
		}
		tr := newFlowTrace(validator, from, asof, loader)
		tr.strict = true
		began := time.Now()
		runErr := runCurrentLots(tr, group, dir)
		for _, w := range tr.order {
			if e = currentAppend(filepath.Join(dir, "final-pools.private.jsonl"), currentPoolState(w)); e != nil {
				t.Fatal("final pool save failed")
			}
		}
		rows := []CommissionFlow{}
		for k, a := range tr.Fates {
			rows = append(rows, CommissionFlow{Chain: apiMainnet, Validator: validator, Rule: flowRuleNames[k.Rule], Month: k.Month, Class: k.Class, Detail: k.Detail, ViaUnbonding: k.ViaUnbonding, AmountBase: a.String()})
		}
		if e = routePrivate(filepath.Join(dir, "rows.private.json"), rows); e != nil {
			t.Fatal("fates save failed")
		}
		if e = routePrivate(filepath.Join(dir, "trace-result.private.json"), map[string]any{"error": fmt.Sprint(runErr), "wallets": len(tr.order), "attempted": tr.attempted, "commission": tr.Commission, "elapsed_ms": time.Since(began).Milliseconds(), "pending_events": tr.queue.Len(), "current_source": scope.Source, "all_wallet_cash_and_fee_raw_proof_complete": false, "financially_accepted": false}); e != nil {
			t.Fatal("terminal save failed")
		}
		if e = currentAppend(filepath.Join(root, "cohort-progress.private.jsonl"), map[string]any{"ordinal": i + 1, "wallets": len(tr.order), "run_ok": runErr == nil, "pending_events": tr.queue.Len(), "at": time.Now().UTC()}); e != nil {
			t.Fatal("cohort progress save failed")
		}
		t.Log("current source lot cohort", i+1, "wallets", len(tr.order), "run_ok", runErr == nil)
	}
}
