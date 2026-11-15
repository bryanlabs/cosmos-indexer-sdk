//go:build ops818new06child && ops818new06input && ops818opening06storedv3 && ops818harness && ops818cohorts0607

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
)

func c6PayloadHash(p string) (string, error) {
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
func TestOPS818New06ExactCheckpointChild(t *testing.T) {
	syscall.Umask(0077)
	xcRoot = os.Getenv("OPS818_NEW06_CHILD_ROOT")
	xcRequestRoot = os.Getenv("OPS818_NEW06_PARENT_ROOT")
	xcCohort = "validator-06"
	if xcRoot == "" || xcRequestRoot == "" || xcRoot == xcRequestRoot {
		t.Fatal("exclusive child and unchanged parent required")
	}
	var m xcManifest
	if e := xcRead(filepath.Join(xcRoot, "restore-manifest.private.json"), &m); e != nil || m.Source != xcSource {
		t.Fatal("manifest invalid")
	}
	for path, binding := range m.Files {
		h, e := xcHash(path)
		if e != nil || h != binding.SHA {
			t.Fatal("immutable restore artifact mismatch")
		}
	}
	l := &xcLoader{manifest: m, windows: map[string]xcWindow{}, openings: map[string]xcOpening{}, loaded: map[string]bool{}, used: map[string]string{}, requested: map[string]bool{}}
	for _, w := range m.Windows {
		l.windows[xcID(w.Address, w.Since, w.End)] = w
	}
	var cohort struct{ OpeningEvidence []xcOpening }
	if e := xcRead(filepath.Join(m.Old, "cohort.private.json"), &cohort); e != nil {
		t.Fatal("opening evidence invalid")
	}
	verified := map[string]string{}
	for _, o := range cohort.OpeningEvidence {
		h, ok := verified[o.SourcePath]
		if !ok {
			var e error
			h, e = xcHash(o.SourcePath)
			if e != nil {
				t.Fatal("opening source missing")
			}
			verified[o.SourcePath] = h
		}
		if h != o.SourceSHA256 {
			t.Fatal("opening source mismatch")
		}
		key := xcID(o.Address, o.Day, o.Day)
		if old, ok := l.openings[key]; ok && !currentHoldingsMoneyEqual(old.Holdings, o.Holdings) {
			t.Fatal("opening conflict")
		}
		l.openings[key] = o
	}

	parentCheckpoint := filepath.Join(xcRequestRoot, "blocked-000002")
	tr, e := xcRestoreCheckpoint(l, parentCheckpoint)
	if e != nil {
		t.Fatal("literal parent checkpoint restore rejected")
	}
	var scope n6Scope
	if xcRead(filepath.Join(os.Getenv("OPS818_NEW06_INPUT_ROOT"), "scope.private.json"), &scope) != nil {
		t.Fatal("new exact source scope unavailable")
	}
	if tr.queue.Len() == 0 || tr.queue[0].event.To != scope.Address || !flowDay(tr.queue[0].event.Key.Time).Equal(scope.Since) || !tr.AsOf.Equal(scope.End) || tr.queue[0].event.Category != apiTransfer || tr.queue[0].event.Dir != apiOut || tr.Wallets[scope.Address] != nil {
		t.Fatal("literal failed head/new receiver request mismatch")
	}
	if e = xcCheckpoint(tr, "adopted", "exact-parent-checkpoint-adopted-no-apply", false, l.priorApplied); e != nil {
		t.Fatal("adopted exact checkpoint save failed")
	}
	for _, name := range []string{"states.private.jsonl.gz", "full-queue.private.jsonl.gz"} {
		old, e := c6PayloadHash(filepath.Join(parentCheckpoint, name))
		if e != nil {
			t.Fatal("parent literal checkpoint hash failed")
		}
		fresh, e := c6PayloadHash(filepath.Join(xcRoot, "adopted", name))
		if e != nil || old != fresh {
			t.Fatal("adopted literal queue/state payload changed")
		}
	}
	if e = xcSave(filepath.Join(xcRoot, "adoption-proof.private.json"), map[string]any{"source": xcSource, "parent_pid": 600481, "parent_checkpoint": parentCheckpoint, "parent_checkpoint_unchanged": true, "literal_state_and_queue_payloads_equal": true, "committed_occurrences_replayed": 0, "failed_original_enqueue_seq": tr.queue[0].seq, "adopted_pending_events": tr.queue.Len(), "adopted_seq": tr.seq, "adopted_wallets": len(tr.order), "prior_applied": l.priorApplied, "fresh_input_root": os.Getenv("OPS818_NEW06_INPUT_ROOT"), "financially_accepted": false, "closing_reconciled": false}); e != nil {
		t.Fatal("exact adoption proof save failed")
	}
	fmt.Printf("phase new06_exact_parent_adopted count %d pending %d\n", l.priorApplied, tr.queue.Len())
	if os.Getenv("OPS818_NEW06_PREFLIGHT_ONLY") == "1" {
		return
	}
	if e = os.Mkdir(filepath.Join(xcRoot, "windows"), 0700); e != nil {
		t.Fatal("exclusive windows required")
	}
	if e = os.Mkdir(filepath.Join(xcRoot, "run"), 0700); e != nil {
		t.Fatal("exclusive child run required")
	}
	if e = l.supplement(); e != nil {
		t.Fatal("actual retained parent supplement rejected")
	}
	// Private root points to a symlink-only overlay. The original loader/runner
	// consumes the bound new window without changing any parent or collector.
	applied, phase, runError := xcRun(tr, filepath.Join(xcRoot, "run"))
	if e = xcCheckpoint(tr, "terminal", phase, runError == nil || l.settled, applied); e != nil {
		t.Fatal("child terminal save failed")
	}
	if e = xcSave(filepath.Join(xcRoot, "runtime-result-safe.json"), map[string]any{"source": xcSource, "phase": phase, "pid": os.Getpid(), "applied_new": applied, "pending_events": tr.queue.Len(), "wallets": len(tr.order), "financially_accepted": false, "closing_reconciled": false}); e != nil {
		t.Fatal("child runtime proof save failed")
	}
	fmt.Printf("phase %s count %d pending %d\n", phase, applied, tr.queue.Len())
}
