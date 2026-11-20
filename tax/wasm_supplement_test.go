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

// A bid whose bank leg disagrees with its event price still escrows the
// coins that moved, with the price noted, so a later fill or refund
// releases them instead of the bidder's liquid coins.
func TestMarketplaceBidPriceMismatchEscrowsLeg(t *testing.T) {
	msg, log := fixtureWith(t, "marketplace-set-bid", func(l *txlog.LogMessage) { setAttr(l, eventSetBid, "price", "14000000uatom") })
	rows := parseFixtureMsg(t, msg, log)
	if got := triggers(rows); got != "protocol_deposit:auction_bid:15000000" {
		t.Fatalf("mismatched set-bid: %s", got)
	}
	if !strings.Contains(rows[len(rows)-1].Detail, `"price":"14000000uatom"`) {
		t.Fatalf("price not noted: %s", rows[len(rows)-1].Detail)
	}
	msg, log = fixtureWith(t, "marketplace-update-collection-bid", func(l *txlog.LogMessage) {
		setAttr(l, eventUpdateCollectionBid, "price", "530000uatom")
	})
	rows = parseFixtureMsg(t, msg, log)
	if got := triggers(rows); got != "protocol_withdrawal:auction_refund:373000,protocol_deposit:auction_bid:531000" {
		t.Fatalf("mismatched update-collection-bid: %s", got)
	}
	if !strings.Contains(rows[len(rows)-1].Detail, `"price":"530000uatom"`) {
		t.Fatalf("price not noted: %s", rows[len(rows)-1].Detail)
	}
}

// An accepted bid releases the escrow at the sale price even when the
// marketplace's payouts do not add up to it (a burned or module-routed fee,
// a leg the log split), so the buyer's sale is paid from the bid and the
// escrow never stays open. The payouts are noted on the row.
func TestMarketplaceAcceptBidPayoutMismatchStillFills(t *testing.T) {
	fill := func(rows []TaxableEvent) TaxableEvent {
		t.Helper()
		var out []TaxableEvent
		for _, e := range rows {
			if e.RewardTrigger == triggerBidFilled {
				out = append(out, e)
			}
		}
		if len(out) != 1 {
			t.Fatalf("want one bid_filled: %s", summary(rows))
		}
		return out[0]
	}
	base := fill(replayFixture(t, "marketplace-accept-bid"))
	if strings.Contains(base.Detail, "payouts") {
		t.Fatalf("matching payouts noted: %s", base.Detail)
	}
	msg, log := fixtureWith(t, "marketplace-accept-bid", func(l *txlog.LogMessage) { setAttr(l, eventFinalizeSale, "price", "10000001") })
	got := fill(parseFixtureMsg(t, msg, log))
	if got.Amount != "10000001" || got.ToAddr != base.ToAddr || !strings.Contains(got.Detail, `"payouts":"10000000uatom"`) {
		t.Fatalf("mismatched price fill: %s", summary([]TaxableEvent{got}))
	}
	// The marketplace's last payout leg is missing, as when a fee is burned.
	msg, log = fixtureWith(t, "marketplace-accept-bid", func(l *txlog.LogMessage) {
		last := -1
		for i, e := range l.Events {
			if e.Type == eventFinalizeSale {
				last = -2
			}
			if last != -1 && e.Type == string(CategoryTransfer) && attrMap(e)[attributeSender] == base.FromAddr {
				last = i
			}
		}
		l.Events = append(l.Events[:last], l.Events[last+1:]...)
	})
	got = fill(parseFixtureMsg(t, msg, log))
	if got.Amount != base.Amount || !strings.Contains(got.Detail, `"payouts":`) {
		t.Fatalf("missing leg fill: %s", summary([]TaxableEvent{got}))
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
		{Type: string(CategoryTransfer), Attributes: []cometAbciTypes.EventAttribute{attr("recipient", moduleAddress("fee_collector")), attr("sender", signer), attr("amount", "5000uatom")}},
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
	// A leg to either fee collector inside a message is skipped all the same.
	for _, collector := range []string{"fee_collector", "feemarket-fee-collector"} {
		log = executeLog(contract, transferEv(signer, contract, "7000uatom"), transferEv(signer, moduleAddress(collector), "5000uatom"))
		if got := triggers(parseExecute(t, signer, log)); got != "transfer::7000" {
			t.Fatalf("%s leg counted: %s", collector, got)
		}
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
	if !feeCollectors["cosmos17xpfvakm2amg962yls6f84z3kell8c5lserqta"] || len(feeCollectors) != 2 {
		t.Fatal("fee collector address")
	}
}

// flatten merges every event of a type into one, in first-seen order, the
// way pre-0.50 logs stored a message's events.
func flatten(log *txlog.LogMessage) *txlog.LogMessage {
	var order []string
	merged := map[string]*txlog.LogMessageEvent{}
	for _, e := range log.Events {
		m := merged[e.Type]
		if m == nil {
			m = &txlog.LogMessageEvent{Type: e.Type}
			merged[e.Type] = m
			order = append(order, e.Type)
		}
		m.Attributes = append(m.Attributes, e.Attributes...)
	}
	out := &txlog.LogMessage{}
	for _, t := range order {
		out.Events = append(out.Events, *merged[t])
	}
	return out
}

// A flattened log loses the order of bank legs and contract events and can
// list a contract as a message sender, so it gets no marketplace, TLD or
// denom fee rows, and its fallback rows name only 20-byte wallets, never a
// contract that ran.
func TestWasmSupplementFlattenedLog(t *testing.T) {
	flattened := 0
	for _, n := range []string{"marketplace-set-bid", "marketplace-accept-bid", "marketplace-update-collection-bid", "marketplace-remove-bid", "names-register-tld", "wasm-launchpad-create-denom", "wasm-instantiate-create-denom", "wasm-dao-proposal-payout"} {
		typ, log := protocolFixture(t, n)
		if flattenedLog(log) {
			t.Fatalf("%s: unflattened fixture detected as flattened", n)
		}
		flat := flatten(log)
		msg, err := ReconstructMsg(typ, log)
		if err != nil {
			t.Fatal(err)
		}
		state := &classificationState{}
		classifyWithState(msg, flat, state, 1, "")
		late := state.late
		if !flattenedLog(flat) {
			// One contract, one leg and one sender: merging changed
			// nothing the rules read, so the rows are the same.
			orig := &classificationState{}
			classifyWithState(msg, log, orig, 1, "")
			if summary(late) != summary(orig.late) {
				t.Fatalf("%s: undetected flattening changed the rows", n)
			}
			continue
		}
		flattened++
		contracts := fallbackExcluded(log)
		for _, e := range late {
			switch e.RewardTrigger {
			case flowTriggerAuctionBid, flowTriggerAuctionRefund, triggerBidFilled, triggerNameRegistration, triggerDenomCreationFee:
				t.Fatalf("%s: flattened log wrote %s", n, e.RewardTrigger)
			}
			if !strings.Contains(e.Detail, `"fallback":"bank"`) {
				t.Fatalf("%s: non-fallback supplement row %s", n, summary([]TaxableEvent{e}))
			}
			for _, a := range []string{e.FromAddr, e.ToAddr} {
				if contracts[a] && a != firstContract(log) && !excludedContract(log, a) {
					t.Fatalf("%s: module named", n)
				}
				_, bz, _ := bech32.DecodeAndConvert(a)
				if !contracts[a] && len(bz) != 20 {
					t.Fatalf("%s: non-wallet %d-byte side in %s", n, len(bz), summary([]TaxableEvent{e}))
				}
			}
		}
	}
	if flattened < 5 {
		t.Fatalf("only %d fixtures flattened", flattened)
	}
}

// The TLD registration expense is what the signer paid the TLD contract,
// and is not written when the payment went elsewhere or a row already names
// it.
func TestTLDRegistrationNegative(t *testing.T) {
	got := replayFixture(t, "names-register-tld")
	if got[0].RewardTrigger != triggerNameRegistration {
		t.Fatalf("fixture: %s", summary(got))
	}
	tld, signer := got[0].ToAddr, got[0].FromAddr
	elsewhere := testBech(t, "elsewhere", 32)
	msg, log := fixtureWith(t, "names-register-tld", func(l *txlog.LogMessage) {
		for i, e := range l.Events {
			a := attrMap(e)
			if e.Type == string(CategoryTransfer) && a[attributeSender] == signer && a[attributeRecipient] == tld {
				setAttr(&txlog.LogMessage{Events: l.Events[i : i+1]}, string(CategoryTransfer), attributeRecipient, elsewhere)
			}
		}
	})
	for _, e := range parseFixtureMsg(t, msg, log) {
		if e.RewardTrigger == triggerNameRegistration {
			t.Fatalf("payment to another contract booked as registration: %s", summary([]TaxableEvent{e}))
		}
	}
	// A known row naming the signer's ATOM keeps the rule out.
	known := []TaxableEvent{{Category: string(CategoryTransfer), FromAddr: signer, ToAddr: tld, Amount: "2500000", Denom: fixtureNativeDenom}}
	_, log = protocolFixture(t, "names-register-tld")
	if rows := tldRegistrationEvents(log, signer, known); len(rows) != 0 {
		t.Fatalf("duplicate registration: %s", summary(rows))
	}
}

// The denom creation fee is written only when the creating contract forwards
// exactly what the signer paid it to the distribution module.
func TestForwardedDenomFeeNegative(t *testing.T) {
	got := replayFixture(t, "wasm-launchpad-create-denom")
	if got[0].RewardTrigger != triggerDenomCreationFee {
		t.Fatalf("fixture: %s", summary(got))
	}
	cases := map[string]func(*txlog.LogMessage){
		"forward differs": func(l *txlog.LogMessage) {
			for i, e := range l.Events {
				if e.Type == string(CategoryTransfer) && attrMap(e)[attributeRecipient] == distributionModuleAddress {
					setAttr(&txlog.LogMessage{Events: l.Events[i : i+1]}, string(CategoryTransfer), attributeAmount, "9000000uatom")
				}
			}
		},
		"not to distribution": func(l *txlog.LogMessage) {
			for i, e := range l.Events {
				if e.Type == string(CategoryTransfer) && attrMap(e)[attributeRecipient] == distributionModuleAddress {
					setAttr(&txlog.LogMessage{Events: l.Events[i : i+1]}, string(CategoryTransfer), attributeRecipient, testBech(t, "treasury", 20))
				}
			}
		},
		"forwarder created no denom": func(l *txlog.LogMessage) {
			setAttr(l, "create_denom", "creator", testBech(t, "other-creator", 32))
		},
	}
	for name, edit := range cases {
		msg, log := fixtureWith(t, "wasm-launchpad-create-denom", edit)
		for _, e := range parseFixtureMsg(t, msg, log) {
			if e.RewardTrigger == triggerDenomCreationFee {
				t.Fatalf("%s: fee written: %s", name, summary([]TaxableEvent{e}))
			}
		}
	}
}

// The rows the known patterns write are exactly what the parser at 4e7f529
// wrote for every fixture (testdata/base-4e7f529, dumped from that commit),
// so the supplement only ever adds rows after them.
func TestKnownRowsEqualBaseParser(t *testing.T) {
	names, err := os.ReadDir("testdata/base-4e7f529")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 60 {
		t.Fatalf("only %d base dumps", len(names))
	}
	for _, f := range names {
		n := strings.TrimSuffix(f.Name(), ".txt")
		want, err := os.ReadFile("testdata/base-4e7f529/" + f.Name())
		if err != nil {
			t.Fatal(err)
		}
		typ, log := protocolFixture(t, n)
		msg, err := ReconstructMsg(typ, log)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		known := classifyWithState(msg, log, &classificationState{}, 1, "")
		if got := summary(known); got != string(want) {
			t.Fatalf("%s: known rows differ from the 4e7f529 parser\ngot:\n%s\nwant:\n%s", n, got, want)
		}
	}
}

// A set-ask whose creator pays the marketplace a fee it forwards whole is a
// listing fee expense, inside a MsgExec too; any other coin movement leaves
// the message to the bank fallback.
func TestAskListingFee(t *testing.T) {
	market, seller, feeTo := testBech(t, "market", 32), testBech(t, "seller", 20), testBech(t, "fee-manager", 32)
	setAsk := ev(eventSetAsk, [2]string{attributeContractAddress, market}, [2]string{"id", "a1"}, [2]string{"creator", seller}, [2]string{"price", "1000000uatom"})
	build := func(legs ...txlog.LogMessageEvent) *txlog.LogMessage {
		evs := []txlog.LogMessageEvent{legs[0], setAsk}
		return executeLog(market, append(evs, legs[1:]...)...)
	}
	log := build(transferEv(seller, market, "1000uatom"), transferEv(market, feeTo, "1000uatom"))
	got := parseExecute(t, seller, log)
	if triggers(got) != "expense:listing_fee:1000" || got[0].FromAddr != seller || got[0].ToAddr != feeTo || !strings.Contains(got[0].Detail, `"ask_id":"a1"`) {
		t.Fatalf("listing fee: %s", summary(got))
	}
	if sup := WasmSupplementForLog(log, ""); triggers(sup) != "expense:listing_fee:1000" {
		t.Fatalf("signer-free: %s", summary(sup))
	}
	for name, l := range map[string]*txlog.LogMessage{
		"forward differs": build(transferEv(seller, market, "1000uatom"), transferEv(market, feeTo, "999uatom")),
		"back to seller":  build(transferEv(seller, market, "1000uatom"), transferEv(market, seller, "1000uatom")),
		"extra leg":       build(transferEv(seller, market, "1000uatom"), transferEv(market, feeTo, "1000uatom"), transferEv(market, seller, "5uatom")),
		"no forward":      build(transferEv(seller, market, "1000uatom")),
	} {
		for _, e := range parseExecute(t, seller, l) {
			if e.RewardTrigger == triggerListingFee {
				t.Fatalf("%s: fee written: %s", name, summary([]TaxableEvent{e}))
			}
		}
	}
	other := testBech(t, "other-signer", 20)
	for _, e := range parseExecute(t, other, log) {
		if e.RewardTrigger == triggerListingFee {
			t.Fatalf("other signer: fee written")
		}
	}
}

// Pool shares the log does not show reaching the receiver (a pool that locks
// them) leave the coins as a deposit in that pool; a withdrawal whose shares
// are not sent back and burned brings coins back as a withdrawal.
func TestLiquidityPoolShareLegUnreadable(t *testing.T) {
	setOn := func(l *txlog.LogMessage, action, key, value string) {
		for i, e := range l.Events {
			if e.Type == eventWasm && attrMap(e)[attributeAction] == action {
				setAttr(&txlog.LogMessage{Events: l.Events[i : i+1]}, eventWasm, key, value)
			}
		}
	}
	msg, log := fixtureWith(t, "wasm-pool-provide", func(l *txlog.LogMessage) { setOn(l, "mint", "to", testBech(t, "pool-lock", 32)) })
	rows := parseFixtureMsg(t, msg, log)
	if got := triggers(rows); got != "protocol_deposit:lp_provide:5235000000000,protocol_deposit:lp_provide:605000000" {
		t.Fatalf("locked provide: %s", got)
	}
	msg, log = fixtureWith(t, "wasm-pool-withdraw", func(l *txlog.LogMessage) { setOn(l, "burn", attributeAmount, "1") })
	rows = parseFixtureMsg(t, msg, log)
	if got := triggers(rows); got != "protocol_withdrawal:lp_withdraw:6889706122386,protocol_withdrawal:lp_withdraw:997921148" {
		t.Fatalf("unburned withdraw: %s", got)
	}
}

// A subscriber taking a cancelled stream's subscription back gets the coins
// as a refund of the subscription, the same deposit the wallet still owned.
// No such message exists on cosmoshub-4 yet; the shape is a withdraw's.
func TestStreamswapExitCancelledRefunds(t *testing.T) {
	msg, log := fixtureWith(t, "wasm-streamswap-withdraw", func(l *txlog.LogMessage) {
		for i, e := range l.Events {
			if e.Type == eventWasm && attrMap(e)[attributeAction] == "withdraw" {
				setAttr(&txlog.LogMessage{Events: l.Events[i : i+1]}, eventWasm, attributeAction, streamExitCancelled)
			}
		}
	})
	rows := parseFixtureMsg(t, msg, log)
	if got := triggers(rows); got != "protocol_withdrawal:stream_refund:150000000" {
		t.Fatalf("exit cancelled: %s", got)
	}
	if supplement, fallback := SupplementRow(rows[0]); !supplement || fallback {
		t.Fatal("stream refund must count as a supplement row")
	}
}
