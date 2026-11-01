package tax

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
	dist "github.com/cosmos/cosmos-sdk/x/distribution/types"
	chantypes "github.com/cosmos/ibc-go/v7/modules/core/04-channel/types"
)

const (
	safetyVoucher       = "ibc/14F9BC3E44B8A9C1BE1FB08980FAB87034C9905EF17CF2F5008FC085218811CC"
	safetyV2Voucher     = "ibc/3401BA755A65AADE5A4B1CCD440C819ABB8FC4732DF81C0A0AF85AB00EDF41DE"
	safetyV2Client      = "08-wasm-1369"
	safetySourceChannel = "channel-7"
	safetyNativeReturn  = "transfer/channel-7/uatom"
)

// Synthetic protocol-shaped fixtures, not claims of archived 2025 transactions.
func safetyPacket(t *testing.T, denom string) chantypes.Packet {
	t.Helper()
	data, err := json.Marshal(map[string]string{attributeSender: other, attributeReceiver: del, attributeAmount: "100", attributeDenom: denom})
	if err != nil {
		t.Fatal(err)
	}
	return chantypes.Packet{Sequence: 23, SourcePort: string(CategoryTransfer), SourceChannel: safetySourceChannel, DestinationPort: string(CategoryTransfer), DestinationChannel: "channel-141", Data: data}
}

func safetyCredit(receiver, denom string) []txlog.LogMessageEvent {
	return []txlog.LogMessageEvent{
		ev(eventCoinReceived, [2]string{attributeReceiver, receiver}, [2]string{attributeAmount, "100" + denom}),
		ev(string(CategoryTransfer), [2]string{attributeSender, "escrow"}, [2]string{attributeRecipient, receiver}, [2]string{attributeAmount, "100" + denom}),
	}
}

func safetyPrefix(events []txlog.LogMessageEvent) []txlog.LogMessageEvent {
	var out []txlog.LogMessageEvent
	for _, event := range events {
		copyEvent := txlog.LogMessageEvent{Type: ibcCallbackErrorPrefix + event.Type}
		for _, a := range event.Attributes {
			copyEvent.Attributes = append(copyEvent.Attributes, txlog.Attribute{Key: ibcCallbackErrorPrefix + a.Key, Value: a.Value})
		}
		out = append(out, copyEvent)
	}
	return out
}

func TestSafetyV1ReceiveRequiresSuccessfulLocalCredit(t *testing.T) {
	for _, tc := range []struct {
		name, packetDenom, bankDenom, success string
		credit, callback                      bool
		want                                  int
		incomplete                            bool
	}{
		{"failed_application_even_with_unrelated_credit", fixtureOsmosisDenom, safetyVoucher, attributeFalse, true, false, 0, false},
		{"failed_application_without_credit", fixtureOsmosisDenom, safetyVoucher, attributeFalse, false, false, 0, false},
		{"packet_only_not_a_credit", fixtureOsmosisDenom, safetyVoucher, attributeTrue, false, false, 0, true},
		{"native_return", safetyNativeReturn, fixtureNativeDenom, attributeTrue, true, false, 1, false},
		{"voucher_local_hash", fixtureOsmosisDenom, safetyVoucher, attributeTrue, true, false, 1, false},
		{"wrong_bank_denom", fixtureOsmosisDenom, fixtureNativeDenom, attributeTrue, true, false, 0, true},
		{"discarded_successful_app_cache", fixtureOsmosisDenom, safetyVoucher, attributeTrue, true, true, 0, false},
		{"failed_transfer_callback_prefix", fixtureOsmosisDenom, safetyVoucher, attributeFalse, true, true, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			packet := safetyPacket(t, tc.packetDenom)
			log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
				ev(eventFungibleTokenPacket, [2]string{attributeReceiver, del}, [2]string{attributeSender, other}, [2]string{attributeDenom, tc.packetDenom}, [2]string{attributeAmount, "100"}, [2]string{attributeSuccess, tc.success}),
			}}
			if tc.credit {
				log.Events = append(log.Events, safetyCredit(del, tc.bankDenom)...)
			}
			if tc.callback {
				log.Events = safetyPrefix(log.Events)
			}
			out := safetyParseReceive(t, &chantypes.MsgRecvPacket{Packet: packet}, log, tc.incomplete)
			if len(out) != tc.want {
				t.Fatalf("want %d, got %+v", tc.want, out)
			}
			if tc.want == 1 && (out[0].Denom != tc.bankDenom || out[0].Amount != "100" || out[0].Category != string(CategoryIBCIn) || out[0].FromAddr != other || out[0].ToAddr != del) {
				t.Fatalf("wrong receive: %+v", out)
			}
		})
	}
}

func safetyV2Packet(payloads int) PacketV2 {
	packet := PacketV2{Sequence: 23, SourceClient: fixtureHubClient, DestinationClient: safetyV2Client}
	for i := 0; i < payloads; i++ {
		payload := append(fieldBytes(1, string(CategoryTransfer)), fieldBytes(2, string(CategoryTransfer))...)
		packet.PayloadsRaw = append(packet.PayloadsRaw, payload)
	}
	return packet
}

func safetyReceive(denom string) txlog.LogMessageEvent {
	return ev(eventFungibleTokenPacket, [2]string{attributeSender, other}, [2]string{attributeReceiver, del}, [2]string{attributeAmount, "100"}, [2]string{attributeDenom, denom}, [2]string{attributeSuccess, attributeTrue})
}

func safetyParseReceive(t *testing.T, msg sdk.Msg, log *txlog.LogMessage, wantIncomplete bool) []TaxableEvent {
	t.Helper()
	data, err := (&Parser{}).ParseMessage(msg, log, cfgForTests())
	var incomplete *ClassificationIncompleteError
	if errors.As(err, &incomplete) != wantIncomplete || (err != nil && !wantIncomplete) {
		t.Fatalf("incomplete=%v, error=%v", wantIncomplete, err)
	}
	if wantIncomplete && (len(incomplete.Issues) == 0 || !strings.Contains(err.Error(), "committed bank credit")) {
		t.Fatalf("missing actionable receive diagnostic: %v", err)
	}
	if data == nil {
		t.Fatal("missing diagnostic dataset")
	}
	return (*data).([]TaxableEvent)
}

func TestSafetyV2ReceiveCreditAndDiscardedCallback(t *testing.T) {
	for _, prefix := range []bool{false, true} {
		log := &txlog.LogMessage{Events: append(safetyCredit(del, safetyV2Voucher), safetyReceive("ucoin"))}
		if prefix {
			log.Events = safetyPrefix(log.Events)
		}
		out := safetyParseReceive(t, &MsgRecvPacketV2{Packet: safetyV2Packet(1)}, log, false)
		if prefix {
			if len(out) != 0 {
				t.Fatalf("discarded cache fabricated receipt: %+v", out)
			}
		} else if len(out) != 1 || out[0].Denom != safetyV2Voucher || out[0].Amount != "100" {
			t.Fatalf("committed credit lost: %+v", out)
		}
	}
}

func TestSafetyV1RefundsArePacketLinkedPrincipal(t *testing.T) {
	for _, denom := range []string{fixtureNativeDenom, "transfer/channel-141/uosmo"} {
		bankDenom := denom
		if denom != fixtureNativeDenom {
			bankDenom = safetyVoucher
		}
		packet := safetyPacket(t, denom)
		log := &txlog.LogMessage{Events: safetyCredit(other, bankDenom)}
		for _, tc := range []struct {
			name    string
			msg     sdk.Msg
			want    int
			trigger string
		}{
			{"timeout", &chantypes.MsgTimeout{Packet: packet}, 1, "ibc_timeout_refund"},
			{"timeout_closed", &chantypes.MsgTimeoutOnClose{Packet: packet}, 1, "ibc_timeout_refund"},
			{"error_ack", &chantypes.MsgAcknowledgement{Packet: packet, Acknowledgement: []byte(`{"error":"failed"}`)}, 1, "ibc_error_ack_refund"},
			{"success_ack", &chantypes.MsgAcknowledgement{Packet: packet, Acknowledgement: []byte(`{"result":"AQ=="}`)}, 0, ""},
			{"malformed_ack", &chantypes.MsgAcknowledgement{Packet: packet, Acknowledgement: []byte(`bad`)}, 0, ""},
		} {
			t.Run(denom+"/"+tc.name, func(t *testing.T) {
				out := classify(tc.msg, log)
				if len(out) != tc.want {
					t.Fatalf("want %d principal row (not two bank representations), got %+v", tc.want, out)
				}
				if tc.want == 0 {
					return
				}
				assertSafetyRefund(t, out[0], bankDenom, tc.trigger, "v1")
				if !strings.Contains(out[0].Detail, `"packet_src_channel":"channel-7"`) {
					t.Fatalf("missing packet linkage: %+v", out[0])
				}
			})
		}
		if out := classify(&chantypes.MsgTimeout{Packet: packet}, &txlog.LogMessage{}); len(out) != 0 {
			t.Fatalf("payload alone fabricated refund: %+v", out)
		}
	}
}

func assertSafetyRefund(t *testing.T, e TaxableEvent, denom, trigger, generation string) {
	t.Helper()
	if e.Category != string(CategoryProtocolWithdrawal) || Category(e.Category).Taxable() || e.Denom != denom || e.Amount != "100" || e.ToAddr != other || e.RewardTrigger != trigger {
		t.Fatalf("refund must be principal, not income: %+v", e)
	}
	for _, marker := range []string{`"principal_return":"true"`, `"packet_sequence":"23"`, `"ibc_generation":"` + generation + `"`} {
		if !strings.Contains(e.Detail, marker) {
			t.Fatalf("missing %s: %+v", marker, e)
		}
	}
}

func TestSafetyV2RefundsFromEventsAndSentinelAck(t *testing.T) {
	packet := PacketV2{Sequence: 23, SourceClient: safetyV2Client, DestinationClient: fixtureHubClient}
	sentinel := sha256.Sum256([]byte("UNIVERSAL_ERROR_ACKNOWLEDGEMENT"))
	errorAck := nested(1, sentinel[:])
	for _, tc := range []struct {
		name, path, denom, token string
	}{
		{"native", fixtureNativeDenom, fixtureNativeDenom, `{"denom":{"base":"uatom","trace":[]},"amount":"100"}`},
		{"v2_voucher", "transfer/08-wasm-1369/ucoin", safetyV2Voucher, `{"denom":{"base":"ucoin","trace":[{"port_id":"transfer","channel_id":"08-wasm-1369"}]},"amount":"100"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &txlog.LogMessage{Events: safetyCredit(other, tc.denom)}
			log.Events = append(log.Events, ev("timeout", [2]string{attributeReceiver, other}, [2]string{"refund_tokens", tc.token}))
			out := classify(&MsgTimeoutV2{Packet: packet}, log)
			if len(out) != 1 {
				t.Fatalf("timeout: %+v", out)
			}
			assertSafetyRefund(t, out[0], tc.denom, "ibc_timeout_refund", "v2")
			log.Events = append(safetyCredit(other, tc.denom),
				ev(eventFungibleTokenPacket, [2]string{attributeSender, other}, [2]string{attributeReceiver, del}, [2]string{attributeDenom, tc.path}, [2]string{attributeAmount, "100"}),
				ev(eventFungibleTokenPacket, [2]string{"error", "failed receive"}),
			)
			out = classify(&MsgAcknowledgementV2{Packet: packet, AcknowledgementRaw: errorAck}, log)
			if len(out) != 1 {
				t.Fatalf("error ack: %+v", out)
			}
			assertSafetyRefund(t, out[0], tc.denom, "ibc_error_ack_refund", "v2")
			for _, ack := range [][]byte{nested(1, []byte(`{"result":"AQ=="}`)), nested(1, []byte(`{"error":"not_the_v2_sentinel"}`)), nil, {0xff}} {
				if out := classify(&MsgAcknowledgementV2{Packet: packet, AcknowledgementRaw: ack}, log); len(out) != 0 {
					t.Fatalf("non-sentinel ack fabricated refund: %+v", out)
				}
			}
			if out := classify(&MsgTimeoutV2{Packet: packet}, &txlog.LogMessage{Events: log.Events[2:]}); len(out) != 0 {
				t.Fatalf("no credit fabricated refund: %+v", out)
			}
		})
	}
}

func safetyUncached(t *testing.T, m sdk.Msg) *codectypes.Any {
	t.Helper()
	a, err := codectypes.NewAnyWithValue(m)
	if err != nil {
		t.Fatal(err)
	}
	return &codectypes.Any{TypeUrl: a.TypeUrl, Value: append([]byte(nil), a.Value...)}
}

func TestSafetyAuthzUncachedMixedAndNestedAlignment(t *testing.T) {
	reward := safetyUncached(t, &dist.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val})
	send := safetyUncached(t, &bank.MsgSend{FromAddress: del, ToAddress: other, Amount: sdk.NewCoins(sdk.NewInt64Coin(fixtureNativeDenom, 7))})
	unknown := &codectypes.Any{TypeUrl: "/not.registered.Inner", Value: []byte{8, 1}}
	nestedExec := safetyUncached(t, &authz.MsgExec{Grantee: del, Msgs: []*codectypes.Any{unknown, reward}})
	msg := &authz.MsgExec{Grantee: other, Msgs: []*codectypes.Any{send, nestedExec, reward}}
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("withdraw_rewards", [2]string{attributeDelegator, del}, [2]string{attributeValidator, val}, [2]string{attributeAmount, "111uatom"}, [2]string{attributeAuthzMsgIndex, "0"}, [2]string{attributeAuthzMsgIndex, "1"}),
		ev("withdraw_rewards", [2]string{attributeDelegator, del}, [2]string{attributeValidator, val}, [2]string{attributeAmount, "222uatom"}, [2]string{attributeAuthzMsgIndex, "1"}, [2]string{attributeAuthzMsgIndex, "1"}),
		ev("withdraw_rewards", [2]string{attributeDelegator, del}, [2]string{attributeValidator, val}, [2]string{attributeAmount, "333uatom"}, [2]string{attributeAuthzMsgIndex, "2"}),
	}}
	before := summary(classify(msg, log))
	data, err := (&Parser{}).ParseMessage(msg, log, cfgForTests())
	var incomplete *ClassificationIncompleteError
	if !errors.As(err, &incomplete) || !strings.Contains(err.Error(), "authz 1/0") || !strings.Contains(err.Error(), unknown.TypeUrl) {
		t.Fatalf("missing unresolved-child diagnostic: %v", err)
	}
	out := (*data).([]TaxableEvent)
	if len(out) != 3 || out[0].Amount != "7" || out[1].Amount != "222" || out[2].Amount != "333" {
		t.Fatalf("known siblings lost or nested indices contaminated: %+v", out)
	}
	if summary(out) != before || summary(classify(msg, log)) != before {
		t.Fatal("classification modified original event alignment")
	}
}

func TestSafetyAuthzAllKnownUncachedAndBounded(t *testing.T) {
	inner := safetyUncached(t, &bank.MsgSend{FromAddress: del, ToAddress: other, Amount: sdk.NewCoins(sdk.NewInt64Coin(fixtureNativeDenom, 7))})
	msg := &authz.MsgExec{Msgs: []*codectypes.Any{inner}}
	data, err := (&Parser{}).ParseMessage(msg, nil, cfgForTests())
	if err != nil || len((*data).([]TaxableEvent)) != 1 {
		t.Fatalf("uncached known inner skipped: %v / %v", data, err)
	}
	for i := 0; i < maxAuthzDepth+2; i++ {
		inner = safetyUncached(t, &authz.MsgExec{Msgs: []*codectypes.Any{inner}})
	}
	_, err = (&Parser{}).ParseMessage(&authz.MsgExec{Msgs: []*codectypes.Any{inner}}, nil, cfgForTests())
	if err == nil || !strings.Contains(err.Error(), "nesting limit") {
		t.Fatalf("unbounded nested authz: %v", err)
	}
	_, err = (&Parser{}).ParseMessage(&authz.MsgExec{Msgs: []*codectypes.Any{nil}}, nil, cfgForTests())
	if err == nil || !strings.Contains(err.Error(), "nil inner Any") {
		t.Fatalf("nil authz not surfaced: %v", err)
	}
	many := make([]*codectypes.Any, maxAuthzMessages+1)
	for i := range many {
		many[i] = safetyUncached(t, &bank.MsgSend{FromAddress: del, ToAddress: other, Amount: sdk.NewCoins(sdk.NewInt64Coin(fixtureNativeDenom, 7))})
	}
	data, err = (&Parser{}).ParseMessage(&authz.MsgExec{Msgs: many}, nil, cfgForTests())
	if err == nil || !strings.Contains(err.Error(), "budget exceeded") || len((*data).([]TaxableEvent)) != maxAuthzMessages {
		t.Fatalf("authz message budget not bounded: %v", err)
	}
	malformed := &codectypes.Any{TypeUrl: "/cosmos.bank.v1beta1.MsgSend", Value: []byte{0xff}}
	_, err = (&Parser{}).ParseMessage(&authz.MsgExec{Msgs: []*codectypes.Any{malformed}}, nil, cfgForTests())
	if err == nil || !strings.Contains(err.Error(), "decode /cosmos.bank") {
		t.Fatalf("malformed inner not surfaced: %v", err)
	}
}

func TestSafetyAuthzUncachedCustomLegacyInner(t *testing.T) {
	// The SDK's global proto registry does not know our legacy aliases.
	msg := &authz.MsgExec{Msgs: []*codectypes.Any{{TypeUrl: LegacyTypeURLWithdrawAllTokenizeShareReward, Value: fieldBytes(1, del)}}}
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("withdraw_tokenize_share_reward", [2]string{attributeAmount, "17uatom"}, [2]string{attributeAuthzMsgIndex, "0"}),
	}}
	data, err := (&Parser{}).ParseMessage(msg, log, cfgForTests())
	if err != nil {
		t.Fatal(err)
	}
	out := (*data).([]TaxableEvent)
	if len(out) != 1 || out[0].Amount != "17" || out[0].Category != string(CategoryReward) || out[0].ToAddr != del {
		t.Fatalf("uncached legacy inner lost: %+v", out)
	}
}

func TestSafetyUnknownClaimPreservesPrincipalOrIncomeWithoutGuessing(t *testing.T) {
	contract := "unreviewed-contract"
	for _, label := range []string{"principal_return", "income_payment"} {
		t.Run(label, func(t *testing.T) {
			// The same claim and bank evidence is compatible with either
			// meaning. The parser must not choose between them by action name.
			log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
				ev(eventWasm, [2]string{attributeContractAddress, contract}, [2]string{attributeAction, actionClaim}, [2]string{"user", del}),
				ev(eventWasm, [2]string{attributeContractAddress, contract}, [2]string{attributeAction, actionClaim}, [2]string{"user", del}),
				ev(string(CategoryTransfer), [2]string{attributeSender, contract}, [2]string{attributeRecipient, del}, [2]string{attributeAmount, fixtureTenthMilliAtom}, [2]string{attributeSender, contract}, [2]string{attributeRecipient, other}, [2]string{attributeAmount, "50uatom"}),
				ev(string(CategoryTransfer), [2]string{attributeSender, del}, [2]string{attributeRecipient, contract}, [2]string{attributeAmount, "7uatom"}),
				ev(eventCoinReceived, [2]string{attributeReceiver, del}, [2]string{attributeAmount, fixtureTenthMilliAtom}),
			}}
			out := classify(&wasmtypes.MsgExecuteContract{}, log)
			if len(out) != 3 {
				t.Fatalf("unknown claim lost or duplicated transfer legs: %+v", out)
			}
			for _, e := range out {
				if e.Category != string(CategoryTransfer) || e.RewardTrigger != "unknown_claim" || !strings.Contains(e.Detail, `"review_required":"true"`) || !strings.Contains(e.Detail, `"review_reason":"unknown_claim_semantics"`) {
					t.Fatalf("guessed claim semantics: %+v", e)
				}
			}
		})
	}
	// Reviewed fixtures must keep principal and reward meanings unchanged.
	for _, tc := range []struct {
		name     string
		category Category
		trigger  string
	}{
		{"dao-stake", CategoryProtocolDeposit, "dao_stake"},
		{"dao-claim", CategoryProtocolWithdrawal, "dao_unstake"},
		{"dao-rewards-claim", CategoryIncome, "dao_rewards"},
		{"govscore-claim", CategoryIncome, "governance_score"},
	} {
		out := replayFixture(t, tc.name)
		if len(out) != 1 || out[0].Category != string(tc.category) || out[0].RewardTrigger != tc.trigger || strings.Contains(out[0].Detail, "review_required") {
			t.Fatalf("known %s semantics changed: %+v", tc.name, out)
		}
	}
}

func TestSafetyICSAllocationIsNotWalletReward(t *testing.T) {
	// ICS v6.3.0 and v7.0.0 provider/keeper/distribution.go both emit this
	// distributed_ics_rewards shape. Synthetic amounts, verified event keys.
	for _, version := range []string{"v6.3.0", "v7.0.0"} {
		t.Run(version, func(t *testing.T) {
			log := &txlog.LogMessage{Events: append(safetyCredit("provider-module", safetyVoucher),
				ev("distributed_ics_rewards", [2]string{"module", "provider"}, [2]string{"consumer_id", "7"}, [2]string{"consumer_chain_id", "consumer-1"}, [2]string{"total_rewards", "100" + safetyVoucher}, [2]string{"distributed_rewards", "98" + safetyVoucher}, [2]string{"community_pool_rewards", "2" + safetyVoucher}),
				ev("withdraw_rewards", [2]string{attributeDelegator, del}, [2]string{attributeValidator, val}, [2]string{attributeAmount, "25" + safetyVoucher}),
			)}
			out := classify(&dist.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val}, log)
			if len(out) != 1 || out[0].Amount != "25" || out[0].Denom != safetyVoucher || out[0].ToAddr != del || out[0].Category != string(CategoryReward) {
				t.Fatalf("provider accounting duplicated wallet withdrawal: %+v", out)
			}
			log.Events = log.Events[:len(log.Events)-1]
			if out := classify(&dist.MsgWithdrawDelegatorReward{DelegatorAddress: del}, log); len(out) != 0 {
				t.Fatalf("provider allocation became income without withdrawal: %+v", out)
			}
		})
	}
}
