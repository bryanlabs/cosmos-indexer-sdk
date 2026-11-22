package main

import (
	"encoding/base64"
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// CometBFT 0.38: finalize_block_events with mode, plain strings.
const finalizeBlock = `{"jsonrpc":"2.0","id":-1,"result":{"height":"500","txs_results":[
 {"code":0,"events":[
   {"type":"withdraw_rewards","attributes":[{"key":"amount","value":"12.5uatom,3ibc/AB"},{"key":"validator","value":"valA"}]},
   {"type":"withdraw_commission","attributes":[{"key":"amount","value":"7uatom"}]},
   {"type":"unbond","attributes":[{"key":"amount","value":"100uatom"},{"key":"completion_time","value":"2026-12-12T01:02:03Z"}]}]},
 {"code":5,"events":[{"type":"withdraw_rewards","attributes":[{"key":"amount","value":"999uatom"}]}]}],
 "finalize_block_events":[
   {"type":"mint","attributes":[{"key":"bonded_ratio","value":"0.600000000000000000"},{"key":"inflation","value":"0.100000000000000000"},
     {"key":"annual_provisions","value":"1000.5"},{"key":"amount","value":"12345"},{"key":"mode","value":"BeginBlock"}]},
   {"type":"commission","attributes":[{"key":"amount","value":"1.000000000000000001uatom"},{"key":"validator","value":"valA"},{"key":"mode","value":"BeginBlock"}]},
   {"type":"rewards","attributes":[{"key":"amount","value":"10.5uatom"},{"key":"validator","value":"valA"},{"key":"mode","value":"BeginBlock"}]},
   {"type":"rewards","attributes":[{"key":"amount","value":"2uatom"},{"key":"validator","value":"valB"},{"key":"mode","value":"BeginBlock"}]},
   {"type":"rewards","attributes":[{"key":"amount","value":"5ibc/XYZ"},{"key":"validator","value":"valC"},{"key":"mode","value":"BeginBlock"}]},
   {"type":"active_proposal","attributes":[{"key":"proposal_id","value":"996"},{"key":"proposal_result","value":"proposal_passed"},{"key":"mode","value":"EndBlock"}]},
   {"type":"rewards","attributes":[{"key":"amount","value":"4uatom"},{"key":"validator","value":"valA"},{"key":"mode","value":"EndBlock"}]}]}}`

func TestParseFinalizeBlock(t *testing.T) {
	b, err := parseBlockResults([]byte(finalizeBlock), 500)
	require.NoError(t, err)
	require.Equal(t, "12345", b.Mint["amount"])
	require.Equal(t, "0.100000000000000000", b.Mint["inflation"])
	require.Equal(t, "12.5", fixString(b.Rewards), "end block rewards and non-uatom amounts are not counted")
	require.Equal(t, "1.000000000000000001", fixString(b.Commission))
	require.Equal(t, "10.5", fixString(b.PerVal["valA"][0]))
	require.Equal(t, "1.000000000000000001", fixString(b.PerVal["valA"][1]))
	require.Equal(t, "2", fixString(b.PerVal["valB"][0]))
	require.NotContains(t, b.PerVal, "valC")
	require.Len(t, b.Proposals, 1)
	require.True(t, b.Proposals[0].passed())
	require.Equal(t, "996", *b.Proposals[0].ID)
	require.Equal(t, "12.5", fixString(b.WithdrawR), "failed transactions are not counted")
	require.Equal(t, "7", fixString(b.WithdrawC))
	require.Equal(t, "100", fixString(b.Unbond["2026-12-12"]))

	_, err = parseBlockResults([]byte(finalizeBlock), 501)
	require.ErrorContains(t, err, "height mismatch")
}

// Tendermint 0.34: base64 keys and values in begin_block_events and end_block_events.
func TestParseBase64Block(t *testing.T) {
	attr := func(k, v string) string { return `{"key":"` + b64(k) + `","value":"` + b64(v) + `","index":true}` }
	body := `{"result":{"height":"7","txs_results":null,"begin_block_events":[
 {"type":"mint","attributes":[` + attr("bonded_ratio", "0.5") + `,` + attr("inflation", "0.07") + `,` + attr("annual_provisions", "70") + `,` + attr("amount", "11") + `]},
 {"type":"rewards","attributes":[` + attr("amount", "3.25uatom") + `,` + attr("validator", "valZ") + `]},
 {"type":"rewards","attributes":[` + attr("amount", "1uatom") + `]}],
 "end_block_events":[{"type":"active_proposal","attributes":[` + attr("proposal_id", "88") + `,` + attr("proposal_result", "proposal_failed") + `]}]}}`
	b, err := parseBlockResults([]byte(body), 7)
	require.NoError(t, err)
	require.Equal(t, "11", b.Mint["amount"])
	require.Equal(t, "0.07", b.Mint["inflation"])
	require.Equal(t, "4.25", fixString(b.Rewards))
	require.Equal(t, "3.25", fixString(b.PerVal["valZ"][0]))
	require.Equal(t, "1", fixString(b.PerVal["?"][0]), "an event without a validator is kept under ?")
	require.Len(t, b.Proposals, 1)
	require.False(t, b.Proposals[0].passed())
}

func TestParseRefusals(t *testing.T) {
	_, err := parseBlockResults([]byte(`{"result":{"height":"9","txs_results":[]}}`), 9)
	require.ErrorContains(t, err, "no block events key")
	two := `{"result":{"height":"9","begin_block_events":[{"type":"mint","attributes":[]},{"type":"mint","attributes":[]}]}}`
	_, err = parseBlockResults([]byte(two), 9)
	require.ErrorContains(t, err, "two mint events")
	_, err = parseBlockResults([]byte(`{"error":{"code":1}}`), 9)
	require.ErrorIs(t, err, errNoResult)
	bad := `{"result":{"height":"9","begin_block_events":[{"type":"rewards","attributes":[{"key":"amount","value":"1.2.3uatom"}]}]}}`
	_, err = parseBlockResults([]byte(bad), 9)
	require.Error(t, err)
}

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic(s)
	}
	return r
}

func TestRounding(t *testing.T) {
	for in, want := range map[string]string{"2.5": "2", "3.5": "4", "-2.5": "-2", "2.5000001": "3", "-3.5": "-4", "0.49": "0"} {
		require.Equal(t, want, roundHalfEven(rat(in)).String(), in)
	}
	// Decimal(1)/Decimal(3) at 60 digits, and a tie at the 60th digit.
	third := roundSig(rat("1/3"), 60)
	require.Equal(t, "0."+repeat("3", 60), third.FloatString(60))
	tie := rat("1" + repeat("0", 59) + "5") // 61 digits ending in 5: ties to even (…0)
	require.Equal(t, "1"+repeat("0", 60), roundSig(tie, 60).FloatString(0))
	require.Equal(t, "0.1", meanPlaces(mustFix("0.3"), 3, 8))
	require.Equal(t, "0.63522341", meanPlaces(mustFix("1.27044682"), 2, 8))
	require.Equal(t, "0.33333333", meanPlaces(mustFix("1"), 3, 8))
	require.Equal(t, "0.66666667", meanPlaces(mustFix("2"), 3, 8))
	require.Equal(t, "0.02", taxString(mustFix("0.02")))
	require.Equal(t, "0.10", taxString(mustFix("0.1")))
	require.Equal(t, "0.025", taxString(mustFix("0.025")))
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func mustFix(s string) *big.Int {
	v, err := parseFix(s)
	if err != nil {
		panic(err)
	}
	return v
}

func TestCommunityTaxProto(t *testing.T) {
	// QueryParamsResponse{params: {community_tax: "20000000000000000", base_proposer_reward: "0", withdraw_addr_enabled: true}}
	inner := append([]byte{0x0a, 17}, []byte("20000000000000000")...)
	inner = append(inner, 0x12, 1, '0', 0x20, 1)
	msg := append([]byte{0x0a, byte(len(inner))}, inner...)
	s, err := communityTaxFromParams(msg)
	require.NoError(t, err)
	v, err := legacyDec(s)
	require.NoError(t, err)
	require.Equal(t, "0.02", fixString(v))
}
