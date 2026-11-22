package tax

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	sdkmath "cosmossdk.io/math"
	"github.com/DefiantLabs/cosmos-indexer/config"
	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/gogoproto/proto"
	icatypes "github.com/cosmos/ibc-go/v7/modules/apps/27-interchain-accounts/types"
	chantypes "github.com/cosmos/ibc-go/v7/modules/core/04-channel/types"
)

const (
	icaTestAccount  = "cosmos1icahostaccount"
	icaTestWithdraw = "cosmos1icawithdraw"
	icaTestOwner    = "cosmos1recordowner"
	icaMemoKey      = "memo"
)

func protoBytesField(num uint64, b []byte) []byte {
	out := binary.AppendUvarint(nil, num<<3|2)
	out = binary.AppendUvarint(out, uint64(len(b)))
	return append(out, b...)
}

func icaPacket(t *testing.T, msgs ...*codectypes.Any) chantypes.Packet {
	t.Helper()
	tx, err := proto.Marshal(&icatypes.CosmosTx{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"type":"TYPE_EXECUTE_TX","data":%q,"memo":""}`, base64.StdEncoding.EncodeToString(tx))
	return chantypes.Packet{SourcePort: "icacontroller-cosmoshub-4.DELEGATION", DestinationPort: icaHostPort, Data: []byte(data)}
}

func icaRedeemAny(t *testing.T, validator, record string) *codectypes.Any {
	t.Helper()
	shares := sdk.Coin{Denom: validator + "/" + record, Amount: sdkmath.NewInt(10009996)}
	raw, err := shares.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	value := append(protoBytesField(1, []byte(icaTestAccount)), protoBytesField(2, raw)...)
	return &codectypes.Any{TypeUrl: LegacyTypeURLRedeemTokensForShares, Value: value}
}

func icaSendAny(t *testing.T, to string) *codectypes.Any {
	t.Helper()
	value, err := proto.Marshal(&banktypes.MsgSend{FromAddress: icaTestAccount, ToAddress: to, Amount: sdk.NewCoins(sdk.NewInt64Coin(fixtureNativeDenom, 7))})
	if err != nil {
		t.Fatal(err)
	}
	return &codectypes.Any{TypeUrl: "/cosmos.bank.v1beta1.MsgSend", Value: value}
}

func icaSuccess() indexerTxTypes.LogMessageEvent {
	return ev("ics27_packet", [2]string{attributeSuccess, attributeTrue})
}

// Mainnet message 42964074, trimmed to one denom: Stride's delegation ICA
// redeems shares of record 12656 inside the relayer's MsgRecvPacket. The ICA
// earns 211028 on its own delegation, paid to its withdraw address, and the
// record withdraws 128962939688 and forwards its whole 152866892596 balance to
// its owner, the account that tokenized it. Before the ICA host packet was
// classified, none of this was booked.
func TestICAHostLegacyRedeemBooksICAAndRecordOwner(t *testing.T) {
	record := lsmRecordAccount("12656")
	ibc := lsmTestIBCDenom
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		icaSuccess(),
		ev("redeem_shares", [2]string{attributeDelegator, icaTestAccount}, [2]string{attributeValidator, lsmTestValoperA}, [2]string{attributeAmount, "10009996" + lsmTestValoperA + "/12656"}),
		ev(string(CategoryTransfer),
			[2]string{attributeRecipient, record}, [2]string{attributeSender, distributionModuleAddress}, [2]string{attributeAmount, "128962939688" + ibc},
			[2]string{attributeRecipient, icaTestOwner}, [2]string{attributeSender, record}, [2]string{attributeAmount, "152866892596" + ibc},
			[2]string{attributeRecipient, icaTestWithdraw}, [2]string{attributeSender, distributionModuleAddress}, [2]string{attributeAmount, "211028" + ibc},
		),
		ev("withdraw_rewards",
			[2]string{attributeAmount, "128962939688" + ibc}, [2]string{attributeValidator, lsmTestValoperA},
			[2]string{attributeAmount, "211028" + ibc}, [2]string{attributeValidator, lsmTestValoperA},
		),
		ev("withdraw_tokenize_share_reward", [2]string{attributeWithdrawAddress, icaTestOwner}, [2]string{attributeAmount, "152866892596" + ibc}),
	}}
	out := classify(&chantypes.MsgRecvPacket{Packet: icaPacket(t, icaRedeemAny(t, lsmTestValoperA, "12656"))}, log)
	requireRewardRows(t, out, []rewardRow{
		{lsmTestValoperA, "211028", ibc},
		{lsmTestValoperA, "152866892596", ibc},
	})
	requireRewardRecipients(t, out, icaTestAccount, icaTestOwner)
	redeemed := false
	for _, e := range out {
		if e.Category == string(CategoryRedeemShares) && e.FromAddr == icaTestAccount {
			redeemed = true
		}
	}
	if !redeemed {
		t.Fatalf("no redeem_shares row for the ICA account: %+v", out)
	}
}

// The host reverts a failed execution and acknowledges with an error; older
// packets carry no ics27_packet event, only the acknowledgement.
func TestICAHostRevertedPacketBooksNothing(t *testing.T) {
	ack := hex32(`{"error":"ABCI code: 5: error handling packet: see events for details"}`)
	for name, events := range map[string][]indexerTxTypes.LogMessageEvent{
		"ics27":           {ev("ics27_packet", [2]string{attributeSuccess, "false"})},
		"acknowledgement": {ev(eventWriteAcknowledgement, [2]string{attributePacketAckHex, ack})},
	} {
		v, err := (&Parser{}).ParseMessage(&chantypes.MsgRecvPacket{Packet: icaPacket(t, icaSendAny(t, other))}, &indexerTxTypes.LogMessage{Events: events}, config.IndexConfig{})
		if err != nil || len((*v).([]TaxableEvent)) != 0 {
			t.Fatalf("%s: rows %+v, err %v", name, *v, err)
		}
	}
}

func hex32(s string) string { return fmt.Sprintf("%x", s) }

// CosmWasm controllers send the enum as a number and the bytes as an array.
func TestICAHostNumericPacketEncoding(t *testing.T) {
	tx, err := proto.Marshal(&icatypes.CosmosTx{Messages: []*codectypes.Any{icaSendAny(t, other)}})
	if err != nil {
		t.Fatal(err)
	}
	numbers := make([]int, len(tx))
	for i, b := range tx {
		numbers[i] = int(b)
	}
	data, err := json.Marshal(map[string]any{"type": 1, "data": numbers, icaMemoKey: ""})
	if err != nil {
		t.Fatal(err)
	}
	ack := hex32(`{"result":"Eg8KDS9jb3Ntb3MuYmFuay52MWJldGExLk1zZ1NlbmRSZXNwb25zZQ=="}`)
	out := classify(&chantypes.MsgRecvPacket{Packet: chantypes.Packet{DestinationPort: icaHostPort, Data: data}},
		&indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{ev(eventWriteAcknowledgement, [2]string{attributePacketAckHex, ack})}})
	if len(out) != 1 || out[0].Category != string(CategoryTransfer) || out[0].FromAddr != icaTestAccount || out[0].ToAddr != other || out[0].Amount != "7" {
		t.Fatalf("rows = %+v", out)
	}
}

// Several inner messages share one merged event log, so only bank sends,
// which need no log, are booked; anything else is reported unresolved.
func TestICAHostBatchNeedsMessageOnlyTypes(t *testing.T) {
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{icaSuccess()}}
	out := classify(&chantypes.MsgRecvPacket{Packet: icaPacket(t, icaSendAny(t, other), icaSendAny(t, del))}, log)
	if len(out) != 2 || out[0].ToAddr != other || out[1].ToAddr != del {
		t.Fatalf("rows = %+v", out)
	}
	_, err := (&Parser{}).ParseMessage(&chantypes.MsgRecvPacket{Packet: icaPacket(t, icaSendAny(t, other), icaRedeemAny(t, lsmTestValoperA, "12656"))}, log, config.IndexConfig{})
	var incomplete *ClassificationIncompleteError
	if !errors.As(err, &incomplete) || !strings.Contains(err.Error(), "2 inner messages share one event log (/cosmos.bank.v1beta1.MsgSend x1, "+LegacyTypeURLRedeemTokensForShares+" x1; coins moved false)") {
		t.Fatalf("err = %v", err)
	}
}

func TestICAHostWithoutResultIsUnresolved(t *testing.T) {
	_, err := (&Parser{}).ParseMessage(&chantypes.MsgRecvPacket{Packet: icaPacket(t, icaSendAny(t, other))}, &indexerTxTypes.LogMessage{}, config.IndexConfig{})
	var incomplete *ClassificationIncompleteError
	if !errors.As(err, &incomplete) {
		t.Fatalf("err = %v", err)
	}
}
