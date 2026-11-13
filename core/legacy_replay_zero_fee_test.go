package core

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/legacy"
	rr "github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"
	amino "github.com/tendermint/go-amino"
)

func TestReplayExplicitZeroFee(t *testing.T) {
	for _, denom := range []string{"uatom", ""} {
		for _, committed := range []bool{true, false} {
			code := uint32(0)
			if !committed {
				code = 5
			}
			h, items, r := replayFixture(t, code, committed)
			// NewCoins strips zero coins, so retain the signed coin literally.
			items[0].Tx.Fee = sdk.Coins{{Denom: denom, Amount: sdk.ZeroInt()}}
			r.Records[1].Denom, r.Records[1].Amount = denom, "0"
			out, err := LegacyReplayBlock(h, items, r)
			require.NoError(t, err)
			require.Equal(t, committed, out.Inclusions[0].FeeCharged)
			var kept []rr.Record
			require.NoError(t, json.Unmarshal([]byte(out.Events[0].Attributes[0][1]), &kept))
			require.Equal(t, r.Records, kept)
		}
	}
}

func TestReplayZeroFeeRefusals(t *testing.T) {
	for _, name := range []string{"unknown", "payer", "duplicate", "missing", "noncanonical", "leading zero", "positive", "message", "aborted message", "end", "status", "nil coins", "not invoked evidence", "aborted missing", "aborted unknown", "aborted committed zero", "not invoked aborted evidence"} {
		t.Run(name, func(t *testing.T) {
			h, items, r := replayFixture(t, 0, true)
			items[0].Tx.Fee = sdk.Coins{{Denom: "uatom", Amount: sdk.ZeroInt()}}
			r.Records[1].Amount = "0"
			switch name {
			case "unknown":
				r.Records[1].Denom = "other"
			case "payer":
				r.Records[1].Address = "other"
			case "duplicate":
				r.Records = append(r.Records[:2], append([]rr.Record{r.Records[1]}, r.Records[2:]...)...)
			case "missing":
				r.Records = append(r.Records[:1], r.Records[2:]...)
			case "noncanonical":
				r.Records[1].Amount = "-0"
			case "leading zero":
				r.Records[1].Amount = "00"
			case "aborted missing", "aborted unknown", "aborted committed zero", "not invoked aborted evidence":
				h, items, r = replayFixture(t, 5, false)
				items[0].Tx.Fee = sdk.Coins{{Denom: "uatom", Amount: sdk.ZeroInt()}}
				r.Records[1].Amount = "0"
				if name == "aborted missing" {
					r.Records = append(r.Records[:1], r.Records[2:]...)
				} else if name == "aborted unknown" {
					r.Records[1].Denom = "other"
				} else if name == "aborted committed zero" {
					r.Records[1].Status = "committed"
				} else {
					r.Records[2].Label = "ante_not_invoked"
				}
			case "positive":
				r.Records[1].Amount = "1"
			case "message":
				r.Records[3].Amount = "0"
			case "aborted message":
				items[0].Result.Code = 5
				r.Records[6].Code = 5
				for i := 3; i <= 5; i++ {
					r.Records[i].Status = "aborted"
				}
				r.Records[3].Amount = "0"
			case "end":
				r.Records[1].Phase, r.Records[1].Tx = "end_block", -1
			case "status":
				r.Records[1].Status = "aborted"
			case "nil coins":
				items[0].Tx.Fee = nil
			case "not invoked evidence":
				r.Records[2].Label = "ante_not_invoked"
			}
			_, err := LegacyReplayBlock(h, items, r)
			require.Error(t, err)
		})
	}
}

func TestReplayZeroFeeNotInvokedAndNilCoins(t *testing.T) {
	for _, notInvoked := range []bool{true, false} {
		h, items, r := replayFixture(t, 5, false)
		if notInvoked {
			items[0].Tx.Fee = sdk.Coins{{Denom: "uatom", Amount: sdk.ZeroInt()}}
			r.Records[2].Label = "ante_not_invoked"
		} else {
			items[0].Tx.Fee = nil
		}
		r.Records = append(r.Records[:1], r.Records[2:]...)
		_, err := LegacyReplayBlock(h, items, r)
		require.NoError(t, err)
	}
}

func TestReplayZeroFeeMultiplicityAndMixedFee(t *testing.T) {
	for _, committed := range []bool{true, false} {
		code := uint32(0)
		if !committed {
			code = 5
		}
		for _, missing := range []bool{false, true} {
			h, items, r := replayFixture(t, code, committed)
			items[0].Tx.Fee = append(items[0].Tx.Fee, sdk.Coin{Denom: "other", Amount: sdk.ZeroInt()}, sdk.Coin{Denom: "other", Amount: sdk.ZeroInt()})
			zero := r.Records[1]
			zero.Denom, zero.Amount = "other", "0"
			added := []rr.Record{zero, zero}
			if missing {
				added = added[:1]
			}
			r.Records = append(r.Records[:2], append(added, r.Records[2:]...)...)
			_, err := LegacyReplayBlock(h, items, r)
			if missing {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		}
	}
}

func TestReplayDuplicateEmptyDenomZeroFee(t *testing.T) {
	h, items, r := replayFixture(t, 0, true)
	items[0].Tx.Fee = sdk.Coins{{Denom: "", Amount: sdk.ZeroInt()}, {Denom: "", Amount: sdk.ZeroInt()}}
	r.Records[1].Denom, r.Records[1].Amount = "", "0"
	zero := r.Records[1]
	r.Records = append(r.Records[:2], append([]rr.Record{zero}, r.Records[2:]...)...)
	_, err := LegacyReplayBlock(h, items, r)
	require.NoError(t, err)
	r.Records = append(r.Records[:2], r.Records[3:]...)
	_, err = LegacyReplayBlock(h, items, r)
	require.Error(t, err)
}

func TestReplayZeroFeeWireDecode(t *testing.T) {
	for _, denom := range []string{"uatom", ""} {
		h, items, r := replayFixture(t, 0, true)
		cdc := amino.NewCodec()
		cdc.RegisterInterface((*intakeMsg)(nil), nil)
		cdc.RegisterConcrete(intakeSend{}, "cosmos-sdk/MsgSend", nil)
		cdc.RegisterConcrete(intakeTx{}, "auth/StdTx", nil)
		a, b := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20)
		raw, err := cdc.MarshalBinaryLengthPrefixed(intakeTx{Msgs: []intakeMsg{intakeSend{a, b, []intakeCoin{{"uatom", "53"}}}}, Fee: intakeFee{Amount: []intakeCoin{{denom, "0"}}, Gas: 90000}})
		require.NoError(t, err)
		tx, err := legacy.Decode("cosmoshub-2", raw)
		require.NoError(t, err)
		require.Len(t, tx.Fee, 1)
		require.True(t, tx.Fee[0].Amount.IsZero())
		require.Equal(t, denom, tx.Fee[0].Denom)
		h.Block.Block.Txs[0] = raw
		items[0].Tx = tx
		items[0].Hash = fmtHash(raw)
		r.Records[1].Denom, r.Records[1].Amount = denom, "0"
		_, err = LegacyReplayBlock(h, items, r)
		require.NoError(t, err)
	}
}
