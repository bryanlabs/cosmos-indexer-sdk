package taxapi

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommissionComputeOnlyFreshRootWithoutAdoptionQuery(t *testing.T) {
	s, m, o, from, canonical, out, sample := correctedCandidateFixture(t)
	root := canonical[0]
	expectCandidateMissing(m, o.Version, root)
	ran := 0
	require.NoError(t, s.RunCommissionCaptureCandidate(context.Background(), o, nil, canonical, []string{root}, apiMainnet, from, out, sample, func(context.Context, string) error { ran++; return nil }, CommissionCaptureCandidateOptions{ComputeOnly: true}))
	require.Equal(t, 1, ran)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestCommissionComputeOnlyRequiresExplicitEmptySources(t *testing.T) {
	for _, test := range []struct {
		compute bool
		sources []string
	}{{false, nil}, {true, []string{"made-up-source"}}} {
		s, m, o, from, canonical, out, sample := correctedCandidateFixture(t)
		ran := false
		require.Error(t, s.RunCommissionCaptureCandidate(context.Background(), o, test.sources, canonical, canonical[:1], apiMainnet, from, out, sample, func(context.Context, string) error { ran = true; return nil }, CommissionCaptureCandidateOptions{ComputeOnly: test.compute}))
		require.False(t, ran)
		require.NoError(t, m.ExpectationsWereMet())
	}
}
func TestCommissionComputeOnlyRejectsIncompatibleOwnGeneration(t *testing.T) {
	s, m, o, from, canonical, out, sample := correctedCandidateFixture(t)
	root := canonical[0]
	own := candidateSnapshot(t, o, o.Version, root, flowStatusReconciled, from)
	own.Build = "different-build"
	expectAdoption(m, own)
	ran := false
	require.Error(t, s.RunCommissionCaptureCandidate(context.Background(), o, nil, canonical, []string{root}, apiMainnet, from, out, sample, func(context.Context, string) error { ran = true; return nil }, CommissionCaptureCandidateOptions{ComputeOnly: true}))
	require.False(t, ran)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestCommissionComputeOnlyPreservesExactOwnGeneration(t *testing.T) {
	s, m, o, from, canonical, out, sample := correctedCandidateFixture(t)
	root := canonical[0]
	expectAdoption(m, candidateSnapshot(t, o, o.Version, root, flowStatusReconciled, from))
	ran := false
	require.NoError(t, s.RunCommissionCaptureCandidate(context.Background(), o, nil, canonical, []string{root}, apiMainnet, from, out, sample, func(context.Context, string) error { ran = true; return nil }, CommissionCaptureCandidateOptions{ComputeOnly: true}))
	require.False(t, ran)
	require.NoError(t, m.ExpectationsWereMet())
}
