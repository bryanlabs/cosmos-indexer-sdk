package taxapi

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	querytypes "github.com/cosmos/cosmos-sdk/types/query"
	"github.com/stretchr/testify/require"
)

type incompleteFlowLedger struct {
	*fakeFlowLedger
	incomplete string
	failure    error
}

func (l *incompleteFlowLedger) opening(addr string, day time.Time) (flowHoldings, bool, error) {
	if addr == l.incomplete {
		return flowHoldings{}, false, l.failure
	}
	return l.fakeFlowLedger.opening(addr, day)
}

func TestFlowStrictDiscoveryFailsClosedAtLaterHop(t *testing.T) {
	for _, archiveError := range []error{errors.New("archive state unavailable"), nil} {
		base := newFakeFlowLedger()
		base.commission(flowAt(1, 0), flowTestReceiver, 100)
		base.send(flowAt(2, 0), flowTestReceiver, flowRowsHolder, 100)
		loader := &incompleteFlowLedger{fakeFlowLedger: base, incomplete: flowRowsHolder, failure: archiveError}
		tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 3, 0), loader)
		tr.strict = true
		require.Error(t, tr.run([]string{flowTestReceiver}))
		require.True(t, tr.attempted[flowRowsHolder], "failed archive address remains in evidence scope")
		require.NotContains(t, tr.Wallets, flowRowsHolder)
		require.Empty(t, tr.Unresolved, "strict failures must not become accepted unresolved fates")
	}
	base := newFakeFlowLedger()
	base.commission(flowAt(1, 0), flowTestReceiver, 100)
	base.send(flowAt(2, 0), flowTestReceiver, flowRowsHolder, 100)
	legacy := &incompleteFlowLedger{fakeFlowLedger: base, incomplete: flowRowsHolder, failure: errors.New("archive state unavailable")}
	tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 3, 0), legacy)
	require.NoError(t, tr.run([]string{flowTestReceiver}))
	require.Equal(t, []string{flowRowsHolder}, tr.Unresolved, "legacy behavior remains unchanged")
}

func TestFlowStrictNoReceiverFailsClosed(t *testing.T) {
	base := newFakeFlowLedger()
	base.commission(flowAt(1, 0), flowTestReceiver, 100)
	base.send(flowAt(2, 0), flowTestReceiver, "", 100)
	tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 3, 0), base)
	tr.strict = true
	require.ErrorContains(t, tr.run([]string{flowTestReceiver}), "unresolved")
}

func TestFlowArchivePaginationMustBeExplicit(t *testing.T) {
	_, err := flowArchivePageKey(nil)
	require.Error(t, err)
	key, err := flowArchivePageKey(&querytypes.PageResponse{})
	require.NoError(t, err)
	require.Empty(t, key)
	key, err = flowArchivePageKey(&querytypes.PageResponse{NextKey: []byte("next")})
	require.NoError(t, err)
	require.Equal(t, []byte("next"), key)
}

func TestCommissionFlowVersionReuseValidatesProvenanceAndStatus(t *testing.T) {
	from := flowTestStart
	asOf := from.AddDate(0, 3, 0)
	o := CommissionFlowBuildOptions{Version: "v1", Build: "sha256:one", AsOf: asOf}
	run := CommissionFlowRun{Chain: apiMainnet, Validator: flowTestValidator, FromDay: from, AsOf: asOf.AddDate(0, 0, -1), Status: flowStatusReconciled}
	encoded, err := json.Marshal(run)
	require.NoError(t, err)
	snapshot := CommissionFlowVersion{Version: o.Version, Build: o.Build, Chain: apiMainnet, Validator: flowTestValidator, RunJSON: string(encoded)}
	require.NoError(t, reusableCommissionFlowVersion(snapshot, o, apiMainnet, flowTestValidator, from))
	for _, mutate := range []func(*CommissionFlowVersion){
		func(s *CommissionFlowVersion) { s.Build = "other" },
		func(s *CommissionFlowVersion) { s.Version = "other" },
		func(s *CommissionFlowVersion) { s.Validator = "other" },
		func(s *CommissionFlowVersion) { s.Chain = apiTestnet },
		func(s *CommissionFlowVersion) { s.RunJSON = "{}" },
		func(s *CommissionFlowVersion) { s.RunJSON = "invalid" },
	} {
		changed := snapshot
		mutate(&changed)
		require.Error(t, reusableCommissionFlowVersion(changed, o, apiMainnet, flowTestValidator, from))
	}
	require.Error(t, reusableCommissionFlowVersion(snapshot, o, apiMainnet, flowTestValidator, from.AddDate(0, 0, 1)))
	for _, status := range []string{flowStatusOK, flowStatusDrift, flowStatusError, "unknown"} {
		run.Status = status
		encoded, err = json.Marshal(run)
		require.NoError(t, err)
		snapshot.RunJSON = string(encoded)
		err = reusableCommissionFlowVersion(snapshot, o, apiMainnet, flowTestValidator, from)
		if status == flowStatusOK || status == flowStatusDrift {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
}
