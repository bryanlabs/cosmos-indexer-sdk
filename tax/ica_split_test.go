package tax

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/config"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	disttypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/cosmos/gogoproto/proto"
	chantypes "github.com/cosmos/ibc-go/v7/modules/core/04-channel/types"
)

func requireICARewardRows(t *testing.T, out []TaxableEvent, want []rewardRow) {
	t.Helper()
	counts := map[rewardRow]int{}
	for _, r := range want {
		counts[r]++
	}
	got := redeemRewardRows(out)
	for _, r := range got {
		counts[r]--
	}
	for _, n := range counts {
		if n != 0 {
			t.Fatalf("reward identity/amount multiset differs: got %d rows, want %d", len(got), len(want))
		}
	}
}

func icaAny(t *testing.T, m sdk.Msg) *codectypes.Any {
	t.Helper()
	a, err := codectypes.NewAnyWithValue(m)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func icaParse(t *testing.T, msgs []*codectypes.Any, log *txlog.LogMessage) ([]TaxableEvent, error) {
	t.Helper()
	v, err := (&Parser{}).ParseMessage(&chantypes.MsgRecvPacket{Packet: icaPacket(t, msgs...)}, log, config.IndexConfig{})
	if v == nil {
		return nil, err
	}
	return (*v).([]TaxableEvent), err
}

func icaAssertUnresolved(t *testing.T, msgs []*codectypes.Any, log *txlog.LogMessage) {
	t.Helper()
	out, err := icaParse(t, msgs, log)
	var incomplete *ClassificationIncompleteError
	if !errors.As(err, &incomplete) || len(out) != 0 {
		t.Fatalf("want no rows and incomplete classification, got %d rows, error %v", len(out), err)
	}
}

// Retained eight-redeem host packet. All identities, record IDs, denominations,
// channels and amounts are replaced consistently. Event/attribute ordering and
// equalities are retained. No raw packet bytes or wallet identities are stored.
func icaRetainedRedeems(t *testing.T) ([]*codectypes.Any, *txlog.LogMessage) {
	t.Helper()
	raw, err := os.ReadFile("testdata/ica-multi-redeem-legacy-sanitized.json")
	if err != nil {
		t.Fatal(err)
	}
	replace := []string{"ICA_ACCOUNT", icaTestAccount, "DISTRIBUTION", distributionModuleAddress, "BONDED_TOKENS_POOL", authtypes.NewModuleAddress(stakingtypes.BondedPoolName).String(), "NOT_BONDED_TOKENS_POOL", authtypes.NewModuleAddress(stakingtypes.NotBondedPoolName).String(), "LIQUID", authtypes.NewModuleAddress("liquid").String()}
	for i := 0; i < 32; i++ {
		replace = append(replace, "VALIDATOR_"+strconv.Itoa(i)+"/", sdk.ValAddress(bytes.Repeat([]byte{byte(i + 1)}, 20)).String()+"/")
		replace = append(replace, "\"VALIDATOR_"+strconv.Itoa(i)+"\"", "\""+sdk.ValAddress(bytes.Repeat([]byte{byte(i + 1)}, 20)).String()+"\"")
	}
	for i := 100; i < 108; i++ {
		replace = append(replace, "RECORD_"+strconv.Itoa(i), lsmRecordAccount(strconv.Itoa(i)))
	}
	raw = []byte(strings.NewReplacer(replace...).Replace(string(raw)))
	var log txlog.LogMessage
	if err := json.Unmarshal(raw, &log); err != nil {
		t.Fatal(err)
	}
	var msgs []*codectypes.Any
	for _, e := range log.Events {
		if e.Type != string(CategoryRedeemShares) {
			continue
		}
		groups, err := legacyRecords(e.Attributes, attributeDelegator)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range groups {
			coin, err := sdk.ParseCoinNormalized(attrValue(g, attributeAmount))
			if err != nil {
				t.Fatal(err)
			}
			amount, err := coin.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			value := append(protoBytesField(1, []byte(attrValue(g, attributeDelegator))), protoBytesField(2, amount)...)
			msgs = append(msgs, &codectypes.Any{TypeUrl: LegacyTypeURLRedeemTokensForShares, Value: value})
		}
	}
	return msgs, &log
}

func TestICAHostRetainedMergedRedeems(t *testing.T) {
	msgs, log := icaRetainedRedeems(t)
	if len(msgs) != 8 {
		t.Fatal("fixture must contain eight redeems")
	}
	out, err := icaParse(t, msgs, log)
	if err != nil {
		inners := make([]sdk.Msg, len(msgs))
		for i, a := range msgs {
			inners[i], _ = unpackAuthzInner(a)
		}
		_, detail := icaLegacyRedeemSubLogs(inners, log)
		t.Fatalf("retained packet unresolved: %v; split detail: %v", err, detail)
	}
	var want []rewardRow
	for _, e := range log.Events {
		if e.Type != eventWithdrawTokenizeShareReward {
			continue
		}
		groups, err := legacyRecords(e.Attributes, attributeWithdrawAddress)
		if err != nil {
			t.Fatal(err)
		}
		for i, g := range groups {
			inner, err := unpackAuthzInner(msgs[i])
			if err != nil {
				t.Fatal(err)
			}
			validator := shareDenomValidator(inner.(*LegacyMsgRedeemTokensForShares).AmountRaw)
			coins, err := sdk.ParseCoinsNormalized(attrValue(g, attributeAmount))
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range coins {
				want = append(want, rewardRow{validator, c.Amount.String(), c.Denom})
			}
		}
	}
	// Only distribution transfers to an account other than a record are the
	// ICA's own income. Record withdrawals are already in each owner's payout.
	records := map[string]bool{}
	for _, a := range msgs {
		inner, _ := unpackAuthzInner(a)
		_, id := shareDenomRecord(inner.(*LegacyMsgRedeemTokensForShares).AmountRaw)
		records[lsmRecordAccount(id)] = true
	}
	for _, e := range log.Events {
		if e.Type != eventWithdrawRewards {
			continue
		}
		groups, err := rewardAttributeGroups(e)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range groups {
			coins, err := sdk.ParseCoinsNormalized(g[attributeAmount])
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, p := range transferRuns(log, distributionModuleAddress) {
				if !records[p.recipient] && p.coins.IsEqual(coins) {
					found = true
					break
				}
			}
			if found {
				for _, c := range coins {
					want = append(want, rewardRow{g[attributeValidator], c.Amount.String(), c.Denom})
				}
			}
		}
	}
	requireICARewardRows(t, out, want)
	redeems := 0
	for _, e := range out {
		if e.Category == string(CategoryRedeemShares) {
			redeems++
		}
	}
	if redeems != len(msgs) {
		t.Fatalf("got %d redeem rows, want %d", redeems, len(msgs))
	}
	// The splitter neither changes nor consumes the retained evidence.
	before, _ := json.Marshal(log)
	if _, err := icaParse(t, msgs, log); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(log)
	if !bytes.Equal(before, after) {
		t.Fatal("splitter mutated input evidence")
	}
}

func TestICAHostMergedRedeemsKeepOwnAndOwnerIncomeSeparate(t *testing.T) {
	msgs, log := icaRetainedRedeems(t)
	var want []rewardRow
	for i := range log.Events {
		e := &log.Events[i]
		switch e.Type {
		case eventWithdrawRewards:
			for j := range e.Attributes {
				if e.Attributes[j].Key == attributeAmount && e.Attributes[j].Value == "0stake" {
					e.Attributes[j].Value = icaTestFiveUatom
				}
			}
		case string(CategoryTransfer):
			groups, _ := legacyRecords(e.Attributes, attributeRecipient)
			attrs := []txlog.Attribute{}
			for _, g := range groups {
				attrs = append(attrs, g...)
				if attrValue(g, attributeRecipient) == icaTestAccount {
					attrs = append(attrs, txlog.Attribute{Key: attributeRecipient, Value: icaTestWithdraw}, txlog.Attribute{Key: attributeSender, Value: distributionModuleAddress}, txlog.Attribute{Key: attributeAmount, Value: icaTestFiveUatom})
				}
			}
			e.Attributes = attrs
		case eventCoinSpent, eventCoinReceived:
			key, who := attributeSpender, distributionModuleAddress
			if e.Type == eventCoinReceived {
				key, who = attributeReceiver, icaTestWithdraw
			}
			for range msgs {
				e.Attributes = append(e.Attributes, txlog.Attribute{Key: key, Value: who}, txlog.Attribute{Key: attributeAmount, Value: icaTestFiveUatom})
			}
		}
	}
	baselineMsgs, baselineLog := icaRetainedRedeems(t)
	baseline, err := icaParse(t, baselineMsgs, baselineLog)
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, redeemRewardRows(baseline)...)
	for _, a := range msgs {
		m, _ := unpackAuthzInner(a)
		want = append(want, rewardRow{shareDenomValidator(m.(*LegacyMsgRedeemTokensForShares).AmountRaw), "5", fixtureNativeDenom})
	}
	out, err := icaParse(t, msgs, log)
	if err != nil {
		t.Fatal(err)
	}
	requireICARewardRows(t, out, want)
	own := 0
	for _, e := range out {
		if e.Category == string(CategoryReward) && e.Amount == "5" && e.Denom == fixtureNativeDenom {
			if e.ToAddr != icaTestAccount {
				t.Fatal("own reward attributed to record owner or withdraw-address payee")
			}
			own++
		}
	}
	if own != len(msgs) {
		t.Fatalf("own reward count %d, want %d", own, len(msgs))
	}
}

func TestICAHostOrderedRedeemsMatchMergedEvidence(t *testing.T) {
	msgs, log := icaRetainedRedeems(t)
	economic := &txlog.LogMessage{}
	for _, e := range log.Events {
		if e.Type != "ics27_packet" {
			economic.Events = append(economic.Events, e)
		}
	}
	inners := make([]sdk.Msg, len(msgs))
	for i, a := range msgs {
		inners[i], _ = unpackAuthzInner(a)
	}
	subs, err := icaLegacyRedeemSubLogs(inners, economic)
	if err != nil {
		t.Fatal(err)
	}
	ordered := &txlog.LogMessage{Events: []txlog.LogMessageEvent{icaSuccess()}}
	for i, sub := range subs {
		var terminal txlog.LogMessageEvent
		for _, e := range sub.Events {
			if e.Type == string(CategoryRedeemShares) {
				terminal = e
				continue
			}
			ordered.Events = append(ordered.Events, e)
		}
		ordered.Events = append(ordered.Events, terminal)
		// The modern and legacy namespaces share wire fields and economic events.
		msgs[i] = &codectypes.Any{TypeUrl: TypeURLRedeemTokensForShares, Value: msgs[i].Value}
	}
	expected, err := icaParse(t, msgs, log)
	if err != nil {
		t.Fatal(err)
	}
	msgs = append(msgs, icaSendAny(t, other))
	ordered.Events = append(ordered.Events, ev(string(CategoryTransfer), [2]string{attributeRecipient, other}, [2]string{attributeSender, icaTestAccount}, [2]string{attributeAmount, icaTestSevenUatom}))
	out, err := icaParse(t, msgs, ordered)
	if err != nil {
		t.Fatal(err)
	}
	requireICARewardRows(t, out, redeemRewardRows(expected))
	if len(out) != len(expected)+1 {
		t.Fatalf("ordered/merged row count differs: %d versus %d", len(out), len(expected))
	}
}

func TestICAHostContradictoryExecutionEvidenceFailsClosed(t *testing.T) {
	msgs := []*codectypes.Any{icaSendAny(t, other), icaSendAny(t, del)}
	icaAssertUnresolved(t, msgs, &txlog.LogMessage{Events: []txlog.LogMessageEvent{icaSuccess(), ev(eventWriteAcknowledgement, [2]string{attributePacketAckHex, hex32(`{"error":"execution reverted"}`)})}})
	icaAssertUnresolved(t, msgs, &txlog.LogMessage{Events: []txlog.LogMessageEvent{icaSuccess(), ev("ics27_packet", [2]string{attributeSuccess, "false"})}})
}

func TestICAHostMergedRedeemEvidenceCorruptionFailsClosed(t *testing.T) {
	for _, kind := range []string{string(CategoryRedeemShares), eventWithdrawRewards, eventWithdrawTokenizeShareReward, string(CategoryTransfer), eventCoinSpent, eventCoinReceived, eventBurn} {
		t.Run(kind, func(t *testing.T) {
			msgs, log := icaRetainedRedeems(t)
			for i := range log.Events {
				if log.Events[i].Type != kind {
					continue
				}
				for j := range log.Events[i].Attributes {
					if log.Events[i].Attributes[j].Key == attributeAmount {
						log.Events[i].Attributes[j].Value = "999999uatom"
						break
					}
				}
				break
			}
			icaAssertUnresolved(t, msgs, log)
		})
	}
	msgs, log := icaRetainedRedeems(t)
	msgs[1] = msgs[0]
	icaAssertUnresolved(t, msgs, log)
	msgs, log = icaRetainedRedeems(t)
	log.Events = append(log.Events, ev("unknown_economic_event", [2]string{attributeAmount, "19uatom"}))
	icaAssertUnresolved(t, msgs, log)
}

func TestICAHostOrderedWithdrawalsAndSend(t *testing.T) {
	msgs := []*codectypes.Any{
		icaAny(t, &disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: icaTestAccount, ValidatorAddress: lsmTestValoperA}),
		icaAny(t, &disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: icaTestAccount, ValidatorAddress: lsmTestValoperB}),
		icaSendAny(t, other),
	}
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		icaSuccess(),
		ev(string(CategoryTransfer), [2]string{attributeRecipient, icaTestWithdraw}, [2]string{attributeSender, distributionModuleAddress}, [2]string{attributeAmount, "11uatom,13" + lsmTestIBCDenom}),
		ev(eventWithdrawRewards, [2]string{attributeAmount, "11uatom,13" + lsmTestIBCDenom}, [2]string{attributeValidator, lsmTestValoperA}, [2]string{attributeDelegator, icaTestAccount}),
		ev(string(CategoryTransfer), [2]string{attributeRecipient, icaTestWithdraw}, [2]string{attributeSender, distributionModuleAddress}, [2]string{attributeAmount, icaTestSeventeenUatom}),
		ev(eventWithdrawRewards, [2]string{attributeAmount, icaTestSeventeenUatom}, [2]string{attributeValidator, lsmTestValoperB}, [2]string{attributeDelegator, icaTestAccount}),
		ev(string(CategoryTransfer), [2]string{attributeRecipient, other}, [2]string{attributeSender, icaTestAccount}, [2]string{attributeAmount, icaTestSevenUatom}),
	}}
	out, err := icaParse(t, msgs, log)
	if err != nil {
		t.Fatal(err)
	}
	requireICARewardRows(t, out, []rewardRow{{lsmTestValoperA, "11", fixtureNativeDenom}, {lsmTestValoperA, "13", lsmTestIBCDenom}, {lsmTestValoperB, "17", fixtureNativeDenom}})
	if len(out) != 4 {
		t.Fatalf("got %d rows", len(out))
	}
	// A terminal missing from the first withdrawal must not leak its income to
	// the next inner. An extra transfer must not be swallowed by the send.
	malformed := *log
	malformed.Events = append([]txlog.LogMessageEvent(nil), log.Events...)
	malformed.Events = append(malformed.Events[:2], malformed.Events[3:]...)
	icaAssertUnresolved(t, msgs, &malformed)
	log.Events = append(log.Events, ev(string(CategoryTransfer), [2]string{attributeRecipient, other}, [2]string{attributeSender, icaTestAccount}, [2]string{attributeAmount, "23uatom"}))
	icaAssertUnresolved(t, msgs, log)
}

func TestICAHostLegacyDelegationAutoRewards(t *testing.T) {
	msgs := []*codectypes.Any{
		icaAny(t, &stakingtypes.MsgDelegate{DelegatorAddress: icaTestAccount, ValidatorAddress: lsmTestValoperA, Amount: sdk.NewInt64Coin(fixtureNativeDenom, 31)}),
		icaAny(t, &stakingtypes.MsgDelegate{DelegatorAddress: icaTestAccount, ValidatorAddress: lsmTestValoperB, Amount: sdk.NewInt64Coin(fixtureNativeDenom, 37)}),
	}
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		icaSuccess(),
		ev(eventMessage, [2]string{attributeAction, typeURLIBCRecvPacket}, [2]string{attributeSender, other}, [2]string{attributeModule, moduleIBCChannel},
			[2]string{attributeSender, distributionModuleAddress}, [2]string{attributeSender, icaTestAccount}, [2]string{attributeModule, moduleStaking}, [2]string{attributeSender, icaTestAccount},
			[2]string{attributeSender, distributionModuleAddress}, [2]string{attributeSender, icaTestAccount}, [2]string{attributeModule, moduleStaking}, [2]string{attributeSender, icaTestAccount}, [2]string{attributeModule, moduleIBCChannel}),
		ev(string(CategoryTransfer),
			[2]string{attributeRecipient, icaTestWithdraw}, [2]string{attributeSender, distributionModuleAddress}, [2]string{attributeAmount, icaTestElevenUatom},
			[2]string{attributeRecipient, "cosmos1syntheticpool"}, [2]string{attributeSender, icaTestAccount}, [2]string{attributeAmount, icaTestThirtyOneUatom},
			[2]string{attributeRecipient, icaTestWithdraw}, [2]string{attributeSender, distributionModuleAddress}, [2]string{attributeAmount, icaTestThirteenUatom},
			[2]string{attributeRecipient, "cosmos1syntheticpool"}, [2]string{attributeSender, icaTestAccount}, [2]string{attributeAmount, "37uatom"}),
		ev(eventWithdrawRewards, [2]string{attributeAmount, icaTestElevenUatom}, [2]string{attributeValidator, lsmTestValoperA}, [2]string{attributeAmount, icaTestThirteenUatom}, [2]string{attributeValidator, lsmTestValoperB}),
		ev(string(CategoryDelegate), [2]string{attributeValidator, lsmTestValoperA}, [2]string{attributeAmount, icaTestThirtyOneUatom}, [2]string{attributeValidator, lsmTestValoperB}, [2]string{attributeAmount, "37uatom"}),
	}}
	out, err := icaParse(t, msgs, log)
	if err != nil {
		t.Fatal(err)
	}
	requireICARewardRows(t, out, []rewardRow{{lsmTestValoperA, "11", fixtureNativeDenom}, {lsmTestValoperB, "13", fixtureNativeDenom}})
	wrapped := []*codectypes.Any{icaAny(t, &authz.MsgExec{Grantee: icaTestAccount, Msgs: []*codectypes.Any{msgs[0]}}), msgs[1]}
	// The relayer can equal an inner signer. Remove one positional outer
	// sender only, never all attributes with the same address value.
	sameSigner := &txlog.LogMessage{Events: append([]txlog.LogMessageEvent(nil), log.Events...)}
	for i, e := range sameSigner.Events {
		if e.Type == eventMessage {
			sameSigner.Events[i].Attributes = append([]txlog.Attribute(nil), e.Attributes...)
			sameSigner.Events[i].Attributes[1].Value = icaTestAccount
		}
	}
	if sameOut, err := icaParse(t, wrapped, sameSigner); err != nil {
		t.Fatal(err)
	} else {
		requireICARewardRows(t, sameOut, redeemRewardRows(out))
	}
	wrappedOut, err := icaParse(t, wrapped, log)
	if err != nil {
		t.Fatal(err)
	}
	requireICARewardRows(t, wrappedOut, redeemRewardRows(out))
	zeroLog := &txlog.LogMessage{}
	for _, e := range log.Events {
		attrs := []txlog.Attribute{}
		if e.Type == string(CategoryTransfer) {
			groups, _ := legacyRecords(e.Attributes, attributeRecipient)
			for _, g := range groups {
				if attrValue(g, attributeSender) != distributionModuleAddress {
					attrs = append(attrs, g...)
				}
			}
		} else {
			for _, a := range e.Attributes {
				if e.Type == eventMessage && a.Key == attributeSender && a.Value == distributionModuleAddress {
					continue
				}
				if e.Type == eventWithdrawRewards && a.Key == attributeAmount {
					a.Value = "0stake"
				}
				attrs = append(attrs, a)
			}
		}
		zeroLog.Events = append(zeroLog.Events, txlog.LogMessageEvent{Type: e.Type, Attributes: attrs})
	}
	zeroOut, err := icaParse(t, wrapped, zeroLog)
	if err != nil {
		t.Fatal(err)
	}
	requireICARewardRows(t, zeroOut, nil)
	// An extra inner signer must survive outer-marker stripping and veto
	// both the direct staking and legacy Exec boundary proofs.
	bad := &txlog.LogMessage{Events: append([]txlog.LogMessageEvent(nil), log.Events...)}
	for i, e := range bad.Events {
		if e.Type == eventMessage {
			bad.Events[i].Attributes = append(append([]txlog.Attribute(nil), e.Attributes...), txlog.Attribute{Key: attributeSender, Value: icaTestAccount})
		}
	}
	icaAssertUnresolved(t, msgs, bad)
	icaAssertUnresolved(t, wrapped, bad)
	log.Events = append(log.Events, ev(eventWithdrawRewards, [2]string{attributeAmount, "19uatom"}, [2]string{attributeValidator, lsmTestValoperA}))
	icaAssertUnresolved(t, msgs, log)
}

func TestICAHostIndexedExecWithSendSibling(t *testing.T) {
	leafA := icaAny(t, &disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: icaTestAccount, ValidatorAddress: lsmTestValoperA})
	leafB := icaAny(t, &disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: icaTestAccount, ValidatorAddress: lsmTestValoperB})
	nested := icaAny(t, &authz.MsgExec{Grantee: icaTestAccount, Msgs: []*codectypes.Any{leafB}})
	exec := icaAny(t, &authz.MsgExec{Grantee: icaTestAccount, Msgs: []*codectypes.Any{leafA, nested}})
	msgs := []*codectypes.Any{exec, icaSendAny(t, other)}
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		icaSuccess(),
		ev(string(CategoryTransfer),
			[2]string{attributeRecipient, icaTestWithdraw}, [2]string{attributeSender, distributionModuleAddress}, [2]string{attributeAmount, icaTestElevenUatom}, [2]string{attributeAuthzMsgIndex, "0"},
			[2]string{attributeRecipient, other}, [2]string{attributeSender, icaTestAccount}, [2]string{attributeAmount, icaTestSevenUatom},
			[2]string{attributeRecipient, icaTestWithdraw}, [2]string{attributeSender, distributionModuleAddress}, [2]string{attributeAmount, icaTestThirteenUatom}, [2]string{attributeAuthzMsgIndex, "0"}, [2]string{attributeAuthzMsgIndex, "1"}),
		ev(eventWithdrawRewards,
			[2]string{attributeAmount, icaTestElevenUatom}, [2]string{attributeValidator, lsmTestValoperA}, [2]string{attributeAuthzMsgIndex, "0"},
			[2]string{attributeAmount, icaTestThirteenUatom}, [2]string{attributeValidator, lsmTestValoperB}, [2]string{attributeAuthzMsgIndex, "0"}, [2]string{attributeAuthzMsgIndex, "1"}),
	}}
	out, err := icaParse(t, msgs, log)
	if err != nil {
		t.Fatal(err)
	}
	requireICARewardRows(t, out, []rewardRow{{lsmTestValoperA, "11", fixtureNativeDenom}, {lsmTestValoperB, "13", fixtureNativeDenom}})
	if len(out) != 3 {
		t.Fatalf("expected two rewards and one sibling send, got %d rows", len(out))
	}
	// Reused indices cannot identify which of two outer Execs emitted a record.
	icaAssertUnresolved(t, []*codectypes.Any{exec, exec}, log)
	// An untagged reward belongs to no body-only sibling.
	log.Events = append(log.Events, ev(eventWithdrawRewards, [2]string{attributeAmount, icaTestSeventeenUatom}, [2]string{attributeValidator, lsmTestValoperA}))
	icaAssertUnresolved(t, msgs, log)
}

func TestICAHostBodyOnlyBatchesRejectUnassignedIncome(t *testing.T) {
	msgs := []*codectypes.Any{icaSendAny(t, other), icaSendAny(t, del)}
	noEffect := icaAny(t, &disttypes.MsgSetWithdrawAddress{DelegatorAddress: icaTestAccount, WithdrawAddress: icaTestWithdraw})
	independent, err := icaParse(t, []*codectypes.Any{msgs[0], noEffect}, &txlog.LogMessage{Events: []txlog.LogMessageEvent{icaSuccess()}})
	if err != nil || len(independent) != 1 {
		t.Fatalf("event-independent/no-effect batch: %d rows, error %v", len(independent), err)
	}
	icaAssertUnresolved(t, msgs, &txlog.LogMessage{Events: []txlog.LogMessageEvent{icaSuccess(), ev(eventWithdrawRewards, [2]string{attributeAmount, icaTestElevenUatom}, [2]string{attributeValidator, lsmTestValoperA})}})
	// MultiSend remains body-only when the committed packet has no inner log.
	m := &banktypes.MsgMultiSend{Inputs: []banktypes.Input{{Address: icaTestAccount, Coins: sdk.NewCoins(sdk.NewInt64Coin(fixtureNativeDenom, 7))}}, Outputs: []banktypes.Output{{Address: other, Coins: sdk.NewCoins(sdk.NewInt64Coin(fixtureNativeDenom, 7))}}}
	a := icaAny(t, m)
	// Round-trip so cached Any values cannot conceal wire decoding problems.
	raw, err := proto.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	a = &codectypes.Any{}
	if err := proto.Unmarshal(raw, a); err != nil {
		t.Fatal(err)
	}
	out, err := icaParse(t, []*codectypes.Any{a, icaSendAny(t, del)}, &txlog.LogMessage{Events: []txlog.LogMessageEvent{icaSuccess()}})
	if err != nil || len(out) != 2 {
		t.Fatalf("body-only batch: %d rows, error %v", len(out), err)
	}
}
