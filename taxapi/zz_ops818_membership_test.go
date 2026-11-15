//go:build ops818harness

package taxapi

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// This read-only experiment records a real tagged route before doing the
// expensive collector load. The sentinel fate is never stored or published.
type collectorProofLoader struct {
	*dbFlowLoader
	target, receipt string
}

func (l *collectorProofLoader) stop(addr string) (flowStop, error) {
	if addr == l.target {
		if err := os.WriteFile(l.receipt, []byte("tagged commission route reached collector\n"), 0600); err != nil {
			return flowStop{}, err
		}
		return flowStop{Class: flowBeyondHops, Detail: "private membership experiment"}, nil
	}
	return l.dbFlowLoader.stop(addr)
}

func (l *collectorProofLoader) events(addr string, start, end time.Time) ([]flowEvent, error) {
	f, err := os.OpenFile(l.receipt+".progress", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	began := time.Now()
	fmt.Fprintf(f, "load %s %s\n", addr, start.Format(time.DateOnly))
	rows, err := l.dbFlowLoader.events(addr, start, end)
	fmt.Fprintf(f, "done %s %d %s %v\n", addr, len(rows), time.Since(began), err)
	return rows, err
}

func TestOPS818CollectorMembership(t *testing.T) {
	s := connect(t, "jit=off", "statement_timeout=90s", "lock_timeout=2s")
	s.readOnlyLedger = true
	var labels []AddressLabel
	if err := s.db.Where("chain = ?", apiMainnet).Find(&labels).Error; err != nil {
		t.Fatal(err)
	}
	node, err := NewLedgerNode()
	if err != nil {
		t.Fatal(err)
	}
	base := &dbFlowLoader{s: s, chain: apiMainnet, node: node, private: true, mu: &sync.Mutex{}, labels: map[string]AddressLabel{}, chans: map[string]string{}}
	if rpc := os.Getenv("H_ARCHIVE_RPC"); rpc != "" {
		base.archive, err = newFlowArchive(rpc)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			b, _ := json.MarshalIndent(base.archive.evidence(nil), "", "  ")
			_ = os.WriteFile(os.Getenv("H_MEMBERSHIP_OUT")+".archive.private.json", b, 0600)
		})
	}
	for _, l := range labels {
		base.labels[l.Address] = l
	}
	l := &collectorProofLoader{dbFlowLoader: base, target: os.Getenv("H_COLLECTOR"), receipt: os.Getenv("H_MEMBERSHIP_OUT")}
	v := os.Getenv("H_VALS")
	receivers, err := s.commissionReceivers(apiMainnet, v)
	if err != nil {
		t.Fatal(err)
	}
	var group []string
	seen := map[string]bool{}
	for _, r := range receivers {
		if r.Address != "" && !seen[r.Address] {
			group = append(group, r.Address)
			seen[r.Address] = true
		}
	}
	if op := operatorAccount(v); op != "" && !seen[op] {
		group = append(group, op)
	}
	tr := newFlowTrace(v, time.Date(2024, 3, 21, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), l)
	tr.strict = true
	if err := tr.run(group); err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(l.receipt)
	t.Log("membership marker exists", err == nil)
}
