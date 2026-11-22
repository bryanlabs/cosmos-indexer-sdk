package taxapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// One mutable private snapshot per selected root keeps progress separate from
// immutable capture evidence and the bounded native failure log.
type commissionProgressSink struct {
	mu      sync.Mutex
	dir     string
	allowed map[string]bool
	latest  map[string]commissionFlowProgress
	dirty   map[string]bool
	err     error
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

func newCommissionProgressSink(root string, validators []string, interval time.Duration) (*commissionProgressSink, error) {
	info, err := os.Lstat(root)
	if err != nil || !filepath.IsAbs(root) || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, fmt.Errorf("private progress requires an existing absolute mode-0700 capture directory")
	}
	dir := filepath.Join(root, "progress")
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() || st.Mode().Perm() != 0700 {
		return nil, fmt.Errorf("private progress directory must be a regular 0700 directory")
	}
	s := &commissionProgressSink{dir: dir, allowed: map[string]bool{}, latest: map[string]commissionFlowProgress{}, dirty: map[string]bool{}, stop: make(chan struct{}), done: make(chan struct{})}
	for _, v := range validators {
		s.allowed[v] = true
	}
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
			case <-s.stop:
				s.flush()
				return
			}
			if !s.flush() {
				return
			}
		}
	}()
	return s, nil
}

func (s *commissionProgressSink) update(p commissionFlowProgress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.allowed[p.Validator] {
		s.err = fmt.Errorf("progress root is outside selected membership")
		return
	}
	s.latest[p.Validator] = p
	s.dirty[p.Validator] = true
}

func (s *commissionProgressSink) flush() bool {
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return false
	}
	pending := make(map[string]commissionFlowProgress, len(s.dirty))
	for v := range s.dirty {
		pending[v] = s.latest[v]
	}
	s.dirty = map[string]bool{}
	s.mu.Unlock()
	for v, p := range pending {
		if err := s.write(v, p); err != nil {
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
			return false
		}
	}
	return true
}

func (s *commissionProgressSink) write(v string, p commissionFlowProgress) error {
	hash := sha256.Sum256([]byte(v))
	f, err := os.CreateTemp(s.dir, ".pending-")
	if err != nil {
		return err
	}
	defer f.Close()
	if err = json.NewEncoder(f).Encode(p); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(s.dir, hex.EncodeToString(hash[:])+".json"))
}

func (s *commissionProgressSink) close() error {
	s.once.Do(func() { close(s.stop) })
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
