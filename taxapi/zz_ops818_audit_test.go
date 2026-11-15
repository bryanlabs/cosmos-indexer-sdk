//go:build ops818harness

package taxapi

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestOPS818WalletOverdrawAudit(t *testing.T) {
	s := connect(t, "jit=off", "statement_timeout=90s", "lock_timeout=2s")
	file, err := os.Open(os.Getenv("H_AUDIT_CSV"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(file).ReadAll()
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	type record struct {
		Addr                            string
		Category                        string
		Trigger                         string
		Key                             flowKey
		BeforeLiquid, Amount, Shortfall decimal.Decimal
	}
	var records []record
	for _, row := range rows {
		if len(row) < 10 || row[1] == "0" || !decimal.RequireFromString(row[8]).IsPositive() {
			continue
		}
		from, err := time.Parse(time.DateOnly, row[6])
		if err != nil {
			t.Fatal(err)
		}
		l := &dbFlowLoader{s: s, chain: apiMainnet, mu: &sync.Mutex{}, labels: map[string]AddressLabel{}, chans: map[string]string{}, private: true}
		tr := newFlowTrace(os.Getenv("H_VALS"), from, time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), l)
		if err := tr.discover(row[0], 0, from, nil); err != nil {
			t.Fatal(err)
		}
		sort.SliceStable(tr.queue, func(i, j int) bool { return tr.queue[i].key.before(tr.queue[j].key) })
		for _, q := range tr.queue {
			before := q.wallet.Overdraw[0]
			liquid := q.wallet.pools[0].liquid.total()
			if err := tr.apply(q.wallet, q.event, false); err != nil {
				t.Fatal(err)
			}
			if delta := q.wallet.Overdraw[0].Sub(before); delta.IsPositive() {
				records = append(records, record{Addr: row[0], Category: q.event.Category, Trigger: q.event.Trigger, Key: q.key, BeforeLiquid: liquid, Amount: q.event.Amount, Shortfall: delta})
			}
		}
		t.Log("wallet audit native-source shortfalls", len(records), "matches full trace", tr.order[0].Overdraw[0].Equal(decimal.RequireFromString(row[8])))
	}
	b, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("H_AUDIT_OUT"), b, 0600); err != nil {
		t.Fatal(err)
	}
}
