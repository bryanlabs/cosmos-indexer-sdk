package taxapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/stretchr/testify/require"
)

func correctionWriteManifest(t *testing.T, p *CommissionSourceCorrections) (string, string) {
	t.Helper()
	raw, e := json.Marshal(p)
	require.NoError(t, e)
	path := filepath.Join(t.TempDir(), "corrections.json")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	return path, privateSHA(raw)
}

func correctionPinRewardFree(t *testing.T, digest string) {
	t.Helper()
	correctionRewardFreeManifests[digest] = true
	t.Cleanup(func() { delete(correctionRewardFreeManifests, digest) })
}

func TestCommissionSourceCorrectionRewardFreeManifestPinnedByHash(t *testing.T) {
	six := correctionTestManifest(t)
	sixPath, sixDigest := correctionWriteManifest(t, six)
	free := correctionTestManifest(t)
	free.Rewards = []CommissionNativeRewardCorrection{}
	freePath, freeDigest := correctionWriteManifest(t, free)

	_, e := LoadCommissionSourceCorrections(freePath, freeDigest)
	require.ErrorContains(t, e, "bounded scope mismatch", "an unpinned manifest may not drop the reward corrections")

	correctionPinRewardFree(t, freeDigest)
	got, e := LoadCommissionSourceCorrections(freePath, freeDigest)
	require.NoError(t, e)
	require.Empty(t, got.Rewards)
	require.Empty(t, got.rewardsByWallet)
	require.Len(t, got.Fees, 786)
	require.Len(t, got.feesByWallet["constituent"], 786)
	require.Len(t, got.Windows, 6)
	require.Equal(t, freeDigest, got.digest)

	old, e := LoadCommissionSourceCorrections(sixPath, sixDigest)
	require.NoError(t, e, "the six-reward manifest stays valid for rows already stored under it")
	require.Len(t, old.Rewards, 6)

	correctionPinRewardFree(t, sixDigest)
	_, e = LoadCommissionSourceCorrections(sixPath, sixDigest)
	require.ErrorContains(t, e, "bounded scope mismatch", "a pinned reward-free hash means exactly zero rewards")

	partial := correctionTestManifest(t)
	partial.Rewards = partial.Rewards[:4]
	partialPath, partialDigest := correctionWriteManifest(t, partial)
	_, e = LoadCommissionSourceCorrections(partialPath, partialDigest)
	require.Error(t, e)
	correctionPinRewardFree(t, partialDigest)
	_, e = LoadCommissionSourceCorrections(partialPath, partialDigest)
	require.Error(t, e)

	require.Error(t, free.validate(), "direct validation keeps the six-reward bound")
}

func TestCommissionSourceCorrectionRewardFreeManifestKeepsFeeChecks(t *testing.T) {
	p := correctionTestManifest(t)
	p.Rewards = []CommissionNativeRewardCorrection{}
	p.Fees[0].SourceBANKFeeDebitOverlap = correctionBool(true)
	path, digest := correctionWriteManifest(t, p)
	correctionPinRewardFree(t, digest)
	_, e := LoadCommissionSourceCorrections(path, digest)
	require.ErrorContains(t, e, "native fee source binding invalid")
}

func TestCommissionSourceCorrectionRewardFreeVersionBinding(t *testing.T) {
	p := correctionTestManifest(t)
	p.Rewards = []CommissionNativeRewardCorrection{}
	path, digest := correctionWriteManifest(t, p)
	correctionPinRewardFree(t, digest)
	free, e := LoadCommissionSourceCorrections(path, digest)
	require.NoError(t, e)
	six := correctionTestManifest(t)
	authz := &CommissionPrivateProfile{digest: free.BaseProfileSHA256, From: free.From, AsOf: free.AsOf}
	o := CommissionFlowBuildOptions{Version: "candidate-profile" + authz.digest[:12] + "-sources" + free.digest[:12], Build: "new-source-build", AsOf: free.AsOf, PrivateProfile: authz, SourceCorrections: free}
	require.NoError(t, o.validate())
	stale := o
	stale.Version = "candidate-profile" + authz.digest[:12] + "-sources" + six.digest[:12]
	require.Error(t, stale.validate(), "a version named for the six-reward manifest cannot run the reward-free one")
	prior := o
	prior.SourceCorrections = six
	require.NotEqual(t, o.profileSHA(), prior.profileSHA())
}

// After the re-index stores message 4 itself, a reward recipient's indexed
// reward passes through untouched and nothing is added on top of it.
func TestCommissionSourceCorrectionRewardFreeAddsNothingAtReadTime(t *testing.T) {
	p := correctionTestManifest(t)
	p.Rewards = []CommissionNativeRewardCorrection{}
	path, digest := correctionWriteManifest(t, p)
	correctionPinRewardFree(t, digest)
	free, e := LoadCommissionSourceCorrections(path, digest)
	require.NoError(t, e)
	s, _ := correctionTestSQL(t, free)
	rows := walletRows{Events: []tax.TaxableEvent{{TxHash: "indexed", MessageID: 71, SubIndex: 0, Category: apiReward, Denom: apiUatom, Amount: "2", ToAddr: "reward-a"}}}
	out, e := s.applyCommissionSourceCorrections(apiMainnet, "reward-a", free.From, free.AsOf, rows)
	require.NoError(t, e)
	require.Equal(t, rows, out)
	require.Empty(t, out.CommissionCorrections)
	receipts := s.sourceCorrectionReceipts.receipts()
	require.Len(t, receipts, 1)
	require.Equal(t, "not_applicable_to_this_observed_wallet_period", receipts[0].Status)
	require.Equal(t, digest, receipts[0].ProfileSHA256)
}

// Private retained files stay outside the repository: the six-reward manifest
// and the reward-free manifest derived from it, which must be byte for byte the
// same document apart from the empty reward list.
func TestCommissionSourceCorrectionRewardFreeRetainedManifest(t *testing.T) {
	sixPath := os.Getenv("COMMISSION_CORRECTIONS_SIX_REWARDS")
	freePath := os.Getenv("COMMISSION_CORRECTIONS_REWARD_FREE")
	if sixPath == "" || freePath == "" {
		t.Skip("retained private manifests not configured")
	}
	sixRaw, e := os.ReadFile(sixPath)
	require.NoError(t, e)
	freeRaw, e := os.ReadFile(freePath)
	require.NoError(t, e)
	require.True(t, correctionRewardFreeManifests[privateSHA(freeRaw)], "retained reward-free manifest must be the pinned one")
	six, e := LoadCommissionSourceCorrections(sixPath, privateSHA(sixRaw))
	require.NoError(t, e)
	free, e := LoadCommissionSourceCorrections(freePath, privateSHA(freeRaw))
	require.NoError(t, e)
	require.Len(t, six.Rewards, 6)
	require.Empty(t, free.Rewards)
	var a, b map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(sixRaw, &a))
	require.NoError(t, json.Unmarshal(freeRaw, &b))
	require.JSONEq(t, "[]", string(b["rewards"]))
	delete(a, "rewards")
	delete(b, "rewards")
	require.Equal(t, a, b)
}
