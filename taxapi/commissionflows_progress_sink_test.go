package taxapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func progressTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCommissionProgressSinkBoundsSnapshotsAndPreservesEvidence(t *testing.T) {
	root := progressTestRoot(t)
	if err := os.WriteFile(filepath.Join(root, "native-evidence"), []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := newCommissionProgressSink(root, []string{"validator"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100000; i++ {
		s.update(commissionFlowProgress{Validator: "validator", Sequence: i, Phase: "events", ClosingRemaining: -1})
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(s.dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("bounded snapshot count: %v %d", err, len(files))
	}
	path := filepath.Join(s.dir, files[0].Name())
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 || st.Size() > 4096 {
		t.Fatalf("snapshot privacy/size: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p commissionFlowProgress
	if err := json.Unmarshal(data, &p); err != nil || p.Sequence != 99999 || p.ClosingRemaining != -1 {
		t.Fatalf("latest unknown-discovery state: %v %+v", err, p)
	}
	data, err = os.ReadFile(filepath.Join(root, "native-evidence"))
	if err != nil || string(data) != "unchanged" {
		t.Fatal("existing evidence changed")
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
}

func TestCommissionProgressSinkConcurrentRoots(t *testing.T) {
	roots := []string{"one", "two", "three", "four"}
	s, err := newCommissionProgressSink(progressTestRoot(t), roots, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, v := range roots {
		wg.Add(1)
		go func(v string) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				s.update(commissionFlowProgress{Validator: v, Sequence: i})
			}
		}(v)
	}
	wg.Wait()
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(s.dir)
	if err != nil || len(files) != len(roots) {
		t.Fatalf("root snapshots: %v %d", err, len(files))
	}
}

func TestCommissionProgressSinkRejectsUnexpectedRootAndSymlink(t *testing.T) {
	root := progressTestRoot(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "progress")); err != nil {
		t.Fatal(err)
	}
	if _, err := newCommissionProgressSink(root, []string{"one"}, time.Hour); err == nil {
		t.Fatal("accepted symlinked progress directory")
	}
	s, err := newCommissionProgressSink(progressTestRoot(t), []string{"one"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s.update(commissionFlowProgress{Validator: "unexpected"})
	if err := s.close(); err == nil {
		t.Fatal("unexpected membership silently accepted")
	}
}
