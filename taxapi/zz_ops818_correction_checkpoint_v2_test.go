//go:build ops818harness && ops818correctionreconstructv3

package taxapi

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

var crMissingOpening = errors.New("private retained opening absent")
var crMissingClass = errors.New("private retained classification absent")
var crMissingBinding = errors.New("private converted binding absent")

func crIsMissingSource(e error) bool {
	return errors.Is(e, crMissingOpening) || errors.Is(e, crMissingClass) || errors.Is(e, crMissingBinding)
}
func crCoverage(l *crCorrectedLoader) []map[string]any {
	out := []map[string]any{}
	for _, w := range l.bridge.Windows {
		reason := "actor never requested by this private trace"
		if l.observed[w.Actor] {
			reason = "this exact source window never loaded; alternate exact window or absent retained source"
		}
		if _, ok := l.used[w.SHA]; ok {
			reason = "exact source input/window consumed"
		}
		out = append(out, map[string]any{"input_sha256": w.SHA, "consumed": l.used[w.SHA], "reason": reason})
	}
	return out
}
func crPrivateCheckpoint(tr *flowTrace, l *crCorrectedLoader, name, phase string, n int) error {
	dir := filepath.Join(rcRoot, name)
	if e := os.Mkdir(dir, 0700); e != nil {
		return e
	}
	enc, close, e := rcCheckpointWriter(filepath.Join(dir, "full-queue.private.jsonl.gz"))
	if e != nil {
		return e
	}
	q := append(flowQueue(nil), tr.queue...)
	sort.Sort(q)
	for _, x := range q {
		if e = enc.Encode(rcQueueRecord{x.wallet.Addr, x.event, x.seq}); e != nil {
			_ = close()
			return e
		}
	}
	if e = close(); e != nil {
		return e
	}
	enc, close, e = rcCheckpointWriter(filepath.Join(dir, "states.private.jsonl.gz"))
	if e != nil {
		return e
	}
	for _, w := range tr.order {
		if e = enc.Encode(rcSnapshot(w)); e != nil {
			_ = close()
			return e
		}
	}
	if e = close(); e != nil {
		return e
	}
	if e = rcSave(filepath.Join(dir, "partial-rows.private.json"), map[string]any{"rows": rcMarkedFates(tr), "partial": true, "financially_accepted": false, "closing_reconciled": false}); e != nil {
		return e
	}
	hashes := map[string]string{}
	for _, name := range []string{"full-queue.private.jsonl.gz", "states.private.jsonl.gz", "partial-rows.private.json"} {
		h, e := rcHash(filepath.Join(dir, name))
		if e != nil {
			return e
		}
		hashes[name] = h
	}
	runtime := map[string]any{"source": rcSource, "schema": "private-native-corrected-checkpoint-v2", "correction_bridge_sha256": l.bridgeSHA, "seq": tr.seq, "pending_events": tr.queue.Len(), "wallets": len(tr.order), "validator": tr.Validator, "from": tr.From, "asof_exclusive": tr.AsOf, "attempted": tr.attempted, "stops": tr.stops, "commission": tr.Commission, "unresolved": tr.Unresolved, "applied_total": n, "original_prefix_records": l.prefix, "consumed_handlers": l.used, "registered_window_coverage": crCoverage(l), "financially_accepted": false, "closing_reconciled": false}
	// Deliberately NO trace-state.private.json. The unchanged plain restore
	// fails closed on this format before it can discard a correction handler.
	if e = rcSave(filepath.Join(dir, "corrected-trace-state.private.json"), map[string]any{"runtime": runtime, "component_sha256": hashes, "phase": phase, "correction_bridge_path": l.bridgePath, "corrected_resume": "verified deterministic reconstruction only", "financially_accepted": false}); e != nil {
		return e
	}
	return rcSave(filepath.Join(dir, "correction-source-provenance.private.json"), map[string]any{"source_pins": l.plain.base.used, "bridge_source_pins": l.bridge.Pins, "registered_exact_source_windows": l.bridge.Windows, "actually_consumed_source_sha256_addition_counts": l.used, "original_statuses": l.plain.base.manifest.Statuses, "original_files_modified": false, "new_enqueue_sequences_not_claimed_original": true, "source_queries": 0, "financially_accepted": false})
}

type crResumeDescriptor struct {
	Runtime map[string]any    `json:"runtime"`
	Hashes  map[string]string `json:"component_sha256"`
}

func crReadResume(path, bridge string) (crResumeDescriptor, error) {
	var d crResumeDescriptor
	if e := rcRead(filepath.Join(path, "corrected-trace-state.private.json"), &d); e != nil {
		return d, e
	}
	if d.Runtime["schema"] != "private-native-corrected-checkpoint-v2" || d.Runtime["source"] != rcSource || d.Runtime["correction_bridge_sha256"] != bridge {
		return d, errors.New("corrected resume source/bridge mismatch")
	}
	if len(d.Hashes) != 3 {
		return d, errors.New("corrected resume components incomplete")
	}
	for n, h := range d.Hashes {
		if n != "full-queue.private.jsonl.gz" && n != "states.private.jsonl.gz" && n != "partial-rows.private.json" {
			return d, errors.New("unknown corrected component")
		}
		if e := crPin(filepath.Join(path, n), h); e != nil {
			return d, e
		}
	}
	return d, nil
}
func crVerifyResume(want, actual string, bridge string) error {
	a, e := crReadResume(want, bridge)
	if e != nil {
		return e
	}
	b, e := crReadResume(actual, bridge)
	if e != nil {
		return e
	}
	if !rcEqual(a.Runtime, b.Runtime) || !rcEqual(a.Hashes, b.Hashes) {
		return errors.New("corrected reconstruction resume state/queue/fate/handler mismatch")
	}
	return nil
}
func TestOPS818CorrectionDependencyTypesV2(t *testing.T) {
	if !crIsMissingSource(crMissingOpening) || !crIsMissingSource(crMissingClass) || !crIsMissingSource(crMissingBinding) {
		t.Fatal("missing source not classified")
	}
	for _, e := range []error{errors.New("converted source bytes mismatch"), errors.New("bounds/order mismatch"), errors.New("destination unresolved")} {
		if crIsMissingSource(e) {
			t.Fatal("integrity/routing error masked as missing source")
		}
	}
}
func TestOPS818CorrectionCheckpointFormatV2(t *testing.T) {
	old := rcRoot
	defer func() { rcRoot = old }()
	rcRoot = t.TempDir()
	tr := newFlowTrace("fixture", time.Time{}, time.Time{}, nil)
	l := &crCorrectedLoader{plain: &crReconstructLoader{&rcLoader{}}, used: map[string]int{}, observed: map[string]bool{}, bridgeSHA: "bridge-fixture"}
	if e := crPrivateCheckpoint(tr, l, "fixture", "test", 0); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(rcRoot, "fixture", "trace-state.private.json")); !os.IsNotExist(e) {
		t.Fatal("plain checkpoint format exposed")
	}
	if _, e := crReadResume(filepath.Join(rcRoot, "fixture"), "different-bridge"); e == nil {
		t.Fatal("different bridge accepted")
	}
	if e := crVerifyResume(filepath.Join(rcRoot, "fixture"), filepath.Join(rcRoot, "fixture"), "bridge-fixture"); e != nil {
		t.Fatal(e)
	}
}
