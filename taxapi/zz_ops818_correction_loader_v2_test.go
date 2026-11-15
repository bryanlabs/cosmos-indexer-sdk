//go:build ops818harness && ops818correctionreconstructv3

package taxapi

import (
	"container/heap"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/shopspring/decimal"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

type crAddition struct {
	ID    string    `json:"source_event_id"`
	Event flowEvent `json:"event"`
}
type crCorrectionWindow struct {
	Actor     string       `json:"actor"`
	Since     time.Time    `json:"since"`
	End       time.Time    `json:"asof_exclusive"`
	Input     string       `json:"original_input_path"`
	SHA       string       `json:"original_input_sha256"`
	Additions []crAddition `json:"additions"`
}
type crBridge struct {
	Schema     string               `json:"schema"`
	Source     string               `json:"source"`
	Pins       map[string]string    `json:"source_pins"`
	Windows    []crCorrectionWindow `json:"windows"`
	Unresolved []json.RawMessage    `json:"unresolved_message_mappings"`
}
type crCorrectedLoader struct {
	plain                 *crReconstructLoader
	bridge                crBridge
	used                  map[string]int
	observed              map[string]bool
	ids                   map[string]bool
	bridgePath, bridgeSHA string
	prefix                int
}

func crLoadBridge(path, want string) (crBridge, error) {
	var b crBridge
	if e := crPin(path, want); e != nil {
		return b, e
	}
	if e := rcRead(path, &b); e != nil {
		return b, e
	}
	if b.Source != rcSource || b.Schema != "native-correction-flow-bridge-v2" || len(b.Windows) != 5 || len(b.Unresolved) != 0 {
		return b, errors.New("correction bridge incomplete")
	}
	for p, h := range b.Pins {
		if e := crPin(p, h); e != nil {
			return b, e
		}
	}
	for _, w := range b.Windows {
		if e := crPin(w.Input, w.SHA); e != nil {
			return b, e
		}
		seen := map[string]bool{}
		for _, a := range w.Additions {
			if len(a.ID) != 64 || seen[a.ID] || a.Event.Key.Time.Before(w.Since) || !a.Event.Key.Time.Before(w.End) {
				return b, errors.New("correction identity/scope invalid")
			}
			seen[a.ID] = true
			e := a.Event
			if !(e.Category == apiFee && e.Dir == apiOut && e.From == w.Actor || e.Category == "reward" && e.Dir == "in" && e.To == w.Actor) || !e.Amount.IsPositive() {
				return b, errors.New("correction economic category invalid")
			}
		}
	}
	return b, nil
}
func crMergeOccurrences(old []flowEvent, added []crAddition) []flowEvent {
	out := append([]flowEvent(nil), old...)
	for _, a := range added {
		out = append(out, a.Event)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key.before(out[j].Key) })
	return out
}
func (l *crCorrectedLoader) events(a string, s, e time.Time) ([]flowEvent, error) {
	l.observed[a] = true
	old, err := l.plain.events(a, s, e)
	if err != nil {
		return nil, err
	}
	key := rcID(a, s, e)
	bound, ok := l.plain.base.windows[key]
	actor := false
	matches := 0
	out := old
	for _, w := range l.bridge.Windows {
		if w.Actor != a {
			continue
		}
		actor = true
		if ok && w.Since.Equal(s) && w.End.Equal(e) && w.SHA == bound.InputSHA {
			matches++
			for _, addition := range w.Additions {
				if l.ids[addition.ID] {
					return nil, errors.New("native correction occurrence applied twice")
				}
				l.ids[addition.ID] = true
			}
			out = crMergeOccurrences(old, w.Additions)
			l.used[w.SHA] = len(w.Additions)
		}
	}
	if actor && matches != 1 {
		return nil, errors.New("bridge actor loaded without exact correction scope")
	}
	return out, nil
}
func (l *crCorrectedLoader) opening(a string, d time.Time) (flowHoldings, bool, error) {
	return l.plain.opening(a, d)
}
func (l *crCorrectedLoader) stop(a string) (flowStop, error)   { return l.plain.stop(a) }
func (l *crCorrectedLoader) ibcDestination(e flowEvent) string { return l.plain.ibcDestination(e) }
func crInitialWithLoader(base *rcLoader, l flowLoader) (*flowTrace, error) {
	dir := filepath.Join(base.manifest.Capture, "validator-01")
	var ident struct {
		Validator string    `json:"validator"`
		From      time.Time `json:"from"`
		End       time.Time `json:"asof_exclusive"`
		Source    string    `json:"source"`
		Roots     []string  `json:"roots"`
	}
	if e := rcRead(filepath.Join(dir, "identity.private.json"), &ident); e != nil {
		return nil, e
	}
	if ident.Source != rcSource {
		return nil, errors.New("corrected initial identity mismatch")
	}
	tr := newFlowTrace(ident.Validator, ident.From, ident.End, l)
	tr.strict = true
	e := rcLines(filepath.Join(dir, "initial-states.private.jsonl"), func(b []byte) error {
		var s rcState
		if e := json.Unmarshal(b, &s); e != nil {
			return e
		}
		if len(tr.order) >= len(ident.Roots) || s.Address != ident.Roots[len(tr.order)] {
			return errors.New("corrected initial root order mismatch")
		}
		w, e := rcWallet(s)
		if e != nil {
			return e
		}
		if tr.Wallets[w.Addr] != nil {
			return errors.New("corrected initial duplicate")
		}
		tr.Wallets[w.Addr] = w
		tr.order = append(tr.order, w)
		tr.attempted[w.Addr] = true
		events, e := l.events(w.Addr, w.Since, tr.AsOf)
		if e != nil {
			return e
		}
		rcEnqueue(tr, w, events, nil)
		return nil
	})
	if e != nil {
		return nil, e
	}
	if len(tr.order) != len(ident.Roots) {
		return nil, errors.New("corrected initial root count mismatch")
	}
	return tr, nil
}
func TestOPS818CorrectionOccurrenceMergeV2(t *testing.T) {
	at := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	old := []flowEvent{{Key: flowKey{Time: at, Height: 1}, Detail: "old-a"}, {Key: flowKey{Time: at, Height: 1}, Detail: "old-b"}}
	added := []crAddition{{ID: "new-a", Event: flowEvent{Key: flowKey{Time: at}, Detail: "new-a"}}, {ID: "new-b", Event: flowEvent{Key: flowKey{Time: at}, Detail: "new-b"}}}
	got := crMergeOccurrences(old, added)
	if len(got) != 4 || got[0].Detail != "new-a" || got[1].Detail != "new-b" || got[2].Detail != "old-a" || got[3].Detail != "old-b" {
		t.Fatal("stable source occurrences/order lost")
	}
	if old[0].Detail != "old-a" || len(old) != 2 {
		t.Fatal("original source slice mutated")
	}
}
func TestOPS818CorrectionRewardOtherOriginV2(t *testing.T) {
	tr := newFlowTrace("tracked-validator", time.Time{}, time.Time{}, nil)
	w := &flowWallet{Addr: "fixture-wallet"}
	for r := 0; r < flowRules; r++ {
		w.pools[r] = newFlowPools(r)
		w.pools[r].liquid.parts = []flowPart{{Origin: "existing-tracked-origin", Amount: decimal.NewFromInt(20)}, {Origin: flowOriginOther, Amount: decimal.NewFromInt(100)}}
	}
	e := flowEvent{Category: "reward", Dir: "in", To: w.Addr, Amount: decimal.NewFromInt(7)}
	if err := tr.apply(w, e, true); err != nil {
		t.Fatal(err)
	}
	if len(tr.Commission) != 0 || len(tr.Fates) != 0 {
		t.Fatal("reward reclassified as tracked commission")
	}
	for r := 0; r < flowRules; r++ {
		sums := map[string]decimal.Decimal{}
		for _, p := range w.pools[r].liquid.parts {
			sums[p.Origin] = sums[p.Origin].Add(p.Amount)
		}
		if !sums["existing-tracked-origin"].Equal(decimal.NewFromInt(20)) || !sums[flowOriginOther].Equal(decimal.NewFromInt(107)) {
			t.Fatal("existing capital/lot origin changed")
		}
	}
}
func TestOPS818CorrectionPrivateReplayV2(t *testing.T) {
	root := os.Getenv("OPS818_CORRECTED_REPLAY_ROOT")
	if root == "" {
		t.Skip("explicit private corrected replay scope required")
	}
	rcRoot = root
	var scope crReconstructScope
	if e := rcRead(filepath.Join(root, "scope.private.json"), &scope); e != nil {
		t.Fatal("private scope unavailable")
	}
	var extra struct {
		Bridge string `json:"bridge"`
		SHA    string `json:"bridge_sha256"`
	}
	if e := rcRead(filepath.Join(root, "correction-scope.private.json"), &extra); e != nil {
		t.Fatal("private bridge scope unavailable")
	}
	base, e := crReconstructLoad(scope)
	if e != nil {
		t.Fatal(e)
	}
	bridge, e := crLoadBridge(extra.Bridge, extra.SHA)
	if e != nil {
		t.Fatal(e)
	}
	l := &crCorrectedLoader{plain: &crReconstructLoader{base}, bridge: bridge, used: map[string]int{}, observed: map[string]bool{}, ids: map[string]bool{}, bridgePath: extra.Bridge, bridgeSHA: extra.SHA}
	tr, e := crInitialWithLoader(base, l)
	if e != nil {
		t.Fatal(e)
	}
	n, e := crReplayUnchanged(tr, scope, base)
	if e != nil {
		t.Fatal(e)
	}
	l.prefix = n
	if e := crPrivateCheckpoint(tr, l, "corrected-proved-unaffected-prefix", "private_corrected_unaffected_prefix", n); e != nil {
		t.Fatal("corrected prefix checkpoint failed")
	}
	// Every failed attempt is retained. Only an exact rolled-back source-stage
	// failure may become a safe resumable checkpoint; other failures stay failed.
	applied := 0
	resumePath := os.Getenv("OPS818_CORRECTION_CHECKPOINT")
	resumeAt := -1
	if resumePath != "" {
		d, err := crReadResume(resumePath, extra.SHA)
		if err != nil {
			t.Fatal(err)
		}
		v, ok := d.Runtime["applied_total"].(float64)
		if !ok || int(v) < n {
			t.Fatal("corrected resume prefix invalid")
		}
		resumeAt = int(v) - n
	}
	for tr.queue.Len() > 0 {
		if resumeAt == applied {
			if e := crPrivateCheckpoint(tr, l, "resume-reconstructed-verification", "private_verified_reconstruction_resume", n+applied); e != nil {
				t.Fatal(e)
			}
			if e := crVerifyResume(resumePath, filepath.Join(root, "resume-reconstructed-verification"), extra.SHA); e != nil {
				t.Fatal(e)
			}
			resumeAt = -1
		}

		q := heap.Pop(&tr.queue).(flowQueued)
		before := map[string]rcState{q.wallet.Addr: rcSnapshot(q.wallet)}
		if w := tr.Wallets[q.event.To]; w != nil {
			before[w.Addr] = rcSnapshot(w)
		}
		oldOrder, oldSeq, oldPending := len(tr.order), tr.seq, tr.queue.Len()
		hadAttempt, attemptPresent := tr.attempted[q.event.To]
		potentialDiscovery := q.event.Category == apiTransfer && q.event.Dir == apiOut && tr.Wallets[q.event.To] == nil
		oldFates, oldCommission := tr.Fates, tr.Commission
		if potentialDiscovery {
			oldFates = maps.Clone(tr.Fates)
			oldCommission = maps.Clone(tr.Commission)
		}
		oldUnresolved := len(tr.Unresolved)
		e := tr.apply(q.wallet, q.event, true)
		if e != nil {
			_ = rcSave(filepath.Join(root, "failed-attempt.private.json"), map[string]any{"source_wallet": q.wallet.Addr, "event": q.event, "seq": q.seq, "before": before, "error": e.Error(), "partial_attempt_not_accepted": true, "financially_accepted": false})
			if !crIsMissingSource(e) {
				t.Fatal("private integrity/routing failure retained, not a missing-source checkpoint")
			}
			if resumeAt >= 0 {
				t.Fatal("corrected resume target not reached before dependency")
			}
			if !potentialDiscovery || len(tr.order) != oldOrder || tr.seq != oldSeq || tr.queue.Len() != oldPending {
				t.Fatal("private apply failed after discovery mutation; last good checkpoint retained")
			}
			for a, s := range before {
				w, err := rcWallet(s)
				if err != nil {
					t.Fatal("rollback state invalid")
				}
				*tr.Wallets[a] = *w
			}
			tr.Fates = oldFates
			tr.Commission = oldCommission
			tr.Unresolved = tr.Unresolved[:oldUnresolved]
			if attemptPresent {
				tr.attempted[q.event.To] = hadAttempt
			} else {
				delete(tr.attempted, q.event.To)
			}
			heap.Push(&tr.queue, q)
			if err := crPrivateCheckpoint(tr, l, "private-awaiting-retained-source", "private_source_dependency_not_financial_acceptance", n+applied); err != nil {
				t.Fatal("private dependency checkpoint failed")
			}
			fmt.Printf("phase private_corrected_replay_dependency records %d pending %d\n", applied, tr.queue.Len())
			return
		}
		applied++
		if applied%1024 == 0 {
			if e := currentAppend(filepath.Join(root, "corrected-progress.safe.jsonl"), map[string]any{"at": time.Now().UTC(), "applied_after_boundary": applied, "pending_events": tr.queue.Len(), "wallets": len(tr.order), "exact_source_handlers_consumed": len(l.used), "financially_accepted": false}); e != nil {
				t.Fatal("private progress failed")
			}
		}
		if applied%32768 == 0 {
			if e := crPrivateCheckpoint(tr, l, fmt.Sprintf("corrected-checkpoint-%09d", applied), "private_corrected_continuation", n+applied); e != nil {
				t.Fatal("private checkpoint failed")
			}
		}
	}
	if resumeAt >= 0 {
		t.Fatal("corrected resume target not reached")
	}
	if e := crPrivateCheckpoint(tr, l, "private-source-replay-exhausted", "private_source_replay_exhausted_not_reconciled", n+applied); e != nil {
		t.Fatal("private exhausted checkpoint failed")
	}
	fmt.Printf("phase private_corrected_replay_exhausted records %d\n", applied)
}
