package tax

import (
	"encoding/binary"
	"testing"

	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// sdkMsg is a local alias so the table test can assert the interface.
type sdkMsg = sdk.Msg

// field builds one length-delimited protobuf field.
func fieldBytes(num int, val string) []byte {
	out := binary.AppendUvarint(nil, uint64(num)<<3|2)
	out = binary.AppendUvarint(out, uint64(len(val)))
	return append(out, val...)
}

// fieldVarint builds one varint protobuf field.
func fieldVarint(num int, val uint64) []byte {
	out := binary.AppendUvarint(nil, uint64(num)<<3|0)
	return binary.AppendUvarint(out, val)
}

func TestWithdrawAllTokenizeShareRecordRewardUnmarshal(t *testing.T) {
	owner := "cosmos1z835cjxfz73595wunqtn3flx6tglrsy05y673w"
	var m MsgWithdrawAllTokenizeShareRecordReward
	if err := m.Unmarshal(fieldBytes(1, owner)); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.OwnerAddress != owner {
		t.Fatalf("owner = %q, want %q", m.OwnerAddress, owner)
	}
	if len(m.GetSigners()) != 1 {
		t.Fatalf("expected the owner to be the signer")
	}
}

func TestWithdrawTokenizeShareRecordRewardUnmarshal(t *testing.T) {
	owner := "cosmos1z835cjxfz73595wunqtn3flx6tglrsy05y673w"
	b := append(fieldBytes(1, owner), fieldVarint(2, 4213)...)
	var m MsgWithdrawTokenizeShareRecordReward
	if err := m.Unmarshal(b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.OwnerAddress != owner || m.RecordId != 4213 {
		t.Fatalf("got %q / %d", m.OwnerAddress, m.RecordId)
	}
}

func TestTokenizeSharesUnmarshalKeepsCoinBytes(t *testing.T) {
	del := "cosmos1z835cjxfz73595wunqtn3flx6tglrsy05y673w"
	val := "cosmosvaloper1z835cjxfz73595wunqtn3flx6tglrsy05y673w"
	// nested Coin{denom:"uatom", amount:"100"} as its own length-delimited field
	coin := append(fieldBytes(1, "uatom"), fieldBytes(2, "100")...)
	b := append(fieldBytes(1, del), fieldBytes(2, val)...)
	b = append(b, fieldBytes(3, string(coin))...)
	b = append(b, fieldBytes(4, del)...)

	var m MsgTokenizeShares
	if err := m.Unmarshal(b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.DelegatorAddress != del || m.ValidatorAddress != val || m.TokenizedShareOwner != del {
		t.Fatalf("addresses did not round trip: %+v", m)
	}
	if string(m.AmountRaw) != string(coin) {
		t.Fatalf("coin bytes not preserved")
	}
}

// An unknown field must not break decoding: the chain can add one without us.
func TestUnmarshalSkipsUnknownFields(t *testing.T) {
	owner := "cosmos1z835cjxfz73595wunqtn3flx6tglrsy05y673w"
	b := append(fieldBytes(1, owner), fieldVarint(9, 12345)...)
	b = append(b, fieldBytes(7, "something new")...)
	var m MsgWithdrawAllTokenizeShareRecordReward
	if err := m.Unmarshal(b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.OwnerAddress != owner {
		t.Fatalf("owner = %q", m.OwnerAddress)
	}
}

func TestEveryLiquidTypeIsRegistrable(t *testing.T) {
	types := GaiaLiquidMsgTypes()
	if len(types) != 7 {
		t.Fatalf("expected 7 x/liquid message types, got %d", len(types))
	}
	for url, msg := range types {
		if msg == nil {
			t.Fatalf("%s registered as nil", url)
		}
		// every one must survive an empty payload without error
		if u, ok := msg.(interface{ Unmarshal([]byte) error }); ok {
			if err := u.Unmarshal(nil); err != nil {
				t.Fatalf("%s: empty unmarshal: %v", url, err)
			}
		} else {
			t.Fatalf("%s does not implement Unmarshal", url)
		}
	}
}

// Real mainnet transaction C16746F6... at height 32549829: one
// MsgWithdrawAllTokenizeShareRecordReward settling a single record. The tx
// contains four coin_received events (the record's module account, the owner,
// the fee grantee and the tip payee), which is exactly why summing
// coin_received the way delegatorRewardEvents does would report 4685020uatom
// instead of the 2342510uatom actually earned.
func TestClassifyTokenizeShareRewardMainnetFixture(t *testing.T) {
	owner := "cosmos1356y7gg98xgl4mn5f2dt0yr2ysc6gs4gfpfdsy"
	recordAcct := "cosmos1zpqu48nw2h8s08eplmscz3uv6ujpc3awj2p9v3j7c8h2stg8gqqqf8f0ue"
	msg := &MsgWithdrawAllTokenizeShareRecordReward{OwnerAddress: owner}
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("coin_received", [2]string{"receiver", "cosmos13pxn9n3qw79e03844rdadagmg0nshmwf7qvuye"}, [2]string{"amount", "3639uatom"}),
		ev("coin_received", [2]string{"receiver", recordAcct}, [2]string{"amount", "2342510uatom"}),
		ev("withdraw_rewards", [2]string{"amount", "2342510uatom"}, [2]string{"delegator", recordAcct}),
		ev("coin_received", [2]string{"receiver", owner}, [2]string{"amount", "2342510uatom"}),
		ev("withdraw_tokenize_share_reward", [2]string{"withdraw_address", owner}, [2]string{"amount", "2342510uatom"}),
		ev("coin_received", [2]string{"receiver", "cosmos126e0q5adzdny95lujzv0ktwsz32089k4ljaaau"}, [2]string{"amount", "1492uatom"}),
	}}

	out := classify(msg, log)
	if len(out) != 1 {
		t.Fatalf("expected 1 reward event, got %d: %+v", len(out), out)
	}
	if out[0].Category != string(CategoryReward) {
		t.Fatalf("category = %q", out[0].Category)
	}
	if out[0].Amount != "2342510" || out[0].Denom != "uatom" {
		t.Fatalf("amount = %s%s, want 2342510uatom (double counting would give 4685020)", out[0].Amount, out[0].Denom)
	}
	if out[0].ToAddr != owner {
		t.Fatalf("to = %q, want the record owner %q", out[0].ToAddr, owner)
	}
}

// WithdrawAll settles every record the owner holds, so several events sum.
func TestClassifyTokenizeShareRewardSumsMultipleRecords(t *testing.T) {
	owner := "cosmos1356y7gg98xgl4mn5f2dt0yr2ysc6gs4gfpfdsy"
	msg := &MsgWithdrawAllTokenizeShareRecordReward{OwnerAddress: owner}
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("withdraw_tokenize_share_reward", [2]string{"withdraw_address", owner}, [2]string{"amount", "100uatom"}),
		ev("withdraw_tokenize_share_reward", [2]string{"withdraw_address", owner}, [2]string{"amount", "250uatom"}),
	}}
	out := classify(msg, log)
	if len(out) != 1 || out[0].Amount != "350" {
		t.Fatalf("expected a single summed 350uatom event, got %+v", out)
	}
}

// Tokenizing and redeeming are conversions, not disposals: they must decode
// without producing a taxable event.
func TestTokenizeAndRedeemProduceNoTaxableEvent(t *testing.T) {
	for _, msg := range []interface{ String() string }{
		&MsgTokenizeShares{DelegatorAddress: "cosmos1abc"},
		&MsgRedeemTokensForShares{DelegatorAddress: "cosmos1abc"},
		&MsgDisableTokenizeShares{DelegatorAddress: "cosmos1abc"},
		&MsgEnableTokenizeShares{DelegatorAddress: "cosmos1abc"},
	} {
		m, _ := msg.(interface{ String() string })
		out := classify(msg.(sdkMsg), &indexerTxTypes.LogMessage{})
		if len(out) != 0 {
			t.Fatalf("%T produced %d taxable events, expected none", m, len(out))
		}
	}
}

// The interface registry re-marshals every message it unpacks, to cache it back
// into the Any. gogoproto's reflection marshaller panics on hand-written types,
// which took down the indexer, so every custom type must marshal itself and
// return exactly the bytes it decoded from.
func TestEveryCustomTypeRoundTripsItsBytes(t *testing.T) {
	all := map[string]interface {
		Unmarshal([]byte) error
		Marshal() ([]byte, error)
		Size() int
	}{}
	for url, m := range GaiaLiquidMsgTypes() {
		all[url] = m.(interface {
			Unmarshal([]byte) error
			Marshal() ([]byte, error)
			Size() int
		})
	}
	for url, m := range IBCChannelV2MsgTypes() {
		all[url] = m.(interface {
			Unmarshal([]byte) error
			Marshal() ([]byte, error)
			Size() int
		})
	}
	for url, m := range TokenFactoryMsgTypes() {
		all[url] = m.(interface {
			Unmarshal([]byte) error
			Marshal() ([]byte, error)
			Size() int
		})
	}
	if len(all) != 15 {
		t.Fatalf("expected 15 registered custom types, got %d", len(all))
	}

	// Field 1 is a string on some of these and a nested message on others, so an
	// empty length-delimited field 1 is the one shape valid for all of them.
	// Field 11 is modelled by none of them: it proves unknown fields survive the
	// round trip rather than being dropped by a re-encode.
	payload := append(nested(1, nil), fieldVarint(11, 99)...)
	for url, m := range all {
		if err := m.Unmarshal(payload); err != nil {
			t.Fatalf("%s: unmarshal: %v", url, err)
		}
		out, err := m.Marshal()
		if err != nil {
			t.Fatalf("%s: marshal: %v", url, err)
		}
		if string(out) != string(payload) {
			t.Fatalf("%s: round trip changed the bytes", url)
		}
		if m.Size() != len(payload) {
			t.Fatalf("%s: Size() = %d, want %d", url, m.Size(), len(payload))
		}
	}
}
