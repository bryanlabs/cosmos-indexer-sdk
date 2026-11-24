package tax

import (
	"strings"
	"testing"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
)

// contractRedeemLog has the shape of mainnet message 8123609: a DAO DAO
// proposal (37) redeems the DAO's shares of records 67014 and 67027. Each
// record withdraws its reward from distribution and pays it to its owner's
// withdraw address; record 67027's reward includes an IBC denom.
func contractRedeemLog(t *testing.T, proposals, dao, lsmModule, bonded, payee14, payee27 string) *txlog.LogMessage {
	t.Helper()
	rec14, rec27 := lsmRecordAccount("67014"), lsmRecordAccount("67027")
	share14, share27 := lsmTestValoperA+"/67014", lsmTestValoperC+"/67027"
	ibc := lsmTestIBCDenom
	return &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("execute", [2]string{attributeContractAddress, proposals}),
		ev("wasm", [2]string{attributeContractAddress, proposals}, [2]string{"action", "execute"}, [2]string{"proposal_id", "37"}),
		ev("execute", [2]string{attributeContractAddress, dao}),
		transferEv(distributionModuleAddress, rec14, "59964uatom"),
		ev("withdraw_rewards", [2]string{attributeAmount, "59964uatom"}, [2]string{attributeDelegator, rec14}, [2]string{attributeValidator, lsmTestValoperA}),
		transferEv(bonded, lsmModule, "24997499uatom"),
		transferEv(rec14, payee14, "59964uatom"),
		ev(eventWithdrawTokenizeShareReward, [2]string{attributeAmount, "59964uatom"}, [2]string{attributeWithdrawAddress, payee14}),
		transferEv(dao, lsmModule, "25002499"+share14),
		ev("burn", [2]string{attributeAmount, "25002499" + share14}, [2]string{"burner", lsmModule}),
		transferEv(lsmModule, dao, "24997499uatom"),
		ev("redeem_shares", [2]string{attributeAmount, "25002499" + share14}, [2]string{attributeDelegator, dao}, [2]string{attributeValidator, lsmTestValoperA}),
		transferEv(distributionModuleAddress, rec27, "19"+ibc+",5078626uatom"),
		ev("withdraw_rewards", [2]string{attributeAmount, "19" + ibc + ",5078626uatom"}, [2]string{attributeDelegator, rec27}, [2]string{attributeValidator, lsmTestValoperC}),
		transferEv(bonded, lsmModule, "200000000uatom"),
		transferEv(rec27, payee27, "19"+ibc+",5078626uatom"),
		ev(eventWithdrawTokenizeShareReward, [2]string{attributeAmount, "19" + ibc + ",5078626uatom"}, [2]string{attributeWithdrawAddress, payee27}),
		transferEv(dao, lsmModule, "200000000"+share27),
		ev("burn", [2]string{attributeAmount, "200000000" + share27}, [2]string{"burner", lsmModule}),
		transferEv(lsmModule, dao, "200000000uatom"),
		ev("redeem_shares", [2]string{attributeAmount, "200000000" + share27}, [2]string{attributeDelegator, dao}, [2]string{attributeValidator, lsmTestValoperC}),
		ev("execute", [2]string{attributeContractAddress, testBech(t, "proposal-hook", 32)}),
	}}
}

// A record reward the chain settles while a contract redeems shares is the
// record owner's reward, booked as the native redeem books it: category
// reward, trigger redeem, to the withdraw address the record paid, against
// the record's validator. No record account is a wallet side and nothing is
// held for review. Before this rule the three legs were plain transfers from
// the record accounts and the IBC leg was held.
func TestContractRedeemBooksRecordRewardsToPayees(t *testing.T) {
	proposer := testBech(t, "proposer", 20)
	proposals, dao := testBech(t, "dao-proposals", 32), testBech(t, "dao-core", 32)
	lsmModule, bonded := testBech(t, "lsm-module", 32), testBech(t, "bonded-pool", 32)
	payee14, payee27 := testBech(t, "payee-67014", 20), testBech(t, "payee-67027", 20)
	out := parseExecute(t, proposer, contractRedeemLog(t, proposals, dao, lsmModule, bonded, payee14, payee27))

	type row struct{ category, trigger, to, amount, denom, validator string }
	want := []row{
		{string(CategoryReward), "redeem", payee14, "59964", "uatom", lsmTestValoperA},
		{string(CategoryReward), "redeem", payee27, "19", lsmTestIBCDenom, lsmTestValoperC},
		{string(CategoryReward), "redeem", payee27, "5078626", "uatom", lsmTestValoperC},
	}
	if len(out) != len(want) {
		t.Fatalf("rows = %+v, want %d reward rows", out, len(want))
	}
	records := map[string]bool{lsmRecordAccount("67014"): true, lsmRecordAccount("67027"): true}
	for i, e := range out {
		got := row{e.Category, e.RewardTrigger, e.ToAddr, e.Amount, e.Denom, e.ValidatorAddress}
		if got != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, got, want[i])
		}
		if records[e.FromAddr] || records[e.ToAddr] || e.FromAddr != "" {
			t.Fatalf("row %d has a record account or a sender side: %+v", i, e)
		}
		if e.Detail != "" {
			t.Fatalf("row %d detail = %s, want none (not held)", i, e.Detail)
		}
	}
}

// A settlement the log cannot tie to exactly one transfer from a proven
// record account keeps the old booking: the IBC leg stays held as a flagged
// transfer, never dropped and never guessed as a reward.
func TestContractRedeemUnprovenRecordKeepsFallback(t *testing.T) {
	proposer := testBech(t, "proposer", 20)
	contract, stranger, payee := testBech(t, "contract", 32), testBech(t, "stranger", 32), testBech(t, "payee", 20)
	log := executeLog(contract,
		transferEv(stranger, payee, "19"+lsmTestIBCDenom),
		ev(eventWithdrawTokenizeShareReward, [2]string{attributeAmount, "19" + lsmTestIBCDenom}, [2]string{attributeWithdrawAddress, payee}),
	)
	out := parseExecute(t, proposer, log)
	if len(out) != 1 || out[0].Category != string(CategoryTransfer) || out[0].FromAddr != stranger || out[0].ToAddr != payee {
		t.Fatalf("rows = %+v, want the one fallback transfer", out)
	}
	if got := out[0].Detail; got == "" || !strings.Contains(got, ReviewUnclassifiedContractMovement) {
		t.Fatalf("detail = %q, want it held for review", got)
	}
}

// A payee's other coins in the same message still reach the fallback; only
// the settled legs leave it.
func TestContractRedeemKeepsPayeeOtherFlows(t *testing.T) {
	proposer := testBech(t, "proposer", 20)
	proposals, dao := testBech(t, "dao-proposals", 32), testBech(t, "dao-core", 32)
	lsmModule, bonded := testBech(t, "lsm-module", 32), testBech(t, "bonded-pool", 32)
	payee14, payee27 := testBech(t, "payee-67014", 20), testBech(t, "payee-67027", 20)
	log := contractRedeemLog(t, proposals, dao, lsmModule, bonded, payee14, payee27)
	log.Events = append(log.Events, transferEv(dao, payee27, "7uatom"))
	out := parseExecute(t, proposer, log)
	if len(out) != 4 {
		t.Fatalf("rows = %+v, want 3 rewards and the DAO's 7uatom transfer", out)
	}
	last := out[3]
	if last.Category != string(CategoryTransfer) || last.FromAddr != dao || last.ToAddr != payee27 || last.Amount != "7" {
		t.Fatalf("last row = %+v, want the DAO's 7uatom transfer to the payee", last)
	}
}

// Records that settle the same amount to one payee are matched as a group:
// as many transfers as claims books each transfer once, against its own
// record's validator.
func TestContractRedeemEqualSettlementsBookEachRecord(t *testing.T) {
	proposer, contract, payee := testBech(t, "proposer", 20), testBech(t, "contract", 32), testBech(t, "payee", 20)
	recA, recC := lsmRecordAccount("501"), lsmRecordAccount("502")
	claim := ev(eventWithdrawTokenizeShareReward, [2]string{attributeAmount, "10uatom"}, [2]string{attributeWithdrawAddress, payee})
	log := executeLog(contract,
		transferEv(recA, payee, "10uatom"), claim,
		transferEv(recC, payee, "10uatom"), claim,
		transferEv(contract, testBech(t, "lsm-module", 32), "5"+lsmTestValoperA+"/501,5"+lsmTestValoperC+"/502"),
	)
	out := parseExecute(t, proposer, log)
	requireRewardRows(t, out, []rewardRow{{lsmTestValoperA, "10", "uatom"}, {lsmTestValoperC, "10", "uatom"}})
	requireRewardRecipients(t, out, payee, payee)
	if len(out) != 2 {
		t.Fatalf("rows = %+v, want only the two rewards", out)
	}

	// One claim short: the two equal transfers prove neither.
	short := executeLog(contract,
		transferEv(recA, payee, "10uatom"), claim,
		transferEv(recC, payee, "10uatom"),
		transferEv(contract, testBech(t, "lsm-module", 32), "5"+lsmTestValoperA+"/501,5"+lsmTestValoperC+"/502"),
	)
	for _, e := range parseExecute(t, proposer, short) {
		if e.Category == string(CategoryReward) {
			t.Fatalf("unproven group booked a reward: %+v", e)
		}
	}
}

// A settlement paid to a contract is not a wallet side for the fallback, so
// it is not booked here either: the message keeps the rows it had (none).
func TestContractRedeemContractPayeeKeepsOldRows(t *testing.T) {
	proposer, contract := testBech(t, "proposer", 20), testBech(t, "dao", 32)
	rec := lsmRecordAccount("601")
	log := executeLog(contract,
		transferEv(rec, contract, "10uatom"),
		ev(eventWithdrawTokenizeShareReward, [2]string{attributeAmount, "10uatom"}, [2]string{attributeWithdrawAddress, contract}),
		transferEv(contract, testBech(t, "lsm-module", 32), "5"+lsmTestValoperA+"/601"),
	)
	if out := parseExecute(t, proposer, log); len(out) != 0 {
		t.Fatalf("rows = %+v, want none", out)
	}
}
