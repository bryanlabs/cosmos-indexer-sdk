//go:build ops818harness && ops818correctionreconstructv1

package taxapi

import (
	"container/heap"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// This loader is deliberately cached-only. It cannot issue source queries,
// classify a destination, publish an admission, or mutate a retained cache.
type crReconstructLoader struct{ base *rcLoader }

func (l *crReconstructLoader) events(a string, s, e time.Time) ([]flowEvent, error) {
	return l.base.cached(a, s, e)
}
func (l *crReconstructLoader) opening(a string, d time.Time) (flowHoldings, bool, error) {
	x, ok := l.base.openings[rcID(a, d, d)]
	if !ok {
		return flowHoldings{}, false, errors.New("reconstruction lacks retained opening")
	}
	return x.Holdings, x.Known, nil
}
func (l *crReconstructLoader) stop(a string) (flowStop, error) {
	x, ok := l.base.stops[a]
	if !ok {
		return flowStop{}, errors.New("reconstruction lacks retained classification")
	}
	return x, nil
}
func (l *crReconstructLoader) ibcDestination(e flowEvent) string {
	return l.base.ibcDestination(e)
}

type crReconstructScope struct {
	Manifest            string            `json:"manifest"`
	ManifestSHA         string            `json:"manifest_sha256"`
	OriginalIdentitySHA string            `json:"original_identity_sha256"`
	SupplementSHA       string            `json:"supplement_binding_sha256"`
	OriginalAppliedSHA  string            `json:"original_applied_sha256"`
	OriginalInitialSHA  string            `json:"original_initial_sha256"`
	OriginalOpeningSHA  string            `json:"original_opening_sha256"`
	OriginalRoutesSHA   string            `json:"original_routes_sha256"`
	Boundary            flowKey           `json:"earliest_corrected_event_key"`
	BoundaryEvidence    string            `json:"boundary_evidence"`
	BoundaryEvidenceSHA string            `json:"boundary_evidence_sha256"`
	Core                map[string]string `json:"core_files"`
}

func crPin(p, w string) error {
	if len(w) != 64 {
		return errors.New("reconstruction missing pin")
	}
	h, e := rcHash(p)
	if e != nil || h != w {
		return errors.New("reconstruction evidence hash mismatch")
	}
	return nil
}
func crReconstructLoad(scope crReconstructScope) (*rcLoader, error) {
	if e := crPin(scope.Manifest, scope.ManifestSHA); e != nil {
		return nil, e
	}
	var m rcManifest
	if e := rcRead(scope.Manifest, &m); e != nil {
		return nil, e
	}
	if m.Source != rcSource {
		return nil, errors.New("reconstruction source mismatch")
	}
	base := &rcLoader{manifest: m, windows: map[string]rcWindow{}, openings: map[string]rcOpening{}, stops: map[string]flowStop{}, loaded: map[string]bool{}, used: map[string]string{}}
	for _, w := range m.Windows {
		base.windows[w.Key] = w
	}
	dir := filepath.Join(m.Capture, "validator-01")
	for _, p := range []struct{ n, h string }{{"identity.private.json", scope.OriginalIdentitySHA}, {"applied.private.jsonl", scope.OriginalAppliedSHA}, {"initial-states.private.jsonl", scope.OriginalInitialSHA}, {"openings.private.jsonl", scope.OriginalOpeningSHA}, {"tagged-routes.private.jsonl", scope.OriginalRoutesSHA}} {
		if e := crPin(filepath.Join(dir, p.n), p.h); e != nil {
			return nil, e
		}
		base.used[filepath.Join(dir, p.n)] = p.h
	}
	if e := crPin(scope.BoundaryEvidence, scope.BoundaryEvidenceSHA); e != nil {
		return nil, e
	}
	base.used[scope.BoundaryEvidence] = scope.BoundaryEvidenceSHA
	for p, h := range scope.Core {
		if e := crPin(p, h); e != nil {
			return nil, e
		}
		base.used[p] = h
	}
	e := rcLines(filepath.Join(dir, "openings.private.jsonl"), func(b []byte) error {
		var x struct {
			Address  string    `json:"address"`
			Day      time.Time `json:"day"`
			Known    bool      `json:"known"`
			Error    string    `json:"error"`
			Evidence rcOpening `json:"evidence"`
		}
		if e := json.Unmarshal(b, &x); e != nil {
			return e
		}
		if !x.Known || x.Error != "<nil>" || !x.Evidence.Known {
			return errors.New("incomplete retained opening")
		}
		if x.Evidence.Address != x.Address || !x.Evidence.Day.Equal(x.Day) {
			return errors.New("retained opening identity mismatch")
		}
		if e := crPin(x.Evidence.SourcePath, x.Evidence.SourceSHA256); e != nil {
			return e
		}
		base.used[x.Evidence.SourcePath] = x.Evidence.SourceSHA256
		k := rcID(x.Address, x.Day, x.Day)
		if old, ok := base.openings[k]; ok && !rcEqual(old, x.Evidence) {
			return errors.New("conflicting retained opening")
		}
		base.openings[k] = x.Evidence
		return nil
	})
	if e != nil {
		return nil, e
	}
	e = rcLines(filepath.Join(dir, "tagged-routes.private.jsonl"), func(b []byte) error {
		var x struct {
			Event flowEvent `json:"event"`
			Stop  flowStop  `json:"stop"`
		}
		if e := json.Unmarshal(b, &x); e != nil {
			return e
		}
		if x.Event.To == "" {
			return nil
		}
		if old, ok := base.stops[x.Event.To]; ok && !rcEqual(old, x.Stop) {
			return errors.New("conflicting retained classification")
		}
		base.stops[x.Event.To] = x.Stop
		return nil
	})
	if e != nil {
		return nil, e
	}
	if scope.SupplementSHA != "" {
		path := filepath.Join(rcRoot, "source-evidence.private.json")
		if e := crPin(path, scope.SupplementSHA); e != nil {
			return nil, e
		}
		rcRequestRoot = rcRoot
		if e := base.supplement(); e != nil {
			return nil, e
		}
		base.used[path] = scope.SupplementSHA
	}
	return base, nil
}
func crInitial(base *rcLoader) (*flowTrace, error) {
	var initial []rcState
	dir := filepath.Join(base.manifest.Capture, "validator-01")
	if e := rcLines(filepath.Join(dir, "initial-states.private.jsonl"), func(b []byte) error {
		var s rcState
		if e := json.Unmarshal(b, &s); e != nil {
			return e
		}
		initial = append(initial, s)
		return nil
	}); e != nil {
		return nil, e
	}
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
	if ident.Source != rcSource || len(ident.Roots) != len(initial) {
		return nil, errors.New("retained initial identity mismatch")
	}
	if len(initial) == 0 {
		return nil, errors.New("no retained initial states")
	}
	l := &crReconstructLoader{base}
	tr := newFlowTrace(ident.Validator, ident.From, ident.End, l)
	tr.strict = true
	for _, s := range initial {
		if tr.Wallets[s.Address] != nil {
			return nil, errors.New("duplicate initial state")
		}
		w, e := rcWallet(s)
		if e != nil {
			return nil, e
		}
		tr.Wallets[w.Addr] = w
		tr.order = append(tr.order, w)
		tr.attempted[w.Addr] = true
		ev, e := l.events(w.Addr, w.Since, tr.AsOf)
		if e != nil {
			return nil, e
		}
		rcEnqueue(tr, w, ev, nil)
		base.loaded[w.Addr] = true
	}
	return tr, nil
}

// Baseline replay reconstructs fates, commissions, queues and all wallet pools
// with the unchanged retained engine, not by copying final fate rows backwards.
// Every replayed committed record must match its exact original before/after.
func crReplayUnchanged(tr *flowTrace, scope crReconstructScope, base *rcLoader) (int, error) {
	sentinel := errors.New("at earliest correction boundary")
	n := 0
	atBoundary := false
	e := rcLines(filepath.Join(base.manifest.Capture, "validator-01", "applied.private.jsonl"), func(b []byte) error {
		var rec rcApplied
		if e := json.Unmarshal(b, &rec); e != nil {
			return e
		}
		if !rec.Event.Key.before(scope.Boundary) {
			atBoundary = true
			return sentinel
		}
		if rec.Error != "<nil>" || tr.queue.Len() == 0 {
			return errors.New("unaffected prefix contains unsupported failure")
		}
		q := heap.Pop(&tr.queue).(flowQueued)
		if q.wallet.Addr != rec.From || !rcEqual(q.event, rec.Event) {
			return errors.New("reconstruction occurrence/order mismatch")
		}
		for a, s := range rec.Before {
			w := tr.Wallets[a]
			if w == nil || !rcEqual(rcSnapshot(w), s) {
				return errors.New("reconstruction before-state mismatch")
			}
		}
		oldWallets := len(tr.order)
		if e := tr.apply(q.wallet, q.event, true); e != nil {
			return errors.New("reconstruction unchanged apply failed")
		}
		for a, s := range rec.After {
			w := tr.Wallets[a]
			if w == nil || !rcEqual(rcSnapshot(w), s) {
				return errors.New("reconstruction after-state mismatch")
			}
		}
		got := []rcState{}
		for _, w := range tr.order[oldWallets:] {
			got = append(got, rcSnapshot(w))
		}
		if len(got) != len(rec.New) {
			return errors.New("reconstruction discovery count mismatch")
		}
		for i, s := range rec.New {
			if !rcEqual(got[i], s) {
				return errors.New("reconstruction discovery state mismatch")
			}
		}
		n++
		if n%10000 == 0 {
			_ = currentAppend(filepath.Join(rcRoot, "reconstruction-progress.safe.jsonl"), map[string]any{"phase": "reconstructing_unchanged_prefix", "records": n, "wallets": len(tr.order), "pending_events": tr.queue.Len(), "at": time.Now().UTC(), "financially_accepted": false})
		}
		return nil
	})
	if e != nil && !errors.Is(e, sentinel) {
		return n, e
	}
	if !atBoundary || tr.queue.Len() == 0 || tr.queue[0].key.before(scope.Boundary) {
		return n, errors.New("original prefix does not prove earliest correction boundary")
	}
	return n, nil
}
func TestOPS818CorrectionReconstructInitialV1(t *testing.T) {
	root := os.Getenv("OPS818_RECONSTRUCT_ROOT")
	if root == "" {
		t.Skip("explicit private reconstruction scope required")
	}
	rcRoot = root
	var scope crReconstructScope
	if e := rcRead(filepath.Join(root, "scope.private.json"), &scope); e != nil {
		t.Fatal("private reconstruction scope failed")
	}
	base, e := crReconstructLoad(scope)
	if e != nil {
		t.Fatal(e)
	}
	tr, e := crInitial(base)
	if e != nil {
		t.Fatal(e)
	}
	n, e := crReplayUnchanged(tr, scope, base)
	if e != nil {
		t.Fatal(e)
	}
	savedLoader := tr.load
	tr.load = base
	if e = rcCheckpoint(tr, "proved-unaffected-before-earliest-native", "private_reconstructed_baseline", false, n); e != nil {
		t.Fatal("private checkpoint failed")
	}
	tr.load = savedLoader
	keys := []string{}
	for p := range base.used {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	e = rcSave(filepath.Join(root, "reconstruction-result.private.json"), map[string]any{"source": rcSource, "phase": "complete_unaffected_prefix_reconstructed", "records_exact_before_after_checked": n, "wallets": len(tr.order), "pending_events": tr.queue.Len(), "used_file_sha256": base.used, "core_files": scope.Core, "earliest_corrected_event_key": scope.Boundary, "boundary_evidence": scope.BoundaryEvidence, "boundary_evidence_sha256": scope.BoundaryEvidenceSHA, "original_files_modified": false, "new_current_lot_result_proved": false, "financially_accepted": false, "closing_reconciled": false, "public_promoted": false})
	if e != nil {
		t.Fatal("private reconstruction receipt failed")
	}
	fmt.Printf("phase proved_unaffected_checkpoint_complete records %d wallets %d pending %d\n", n, len(tr.order), tr.queue.Len())
}
