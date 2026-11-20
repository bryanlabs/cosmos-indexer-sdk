package taxapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func accelerationGoodSample() CommissionAccelerationHeadroom {
	return CommissionAccelerationHeadroom{ObservedAt: time.Now(), SampleDuration: time.Second, PGQuotaCores: 10, PGUsedCores: 8, PGResidentBytes: 5 << 30, PGMemoryCurrentBytes: 24 << 30, PGInactiveFileBytes: 19 << 30, PGMemoryLimitBytes: 24 << 30, JobUsedBytes: 1 << 30, JobMemoryLimitBytes: 8 << 30, NodeAvailableBytes: 50 << 30, AdmissionGranted: true, PublicHealthy: true, ReplayHealthy: true}
}
func TestCommissionAccelerationGuardCancelsNewContext(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reads atomic.Int64
	sample := func() (CommissionAccelerationHeadroom, error) {
		h := accelerationGoodSample()
		if reads.Add(1) > 1 {
			h.PGUsedCores = 10
		}
		return h, nil
	}
	ctx, stop, err := startCommissionAccelerationGuard(parent, sample, time.Millisecond)
	require.NoError(t, err)
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("guard failed to yield")
	}
	require.NoError(t, parent.Err(), "old parent must not be cancelled")
	stop()
}
func TestCommissionAccelerationGuardFailsBeforeWork(t *testing.T) {
	_, _, err := startCommissionAccelerationGuard(context.Background(), func() (CommissionAccelerationHeadroom, error) {
		h := accelerationGoodSample()
		h.PGUsedCores = 10
		return h, nil
	}, time.Second)
	require.Error(t, err)
}
func TestCommissionAccelerationSampleFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resource.json")
	b, err := json.Marshal(accelerationGoodSample())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, b, 0600))
	_, err = readAccelerationSample(path)
	require.NoError(t, err)
	link := filepath.Join(filepath.Dir(path), "link.json")
	require.NoError(t, os.Symlink(path, link))
	_, err = readAccelerationSample(link)
	require.Error(t, err)
	require.NoError(t, os.WriteFile(path, append(b, []byte(" {}")...), 0600))
	_, err = readAccelerationSample(path)
	require.Error(t, err)
	require.NoError(t, os.Chmod(path, 0644))
	_, err = readAccelerationSample(path)
	require.Error(t, err)
}
func TestCommissionCaptureAdoptionSidecarExclusive(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0700))
	ref := CommissionCaptureReference{Version: "v2", Build: "82a", Chain: apiMainnet, ProfileSHA256: strings.Repeat("a", 64), RootSHA256: strings.Repeat("b", 64), PayloadSHA256: map[string]string{"run": strings.Repeat("c", 64), "rows": strings.Repeat("d", 64), "evidence": strings.Repeat("e", 64)}}
	require.NoError(t, retainCommissionAdoption(root, ref))
	require.NoError(t, retainCommissionAdoption(root, ref))
	encoded, err := MarshalCommissionCaptureReference(ref)
	require.NoError(t, err)
	path := filepath.Join(root, "adoptions", ref.RootSHA256+"-"+privateSHA(encoded)+".json")
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	ref.Version = "different-evidence"
	require.NoError(t, retainCommissionAdoption(root, ref))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, after)
}
