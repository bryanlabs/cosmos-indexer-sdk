//go:build ops818harness

package taxapi

import (
	"container/heap"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/shopspring/decimal"
)

// Private, uncommitted evidence harness. Source reads are transaction-read-only.
// The queue is the production run loop, with receipts before/after each apply.
type materialCaptureLoader struct {
	*dbFlowLoader
	dir     string
	ordinal int
}

func privateJSON(path string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return os.WriteFile(path, b, 0600)
}
func privateAppend(path string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	_, e = f.Write(append(b, '\n'))
	return e
}
func walletFile(addr string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(addr))) }
func (l *materialCaptureLoader) opening(addr string, day time.Time) (flowHoldings, bool, error) {
	h, k, e := l.dbFlowLoader.opening(addr, day)
	_ = privateAppend(filepath.Join(l.dir, "openings.private.jsonl"), struct {
		Address  string
		Day      time.Time
		Holdings flowHoldings
		Known    bool
		Error    string
	}{addr, day, h, k, fmt.Sprint(e)})
	return h, k, e
}
func (l *materialCaptureLoader) events(addr string, start, end time.Time) ([]flowEvent, error) {
	name := filepath.Join(l.dir, walletFile(addr))
	_ = privateAppend(filepath.Join(l.dir, "progress.private.jsonl"), map[string]any{"phase": "reading", "address": addr, "since": start, "at": time.Now().UTC()})
	reader := &Server{db: recordNextReadDB(l.s.db, name+".queries.private.json"), readOnlyLedger: true}
	began := time.Now()
	input, err := reader.readReportInput(l.chain, []string{addr}, start, end, false)
	_ = privateAppend(filepath.Join(l.dir, "timings.private.jsonl"), map[string]any{"address":addr,"since":start,"end":end,"ms":time.Since(began).Milliseconds(),"error":fmt.Sprint(err)})
	if err != nil {
		_ = privateJSON(name+".error.private.json", map[string]any{"address": addr, "since": start, "error": err.Error()})
		return nil, err
	}
	if err = privateJSON(name+".input.private.json", input); err != nil {
		return nil, err
	}
	rows := input.Wallets[0]
	credited, err := l.s.flowReturnCredits(addr, rows.Events)
	if err != nil {
		return nil, err
	}
	out := make([]flowEvent, 0, len(rows.Events)+len(rows.Fees))
	for _, e := range rows.Events {
		if flowSingleHopNative(e, l.s.native.Denom) && !credited[flowCredit{e.MessageID, e.Amount + l.s.native.Denom}] {
			continue
		}
		if fe, ok := flowEventFromRow(e, addr, l.s.native.Denom); ok {
			out = append(out, fe)
		}
	}
	for _, f := range rows.Fees {
		if f.Denom != l.s.native.Denom {
			continue
		}
		v, e := decimal.NewFromString(f.Amount)
		if e != nil {
			continue
		}
		out = append(out, flowEvent{Key: flowKey{Time: f.Ts.UTC()}, Category: apiFee, Dir: apiOut, Amount: v, From: addr})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key.before(out[j].Key) })
	if err = privateJSON(name+".events.private.json", out); err != nil {
		return nil, err
	}
	l.ordinal++
	_ = privateAppend(filepath.Join(l.dir, "progress.private.jsonl"), map[string]any{"phase": "loaded", "address": addr, "ordinal": l.ordinal, "events": len(out), "at": time.Now().UTC()})
	return out, nil
}
func clonedTake(p *flowPool, amount decimal.Decimal) []flowPart {
	copyPool := &flowPool{rule: p.rule, parts: append([]flowPart(nil), p.parts...)}
	parts, _ := copyPool.take(amount)
	return parts
}
func materialRun(tr *flowTrace, group []string, dir string) error {
	for _, addr := range group {
		if err := tr.discover(addr, 0, tr.From, nil); err != nil {
			return err
		}
	}
	for tr.queue.Len() > 0 {
		q := heap.Pop(&tr.queue).(flowQueued)
		w, e := q.wallet, q.event
		var routed [flowRules][]flowPart
		if e.Category == apiTransfer && e.Dir == apiOut && e.From != e.To {
			for r := 0; r < flowRules; r++ {
				routed[r] = clonedTake(w.pools[r].liquid, e.Amount)
			}
		}
		before := map[string][flowRules]decimal.Decimal{}
		for _, wallet := range tr.order {
			before[wallet.Addr] = wallet.Overdraw
		}
		err := tr.apply(w, e, true)
		if tagged(routed) {
			if x := privateAppend(filepath.Join(dir, "tagged-routes.private.jsonl"), map[string]any{"from": w.Addr, "hop": w.Hop, "event": e, "parts": routed, "destination_loaded": tr.Wallets[e.To] != nil, "stop": tr.stops[e.To], "error": fmt.Sprint(err)}); x != nil {
				return x
			}
		}
		for _, wallet := range tr.order {
			prior, existed := before[wallet.Addr]
			if !wallet.Overdraw[0].Equal(prior[0]) || !wallet.Overdraw[1].Equal(prior[1]) {
				if x := privateAppend(filepath.Join(dir, "shortfalls.private.jsonl"), map[string]any{"address": wallet.Addr, "event": e, "before": prior, "after": wallet.Overdraw, "new_discovery_prefix": !existed}); x != nil {
					return x
				}
			}
		}
		if err != nil {
			return err
		}
	}
	tr.settle()
	return nil
}
func TestOPS818MaterialCapture(t *testing.T) {
	root := os.Getenv("H_CAPTURE_DIR")
	if root == "" {
		t.Fatal("private directory required")
	}
	var scope struct {
		Validators []string
		From, AsOf string
		Archive    []FlowArchiveReceipt
		Labels []AddressLabel
	}
	b, e := os.ReadFile(os.Getenv("H_CAPTURE_SCOPE"))
	if e != nil {
		t.Fatal("scope read failed")
	}
	if json.Unmarshal(b, &scope) != nil {
		t.Fatal("scope invalid")
	}
	from, e := time.Parse(time.DateOnly, scope.From)
	if e != nil {
		t.Fatal("from invalid")
	}
	asof, e := time.Parse(time.DateOnly, scope.AsOf)
	if e != nil {
		t.Fatal("asof invalid")
	}
	db, e := indexerDB.PostgresDbConnect("127.0.0.1", "15438", "cosmostax", "postgres", os.Getenv("H_PASS"), "silent", "default_transaction_read_only=on", "application_name=ops818-material-01bbba7-readonly", "jit=off", "plan_cache_mode=force_custom_plan", "statement_timeout=0", "lock_timeout=2s", "effective_io_concurrency=200")
	if e != nil {
		t.Fatal("source connection failed")
	}
	s := NewServer(db, NewOracle(os.Getenv("ORACLE_URL"), os.Getenv("NODE_REST_API")), DefaultNativeAsset)
	s.readOnlyLedger = true
	node, e := NewLedgerNode()
	if e != nil {
		t.Fatal("node config failed")
	}
	labels := scope.Labels
	vals, e := s.commissionValidators(apiMainnet)
	if e != nil {
		t.Fatal("validators read failed")
	}
	base := &dbFlowLoader{s: s, chain: apiMainnet, node: node, private: true, mu: &sync.Mutex{}, labels: map[string]AddressLabel{}, chans: map[string]string{}}
	for _, x := range labels {
		base.labels[x.Address] = x
	}
	for _, x := range derivedFlowLabels(apiMainnet, vals) {
		if _, ok := base.labels[x.Address]; !ok {
			base.labels[x.Address] = x
		}
	}
	cached := map[string]string{}
	for _, receipt := range scope.Archive {
		if fmt.Sprintf("%x", sha256.Sum256([]byte(receipt.Response))) != receipt.SHA256 {
			t.Fatal("retained archive hash mismatch")
		}
		key := fmt.Sprintf("%s|%d|%s|%s", receipt.Method, receipt.Height, receipt.QueryPath, receipt.RequestData)
		if old, ok := cached[key]; ok && old != receipt.Response {
			t.Fatal("conflicting retained response")
		}
		cached[key] = receipt.Response
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/")
		q := r.URL.Query()
		key := fmt.Sprintf("%s|%s|%s|%s", method, q.Get("height"), q.Get("path"), q.Get("data"))
		if raw, ok := cached[key]; ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(raw))
			return
		}
		http.Error(w, "required receipt absent from immutable failed evidence", 502)
	}))
	defer proxy.Close()
	base.archive, e = newFlowArchive(proxy.URL)
	if e != nil {
		t.Fatal("private cache endpoint failed")
	}
	for ordinal, v := range scope.Validators {
		dir := filepath.Join(root, fmt.Sprintf("validator-%02d", ordinal+1))
		if os.Mkdir(dir, 0700) != nil {
			t.Fatal("new immutable capture path required")
		}
		loader := &materialCaptureLoader{dbFlowLoader: base, dir: dir}
		receivers, e := s.commissionReceivers(apiMainnet, v)
		if e != nil {
			t.Fatal("receivers failed")
		}
		group := []string{}
		seen := map[string]bool{}
		for _, r := range receivers {
			if r.Address != "" && !seen[r.Address] {
				seen[r.Address] = true
				group = append(group, r.Address)
			}
		}
		if op := operatorAccount(v); op != "" && !seen[op] {
			group = append(group, op)
		}
		_ = privateJSON(filepath.Join(dir, "identity.private.json"), map[string]any{"validator": v, "from": from, "asof_exclusive": asof, "source": "01bbba79294f773491e249e67b3a51eabaad6398", "roots": group})
		tr := newFlowTrace(v, from, asof, loader)
		tr.strict = true
		began := time.Now()
		runErr := materialRun(tr, group, dir)
		_ = privateJSON(filepath.Join(dir, "trace-result.private.json"), map[string]any{"error": fmt.Sprint(runErr), "wallets": len(tr.order), "attempted": tr.attempted, "commission": tr.Commission, "fates": fmt.Sprint(tr.Fates), "elapsed_ms": time.Since(began).Milliseconds()})
		return
		walletEvidence := []CommissionFlowWalletEvidence{}
		for _, w := range tr.order {
			actual, known, err := base.holdingsAt(w.Addr, asof.AddDate(0, 0, -1))
			_ = privateAppend(filepath.Join(dir, "closings.private.jsonl"), map[string]any{"address": w.Addr, "holdings": actual, "known": known, "error": fmt.Sprint(err)})
			if err == nil && known {
				ev := commissionFlowWalletEvidence(w, actual, asof.AddDate(0, 0, -1))
				ev.OpeningSource = w.OpeningSource
				ev.ClosingSource = actual.Source
				walletEvidence = append(walletEvidence, ev)
			}
		}
		evidence := base.evidence()
		evidence.Wallets = walletEvidence
		evidence.Archive = base.archive.evidence(tr.attempted)
		_ = privateJSON(filepath.Join(dir, "evidence.private.json"), evidence)
		rows := []CommissionFlow{}
		for k, a := range tr.Fates {
			rows = append(rows, CommissionFlow{Chain: apiMainnet, Validator: v, Rule: flowRuleNames[k.Rule], Month: k.Month, Class: k.Class, Detail: k.Detail, ViaUnbonding: k.ViaUnbonding, AmountBase: a.String()})
		}
		_ = privateJSON(filepath.Join(dir, "rows.private.json"), rows)
		_ = privateAppend(filepath.Join(root, "cohort-progress.private.jsonl"), map[string]any{"ordinal": ordinal + 1, "wallets": len(tr.order), "error": fmt.Sprint(runErr), "closed": len(walletEvidence), "at": time.Now().UTC()})
		t.Log("private cohort captured ordinal", ordinal+1, "wallets", len(tr.order), "closing_evidence", len(walletEvidence), "execution_failed", runErr != nil)
	}
}
