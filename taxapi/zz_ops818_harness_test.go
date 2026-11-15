//go:build ops818harness

package taxapi

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/shopspring/decimal"
)

// Not committed. Read-only proof for OPS-818: every per-wallet gate and row
// load the trace makes gives the same result with JIT on and off, and the
// traced fates equal the stored trial rows.

type cmpLoader struct {
	*dbFlowLoader
	on    *Server
	mu    sync.Mutex
	n     int
	pass  int
	fail  int
	diff  int
	kinds map[string]int
	tOn   time.Duration
	tOff  time.Duration
}

func (c *cmpLoader) events(addr string, start, end time.Time) ([]flowEvent, error) {
	t0 := time.Now()
	offIn, offErr := c.s.readReportInput(c.chain, []string{addr}, start, end, false)
	t1 := time.Now()
	onIn, onErr := c.on.readReportInput(c.chain, []string{addr}, start, end, false)
	t2 := time.Now()
	same := fmt.Sprint(offErr) == fmt.Sprint(onErr) && reflect.DeepEqual(offIn, onIn)
	c.mu.Lock()
	c.n++
	c.tOff += t1.Sub(t0)
	c.tOn += t2.Sub(t1)
	if offErr == nil {
		c.pass++
	} else {
		c.fail++
	}
	if !same {
		c.diff++
		if c.kinds == nil {
			c.kinds = map[string]int{}
		}
		c.kinds[diffKind(offIn, onIn, offErr, onErr)]++
	}
	c.mu.Unlock()
	if offErr != nil {
		return nil, offErr
	}
	return c.s.flowEvents(c.chain, addr, start, end)
}

func connect(t *testing.T, runtime ...string) *Server {
	db, err := indexerDB.PostgresDbConnect("127.0.0.1", os.Getenv("H_PORT"), "cosmostax", "postgres", os.Getenv("H_PASS"), "silent",
		append([]string{"default_transaction_read_only=on", "application_name=ops818-harness"}, runtime...)...)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(db, NewOracle(os.Getenv("ORACLE_URL"), os.Getenv("NODE_REST_API")), DefaultNativeAsset)
}

func TestOPS818Harness(t *testing.T) {
	off := connect(t, "jit=off")
	on := connect(t)
	chain := apiMainnet
	from := time.Date(2024, 3, 21, 0, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for _, v := range strings.Split(os.Getenv("H_VALS"), ",") {
		v := v
		wg.Add(1)
		go func() {
			defer wg.Done()
			var run CommissionFlowRun
			if err := off.db.Where("chain = ? AND validator = ?", chain, v).First(&run).Error; err != nil {
				t.Errorf("%s: stored run: %v", v, err)
				return
			}
			asOf := run.AsOf.AddDate(0, 0, 1)
			var labels []AddressLabel
			if err := off.db.Where("chain = ?", chain).Find(&labels).Error; err != nil {
				t.Error(err)
				return
			}
			node, err := NewLedgerNode()
			if err != nil {
				t.Error(err)
				return
			}
			base := &dbFlowLoader{s: off, chain: chain, node: node, labels: map[string]AddressLabel{}, mu: &sync.Mutex{}, chans: map[string]string{}}
			for _, l := range labels {
				base.labels[l.Address] = l
			}
			c := &cmpLoader{dbFlowLoader: base, on: on}
			receivers, err := off.commissionReceivers(chain, v)
			if err != nil {
				t.Error(err)
				return
			}
			group, seen := []string{}, map[string]bool{}
			for _, r := range receivers {
				if r.Address != "" && !seen[r.Address] {
					seen[r.Address] = true
					group = append(group, r.Address)
				}
			}
			if op := operatorAccount(v); op != "" && !seen[op] {
				group = append(group, op)
			}
			started := time.Now()
			tr := newFlowTrace(v, from, asOf, c)
			if err := tr.run(group); err != nil {
				t.Errorf("%s: trace: %v", v, err)
				return
			}
			var stored []CommissionFlow
			if err := off.db.Where("chain = ? AND validator = ?", chain, v).Find(&stored).Error; err != nil {
				t.Error(err)
				return
			}
			want := map[string]string{}
			for _, r := range stored {
				want[fmt.Sprintf("%s|%s|%s|%s|%v", r.Rule, r.Month, r.Class, r.Detail, r.ViaUnbonding)] = r.AmountBase
			}
			got := map[string]string{}
			for k, a := range tr.Fates {
				if a.IsPositive() {
					got[fmt.Sprintf("%s|%s|%s|%s|%v", flowRuleNames[k.Rule], k.Month, k.Class, k.Detail, k.ViaUnbonding)] = a.String()
				}
			}
			mismatch := 0
			for k, a := range want {
				if got[k] != a {
					mismatch++
					t.Logf("mismatch %s stored %s traced %s", k, a, got[k])
				}
			}
			for k := range got {
				if _, ok := want[k]; !ok {
					mismatch++
				}
			}
			t.Logf("%s: wallets %d (stored %d), unresolved %d (stored %d), gate calls %d pass %d fail %d jit-differences %d, gate+load time off %s on %s, rows stored %d traced %d mismatched %d, total %s",
				v, len(tr.order), run.Wallets, len(tr.Unresolved), run.Unresolved, c.n, c.pass, c.fail, c.diff, c.tOff.Round(time.Second), c.tOn.Round(time.Second), len(want), len(got), mismatch, time.Since(started).Round(time.Second))
			t.Logf("%s: difference kinds %v", v, c.kinds)
			if c.diff > 0 || mismatch > 0 {
				t.Errorf("%s: differences found", v)
			}
		}()
	}
	wg.Wait()
}

func TestOPS818Drift(t *testing.T) {
	off := connect(t, "jit=off")
	chain := apiMainnet
	v := os.Getenv("H_VALS")
	var run CommissionFlowRun
	if err := off.db.Where("chain = ? AND validator = ?", chain, v).First(&run).Error; err != nil {
		t.Fatal(err)
	}
	asOf := run.AsOf.AddDate(0, 0, 1)
	var labels []AddressLabel
	if err := off.db.Where("chain = ?", chain).Find(&labels).Error; err != nil {
		t.Fatal(err)
	}
	node, err := NewLedgerNode()
	if err != nil {
		t.Fatal(err)
	}
	l := &dbFlowLoader{s: off, chain: chain, node: node, labels: map[string]AddressLabel{}, mu: &sync.Mutex{}, chans: map[string]string{}}
	for _, x := range labels {
		l.labels[x.Address] = x
	}
	receivers, err := off.commissionReceivers(chain, v)
	if err != nil {
		t.Fatal(err)
	}
	group, seen := []string{}, map[string]bool{}
	for _, r := range receivers {
		if r.Address != "" && !seen[r.Address] {
			seen[r.Address] = true
			group = append(group, r.Address)
		}
	}
	if op := operatorAccount(v); op != "" && !seen[op] {
		group = append(group, op)
	}
	tr := newFlowTrace(v, time.Date(2024, 3, 21, 0, 0, 0, 0, time.UTC), asOf, l)
	if err := tr.run(group); err != nil {
		t.Fatal(err)
	}
	type row struct {
		hop      int
		suf      string
		known    bool
		sim, act decimal.Decimal
		tagged   [flowRules]map[string]decimal.Decimal
	}
	var rows []row
	total := decimal.Zero
	for _, w := range tr.order {
		actual, known, err := off.flowHoldingsAt(chain, w.Addr, asOf.AddDate(0, 0, -1))
		if err != nil {
			t.Fatal(err)
		}
		r := row{hop: w.Hop, suf: w.Addr[len(w.Addr)-6:], known: known, sim: w.simulated()}
		if known {
			r.act = actual.total()
			total = total.Add(r.sim.Sub(r.act).Abs())
		}
		for k := 0; k < flowRules; k++ {
			r.tagged[k] = map[string]decimal.Decimal{}
			for _, p := range w.pools[k].liquid.parts {
				if p.Origin != flowOriginOther {
					r.tagged[k][p.Origin] = r.tagged[k][p.Origin].Add(p.Amount)
				}
			}
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].sim.Sub(rows[i].act).Abs().GreaterThan(rows[j].sim.Sub(rows[j].act).Abs())
	})
	t.Logf("wallets %d drift total %s (stored %s)", len(rows), total.Shift(-6).StringFixed(3), run.DriftBase)
	if out := os.Getenv("H_OUT"); out != "" {
		var b strings.Builder
		for _, w := range tr.order {
			actual, known, _ := off.flowHoldingsAt(chain, w.Addr, asOf.AddDate(0, 0, -1))
			d := w.simulated().Sub(actual.total())
			fmt.Fprintf(&b, "%s,%d,%v,%s,%s,%s,%s,%v,%s,%s\n", w.Addr, w.Hop, known, w.simulated().String(), actual.total().String(), d.String(), w.Since.Format(time.DateOnly), w.Known, w.Overdraw[0].String(), w.Overdraw[1].String())
		}
		if err := os.WriteFile(out, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i, r := range rows {
		if i >= 15 {
			break
		}
		t.Logf("hop %d ..%s known %v sim %s actual %s diff %s tagged-liquid fifo %v pro_rata %v", r.hop, r.suf, r.known,
			r.sim.Shift(-6).StringFixed(3), r.act.Shift(-6).StringFixed(3), r.sim.Sub(r.act).Shift(-6).StringFixed(3), shiftMap(r.tagged[0]), shiftMap(r.tagged[1]))
	}
	for _, r := range rows {
		if r.hop == 2 && (len(r.tagged[0]) > 0 || len(r.tagged[1]) > 0) {
			t.Logf("hop2 ..%s tagged-liquid fifo %v pro_rata %v", r.suf, shiftMap(r.tagged[0]), shiftMap(r.tagged[1]))
		}
	}
}

func shiftMap(m map[string]decimal.Decimal) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v.Shift(-6).StringFixed(3)
	}
	return out
}

// TestOPS818Wallet replays one wallet alone from H_FROM and compares its
// rebuilt holdings with the chain at the end of each month, to find where a
// drift starts.
func TestOPS818Wallet(t *testing.T) {
	off := connect(t, "jit=off")
	chain := apiMainnet
	node, err := NewLedgerNode()
	if err != nil {
		t.Fatal(err)
	}
	var labels []AddressLabel
	if err := off.db.Where("chain = ?", chain).Find(&labels).Error; err != nil {
		t.Fatal(err)
	}
	from, _ := time.Parse(time.DateOnly, os.Getenv("H_FROM"))
	end, _ := time.Parse(time.DateOnly, os.Getenv("H_END"))
	step := 30
	if os.Getenv("H_STEP") != "" {
		fmt.Sscan(os.Getenv("H_STEP"), &step)
	}
	for _, addr := range strings.Split(os.Getenv("H_ADDRS"), ",") {
		for asOf := from.AddDate(0, 0, step); !asOf.After(end); asOf = asOf.AddDate(0, 0, step) {
			l := &dbFlowLoader{s: off, chain: chain, node: node, labels: map[string]AddressLabel{}, mu: &sync.Mutex{}, chans: map[string]string{}}
			for _, x := range labels {
				l.labels[x.Address] = x
			}
			tr := newFlowTrace(os.Getenv("H_VALS"), from, asOf, l)
			if err := tr.run([]string{addr}); err != nil {
				t.Fatal(err)
			}
			w := tr.order[0]
			actual, known, err := off.flowHoldingsAt(chain, addr, asOf.AddDate(0, 0, -1))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("..%s through %s known %v/%v sim %s actual %s diff %s", addr[len(addr)-6:], asOf.AddDate(0, 0, -1).Format(time.DateOnly), w.Known, known,
				w.simulated().Shift(-6).StringFixed(3), actual.total().Shift(-6).StringFixed(3), w.simulated().Sub(actual.total()).Shift(-6).StringFixed(3))
		}
	}
}

func sortedStrings[T any](xs []T) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = fmt.Sprintf("%+v", x)
	}
	sort.Strings(out)
	return out
}

func diffKind(a, b reportInput, ea, eb error) string {
	if fmt.Sprint(ea) != fmt.Sprint(eb) {
		return "error"
	}
	if len(a.Wallets) != len(b.Wallets) {
		return "wallet-count"
	}
	if a.Fingerprint != b.Fingerprint {
		return "fingerprint"
	}
	kind := ""
	for i := range a.Wallets {
		x, y := a.Wallets[i], b.Wallets[i]
		if !reflect.DeepEqual(x.Memos, y.Memos) {
			if len(x.Memos) == 0 && len(y.Memos) == 0 {
				kind += "memos-nil-vs-empty;"
			} else {
				return "memos-content"
			}
		}
		for _, f := range []struct {
			n    string
			p, q []string
			eq   bool
		}{
			{"events", sortedStrings(x.Events), sortedStrings(y.Events), reflect.DeepEqual(x.Events, y.Events)},
			{"fees", sortedStrings(x.Fees), sortedStrings(y.Fees), reflect.DeepEqual(x.Fees, y.Fees)},
		} {
			if f.eq {
				continue
			}
			if reflect.DeepEqual(f.p, f.q) {
				kind += f.n + "-order;"
			} else if len(f.p) == 0 && len(f.q) == 0 {
				kind += f.n + "-nil-vs-empty;"
			} else {
				return f.n + "-content"
			}
		}
	}
	if kind == "" {
		return "other"
	}
	return kind
}
