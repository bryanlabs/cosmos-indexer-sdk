//go:build ops818dynamic0607v1 && ops818harness && ops818cohorts0607

package taxapi

import (
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type dyChildPlan struct {
	Source         string            `json:"source"`
	ParentPID      int               `json:"parent_pid"`
	Checkpoint     string            `json:"checkpoint"`
	ParentPhase    string            `json:"parent_phase"`
	ParentPhaseSHA string            `json:"parent_phase_sha256"`
	Address        string            `json:"address"`
	Since          time.Time         `json:"since"`
	End            time.Time         `json:"asof_exclusive"`
	Key            string            `json:"canonical_window"`
	Files          map[string]string `json:"preserved_files_sha256"`
}

func dyPayloadHash(p string) (string, error) {
	f, e := os.Open(p)
	if e != nil {
		return "", e
	}
	defer f.Close()
	z, e := gzip.NewReader(f)
	if e != nil {
		return "", e
	}
	defer z.Close()
	h := sha256.New()
	if _, e = io.Copy(h, z); e != nil {
		return "", e
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
func TestOPS818DynamicExactChild(t *testing.T) {
	syscall.Umask(0077)
	dyRoot = os.Getenv("OPS818_DYNAMIC_ROOT")
	dyRequestRoot = os.Getenv("OPS818_DYNAMIC_REQUEST_ROOT")
	if dyRoot == "" || dyRequestRoot == "" || dyRoot == dyRequestRoot {
		t.Fatal("exclusive dynamic root required")
	}
	var plan dyChildPlan
	if dyRead(filepath.Join(dyRoot, "adoption-plan.private.json"), &plan) != nil || plan.Source != dySource || plan.Key != dyID(plan.Address, plan.Since, plan.End) || plan.ParentPID != 689529 {
		t.Fatal("exact dynamic plan rejected")
	}
	if syscall.Kill(plan.ParentPID, 0) != nil {
		t.Fatal("original parent must remain live")
	}
	for p, want := range plan.Files {
		got, e := dyHash(p)
		if e != nil || got != want {
			t.Fatal("preserved parent file changed")
		}
	}
	if got, e := dyHash(plan.ParentPhase); e != nil || got != plan.ParentPhaseSHA {
		t.Fatal("parent is not at bound blocked phase")
	}
	var m dyManifest
	if dyRead(filepath.Join(dyRoot, "restore-manifest.private.json"), &m) != nil || m.Source != dySource {
		t.Fatal("restore manifest rejected")
	}
	for p, b := range m.Files {
		got, e := dyHash(p)
		if e != nil || got != b.SHA {
			t.Fatal("original restore binding changed")
		}
	}
	l := &dyLoader{manifest: m, windows: map[string]dyWindow{}, openings: map[string]dyOpening{}, stops: map[string]flowStop{}, loaded: map[string]bool{}, used: map[string]string{}, requested: map[string]bool{}}
	for _, w := range m.Windows {
		key := dyID(w.Address, w.Since, w.End)
		if old, ok := l.windows[key]; ok && !dyEqual(old, w) {
			t.Fatal("retained window conflict")
		}
		l.windows[key] = w
	}
	var cohort struct{ OpeningEvidence []dyOpening }
	if dyRead(filepath.Join(m.Old, "cohort.private.json"), &cohort) != nil {
		t.Fatal("retained openings unavailable")
	}
	for _, o := range cohort.OpeningEvidence {
		got, e := dyHash(o.SourcePath)
		if e != nil || got != o.SourceSHA256 {
			t.Fatal("retained opening changed")
		}
		key := dyID(o.Address, o.Day, o.Day)
		if old, ok := l.openings[key]; ok && !currentHoldingsMoneyEqual(old.Holdings, o.Holdings) {
			t.Fatal("opening conflict")
		}
		l.openings[key] = o
	}
	// A completed exact new source proof is required before restoring or launching.
	if _, ready, e := dyAdmissionEvents(l, plan.Address, plan.Since, plan.End); e != nil || !ready {
		t.Fatal("new source proof not passed")
	}
	tr, e := dyRestoreCheckpoint(l, plan.Checkpoint)
	if e != nil {
		t.Fatal("literal current child restore rejected")
	}
	if tr.queue.Len() == 0 || tr.queue[0].event.To != plan.Address || !flowDay(tr.queue[0].event.Key.Time).Equal(plan.Since) || !tr.AsOf.Equal(plan.End) || tr.queue[0].event.Category != apiTransfer || tr.queue[0].event.Dir != apiOut || tr.Wallets[plan.Address] != nil {
		t.Fatal("exact failed head scope rejected")
	}
	if e = dyCheckpoint(tr, "adopted", "exact-current-child-adopted-no-replay", false, l.priorApplied); e != nil {
		t.Fatal("adopted checkpoint save failed")
	}
	payloads := map[string]string{}
	for _, name := range []string{"states.private.jsonl.gz", "full-queue.private.jsonl.gz"} {
		old, e := dyPayloadHash(filepath.Join(plan.Checkpoint, name))
		if e != nil {
			t.Fatal("old payload rejected")
		}
		fresh, e := dyPayloadHash(filepath.Join(dyRoot, "adopted", name))
		if e != nil || old != fresh {
			t.Fatal("literal committed state or pending queue changed")
		}
		payloads[name] = old
	}
	for _, name := range []string{"partial-rows.private.json", "original-cohort-statuses.private.json"} {
		var old, fresh any
		if dyRead(filepath.Join(plan.Checkpoint, name), &old) != nil || dyRead(filepath.Join(dyRoot, "adopted", name), &fresh) != nil || !dyEqual(old, fresh) {
			t.Fatal("old fates or status changed")
		}
	}
	var oldState, newState map[string]any
	if dyRead(filepath.Join(plan.Checkpoint, "trace-state.private.json"), &oldState) != nil || dyRead(filepath.Join(dyRoot, "adopted/trace-state.private.json"), &newState) != nil {
		t.Fatal("trace state unavailable")
	}
	delete(oldState, "phase")
	delete(newState, "phase")
	if !dyEqual(oldState, newState) {
		t.Fatal("accounting counters or trace state changed")
	}
	if got, e := dyHash(plan.ParentPhase); e != nil || got != plan.ParentPhaseSHA {
		t.Fatal("parent moved during adoption")
	}
	if e = dySave(filepath.Join(dyRoot, "adoption-proof.private.json"), map[string]any{"source": dySource, "parent_pid": plan.ParentPID, "checkpoint": plan.Checkpoint, "prior_applied": l.priorApplied, "pending_events": tr.queue.Len(), "payload_sha256": payloads, "literal_state_queue_fates_counters_status_preserved": true, "committed_occurrences_replayed": 0, "financially_accepted": false, "closing_reconciled": false}); e != nil {
		t.Fatal("adoption proof failed")
	}
	fmt.Printf("phase dynamic_exact_adopted count %d pending %d\n", l.priorApplied, tr.queue.Len())
	if os.Getenv("OPS818_DYNAMIC_PREFLIGHT_ONLY") == "1" {
		return
	}
	if e = os.Mkdir(filepath.Join(dyRoot, "windows"), 0700); e != nil {
		t.Fatal("exclusive windows required")
	}
	if e = os.Mkdir(filepath.Join(dyRoot, "run"), 0700); e != nil {
		t.Fatal("exclusive run required")
	}
	if e = l.supplement(); e != nil {
		t.Fatal("actual cumulative source reply rejected")
	}
	applied, phase, runError := dyRun(tr, filepath.Join(dyRoot, "run"))
	if e = dyCheckpoint(tr, "terminal", phase, runError == nil || l.settled, applied); e != nil {
		t.Fatal("terminal proof failed")
	}
	if e = dySave(filepath.Join(dyRoot, "runtime-result-safe.json"), map[string]any{"source": dySource, "phase": phase, "pid": os.Getpid(), "applied_new": applied, "pending_events": tr.queue.Len(), "financially_accepted": false, "closing_reconciled": false}); e != nil {
		t.Fatal("runtime proof failed")
	}
	if runError != nil {
		t.Fatal("dynamic continuation failed, exact private terminal retained")
	}
}
