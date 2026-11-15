package taxapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func adoptionFixture(t *testing.T) (CommissionFlowBuildOptions, time.Time, CommissionFlowVersion) {
	t.Helper()
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	asOf := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	o := CommissionFlowBuildOptions{Version: "fresh", Build: "exact82a", AsOf: asOf}
	run, err := json.Marshal(CommissionFlowRun{Chain: apiMainnet, Validator: "synthetic-root", FromDay: from, AsOf: asOf.AddDate(0, 0, -1), Status: flowStatusReconciled})
	require.NoError(t, err)
	return o, from, CommissionFlowVersion{Version: "old-v2", Build: o.Build, Chain: apiMainnet, Validator: "synthetic-root", RunJSON: string(run), RowsJSON: "[]", EvidenceJSON: "{}"}
}
func expectAdoption(m sqlmock.Sqlmock, s CommissionFlowVersion) {
	m.ExpectQuery(`SELECT .*tax_commission_flow_versions`).WithArgs(s.Version, s.Chain, s.Validator).WillReturnRows(sqlmock.NewRows([]string{"version", "build", "chain", "validator", "private_profile_sha256", "run_json", "rows_json", "evidence_json"}).AddRow(s.Version, s.Build, s.Chain, s.Validator, s.PrivateProfileSHA256, s.RunJSON, s.RowsJSON, s.EvidenceJSON))
}
func TestCommissionCaptureAdoptionReadOnly(t *testing.T) {
	o, from, snapshot := adoptionFixture(t)
	s, m := correctionTestSQL(t, nil)
	expectAdoption(m, snapshot)
	ref, err := s.ResolveCommissionCapture(o, []string{snapshot.Version}, apiMainnet, snapshot.Validator, from)
	require.NoError(t, err)
	require.Equal(t, snapshot.Version, ref.Version)
	require.Equal(t, privateSHA([]byte("[]")), ref.PayloadSHA256["rows"])
	require.Equal(t, privateSHA([]byte(snapshot.Validator)), ref.RootSHA256)
}
func TestCommissionCaptureAdoptionRejects(t *testing.T) {
	for _, kind := range []string{"build", "profile", "window", "failed", "bad-json", "wrong-root", "corrupt-chunk-index"} {
		t.Run(kind, func(t *testing.T) {
			o, from, snapshot := adoptionFixture(t)
			s, m := correctionTestSQL(t, nil)
			switch kind {
			case "build":
				snapshot.Build = "other"
			case "profile":
				snapshot.PrivateProfileSHA256 = strings.Repeat("a", 64)
			case "window":
				from = from.AddDate(0, 0, 1)
			case "failed":
				var run CommissionFlowRun
				require.NoError(t, json.Unmarshal([]byte(snapshot.RunJSON), &run))
				run.Status = flowStatusError
				b, e := json.Marshal(run)
				require.NoError(t, e)
				snapshot.RunJSON = string(b)
			case "bad-json":
				snapshot.RowsJSON = "broken"
			case "wrong-root":
				var run CommissionFlowRun
				require.NoError(t, json.Unmarshal([]byte(snapshot.RunJSON), &run))
				run.Validator = "other"
				b, e := json.Marshal(run)
				require.NoError(t, e)
				snapshot.RunJSON = string(b)
			case "corrupt-chunk-index":
				snapshot.EvidenceJSON = "broken"
			}
			expectAdoption(m, snapshot)
			ref, err := s.ResolveCommissionCapture(o, []string{snapshot.Version}, apiMainnet, snapshot.Validator, from)
			if kind == "failed" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Nil(t, ref)
		})
	}
}
func TestCommissionCaptureAdoptionMissingAndAllowlist(t *testing.T) {
	o, from, snapshot := adoptionFixture(t)
	for _, versions := range [][]string{{o.Version}, {""}, {"old", "old"}, make([]string, 9)} {
		s, _ := correctionTestSQL(t, nil)
		_, err := s.ResolveCommissionCapture(o, versions, apiMainnet, snapshot.Validator, from)
		require.Error(t, err)
	}
	s, m := correctionTestSQL(t, nil)
	m.ExpectQuery(`SELECT .*tax_commission_flow_versions`).WillReturnRows(sqlmock.NewRows([]string{"version"}))
	ref, err := s.ResolveCommissionCapture(o, []string{"missing"}, apiMainnet, snapshot.Validator, from)
	require.NoError(t, err)
	require.Nil(t, ref)
}
func TestCommissionCaptureUnionPreservesOverlaps(t *testing.T) {
	ref := CommissionCaptureReference{Version: "v2", Build: "82a", Chain: apiMainnet, ProfileSHA256: strings.Repeat("a", 64), RootSHA256: strings.Repeat("b", 64), PayloadSHA256: map[string]string{"run": strings.Repeat("c", 64), "rows": strings.Repeat("d", 64), "evidence": strings.Repeat("e", 64)}}
	dup := ref
	dup.Version = "fresh"
	got, duplicates, err := CommissionCaptureUnion([]CommissionCaptureReference{ref, dup})
	require.NoError(t, err)
	require.Equal(t, ref, got)
	require.Equal(t, 1, duplicates)
	data, err := MarshalCommissionCaptureReference(ref)
	require.NoError(t, err)
	require.NotContains(t, string(data), "synthetic-root")
	dup.RootSHA256 = strings.Repeat("f", 64)
	_, _, err = CommissionCaptureUnion([]CommissionCaptureReference{ref, dup})
	require.Error(t, err)
}
func TestCommissionAccelerationHeadroom(t *testing.T) {
	now := time.Now()
	good := CommissionAccelerationHeadroom{ObservedAt: now, SampleDuration: 10 * time.Second, PGQuotaCores: 10, PGUsedCores: 8, PGResidentBytes: 5 << 30, PGMemoryCurrentBytes: 24 << 30, PGInactiveFileBytes: 19 << 30, PGMemoryLimitBytes: 24 << 30, JobUsedBytes: 1 << 30, JobMemoryLimitBytes: 8 << 30, NodeAvailableBytes: 50 << 30, AdmissionGranted: true, PublicHealthy: true, ReplayHealthy: true}
	require.True(t, good.Allow(now, 1, 1<<30))
	for _, kind := range []string{"saturated", "stale", "future", "pg-memory", "job-memory", "public", "replay", "missing-quota", "zero-interval", "oom", "memory-pressure", "memory-accounting"} {
		t.Run(kind, func(t *testing.T) {
			h := good
			switch kind {
			case "saturated":
				h.PGUsedCores = 10
			case "stale":
				h.ObservedAt = now.Add(-6 * time.Second)
			case "future":
				h.ObservedAt = now.Add(time.Second)
			case "pg-memory":
				h.PGResidentBytes = 24 << 30
			case "job-memory":
				h.JobUsedBytes = 8 << 30
			case "public":
				h.PublicHealthy = false
			case "replay":
				h.ReplayHealthy = false
			case "missing-quota":
				h.PGQuotaCores = 0
			case "zero-interval":
				h.SampleDuration = 0
			case "oom":
				h.PGOOMKills = 1
			case "memory-pressure":
				h.PGMemoryPressureFullAvg10 = 11
			case "memory-accounting":
				h.PGInactiveFileBytes = h.PGMemoryCurrentBytes + 1
			}
			require.False(t, h.Allow(now, 1, 1<<30))
		})
	}
}
