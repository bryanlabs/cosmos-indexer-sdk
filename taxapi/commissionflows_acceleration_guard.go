package taxapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// readAccelerationSample consumes only a bounded private resource receipt. The
// observer must measure PG cgroup quota/usage, resident memory and public/replay
// health. Missing or stale observations never grant permission to run.
func readAccelerationSample(path string) (CommissionAccelerationHeadroom, error) {
	var h CommissionAccelerationHeadroom
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16384 {
		return h, fmt.Errorf("invalid acceleration resource receipt")
	}
	f, err := os.Open(path)
	if err != nil {
		return h, fmt.Errorf("cannot open acceleration resource receipt")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return h, fmt.Errorf("acceleration resource receipt changed during open")
	}
	dec := json.NewDecoder(io.LimitReader(f, 16385))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&h); err != nil {
		return h, fmt.Errorf("invalid acceleration resource sample")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return h, fmt.Errorf("trailing acceleration resource sample")
	}
	return h, nil
}

// startCommissionAccelerationGuard checks before any query, then cancels the
// new collector context within one sampling interval on unsafe headroom. All
// candidate GORM queries must use this context. Old actors are never signalled.
func startCommissionAccelerationGuard(parent context.Context, sample func() (CommissionAccelerationHeadroom, error), interval time.Duration) (context.Context, func(), error) {
	if interval <= 0 || interval > time.Second {
		return nil, nil, fmt.Errorf("invalid acceleration guard interval")
	}
	allowed := func() bool { h, err := sample(); return err == nil && h.Allow(time.Now(), 0.1, 1<<30) }
	if !allowed() {
		return nil, nil, fmt.Errorf("acceleration yielded: no verified PG/job/public/replay headroom")
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !allowed() {
					cancel()
					return
				}
			}
		}
	}()
	var once sync.Once
	return ctx, func() { once.Do(func() { cancel(); <-done }) }, nil
}

// retainCommissionAdoption publishes only a complete exclusive sidecar in the
// new candidate directory. Existing source payloads and SQL rows are untouched.
func retainCommissionAdoption(root string, ref CommissionCaptureReference) error {
	data, err := MarshalCommissionCaptureReference(ref)
	if err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil || !filepath.IsAbs(root) || !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("invalid adoption output directory")
	}
	dir := filepath.Join(root, "adoptions")
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err = os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("invalid adoption sidecar directory")
	}
	final := filepath.Join(dir, ref.RootSHA256+"-"+privateSHA(data)+".json")
	if existing, err := os.ReadFile(final); err == nil {
		if bytes.Equal(existing, data) {
			return nil
		}
		return fmt.Errorf("immutable adoption reference mismatch")
	}
	f, err := os.CreateTemp(dir, "attempt-")
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return fmt.Errorf("adoption attempt incomplete and retained")
	}
	if err = os.Link(f.Name(), final); err != nil {
		return fmt.Errorf("adoption exclusive publish failed; attempt retained")
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
