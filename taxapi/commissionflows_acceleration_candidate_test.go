package taxapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func correctedCandidateFixture(t *testing.T) (*Server, sqlmock.Sqlmock, CommissionFlowBuildOptions, time.Time, []string, string, string) {
	t.Helper()
	c := correctionTestManifest(t)
	p := &CommissionPrivateProfile{digest: c.BaseProfileSHA256, From: c.From, AsOf: c.AsOf}
	o := CommissionFlowBuildOptions{Version: "candidate-profileaaaaaaaaaaaa-sourcescccccccccccc", Build: "exact-native", AsOf: c.AsOf, PrivateProfile: p, SourceCorrections: c}
	s, m := correctionTestSQL(t, nil)
	pool, err := s.db.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	canonical := make([]string, 324)
	for i := range canonical {
		canonical[i] = fmt.Sprintf("synthetic-root-%03d", i)
	}
	out := t.TempDir()
	require.NoError(t, os.Chmod(out, 0700))
	sample := filepath.Join(out, "resource.json")
	b, err := json.Marshal(accelerationGoodSample())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(sample, b, 0600))
	return s, m, o, c.From, canonical, out, sample
}
func candidateSnapshot(t *testing.T, o CommissionFlowBuildOptions, version, root, status string, from time.Time) CommissionFlowVersion {
	t.Helper()
	b, err := json.Marshal(CommissionFlowRun{Chain: apiMainnet, Validator: root, FromDay: from, AsOf: o.AsOf.AddDate(0, 0, -1), Status: status})
	require.NoError(t, err)
	return CommissionFlowVersion{Version: version, Build: o.Build, Chain: apiMainnet, Validator: root, PrivateProfileSHA256: o.profileSHA(), RunJSON: string(b), RowsJSON: "[]", EvidenceJSON: "{}"}
}
func expectCandidateMissing(m sqlmock.Sqlmock, version, root string) {
	m.ExpectQuery(`SELECT .*tax_commission_flow_versions`).WithArgs(version, apiMainnet, root).WillReturnRows(sqlmock.NewRows([]string{"version"}))
}
func TestCommissionAccelerationTargetErrorStillAdoptsSource(t *testing.T) {
	s, m, o, from, canonical, out, sample := correctedCandidateFixture(t)
	root := canonical[0]
	source := "source-profileaaaaaaaaaaaa-sourcescccccccccccc"
	expectAdoption(m, candidateSnapshot(t, o, o.Version, root, flowStatusError, from))
	expectAdoption(m, candidateSnapshot(t, o, source, root, flowStatusReconciled, from))
	ran := false
	require.NoError(t, s.RunCommissionCaptureCandidate(context.Background(), o, []string{source}, canonical, []string{root}, apiMainnet, from, out, sample, func(context.Context, string) error { ran = true; return nil }))
	require.False(t, ran)
	files, err := filepath.Glob(filepath.Join(out, "adoptions", "*.json"))
	require.NoError(t, err)
	require.Len(t, files, 1)
}
func TestCommissionAccelerationCorruptSourceDoesNotBlockFollowingRoots(t *testing.T) {
	s, m, o, from, canonical, out, sample := correctedCandidateFixture(t)
	first, second := canonical[0], canonical[1]
	source := "source-profileaaaaaaaaaaaa-sourcescccccccccccc"
	expectCandidateMissing(m, o.Version, first)
	bad := candidateSnapshot(t, o, source, first, flowStatusReconciled, from)
	bad.EvidenceJSON = "corrupt"
	expectAdoption(m, bad)
	expectCandidateMissing(m, o.Version, second)
	expectCandidateMissing(m, source, second)
	var ran []string
	err := s.RunCommissionCaptureCandidate(context.Background(), o, []string{source}, canonical, []string{first, second}, apiMainnet, from, out, sample, func(_ context.Context, r string) error { ran = append(ran, r); return nil })
	require.ErrorContains(t, err, "1 candidate roots incomplete")
	require.Equal(t, []string{second}, ran)
}
func TestCommissionAccelerationPinnedChildPolicyAndCancelBeforeKill(t *testing.T) {
	t.Setenv("PGAPPNAME", "candidate-owned-test")
	t.Setenv("PGOPTIONS", "-c max_parallel_workers_per_gather=8")
	out := t.TempDir()
	require.NoError(t, os.Chmod(out, 0700))
	policy := filepath.Join(out, "policy")
	app := filepath.Join(out, "application")
	ready := filepath.Join(out, "started")
	t.Setenv("CANDIDATE_TEST_POLICY", policy)
	t.Setenv("CANDIDATE_TEST_APP", app)
	t.Setenv("CANDIDATE_TEST_READY", ready)
	script := []byte("#!/bin/sh\nprintf '%s' \"$PGOPTIONS\" >\"$CANDIDATE_TEST_POLICY\"\nprintf '%s' \"$PGAPPNAME\" >\"$CANDIDATE_TEST_APP\"\nprintf started >\"$CANDIDATE_TEST_READY\"\nexec sleep 60\n")
	binary := filepath.Join(out, "native-fixture")
	require.NoError(t, os.WriteFile(binary, script, 0700))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var called atomic.Bool
	go func() {
		done <- RunPinnedCommissionRoot(ctx, binary, privateSHA(script), nil, "synthetic-root", out, func(application string) error {
			b, err := os.ReadFile(app)
			if err != nil {
				return err
			}
			if application != string(b) || !strings.HasPrefix(application, "candidate-owned-test-a") {
				return fmt.Errorf("wrong cancellation application")
			}
			matches, _ := filepath.Glob(filepath.Join(out, "native-attempt-*", "native.identity"))
			if len(matches) != 1 {
				return fmt.Errorf("missing native identity")
			}
			b, err = os.ReadFile(matches[0])
			if err != nil {
				return err
			}
			pidText := strings.TrimPrefix(strings.SplitN(string(b), "\n", 2)[0], "pid=")
			pid, err := strconv.Atoi(pidText)
			if err != nil {
				return err
			}
			if err := syscall.Kill(pid, 0); err != nil {
				return fmt.Errorf("native killed before backend cancellation")
			}
			called.Store(true)
			return nil
		})
	}()
	require.Eventually(t, func() bool { _, e := os.Stat(ready); return e == nil }, time.Second, 5*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("owned child not cancelled")
	}
	require.True(t, called.Load())
	b, err := os.ReadFile(policy)
	require.NoError(t, err)
	require.Contains(t, string(b), "max_parallel_workers_per_gather=0")
	require.Contains(t, string(b), "statement_timeout=60000")
	require.Contains(t, string(b), "client_connection_check_interval=1000")
	exits, err := filepath.Glob(filepath.Join(out, "native-attempt-*", "native.exit"))
	require.NoError(t, err)
	require.Len(t, exits, 1)
}
