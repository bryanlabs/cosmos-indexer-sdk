package taxapi

// Retained fixtures stay outside the repository. This test performs no source
// queries or captures. SQL mocks exercise the read-time adapter contract; they
// do not establish that a real collector has bound its indexed message IDs.
import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func correctionRetainedExpected(t *testing.T, raw json.RawMessage) map[string]string {
	t.Helper()
	var values map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &values))
	out := map[string]string{}
	for denom, b := range values {
		var amount string
		if len(b) > 0 && b[0] == '"' {
			require.NoError(t, json.Unmarshal(b, &amount))
		} else {
			amount = string(b)
		}
		_, e := correctionInteger(amount)
		require.NoError(t, e)
		out[denom] = amount
	}
	return out
}
func correctionRetainedVector(t *testing.T, old map[string]string, denom, amount string) {
	t.Helper()
	n, e := correctionInteger(amount)
	require.NoError(t, e)
	prior := new(big.Int)
	if old[denom] != "" {
		_, ok := prior.SetString(old[denom], 10)
		require.True(t, ok, "retained integer parse")
	}
	old[denom] = prior.Add(prior, n).String()
}
func TestCommissionSourceCorrectionsRetainedSixWindowConsumption(t *testing.T) {
	root := os.Getenv("COMMISSION_CORRECTIONS_TEST_ROOT")
	if root == "" {
		t.Skip("retained private fixtures not configured")
	}
	manifestPath := filepath.Join(root, "commission-source-corrections.private.json")
	raw, e := os.ReadFile(manifestPath)
	require.NoError(t, e)
	p, e := LoadCommissionSourceCorrections(manifestPath, privateSHA(raw))
	require.NoError(t, e)
	var index struct {
		Scopes []struct {
			Actor          string    `json:"actor"`
			Since          time.Time `json:"since"`
			End            time.Time `json:"asof_exclusive"`
			Window         string    `json:"window"`
			InputSHA       string    `json:"source_input_sha256"`
			InputReference struct {
				Path string `json:"path"`
				SHA  string `json:"sha256"`
			} `json:"offline_input_reference"`
		}
	}
	raw, e = os.ReadFile(filepath.Join(root, "evidence-index.private.json"))
	require.NoError(t, e)
	require.NoError(t, json.Unmarshal(raw, &index))
	require.Len(t, index.Scopes, 6)
	var expected []struct {
		Window string          `json:"window"`
		Fee    json.RawMessage `json:"actual_native_fee_addition"`
		Reward json.RawMessage `json:"actual_native_reward_addition"`
	}
	acceptedPath := os.Getenv("COMMISSION_CORRECTIONS_ACCEPTED_SIX")
	require.NotEmpty(t, acceptedPath, "exact prior companion file required")
	raw, e = os.ReadFile(acceptedPath)
	require.NoError(t, e)
	require.True(t, privateSHA(raw) == correctionSixAuthority, "accepted six companion digest mismatch")
	require.NoError(t, json.Unmarshal(raw, &expected))
	require.Len(t, expected, 6)
	accepted := map[string]int{}
	for i, c := range expected {
		accepted[c.Window] = i
	}
	feeMemberships, rewardCredits := 0, 0
	uniqueFees := map[string]bool{}
	for _, scope := range index.Scopes {
		want, ok := accepted[scope.Window]
		require.True(t, ok, "retained fixture window not in accepted six")
		raw, e = os.ReadFile(scope.InputReference.Path)
		require.NoError(t, e)
		require.True(t, privateSHA(raw) == scope.InputReference.SHA && scope.InputReference.SHA == scope.InputSHA, "original source input digest mismatch")
		var input reportInput
		require.NoError(t, json.Unmarshal(raw, &input))
		require.Len(t, input.Wallets, 1)
		rows := input.Wallets[0]
		unrankedBlocks := correctionUnrankedNativeBlocks(rows.Events, scope.Actor)
		before, e := json.Marshal(rows)
		require.NoError(t, e)
		s, m := correctionTestSQL(t, p)
		for _, i := range p.feesByWallet[scope.Actor] {
			c := p.Fees[i]
			if c.Timestamp.Before(scope.Since) || !c.Timestamp.Before(scope.End) {
				continue
			}
			require.False(t, unrankedBlocks[c.Height], "retained fee intersects unproved native lifecycle phase")
			expectCorrectionFee(m, c, "", 0)
			uniqueFees[c.TxHash] = true
			feeMemberships++
		}
		for _, i := range p.rewardsByWallet[scope.Actor] {
			c := p.Rewards[i]
			if c.Timestamp.Before(scope.Since) || !c.Timestamp.Before(scope.End) {
				continue
			}
			require.False(t, unrankedBlocks[c.Height], "retained reward intersects unproved native lifecycle phase")
			expectCorrectionTx(m, c.CommissionCorrectionIdentity)
			m.ExpectQuery("SELECT m.id,mt.message_type").WillReturnRows(sqlmock.NewRows([]string{"id", "message_type"}).AddRow(9000000000, c.MessageType))
			rewardCredits++
		}
		out, e := s.applyCommissionSourceCorrections(apiMainnet, scope.Actor, scope.Since, scope.End, rows)
		require.NoError(t, e, "retained native/source application")
		after, e := json.Marshal(rows)
		require.NoError(t, e)
		require.True(t, string(before) == string(after), "original rows mutated")
		originalEvents, e := json.Marshal(rows.Events)
		require.NoError(t, e)
		newEvents, e := json.Marshal(out.Events)
		require.NoError(t, e)
		require.True(t, string(originalEvents) == string(newEvents), "original source events mutated")
		actualFees := map[string]string{}
		originalFees := map[string]string{}
		for _, fee := range rows.Fees {
			correctionRetainedVector(t, originalFees, fee.Denom, fee.Amount)
		}
		for _, fee := range out.Fees {
			correctionRetainedVector(t, actualFees, fee.Denom, fee.Amount)
		}
		for denom, amount := range originalFees {
			prior := new(big.Int)
			if actualFees[denom] != "" {
				prior.SetString(actualFees[denom], 10)
			}
			n, _ := new(big.Int).SetString(amount, 10)
			actualFees[denom] = prior.Sub(prior, n).String()
		}
		require.True(t, correctionCoinEqual(actualFees, correctionRetainedExpected(t, expected[want].Fee)), "fee delta differs from accepted bounded companion")
		actualRewards := map[string]string{}
		for _, credit := range out.CommissionCorrections {
			correctionRetainedVector(t, actualRewards, apiUatom, credit.Amount.String())
		}
		require.True(t, correctionCoinEqual(actualRewards, correctionRetainedExpected(t, expected[want].Reward)), "native credit delta differs from accepted bounded companion")
		reads := s.sourceCorrectionReceipts.receipts()
		require.Len(t, reads, 1)
		require.True(t, reads[0].Status == "bounded_native_effects_consumed", "actual fixture consumption receipt missing")
		require.NoError(t, m.ExpectationsWereMet())
	}
	require.Equal(t, 820, feeMemberships)
	require.Len(t, uniqueFees, 786)
	require.Equal(t, 6, rewardCredits)
	if output := os.Getenv("COMMISSION_CORRECTIONS_SAFE_OUTPUT"); output != "" {
		receipt := map[string]any{"at": time.Now().UTC().Format(time.RFC3339Nano), "schema": "retained_six_window_read_time_adapter_consumption_v1", "profile_sha256": p.digest, "combined_private_profile_sha256": (CommissionFlowBuildOptions{PrivateProfile: &CommissionPrivateProfile{digest: p.BaseProfileSHA256}, SourceCorrections: p}).profileSHA(), "bounded_windows": 6, "ambiguous_same_block_lifecycle_intersections": 0, "fee_native_transactions": len(uniqueFees), "fee_window_memberships": feeMemberships, "native_reward_credits": rewardCredits, "original_source_rows_unchanged": true, "fee_and_credit_deltas_equal_pinned_accepted_companions": true, "source_binding_mode": "offline_sqlmock; real indexed message/transaction lookups remain actual trace-time checks", "live_source_or_native_queries": 0, "original_source_or_ledger_writes": 0, "actual_page_roots_affected": "not_measured_by_this_fixture_test", "financially_accepted": false, "publicpromotion": false}
		b, e := json.Marshal(receipt)
		require.NoError(t, e)
		f, e := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		require.NoError(t, e)
		_, e = f.Write(b)
		require.NoError(t, e)
		require.NoError(t, f.Close())
	}
}
