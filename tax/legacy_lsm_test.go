package tax

import (
	"bytes"
	"reflect"
	"testing"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	probe "github.com/DefiantLabs/probe/client/codec/types"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
)

func legacyRegistry() codectypes.InterfaceRegistry {
	r, custom := probe.NewInterfaceRegistry()
	sdk.RegisterInterfaces(r)
	authz.RegisterInterfaces(r)
	bank.RegisterInterfaces(r)
	for url, m := range GaiaLiquidMsgTypes() {
		custom.RegisterCustomTypeURL((*sdk.Msg)(nil), url, m)
	}
	for url, m := range LegacyLSMMsgTypes() {
		custom.RegisterCustomTypeURL((*sdk.Msg)(nil), url, m)
	}
	return r
}

func decodeLegacy(t *testing.T, r codectypes.InterfaceRegistry, url string, wire []byte) sdk.Msg {
	t.Helper()
	a := &codectypes.Any{TypeUrl: url, Value: append([]byte(nil), wire...)}
	var msg sdk.Msg
	if err := r.UnpackAny(a, &msg); err != nil {
		t.Fatal(err)
	}
	packed, err := codectypes.NewAnyWithValue(msg)
	if err != nil {
		t.Fatal(err)
	}
	if packed.TypeUrl != url || !bytes.Equal(packed.Value, wire) || !bytes.Equal(a.Value, wire) {
		t.Fatalf("Any round trip changed %s: %+v", url, packed)
	}
	return msg
}

// Field numbers are from cosmos/cosmos-sdk v0.50.11-lsm tagged tx.proto,
// not marshaled from our hand-written definitions. Include unknown fields to
// verify the registry's Any cache preserves unmodeled historical evidence.
func TestLegacyLSMAllNineWireShapes(t *testing.T) {
	r := legacyRegistry()
	owner := "cosmos1z835cjxfz73595wunqtn3flx6tglrsy05y673w"
	validator := "cosmosvaloper1fixture"
	coin := append(fieldBytes(1, "uatom"), fieldBytes(2, "123")...)
	join := func(parts ...[]byte) []byte {
		var out []byte
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	tests := []struct {
		url    string
		wire   []byte
		fields map[string]interface{}
	}{
		{LegacyTypeURLTokenizeShares, join(fieldBytes(1, owner), fieldBytes(2, validator), fieldBytes(3, string(coin)), fieldBytes(4, "record-owner")), map[string]interface{}{"DelegatorAddress": owner, "ValidatorAddress": validator, "AmountRaw": coin, "TokenizedShareOwner": "record-owner"}},
		{LegacyTypeURLRedeemTokensForShares, join(fieldBytes(1, owner), fieldBytes(2, string(coin))), map[string]interface{}{"DelegatorAddress": owner, "AmountRaw": coin}},
		{LegacyTypeURLTransferTokenizeShareRecord, join(fieldVarint(1, 4213), fieldBytes(2, owner), fieldBytes(3, "new-owner")), map[string]interface{}{"TokenizeShareRecordId": uint64(4213), "Sender": owner, "NewOwner": "new-owner"}},
		{LegacyTypeURLDisableTokenizeShares, fieldBytes(1, owner), map[string]interface{}{"DelegatorAddress": owner}},
		{LegacyTypeURLEnableTokenizeShares, fieldBytes(1, owner), map[string]interface{}{"DelegatorAddress": owner}},
		{LegacyTypeURLValidatorBond, join(fieldBytes(1, owner), fieldBytes(2, validator)), map[string]interface{}{"DelegatorAddress": owner, "ValidatorAddress": validator}},
		{LegacyTypeURLUnbondValidator, fieldBytes(1, owner), map[string]interface{}{"ValidatorAddress": owner}},
		{LegacyTypeURLWithdrawTokenizeShareReward, join(fieldBytes(1, owner), fieldVarint(2, 4213)), map[string]interface{}{"OwnerAddress": owner, "RecordId": uint64(4213)}},
		{LegacyTypeURLWithdrawAllTokenizeShareReward, fieldBytes(1, owner), map[string]interface{}{"OwnerAddress": owner}},
	}
	if len(LegacyLSMMsgTypes()) != len(tests) {
		t.Fatal("missing legacy registration")
	}
	seen := map[reflect.Type]bool{}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			wire := append(tt.wire, fieldVarint(99, 12345)...)
			m := decodeLegacy(t, r, tt.url, wire)
			if seen[reflect.TypeOf(m)] {
				t.Fatal("reused concrete protobuf type")
			}
			seen[reflect.TypeOf(m)] = true
			for field, want := range tt.fields {
				got := reflect.ValueOf(m).Elem().FieldByName(field).Interface()
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s = %v want %v", field, got, want)
				}
			}
			if len(m.GetSigners()) != 1 || m.GetSigners()[0].String() != owner {
				t.Fatalf("wrong signer: %v", m.GetSigners())
			}
			if out := classify(m, &txlog.LogMessage{}); len(out) != 0 {
				t.Fatalf("invented events: %+v", out)
			}
		})
	}
}

// Legacy WithdrawAll emits one aggregate event (keeper.go in v0.50.11-lsm),
// unlike newer per-record settlement. Never sum the internal coin_received hop.
func TestLegacyLSMRewardClassification(t *testing.T) {
	r := legacyRegistry()
	for _, url := range []string{LegacyTypeURLWithdrawTokenizeShareReward, LegacyTypeURLWithdrawAllTokenizeShareReward} {
		t.Run(url, func(t *testing.T) {
			m := decodeLegacy(t, r, url, append(fieldBytes(1, del), fieldVarint(2, 4213)...))
			log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
				ev("coin_received", [2]string{"receiver", "record-module"}, [2]string{"amount", "350uatom"}),
				ev("withdraw_rewards", [2]string{"delegator", "record-module"}, [2]string{"amount", "350uatom"}),
				ev("coin_received", [2]string{"receiver", del}, [2]string{"amount", "350uatom"}),
				ev("withdraw_tokenize_share_reward", [2]string{"withdraw_address", del}, [2]string{"amount", "350uatom"}),
			}}
			out := classify(m, log)
			if len(out) != 1 || out[0].Amount != "350" || out[0].ToAddr != del || out[0].Category != string(CategoryReward) {
				t.Fatalf("wrong rewards: %+v", out)
			}
		})
	}
}

func TestLegacyLSMTokenizeRewards(t *testing.T) {
	m := decodeLegacy(t, legacyRegistry(), LegacyTypeURLTokenizeShares, fieldBytes(1, del))
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("withdraw_rewards", [2]string{"delegator", del}, [2]string{"validator", val}, [2]string{"amount", "500uatom"}),
		ev("coin_received", [2]string{"receiver", del}, [2]string{"amount", "900000uatom"}),
	}}
	out := classify(m, log)
	if len(out) != 1 || out[0].Amount != "500" || out[0].RewardTrigger != "tokenize" || out[0].ValidatorAddress != val {
		t.Fatalf("wrong tokenize income: %+v", out)
	}
}

func TestLegacyLSMMixedAuthzWire(t *testing.T) {
	r := legacyRegistry()
	// Decode the outer MsgExec wire too, so legacy children are unpacked through
	// the exact interface registry used by production, not manually cached.
	reward := &codectypes.Any{TypeUrl: LegacyTypeURLWithdrawAllTokenizeShareReward, Value: fieldBytes(1, del)}
	send, err := codectypes.NewAnyWithValue(&bank.MsgSend{FromAddress: del, ToAddress: other, Amount: sdk.NewCoins(sdk.NewInt64Coin("uatom", 7))})
	if err != nil {
		t.Fatal(err)
	}
	exec := &authz.MsgExec{Grantee: del, Msgs: []*codectypes.Any{send, reward}}
	wire, err := exec.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	a := &codectypes.Any{TypeUrl: "/cosmos.authz.v1beta1.MsgExec", Value: wire}
	var m sdk.Msg
	if err := r.UnpackAny(a, &m); err != nil {
		t.Fatal(err)
	}
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("withdraw_tokenize_share_reward", [2]string{"withdraw_address", del}, [2]string{"amount", "999uatom"}, [2]string{"authz_msg_index", "0"}),
		ev("withdraw_tokenize_share_reward", [2]string{"withdraw_address", del}, [2]string{"amount", "350uatom"}, [2]string{"authz_msg_index", "1"}),
		ev("withdraw_rewards", [2]string{"delegator", "record-module"}, [2]string{"amount", "350uatom"}, [2]string{"authz_msg_index", "1"}),
	}}
	out := classify(m, log)
	if len(out) != 2 || out[0].Category != string(CategoryTransfer) || out[0].Amount != "7" || out[1].Category != string(CategoryReward) || out[1].Amount != "350" || out[1].ToAddr != del {
		t.Fatalf("mixed authz: %+v", out)
	}
}
