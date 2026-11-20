package tax

import (
	"crypto/sha256"
	"os"
	"strings"
	"testing"

	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"
	indexerEvents "github.com/DefiantLabs/cosmos-indexer/cosmos/events"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	cometAbciTypes "github.com/cometbft/cometbft/abci/types"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/cosmos/cosmos-sdk/x/authz"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
	transfertypes "github.com/cosmos/ibc-go/v7/modules/apps/transfer/types"
)

// testBech is a valid synthetic address of n bytes.
const (
	testChannel = "channel-141"
	nineAtom    = "9uatom"
)

func testBech(t *testing.T, seed string, n int) string {
	t.Helper()
	h := sha256.Sum256([]byte("wasm-supplement-test:" + seed))
	s, err := bech32.ConvertAndEncode("cosmos", h[:n])
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func transferEv(from, to, amount string) txlog.LogMessageEvent {
	return ev(string(CategoryTransfer), [2]string{attributeRecipient, to}, [2]string{attributeSender, from}, [2]string{attributeAmount, amount})
}

func executeLog(contract string, evs ...txlog.LogMessageEvent) *txlog.LogMessage {
	all := []txlog.LogMessageEvent{ev("execute", [2]string{attributeContractAddress, contract})}
	return &txlog.LogMessage{Events: append(all, evs...)}
}

func parseExecute(t *testing.T, sender string, log *txlog.LogMessage) []TaxableEvent {
	t.Helper()
	out, err := (&Parser{}).ParseMessage(&wasmtypes.MsgExecuteContract{Sender: sender}, log, cfgForTests())
	if err != nil {
		t.Fatal(err)
	}
	return (*out).([]TaxableEvent)
}

// Every fixture's supplement rows follow its known-pattern rows, which stay
// exactly what the known patterns write, and no bank fallback row names an
// address and native denom an earlier row of the message already names.
func TestWasmSupplementAppendsAfterKnownRows(t *testing.T) {
	names, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range names {
		n, ok := strings.CutSuffix(f.Name(), ".json")
		if !ok {
			continue
		}
		data, _ := os.ReadFile("testdata/" + f.Name())
		if !strings.Contains(string(data), `"message_type"`) {
			continue
		}
		typ, log := protocolFixture(t, n)
		msg, err := ReconstructMsg(typ, log)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		state := &classificationState{}
		known := classifyWithState(msg, log, state, 1, "")
		got := replayFixture(t, n)
		if summary(got) != summary(append(append([]TaxableEvent{}, known...), state.late...)) {
			t.Fatalf("%s: supplement rows are not after the known rows", n)
		}
		named := map[string]bool{}
		for i, e := range got {
			fallback := strings.Contains(e.Detail, `"fallback":"bank"`)
			if fallback && i < len(known) {
				t.Fatalf("%s: a known row carries the fallback marker", n)
			}
			if fallback {
				wallet := e.ToAddr
				if !excludedContract(log, e.FromAddr) && !fallbackModuleSet[e.FromAddr] && e.FromAddr != firstContract(log) {
					wallet = e.FromAddr
				}
				if named[wallet] {
					t.Fatalf("%s: fallback row for %s, which an earlier row names", n, wallet)
				}
			}
			if e.Denom == fixtureNativeDenom {
				named[e.FromAddr], named[e.ToAddr] = true, true
			}
		}
		checked++
	}
	if checked < 50 {
		t.Fatalf("only %d fixtures checked", checked)
	}
}

func fixtureWith(t *testing.T, name string, edit func(*txlog.LogMessage)) (sdk.Msg, *txlog.LogMessage) {
	t.Helper()
	typ, log := protocolFixture(t, name)
	edit(log)
	msg, err := ReconstructMsg(typ, log)
	if err != nil {
		t.Fatal(err)
	}
	return msg, log
}

func setAttr(log *txlog.LogMessage, evType, key, value string) {
	for i := range log.Events {
		if log.Events[i].Type != evType {
			continue
		}
		for j := range log.Events[i].Attributes {
			if log.Events[i].Attributes[j].Key == key {
				log.Events[i].Attributes[j].Value = value
			}
		}
	}
}

func parseFixtureMsg(t *testing.T, msg sdk.Msg, log *txlog.LogMessage) []TaxableEvent {
	t.Helper()
	out, err := (&Parser{}).ParseMessage(msg, log, cfgForTests())
	if err != nil {
		t.Fatal(err)
	}
	return (*out).([]TaxableEvent)
}

func triggers(rows []TaxableEvent) string {
	var s []string
	for _, e := range rows {
		s = append(s, e.Category+":"+e.RewardTrigger+":"+e.Amount)
	}
	return strings.Join(s, ",")
}

// A bid whose bank leg disagrees with its event price gets no escrow row;
// the bank fallback records the coins that actually moved.
func TestMarketplaceBidPriceMismatchFallsBack(t *testing.T) {
	msg, log := fixtureWith(t, "marketplace-set-bid", func(l *txlog.LogMessage) { setAttr(l, eventSetBid, "price", "14000000uatom") })
	if got := triggers(parseFixtureMsg(t, msg, log)); got != "transfer::15000000" {
		t.Fatalf("mismatched set-bid: %s", got)
	}
	msg, log = fixtureWith(t, "marketplace-update-collection-bid", func(l *txlog.LogMessage) {
		setAttr(l, eventUpdateCollectionBid, "price", "530000uatom")
	})
	// Neither the refund nor the deposit is written; the bidder's net
	// movement (531000 in to the marketplace, 373000 back) is one transfer out.
	if got := triggers(parseFixtureMsg(t, msg, log)); got != "transfer::158000" {
		t.Fatalf("mismatched update-collection-bid: %s", got)
	}
	msg, log = fixtureWith(t, "marketplace-accept-bid", func(l *txlog.LogMessage) { setAttr(l, eventFinalizeSale, "price", "10000001") })
	for _, e := range parseFixtureMsg(t, msg, log) {
		if e.RewardTrigger == triggerBidFilled {
			t.Fatal("bid_filled written although payouts do not equal the price")
		}
	}
}

// An update-bid on a single NFT has the update-collection-bid shape plus a
// token id.
func TestMarketplaceUpdateBid(t *testing.T) {
	msg, log := fixtureWith(t, "marketplace-update-collection-bid", func(l *txlog.LogMessage) {
		for i := range l.Events {
			if l.Events[i].Type == eventUpdateCollectionBid {
				l.Events[i].Type = eventUpdateBid
				l.Events[i].Attributes = append(l.Events[i].Attributes, txlog.Attribute{Key: attrTokenID, Value: "7"})
			}
		}
	})
	rows := parseFixtureMsg(t, msg, log)
	if got := triggers(rows); got != "protocol_withdrawal:auction_refund:373000,protocol_deposit:auction_bid:531000" {
		t.Fatalf("update-bid rows: %s", got)
	}
	if !strings.Contains(rows[1].Detail, `"sale":"bid"`) || !strings.Contains(rows[1].Detail, `"token_id":"7"`) {
		t.Fatalf("update-bid detail: %s", rows[1].Detail)
	}
}

// An accept-ask that names a bid is paid by the buyer in the same message;
// it releases no escrow.
func TestMarketplaceAcceptAskReleasesNoBid(t *testing.T) {
	for _, e := range replayFixture(t, "marketplace-accept-ask-bid-attr") {
		if e.Category == string(CategoryProtocolWithdrawal) {
			t.Fatalf("accept-ask wrote %s", e.RewardTrigger)
		}
	}
}

// The fee is a tx-level event without msg_index, so it never reaches the
// message's log and the fallback cannot count it a second time.
func TestWasmFallbackNeverSeesTheFee(t *testing.T) {
	signer, contract := testBech(t, "signer", 20), testBech(t, "contract", 32)
	attr := func(k, v string) cometAbciTypes.EventAttribute {
		return cometAbciTypes.EventAttribute{Key: k, Value: v}
	}
	events := []cometAbciTypes.Event{
		{Type: "coin_spent", Attributes: []cometAbciTypes.EventAttribute{attr("spender", signer), attr("amount", "5000uatom")}},
		{Type: string(CategoryTransfer), Attributes: []cometAbciTypes.EventAttribute{attr("recipient", feeCollectorAddress), attr("sender", signer), attr("amount", "5000uatom")}},
		{Type: "tx", Attributes: []cometAbciTypes.EventAttribute{attr("fee", "5000uatom"), attr("fee_payer", signer)}},
		{Type: "message", Attributes: []cometAbciTypes.EventAttribute{attr("action", "/cosmwasm.wasm.v1.MsgExecuteContract"), attr("sender", signer), attr("msg_index", "0")}},
		{Type: string(CategoryTransfer), Attributes: []cometAbciTypes.EventAttribute{attr("recipient", contract), attr("sender", signer), attr("amount", "7000uatom"), attr("msg_index", "0")}},
		{Type: "execute", Attributes: []cometAbciTypes.EventAttribute{attr("_contract_address", contract), attr("msg_index", "0")}},
	}
	logs, err := indexerEvents.ParseTxEventsToMessageIndexEvents(1, events)
	if err != nil {
		t.Fatal(err)
	}
	log := &txlog.LogMessage{Events: indexerEvents.StringEventstoNormalizedEvents(logs[0].Events)}
	if got := triggers(parseExecute(t, signer, log)); got != "transfer::7000" {
		t.Fatalf("fee reached the fallback: %s", got)
	}
	// A leg to the fee collector inside a message is skipped all the same.
	log = executeLog(contract, transferEv(signer, contract, "7000uatom"), transferEv(signer, feeCollectorAddress, "5000uatom"))
	if got := triggers(parseExecute(t, signer, log)); got != "transfer::7000" {
		t.Fatalf("fee collector leg counted: %s", got)
	}
}

// A failed transaction's messages carry no events, so they produce no rows.
func TestWasmFailedTxNoRows(t *testing.T) {
	signer := testBech(t, "signer", 20)
	for _, log := range []*txlog.LogMessage{nil, {}} {
		if rows := parseExecute(t, signer, log); len(rows) != 0 {
			t.Fatalf("failed tx produced rows: %+v", rows)
		}
	}
}

// Two executes under one MsgExec: the fallback rows of the first inner go
// after the second inner's rows, so a known row never changes position.
func TestWasmFallbackAuthzKeepsSubIndex(t *testing.T) {
	granter, payee, contract := testBech(t, "granter", 20), testBech(t, "payee", 20), testBech(t, "dao", 32)
	exec := safetyUncached(t, &wasmtypes.MsgExecuteContract{Sender: granter, Contract: contract})
	send := safetyUncached(t, &bank.MsgSend{FromAddress: granter, ToAddress: payee, Amount: sdk.NewCoins(sdk.NewInt64Coin(fixtureNativeDenom, 9))})
	msg := &authz.MsgExec{Grantee: payee, Msgs: []*codectypes.Any{exec, send}}
	idx := func(i string) [2]string { return [2]string{attributeAuthzMsgIndex, i} }
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("execute", [2]string{attributeContractAddress, contract}, idx("0")),
		ev(string(CategoryTransfer), [2]string{attributeRecipient, payee}, [2]string{attributeSender, contract}, [2]string{attributeAmount, "400uatom"}, idx("0")),
		ev(string(CategoryTransfer), [2]string{attributeRecipient, payee}, [2]string{attributeSender, granter}, [2]string{attributeAmount, nineAtom}, idx("1")),
	}}
	withoutExec := summary(classify(&authz.MsgExec{Grantee: payee, Msgs: []*codectypes.Any{send}}, &txlog.LogMessage{}))
	rows := parseFixtureMsg(t, msg, log)
	if len(rows) != 2 || summary(rows[:1]) != withoutExec || rows[1].Amount != "400" || rows[1].ToAddr != payee {
		t.Fatalf("authz rows out of order: %s", summary(rows))
	}
}

// Contracts that ran, module accounts and IBC escrow are never a wallet; a
// 32-byte signer is; and a wallet-to-wallet leg is not seen twice.
func TestWasmFallbackWalletSide(t *testing.T) {
	smart, contract, payee := testBech(t, "smart-account", 32), testBech(t, "contract", 32), testBech(t, "payee", 20)
	log := executeLog(contract, transferEv(smart, contract, "100uatom"), transferEv(contract, moduleAddress("distribution"), "100uatom"))
	if got := summary(parseExecute(t, smart, log)); got != "transfer|100|uatom||"+smart+"|"+contract+"|||"+contractDetail(contract, map[string]string{"fallback": "bank"})+"\n" {
		t.Fatalf("32-byte signer: %s", got)
	}
	// A 32-byte address that is not the signer and did not run is left out.
	other := testBech(t, "other-contract", 32)
	log = executeLog(contract, transferEv(payee, contract, "50uatom"), transferEv(contract, other, "50uatom"))
	if got := triggers(parseExecute(t, payee, log)); got != "transfer::50" {
		t.Fatalf("non-signer 32-byte address: %s", got)
	}
	// One wallet paying another directly: each sees only its own row.
	signer := testBech(t, "signer", 20)
	log = executeLog(contract, transferEv(signer, payee, "30uatom"))
	rows := parseExecute(t, signer, log)
	if len(rows) != 2 {
		t.Fatalf("wallet to wallet: %s", summary(rows))
	}
	for _, e := range rows {
		if (e.FromAddr == signer || e.ToAddr == signer) && (e.FromAddr == payee || e.ToAddr == payee) {
			t.Fatalf("row names both wallets: %s", summary(rows))
		}
	}
	// The IBC escrow of the channel the message sent on is not a wallet.
	escrow := escrowAddress(transfertypes.PortID, testChannel)
	log = executeLog(contract, transferEv(signer, contract, "80uatom"), transferEv(contract, escrow, "80uatom"),
		ev("send_packet", [2]string{"packet_src_port", transfertypes.PortID}, [2]string{"packet_src_channel", testChannel}))
	if got := triggers(parseExecute(t, signer, log)); got != "transfer::80" {
		t.Fatalf("escrow counted: %s", got)
	}
}

func TestFallbackModuleAddresses(t *testing.T) {
	if moduleAddress("distribution") != distributionModuleAddress {
		t.Fatal("module address derivation")
	}
	if feeCollectorAddress != "cosmos17xpfvakm2amg962yls6f84z3kell8c5lserqta" {
		t.Fatal("fee collector address")
	}
}
