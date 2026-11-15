//go:build ops818harness && ops818cohorts0607

package taxapi

import (
	"container/heap"
	"os"
	"path/filepath"
	"testing"
)

func TestOPS818CohortsAdditionalProofs(t *testing.T) {
	t.Run("full_run_vs_literal_checkpoint_resume", func(t *testing.T) {
		previous := xcRoot
		var e error
		xcRoot, e = os.MkdirTemp(os.Getenv("TMPDIR"), "cohorts-literal-")
		if e != nil {
			t.Fatal(e)
		}
		defer func() { xcRoot = previous }()
		l := newFakeFlowLedger()
		l.commission(flowAt(0, 1), "root", 100)
		l.send(flowAt(0, 2), "root", "receiver", 25)
		l.send(flowAt(0, 3), "receiver", "root", 10)
		full := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 1, 0), l)
		full.strict = true
		if e = full.run([]string{"root"}); e != nil {
			t.Fatal(e)
		}
		partial := newFlowTrace(flowTestValidator, flowTestStart, full.AsOf, l)
		partial.strict = true
		if e = partial.discover("root", 0, partial.From, nil); e != nil {
			t.Fatal(e)
		}
		q := heap.Pop(&partial.queue).(flowQueued)
		if e = partial.apply(q.wallet, q.event, true); e != nil {
			t.Fatal(e)
		}
		if e = xcCheckpoint(partial, "literal", "synthetic", false, 1); e != nil {
			t.Fatal(e)
		}
		resumed, e := xcRestoreCheckpoint(&xcLoader{}, filepath.Join(xcRoot, "literal"))
		if e != nil {
			t.Fatal(e)
		}
		resumed.load = l
		for resumed.queue.Len() > 0 {
			q := heap.Pop(&resumed.queue).(flowQueued)
			if e = resumed.apply(q.wallet, q.event, true); e != nil {
				t.Fatal(e)
			}
		}
		resumed.settle()
		if !xcEqual(xcFates(full), xcFates(resumed)) || !xcEqual(full.Commission, resumed.Commission) || full.seq != resumed.seq || !xcEqual(full.attempted, resumed.attempted) {
			t.Fatal("uninterrupted result differs")
		}
		for i, w := range full.order {
			if !xcEqual(xcSnapshot(w), xcSnapshot(resumed.order[i])) {
				t.Fatal("uninterrupted pools differ")
			}
		}
	})
	t.Run("negative_and_residual_rejected", func(t *testing.T) {
		w := &flowWallet{Addr: "root"}
		for r := 0; r < flowRules; r++ {
			w.pools[r] = newFlowPools(r)
		}
		s := xcSnapshot(w)
		s.Rules[0].Liquid = []flowPart{{Origin: flowOriginOther, Amount: amt(-1)}}
		s.Rules[0].Total = amt(-1)
		if _, e := xcWallet(s); e == nil {
			t.Fatal("negative pool accepted")
		}
		s = xcSnapshot(w)
		s.Rules[0].Total = amt(1)
		if _, e := xcWallet(s); e == nil {
			t.Fatal("unexplained residual accepted")
		}
	})
	t.Run("settle_exactly_once", func(t *testing.T) {
		l := &xcLoader{settled: true}
		tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 1, 0), l)
		if _, phase, e := xcRun(tr, "unused"); e == nil || phase != "already-settled-rejected" {
			t.Fatal("second settlement accepted")
		}
	})
	t.Run("partial_registration_not_silently_rewound", func(t *testing.T) {
		l := newFakeFlowLedger()
		l.commission(flowAt(0, 1), "root", 30)
		tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 1, 0), l)
		if e := tr.discover("root", 0, tr.From, nil); e != nil {
			t.Fatal(e)
		}
		q := tr.queue[0]
		rec := xcApplied{From: "root", Event: q.event, Error: "synthetic", Before: map[string]xcState{"root": xcSnapshot(q.wallet)}, After: map[string]xcState{"root": xcSnapshot(q.wallet)}, New: []xcState{{Address: "unproven"}}}
		var failed flowQueued
		if e := xcRestoreRecord(tr, rec, func(*flowWallet) ([]flowEvent, error) { return nil, nil }, &failed); e == nil {
			t.Fatal("partial registration admitted without rewind proof")
		}
	})
}
