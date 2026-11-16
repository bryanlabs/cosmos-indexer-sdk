package replayrecords

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const hub1Lines = `{"before":[{"key":"AA==","value":"AQ=="}],"unit":"native_store_bytes","kind":"native_state_checkpoint","code":0,"height":7,"tx":-1,"message":-1,"phase":"block","label":"segment_baseline/store:main:main","status":"committed","address":"","denom":"","amount":""}
{"kind":"accounting","code":0,"height":7,"tx":-1,"message":-1,"phase":"begin_block","label":"","status":"committed","address":"","denom":"","amount":""}
{"classification":"signed_ante_fee","evidence_quality":"signed_fee_and_full_account_reconciliation","unit":"native_bank_tokens","kind":"movement","code":0,"height":7,"tx":0,"message":-1,"phase":"ante","label":"ante_fee","status":"committed","address":"payer","denom":"uatom","amount":"-5"}
{"kind":"accounting","code":0,"height":7,"tx":0,"message":-1,"phase":"ante","label":"","status":"committed","address":"","denom":"","amount":""}
{"before":"9","after":"0","unit":"delegation_shares","validator":"val","delegator":"payer","kind":"share_request","code":0,"height":7,"tx":0,"message":0,"phase":"message","label":"undelegate:requested_source_shares","status":"committed","address":"payer","denom":"","amount":"9.000000000000000000"}
{"before":"90","after":"81","unit":"native_bond_tokens","validator":"val","delegator":"payer","kind":"principal","code":0,"height":7,"tx":0,"message":0,"phase":"message","label":"undelegate","status":"committed","address":"payer","denom":"uatom","amount":"9"}
{"native_view":1,"native_parent":0,"write_operation":"set","before":null,"after":{"x":[1,2]},"unit":"native_store_bytes","kind":"native_write","code":0,"height":7,"tx":0,"message":0,"phase":"message","label":"store:staking:staking/key:AA==","status":"committed","address":"","denom":"","amount":""}
{"validator":"val","delegator":"payer","kind":"accounting","code":0,"height":7,"tx":0,"message":0,"phase":"message","label":"undelegate","status":"committed","address":"","denom":"","amount":""}
{"kind":"accounting","code":0,"height":7,"tx":0,"message":-1,"phase":"transaction","label":"","status":"committed","address":"","denom":"","amount":""}
{"kind":"accounting","code":0,"height":7,"tx":-1,"message":-1,"phase":"end_block","label":"","status":"committed","address":"","denom":"","amount":""}
{"kind":"height_completed","code":0,"height":7,"tx":-1,"message":-1,"phase":"block","label":"","status":"committed","address":"","denom":"","amount":""}
{"kind":"observation_completed","code":0,"height":7,"tx":-1,"message":-1,"phase":"segment","label":"","status":"committed","address":"","denom":"","amount":""}
`

func hub1Proof(body string) Proof {
	sum := sha256.Sum256([]byte(body))
	return Proof{Chain: "cosmoshub-1", Release: Release033, From: 7, Through: 7, VerifiedThrough: 7, SHA256: hex.EncodeToString(sum[:]),
		Records: int64(strings.Count(body, "\n")), RawBytes: int64(len(body))}
}

func hub1Stream(body string, p Proof) ([]Height, Hub1Counts, error) {
	var out []Height
	counts, err := StreamHub1(func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }, p, func(h Height) error { out = append(out, h); return nil })
	return out, counts, err
}

func TestHub1StreamKeepsFinancialRecordsAndCountsNativeEvidence(t *testing.T) {
	heights, counts, err := hub1Stream(hub1Lines, hub1Proof(hub1Lines))
	require.NoError(t, err)
	require.Len(t, heights, 1)
	var kinds []string
	for _, r := range heights[0].Records {
		kinds = append(kinds, r.Kind)
	}
	require.Equal(t, []string{"accounting", "movement", "accounting", "share_request", "principal", "accounting", "accounting", "accounting", "height_completed"}, kinds)
	require.Equal(t, "9", heights[0].Records[4].Amount)
	require.Equal(t, int64(12), counts.Records)
	require.Equal(t, int64(1), counts.Kinds["native_write"])
	require.Equal(t, int64(1), counts.Kinds["native_state_checkpoint"])
	require.Equal(t, int64(1), counts.Heights)
}

func TestHub1StreamRefusesByName(t *testing.T) {
	change := func(old, replacement string) string {
		require.Contains(t, hub1Lines, old)
		return strings.Replace(hub1Lines, old, replacement, 1)
	}
	for name, tc := range map[string]struct{ body, kind, reason string }{
		"unknown kind":   {change(`"kind":"share_request"`, `"kind":"slash_payout"`), "slash_payout", "not admitted"},
		"unknown unit":   {change(`"unit":"native_bank_tokens"`, `"unit":"native_pool_tokens"`), "movement", "unit"},
		"unknown field":  {change(`"kind":"principal"`, `"surprise":1,"kind":"principal"`), "principal", "known shape"},
		"no completion":  {change(`{"kind":"observation_completed","code":0,"height":7,"tx":-1,"message":-1,"phase":"segment","label":"","status":"committed","address":"","denom":"","amount":""}`+"\n", ""), "", "completion"},
		"missing height": {change(`{"kind":"height_completed","code":0,"height":7,"tx":-1,"message":-1,"phase":"block","label":"","status":"committed","address":"","denom":"","amount":""}`+"\n", ""), "observation_completed", "last height"},
	} {
		t.Run(name, func(t *testing.T) {
			heights, _, err := hub1Stream(tc.body, hub1Proof(tc.body))
			var refused *Hub1Error
			require.True(t, errors.As(err, &refused), "%v", err)
			require.Equal(t, tc.kind, refused.Kind)
			require.Contains(t, refused.Reason, tc.reason)
			require.NotContains(t, err.Error(), "payer")
			require.Empty(t, heights, "nothing is handed over before the whole stream is admitted")
		})
	}
}

func TestHub1StreamBoundToItsPin(t *testing.T) {
	for name, edit := range map[string]func(*Proof){
		"digest":  func(p *Proof) { p.SHA256 = strings.Repeat("0", 64) },
		"records": func(p *Proof) { p.Records++ },
		"bytes":   func(p *Proof) { p.RawBytes-- },
		"release": func(p *Proof) { p.Release = Release034 },
		"chain":   func(p *Proof) { p.Chain = "cosmoshub-2" },
		"range":   func(p *Proof) { p.Through = 8; p.VerifiedThrough = 8 },
	} {
		t.Run(name, func(t *testing.T) {
			p := hub1Proof(hub1Lines)
			edit(&p)
			heights, _, err := hub1Stream(hub1Lines, p)
			require.Error(t, err)
			require.Empty(t, heights)
		})
	}
	// The Hub 2 reader never takes a Hub 1 proof, and the reverse.
	p := hub1Proof(hub1Lines)
	_, err := Read(strings.NewReader(hub1Lines), p)
	require.Error(t, err)
}

func TestHub1ExportStream(t *testing.T) {
	body := `{"before":[],"unit":"native_store_bytes","kind":"native_state_checkpoint","code":0,"height":7,"tx":-1,"message":-1,"phase":"zero_height_export","label":"export_baseline/store:main:main","status":"exported","address":"","denom":"","amount":""}
{"validator":"val","delegator":"payer","kind":"accounting","code":0,"height":7,"tx":-1,"message":-1,"phase":"zero_height_export","label":"delegator_reward","status":"exported","address":"","denom":"","amount":""}
{"unit":"native_bank_tokens","validator":"val","delegator":"payer","kind":"movement","code":0,"height":7,"tx":-1,"message":-1,"phase":"zero_height_export","label":"delegator_reward/bank.add","status":"exported","address":"payer","denom":"uatom","amount":"4"}
{"kind":"export_completed","code":0,"height":7,"tx":-1,"message":-1,"phase":"zero_height_export","label":"","status":"exported","address":"","denom":"","amount":""}
`
	p := hub1Proof(body)
	p.Export = true
	heights, _, err := hub1Stream(body, p)
	require.NoError(t, err)
	require.Len(t, heights, 1)
	require.Len(t, heights[0].Records, 3)
	p.Export = false
	_, _, err = hub1Stream(body, p)
	require.Error(t, err)
}
