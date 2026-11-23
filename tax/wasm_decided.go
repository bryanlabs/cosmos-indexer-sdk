package tax

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	sdkmath "cosmossdk.io/math"
	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
)

// Triggers of the decided bank fallback rows: rules that read a contract
// message's own events to decide coins the fallback would otherwise flag.
const (
	// TriggerSkipBridge is a wallet's coin sent out of the Hub through the
	// Skip Go entry point, which hands it to its IBC adapter for one IBC
	// transfer in the same message.
	TriggerSkipBridge = "skip_bridge"
	// TriggerBridgeFee is the fee the entry point kept from that coin and
	// paid to the fee receiver its event names.
	TriggerBridgeFee = "bridge_fee"
	// TriggerAirdrop is a token a DAO sent to holders of another token by a
	// published snapshot while the holders kept the old token.
	TriggerAirdrop = "airdrop"
	// TriggerDAOPayment is a DAO treasury payment a passed proposal made to
	// a named payee: a grant, a contributor or service payment.
	TriggerDAOPayment = "dao_payment"
)

const (
	actionDispatchIBCTransfer       = "dispatch_action_ibc_transfer"
	actionDispatchIBCEurekaTransfer = "dispatch_action_ibc_eureka_transfer"
	attrEurekaFeeAmount             = "eureka_fee_amount"
	attrEurekaFeeDenom              = "eureka_fee_denom"
	attrEurekaFeeReceiver           = "eureka_fee_receiver"
	detailDestinationReceiver       = "destination_receiver"
	detailRoute                     = "route"
	detailBridgeFee                 = "bridge_fee"
	detailProposalID                = "proposal_id"
	detailProposalTitle             = "proposal_title"
	detailEvidence                  = "evidence"
	detailSnapshotTerms             = "snapshot_terms"
	codeSkipEntryPoint              = 127
)

// skipBridgeRows decides a wallet's only unexplained coin in a message where
// it paid the Skip Go entry point and the entry point sent the coin on over
// IBC. It holds only when the wallet signed the message, paid exactly one
// coin to an entry point our node's evidence names (code 127), the entry
// point dispatched an IBC transfer, exactly one ibc_transfer event names a
// Skip Go router as sender and that coin's denomination (or the trace it
// hashes to), and the wallet paid exactly the transferred amount plus the
// fee the entry point reports, with the fee in the same coin and the entry
// point's payment of it to the fee receiver in the message. The transfer is
// recorded as the wallet's ibc_out to the receiver, the fee as an expense
// paid to the entry point. Anything else is left to the review flag.
func skipBridgeRows(log *indexerTxTypes.LogMessage, sender, wallet string, flows []*fallbackFlow, rows []TaxableEvent, contracts []string) (TaxableEvent, *TaxableEvent, bool) {
	if wallet != sender || len(flows) != 1 || !flows[0].net.IsNegative() {
		return TaxableEvent{}, nil, false
	}
	entry := contracts[0]
	if ev, ok := fallbackEvidence[entry]; !ok || ev.codeID != codeSkipEntryPoint || rows[0].ToAddr != entry {
		return TaxableEvent{}, nil, false
	}
	denom, paid := flows[0].denom, flows[0].net.Neg()
	dispatched, eureka := false, false
	fee, feeReceiver, fees := sdkmath.ZeroInt(), "", 0
	var transfers []map[string]string
	for _, ev := range log.Events {
		a := attrMap(ev)
		switch ev.Type {
		case eventWasm:
			if a[attributeContractAddress] != entry {
				continue
			}
			if a[attributeAction] == actionDispatchIBCTransfer || a[attributeAction] == actionDispatchIBCEurekaTransfer {
				dispatched = true
			}
			if _, ok := a[attrEurekaFeeAmount]; ok {
				amt, ok := sdkmath.NewIntFromString(a[attrEurekaFeeAmount])
				if !ok || amt.IsNegative() || a[attrEurekaFeeDenom] != denom || a[attrEurekaFeeReceiver] == "" {
					return TaxableEvent{}, nil, false
				}
				fee, feeReceiver = amt, a[attrEurekaFeeReceiver]
				fees++
			}
		case "ibc_transfer":
			transfers = append(transfers, a)
		case "send_packet":
			if a[attributePacketSourceClient] != "" {
				eureka = true
			}
		}
	}
	if !dispatched || fees > 1 || len(transfers) != 1 {
		return TaxableEvent{}, nil, false
	}
	t := transfers[0]
	sent, ok := sdkmath.NewIntFromString(t[attributeAmount])
	if !ok || !sent.IsPositive() || t[attributeReceiver] == "" || contractKind(t[attributeSender]) != contractRoleRouter || !sameIBCDenom(t[attributeDenom], denom) {
		return TaxableEvent{}, nil, false
	}
	if !paid.Equal(sent.Add(fee)) {
		return TaxableEvent{}, nil, false
	}
	if fee.IsPositive() && !legPaid(log, entry, feeReceiver, denom, fee) {
		return TaxableEvent{}, nil, false
	}
	route := "ibc"
	if eureka {
		route = "ibc_eureka"
	}
	extra := map[string]string{detailFallback: fallbackBank, detailDestinationReceiver: t[attributeReceiver], detailRoute: route}
	if fee.IsPositive() {
		extra[detailBridgeFee] = fee.String()
	}
	out := TaxableEvent{
		Category: string(CategoryIBCOut), FromAddr: wallet, ToAddr: t[attributeReceiver],
		Amount: sent.String(), Denom: denom, RewardTrigger: TriggerSkipBridge,
		Detail: contractDetail(entry, extra),
	}
	if !fee.IsPositive() {
		return out, nil, true
	}
	feeRow := TaxableEvent{
		Category: string(CategoryExpense), FromAddr: wallet, ToAddr: entry,
		Amount: fee.String(), Denom: denom, RewardTrigger: TriggerBridgeFee,
		Detail: contractDetail(entry, map[string]string{detailFallback: fallbackBank}),
	}
	return out, &feeRow, true
}

// sameIBCDenom reports whether an ibc_transfer event's denomination is the
// wallet's coin: the same base denomination, or the trace whose hash is the
// wallet's ibc/ denomination.
func sameIBCDenom(eventDenom, denom string) bool {
	if eventDenom == denom {
		return true
	}
	if !strings.HasPrefix(denom, "ibc/") || eventDenom == "" {
		return false
	}
	sum := sha256.Sum256([]byte(eventDenom))
	return strings.EqualFold(hex.EncodeToString(sum[:]), strings.TrimPrefix(denom, "ibc/"))
}

// legPaid reports a bank leg from one address to another of exactly amount.
func legPaid(log *indexerTxTypes.LogMessage, from, to, denom string, amount sdkmath.Int) bool {
	for _, l := range transfersIn(log) {
		if l.from == from && l.to == to && l.coins.AmountOf(denom).Equal(amount) {
			return true
		}
	}
	return false
}

type daoClass int

const (
	daoClassPayment daoClass = iota + 1
	daoClassPrize
	daoClassAirdrop
)

// daoProposal is one executed DAO DAO proposal whose own title and text,
// read from our node's proposal module, decide what its payouts are.
type daoProposal struct {
	module string
	id     uint64
	dao    string
	class  daoClass
	title  string
}

// phmnSnapshotEvidence and phmnSnapshotTerms are the published terms of the
// PHMN snapshot distribution: holders of the old Juno CW20 PHMN, counted at
// a published snapshot, received new Cosmos Hub PHMN, kept their old PHMN,
// and later transfers of the old token do not change the distribution.
const (
	phmnSnapshotEvidence = "https://github.com/Validator-POSTHUMAN/new-phmn"
	phmnSnapshotTerms    = "new PHMN sent by the published final snapshot of old Juno CW20 PHMN holdings; holders were not asked to move or surrender old PHMN, which stays transferable, and later old PHMN transfers do not change the distribution"
)

// daoProposals are the executed proposals whose text decides their payouts.
// Payment proposals pay grants, contributors or service providers; the prize
// proposal pays a named competition winner; the airdrop proposals send new
// PHMN to holders by snapshot while the holders keep the old token. Every
// other DAO payout stays flagged for review.
var daoProposals = []daoProposal{
	{"cosmos14pdk5alpuxm27wrkp5c2acll00xs6a83704qwvuez6wd3xqput5qzmtrhu", 535, "cosmos13zm96r5v73ucfmu3fdf838pjtrm3vknxwkhyqqadmvnjpvl4utzsv9ck5y", daoClassPayment, "Payment Asteroid maintenance"},
	{"cosmos19ffhpk0s8gy524m5gxu6kdk0xf2w0wszthqhdutuayc5pptzrxzstephyv", 16, "cosmos1hwrt3tv3qwhzzt8qmyqxjdj0f8yr923mtz73qvy9g3n8crrenkks6sae4p", daoClassPayment, "Payment Mr T"},
	{"cosmos19ffhpk0s8gy524m5gxu6kdk0xf2w0wszthqhdutuayc5pptzrxzstephyv", 31, "cosmos1hwrt3tv3qwhzzt8qmyqxjdj0f8yr923mtz73qvy9g3n8crrenkks6sae4p", daoClassPayment, "Payment Mr T"},
	{"cosmos1k5699x6ph42th2nl3ysz7q69fqq6l6pz9mnuv6t2kz8a30u9nxmq87w88f", 11, "cosmos1tgtp043ktekcyrssftg50u2a8la80htseeu3zf0djzljhqws7e9s09ajus", daoClassPrize, "OPHIR COMPETITION PAYOUT"},
	{"cosmos1lwn2ell964rnjm9zg7ysqcz6cdr64srgh3r2zp84f0w3chfvzw3syutpza", 5, "cosmos146s5j3t7gh2g37ywm47dp8avhesu2htvjjaxq7z55e7xj0rq0k8q5qnjjy", daoClassAirdrop, "Distribute 55,680.128854 PHMN to holders with balances greater than 100 PHMN"},
	{"cosmos1lwn2ell964rnjm9zg7ysqcz6cdr64srgh3r2zp84f0w3chfvzw3syutpza", 6, "cosmos146s5j3t7gh2g37ywm47dp8avhesu2htvjjaxq7z55e7xj0rq0k8q5qnjjy", daoClassAirdrop, "Distribute 5,770.769254 PHMN to other holders, part 1"},
	{"cosmos1lwn2ell964rnjm9zg7ysqcz6cdr64srgh3r2zp84f0w3chfvzw3syutpza", 7, "cosmos146s5j3t7gh2g37ywm47dp8avhesu2htvjjaxq7z55e7xj0rq0k8q5qnjjy", daoClassAirdrop, "Distribute 2,966.991703 PHMN to remaining holders above 10 PHMN, part 1 of 2"},
	{"cosmos1lwn2ell964rnjm9zg7ysqcz6cdr64srgh3r2zp84f0w3chfvzw3syutpza", 8, "cosmos146s5j3t7gh2g37ywm47dp8avhesu2htvjjaxq7z55e7xj0rq0k8q5qnjjy", daoClassAirdrop, "Distribute 1,625.236214 PHMN to remaining holders above 10 PHMN, part 2 of 2"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 10, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Hydro Grants Feb 2025"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 11, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Hydro Grants Feb 2025 (Tests)"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 12, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Hydro Grants Feb 2025 - p2"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 14, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Test new addresses for Mar-2025"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 15, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Process recurring payments for Mar-2025"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 16, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Process other payments for Mar-2025"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 17, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Process recurring payments for April-2025"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 20, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Process Current marketplace payment #2"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 21, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Process payments for May-2025"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 23, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Process Current marketplace payment #3"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 24, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Process payments for June-2025"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 25, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Process payments for July-2025"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 26, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Test a new address on Cosmos"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 27, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Process payments for August-2025"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 28, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Process payments for September-2025"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 29, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Test a new address on Cosmos"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 30, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Process payments for October-2025"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 31, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Test a new address on Cosmos"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 32, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Process payments for November-2025"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 33, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Process payments for December-2025"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 34, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Process payment for January-2026"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 7, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Jan 2025 payments"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 8, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Hydro Grants Jan 2025 (Tests)"},
	{"cosmos1wn7m0mr9jscxhhvua2qydh7xnuu7sy69tyvwj5ue7f2ttrlq3rsq468g4y", 9, "cosmos1stxad87r0c9x9skn5ap6eyx9wwmj5xuqcpd3n974qpg5stv92sqqj2zrh4", daoClassPayment, "Hydro Grants Jan 2025 (Tests)"},
}

var daoProposalIndex = func() map[string]map[uint64]daoProposal {
	m := map[string]map[uint64]daoProposal{}
	for _, p := range daoProposals {
		if m[p.module] == nil {
			m[p.module] = map[uint64]daoProposal{}
		}
		m[p.module][p.id] = p
	}
	return m
}()

// daoProposalClass decides a wallet's only unexplained coins in a message
// that executed one of daoProposals: coins the wallet only received, every
// one of them from that proposal's DAO. Payments are income with
// TriggerDAOPayment, the prize is income with the prize trigger, and the
// snapshot distribution is an airdrop: ordinary income at fair value on
// receipt (Rev. Rul. 2019-24), with the published terms in the detail.
func daoProposalClass(log *indexerTxTypes.LogMessage, flows []*fallbackFlow, contracts []string) (string, map[string]string, bool) {
	for _, f := range flows {
		if !f.net.IsPositive() {
			return "", nil, false
		}
	}
	var found *daoProposal
	for _, ev := range log.Events {
		if ev.Type != eventWasm {
			continue
		}
		a := attrMap(ev)
		id, ok := sdkmath.NewIntFromString(a[detailProposalID])
		if !ok || !id.IsUint64() || a["dao"] == "" {
			continue
		}
		p, ok := daoProposalIndex[a[attributeContractAddress]][id.Uint64()]
		if !ok || p.dao != a["dao"] {
			continue
		}
		if found != nil && found.module+"/"+found.title != p.module+"/"+p.title {
			return "", nil, false
		}
		found = &p
	}
	if found == nil {
		return "", nil, false
	}
	for _, c := range contracts {
		if c != found.dao {
			return "", nil, false
		}
	}
	extra := map[string]string{detailFallback: fallbackBank, detailProposalTitle: found.title}
	if v := proposalIDText(found.id); v != "" {
		extra[detailProposalID] = v
	}
	switch found.class {
	case daoClassPayment:
		return TriggerDAOPayment, extra, true
	case daoClassPrize:
		return protocolPrize, extra, true
	case daoClassAirdrop:
		extra[detailEvidence] = phmnSnapshotEvidence
		extra[detailSnapshotTerms] = phmnSnapshotTerms
		return TriggerAirdrop, extra, true
	}
	return "", nil, false
}

func proposalIDText(id uint64) string {
	return sdkmath.NewIntFromUint64(id).String()
}
