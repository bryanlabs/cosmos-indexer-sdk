//go:build ops818dynamic0607v2 && ops818harness && ops818cohorts0607

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

type dzChildPlan struct {
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

func dzPayloadHash(p string) (string, error) {
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
func TestOPS818DynamicV2ExactChild(t *testing.T) {
	syscall.Umask(0077)
	dzRoot = os.Getenv("OPS818_DYNAMIC_ROOT")
	dzRequestRoot = os.Getenv("OPS818_DYNAMIC_REQUEST_ROOT")
	if dzRoot == "" || dzRequestRoot == "" || dzRoot == dzRequestRoot {
		t.Fatal("exclusive dynamic root required")
	}
	var plan dzChildPlan
	if dzRead(filepath.Join(dzRoot, "adoption-plan.private.json"), &plan) != nil || plan.Source != dzSource || plan.Key != dzID(plan.Address, plan.Since, plan.End) || plan.ParentPID != 600482 {
		t.Fatal("exact dynamic plan rejected")
	}
	if syscall.Kill(plan.ParentPID, 0) != nil {
		t.Fatal("original parent must remain live")
	}
	for p, want := range plan.Files {
		got, e := dzHash(p)
		if e != nil || got != want {
			t.Fatal("preserved parent file changed")
		}
	}
	if got, e := dzHash(plan.ParentPhase); e != nil || got != plan.ParentPhaseSHA {
		t.Fatal("parent is not at bound blocked phase")
	}
	var m dzManifest
	if dzRead(filepath.Join(dzRoot, "restore-manifest.private.json"), &m) != nil || m.Source != dzSource {
		t.Fatal("restore manifest rejected")
	}
	for p, b := range m.Files {
		got, e := dzHash(p)
		if e != nil || got != b.SHA {
			t.Fatal("original restore binding changed")
		}
	}
	l := &dzLoader{manifest: m, windows: map[string]dzWindow{}, openings: map[string]dzOpening{}, stops: map[string]flowStop{}, loaded: map[string]bool{}, used: map[string]string{}, requested: map[string]bool{}}
	for _, w := range m.Windows {
		key := dzID(w.Address, w.Since, w.End)
		if old, ok := l.windows[key]; ok && !dzEqual(old, w) {
			t.Fatal("retained window conflict")
		}
		l.windows[key] = w
	}
	var cohort struct{ OpeningEvidence []dzOpening }
	if dzRead(filepath.Join(m.Old, "cohort.private.json"), &cohort) != nil {
		t.Fatal("retained openings unavailable")
	}
	for _, o := range cohort.OpeningEvidence {
		got, e := dzHash(o.SourcePath)
		if e != nil || got != o.SourceSHA256 {
			t.Fatal("retained opening changed")
		}
		key := dzID(o.Address, o.Day, o.Day)
		if old, ok := l.openings[key]; ok && !currentHoldingsMoneyEqual(old.Holdings, o.Holdings) {
			t.Fatal("opening conflict")
		}
		l.openings[key] = o
	}
	// A completed exact new source proof is required before restoring or launching.
	if _, ready, e := dzAdmissionEvents(l, plan.Address, plan.Since, plan.End); e != nil || !ready {
		t.Fatal("new source proof not passed")
	}
	tr, e := dzRestoreCheckpoint(l, plan.Checkpoint)
	if e != nil {
		t.Fatal("literal current child restore rejected")
	}
	if tr.queue.Len() == 0 || tr.queue[0].event.To != plan.Address || !flowDay(tr.queue[0].event.Key.Time).Equal(plan.Since) || !tr.AsOf.Equal(plan.End) || tr.queue[0].event.Category != apiTransfer || tr.queue[0].event.Dir != apiOut || tr.Wallets[plan.Address] != nil {
		t.Fatal("exact failed head scope rejected")
	}
	if e = dzCheckpoint(tr, "adopted", "exact-current-child-adopted-no-replay", false, l.priorApplied); e != nil {
		t.Fatal("adopted checkpoint save failed")
	}
	payloads := map[string]string{}
	for _, name := range []string{"states.private.jsonl.gz", "full-queue.private.jsonl.gz"} {
		old, e := dzPayloadHash(filepath.Join(plan.Checkpoint, name))
		if e != nil {
			t.Fatal("old payload rejected")
		}
		fresh, e := dzPayloadHash(filepath.Join(dzRoot, "adopted", name))
		if e != nil || old != fresh {
			t.Fatal("literal committed state or pending queue changed")
		}
		payloads[name] = old
	}
	for _, name := range []string{"partial-rows.private.json", "original-cohort-statuses.private.json"} {
		var old, fresh any
		if dzRead(filepath.Join(plan.Checkpoint, name), &old) != nil || dzRead(filepath.Join(dzRoot, "adopted", name), &fresh) != nil || !dzEqual(old, fresh) {
			t.Fatal("old fates or status changed")
		}
	}
	var oldState, newState map[string]any
	if dzRead(filepath.Join(plan.Checkpoint, "trace-state.private.json"), &oldState) != nil || dzRead(filepath.Join(dzRoot, "adopted/trace-state.private.json"), &newState) != nil {
		t.Fatal("trace state unavailable")
	}
	delete(oldState, "phase")
	delete(newState, "phase")
	if !dzEqual(oldState, newState) {
		t.Fatal("accounting counters or trace state changed")
	}
	if got, e := dzHash(plan.ParentPhase); e != nil || got != plan.ParentPhaseSHA {
		t.Fatal("parent moved during adoption")
	}
	if e = dzSave(filepath.Join(dzRoot, "adoption-proof.private.json"), map[string]any{"source": dzSource, "parent_pid": plan.ParentPID, "checkpoint": plan.Checkpoint, "prior_applied": l.priorApplied, "pending_events": tr.queue.Len(), "payload_sha256": payloads, "literal_state_queue_fates_counters_status_preserved": true, "committed_occurrences_replayed": 0, "financially_accepted": false, "closing_reconciled": false}); e != nil {
		t.Fatal("adoption proof failed")
	}
	fmt.Printf("phase dynamic_exact_adopted count %d pending %d\n", l.priorApplied, tr.queue.Len())
	if os.Getenv("OPS818_DYNAMIC_PREFLIGHT_ONLY") == "1" {
		return
	}
	if e = os.Mkdir(filepath.Join(dzRoot, "windows"), 0700); e != nil {
		t.Fatal("exclusive windows required")
	}
	if e = os.Mkdir(filepath.Join(dzRoot, "run"), 0700); e != nil {
		t.Fatal("exclusive run required")
	}
	if e = l.supplement(); e != nil {
		t.Fatal("actual cumulative source reply rejected")
	}
	applied, phase, runError := dzRun(tr, filepath.Join(dzRoot, "run"))
	if e = dzCheckpoint(tr, "terminal", phase, runError == nil || l.settled, applied); e != nil {
		t.Fatal("terminal proof failed")
	}
	if e = dzSave(filepath.Join(dzRoot, "runtime-result-safe.json"), map[string]any{"source": dzSource, "phase": phase, "pid": os.Getpid(), "applied_new": applied, "pending_events": tr.queue.Len(), "financially_accepted": false, "closing_reconciled": false}); e != nil {
		t.Fatal("runtime proof failed")
	}
	if runError != nil {
		t.Fatal("dynamic continuation failed, exact private terminal retained")
	}
}
