package tax

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
)

func decidedLine(category, amount, denom, from, to, trigger, contract string, extra map[string]string) string {
	return strings.Join([]string{category, amount, denom, "", from, to, "", trigger, contractDetail(contract, extra)}, "|") + "\n"
}

// registerSkipEntry marks a contract as the Skip Go entry point (code 127)
// by node evidence, for one test.
func registerSkipEntry(t *testing.T, addr string) {
	t.Helper()
	fallbackEvidence[addr] = struct {
		codeID uint64
		label  string
	}{codeSkipEntryPoint, "test entry point"}
	t.Cleanup(func() { delete(fallbackEvidence, addr) })
}

func skipBridgeLog(signer, entry, adapter, feeReceiver, receiver, denom, trace string, paid, fee, sent string) *txlog.LogMessage {
	evs := []txlog.LogMessageEvent{transferEv(signer, entry, paid+denom)}
	dispatch := [][2]string{{attributeContractAddress, entry}, {attributeAction, actionDispatchIBCTransfer}}
	if fee != "" {
		dispatch = append(dispatch, [2]string{attrEurekaFeeAmount, fee}, [2]string{attrEurekaFeeDenom, denom}, [2]string{attrEurekaFeeReceiver, feeReceiver})
	}
	evs = append(evs, ev(eventWasm, dispatch...))
	if fee != "" {
		evs = append(evs, transferEv(entry, feeReceiver, fee+denom))
	}
	evs = append(evs,
		transferEv(entry, adapter, sent+denom),
		ev(eventWasm, [2]string{attributeContractAddress, adapter}, [2]string{attributeAction, "execute_ibc_transfer"}),
		ev("ibc_transfer", [2]string{attributeSender, adapter}, [2]string{attributeReceiver, receiver}, [2]string{attributeDenom, trace}, [2]string{attributeAmount, sent}),
		ev("send_packet", [2]string{attributePacketSourceClient, "client-0"}))
	return executeLog(entry, evs...)
}

// A coin a wallet bridges out through the Skip Go entry point is its ibc_out
// to the receiver the adapter's transfer names, and the fee the entry point
// pays its fee receiver is the wallet's expense, after every other row.
func TestSkipBridgeRows(t *testing.T) {
	signer := testBech(t, "signer", 20)
	entry, adapter, feeReceiver := testBech(t, "entry", 32), testBech(t, "adapter", 32), testBech(t, "feercv", 32)
	registerSkipEntry(t, entry)
	registerKind(t, adapter, contractRoleRouter, true)
	const trace, receiver = "transfer/channel-0/uatom", "0x00000000000000000000000000000000000000aa"
	sum := sha256.Sum256([]byte(trace))
	denom := "ibc/" + strings.ToUpper(hex.EncodeToString(sum[:]))
	bridged := func(fee string) map[string]string {
		m := map[string]string{detailFallback: fallbackBank, detailDestinationReceiver: receiver, detailRoute: "ibc_eureka"}
		if fee != "" {
			m[detailBridgeFee] = fee
		}
		return m
	}
	flagged := func(amount string) string {
		return fallbackLine(string(CategoryTransfer), amount, denom, signer, entry, entry, true)
	}
	cases := []struct {
		name string
		log  *txlog.LogMessage
		want string
	}{
		{"with a fee", skipBridgeLog(signer, entry, adapter, feeReceiver, receiver, denom, trace, "105", "5", "100"),
			decidedLine(string(CategoryIBCOut), "100", denom, signer, receiver, TriggerSkipBridge, entry, bridged("5")) +
				decidedLine(string(CategoryExpense), "5", denom, signer, entry, TriggerBridgeFee, entry, map[string]string{detailFallback: fallbackBank})},
		{"without a fee", skipBridgeLog(signer, entry, adapter, feeReceiver, receiver, denom, trace, "100", "", "100"),
			decidedLine(string(CategoryIBCOut), "100", denom, signer, receiver, TriggerSkipBridge, entry, bridged(""))},
		{"paid more than transfer plus fee stays flagged", skipBridgeLog(signer, entry, adapter, feeReceiver, receiver, denom, trace, "106", "5", "100"),
			flagged("106")},
		{"a transfer of another coin stays flagged", skipBridgeLog(signer, entry, adapter, feeReceiver, receiver, denom, "transfer/channel-9/uother", "105", "5", "100"),
			flagged("105")},
	}
	for _, c := range cases {
		if got := summary(parseExecute(t, signer, c.log)); got != c.want {
			t.Errorf("%s:\ngot:\n%swant:\n%s", c.name, got, c.want)
		}
	}

	// An entry point without code 127 evidence, or an adapter not known as a
	// router, leaves the coin flagged.
	other := testBech(t, "other", 32)
	registerKind(t, other, contractRoleRouter, true)
	if got, want := summary(parseExecute(t, signer, skipBridgeLog(signer, other, adapter, feeReceiver, receiver, denom, trace, "105", "5", "100"))),
		fallbackLine(string(CategoryTransfer), "105", denom, signer, other, other, true); got != want {
		t.Errorf("no entry evidence:\ngot:\n%swant:\n%s", got, want)
	}
	unknownAdapter := testBech(t, "adapter2", 32)
	if got := summary(parseExecute(t, signer, skipBridgeLog(signer, entry, unknownAdapter, feeReceiver, receiver, denom, trace, "105", "5", "100"))); got != flagged("105") {
		t.Errorf("unknown adapter:\ngot:\n%swant:\n%s", got, flagged("105"))
	}
	// A coin another wallet signed for is not the signer's bridge.
	payer := testBech(t, "payer", 20)
	log := skipBridgeLog(payer, entry, adapter, feeReceiver, receiver, denom, trace, "105", "5", "100")
	if got := summary(parseExecute(t, signer, log)); strings.Contains(got, TriggerSkipBridge) {
		t.Errorf("non-signer bridged: %s", got)
	}
}

// registerProposal pins one DAO proposal for one test.
func registerProposal(t *testing.T, p daoProposal) {
	t.Helper()
	if daoProposalIndex[p.module] == nil {
		daoProposalIndex[p.module] = map[uint64]daoProposal{}
	}
	daoProposalIndex[p.module][p.id] = p
	t.Cleanup(func() { delete(daoProposalIndex[p.module], p.id) })
}

func proposalLog(module, dao, id, recipient, amount string) *txlog.LogMessage {
	return executeLog(module,
		ev(eventWasm, [2]string{attributeContractAddress, module}, [2]string{attributeAction, "execute"}, [2]string{detailProposalID, id}, [2]string{"dao", dao}),
		ev("execute", [2]string{attributeContractAddress, dao}),
		transferEv(dao, recipient, amount))
}

// A payout from a pinned DAO proposal takes the class the proposal's text
// decides; any other proposal of the same module, a payout from another
// address, or a coin the wallet paid stays flagged.
func TestDAOProposalClass(t *testing.T) {
	signer, payee := testBech(t, "signer", 20), testBech(t, "payee", 20)
	module, dao, other := testBech(t, "module", 32), testBech(t, "dao", 32), testBech(t, "otherdao", 32)
	registerProposal(t, daoProposal{module, 7, dao, daoClassPayment, "Process payments"})
	registerProposal(t, daoProposal{module, 8, dao, daoClassPrize, "Competition payout"})
	registerProposal(t, daoProposal{module, 9, dao, daoClassAirdrop, "Distribute PHMN to holders"})
	const token = "factory/test/PHMN"
	income := string(CategoryIncome)
	pinned := func(id, title string, more map[string]string) map[string]string {
		m := map[string]string{detailFallback: fallbackBank, detailProposalID: id, detailProposalTitle: title}
		for k, v := range more {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name string
		log  *txlog.LogMessage
		want string
	}{
		{"payment", proposalLog(module, dao, "7", payee, "12"+token),
			decidedLine(income, "12", token, dao, payee, TriggerDAOPayment, dao, pinned("7", "Process payments", nil))},
		{"prize", proposalLog(module, dao, "8", payee, "3"+token),
			decidedLine(income, "3", token, dao, payee, protocolPrize, dao, pinned("8", "Competition payout", nil))},
		{"airdrop names the published terms", proposalLog(module, dao, "9", payee, "40"+token),
			decidedLine(income, "40", token, dao, payee, TriggerAirdrop, dao, pinned("9", "Distribute PHMN to holders",
				map[string]string{detailEvidence: phmnSnapshotEvidence, detailSnapshotTerms: phmnSnapshotTerms}))},
		{"an unpinned proposal stays flagged", proposalLog(module, dao, "10", payee, "12"+token),
			fallbackLine(string(CategoryTransfer), "12", token, dao, payee, dao, true)},
		{"another DAO under a pinned id stays flagged", proposalLog(module, other, "7", payee, "12"+token),
			fallbackLine(string(CategoryTransfer), "12", token, other, payee, other, true)},
	}
	for _, c := range cases {
		if got := summary(parseExecute(t, signer, c.log)); got != c.want {
			t.Errorf("%s:\ngot:\n%swant:\n%s", c.name, got, c.want)
		}
	}
	// Coins a wallet pays in the proposal's message are never its income.
	log := proposalLog(module, dao, "7", payee, "12"+token)
	log.Events = append(log.Events, transferEv(signer, dao, "1"+token))
	if got := summary(parseExecute(t, signer, log)); strings.Count(got, ReviewUnclassifiedContractMovement) != 1 || !strings.Contains(got, TriggerDAOPayment) {
		t.Errorf("paid coin:\n%s", got)
	}

	// Native ATOM a pinned proposal pays is income too, and so are the
	// wallet's other coins from the same payout.
	both := proposalLog(module, dao, "7", payee, "12"+token)
	both.Events = append(both.Events, transferEv(dao, payee, "5"+fixtureNativeDenom))
	want := decidedLine(income, "5", fixtureNativeDenom, dao, payee, TriggerDAOPayment, dao, pinned("7", "Process payments", nil)) +
		decidedLine(income, "12", token, dao, payee, TriggerDAOPayment, dao, pinned("7", "Process payments", nil))
	if got := summary(parseExecute(t, signer, both)); got != want {
		t.Errorf("native and token payout:\ngot:\n%swant:\n%s", got, want)
	}
	if got, want := summary(parseExecute(t, signer, proposalLog(module, dao, "7", payee, "5"+fixtureNativeDenom))),
		decidedLine(income, "5", fixtureNativeDenom, dao, payee, TriggerDAOPayment, dao, pinned("7", "Process payments", nil)); got != want {
		t.Errorf("native payout:\ngot:\n%swant:\n%s", got, want)
	}
	// Native ATOM under an unpinned proposal stays the plain transfer it
	// always was, and the wallet's other coins stay flagged.
	unpinned := proposalLog(module, dao, "10", payee, "12"+token)
	unpinned.Events = append(unpinned.Events, transferEv(dao, payee, "5"+fixtureNativeDenom))
	if got := summary(parseExecute(t, signer, unpinned)); strings.Contains(got, string(CategoryIncome)) || strings.Count(got, ReviewUnclassifiedContractMovement) != 1 {
		t.Errorf("unpinned native payout:\n%s", got)
	}
	// A wallet that paid the DAO ATOM in the message gets no income from it,
	// and its other coins stay flagged.
	paid := proposalLog(module, dao, "7", payee, "12"+token)
	paid.Events = append(paid.Events, transferEv(payee, dao, "5"+fixtureNativeDenom))
	if got := summary(parseExecute(t, signer, paid)); strings.Contains(got, string(CategoryIncome)) || strings.Count(got, ReviewUnclassifiedContractMovement) != 1 {
		t.Errorf("paid native:\n%s", got)
	}
}

// The pinned proposals are well formed: one per module and id, every one
// with a DAO and a class.
func TestDAOProposalsPinned(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range daoProposals {
		key := p.module + "/" + proposalIDText(p.id)
		if seen[key] || p.dao == "" || p.class == 0 || p.title == "" || !strings.HasPrefix(p.module, "cosmos1") || !strings.HasPrefix(p.dao, "cosmos1") {
			t.Fatalf("bad pin %d", p.id)
		}
		seen[key] = true
	}
	if len(daoProposals) != 32 {
		t.Fatalf("pins: %d", len(daoProposals))
	}
}
