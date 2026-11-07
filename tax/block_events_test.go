package tax

import (
	"encoding/json"
	"testing"
	"time"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
)

const (
	blockOwner = "cosmos1owner0000000000000000000000000000000"
	blockOther = "cosmos1other0000000000000000000000000000000"
	notBonded  = "cosmos1tygms3xhhs3yv487phx3dw4a95jn7t7lpm470r"
)

func blockEvent(id uint, phase, typ string, kv ...string) BlockEvent {
	e := BlockEvent{ID: id, Height: 33000000, Timestamp: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), Phase: phase, Type: typ}
	for i := 0; i+1 < len(kv); i += 2 {
		e.Attributes = append(e.Attributes, txlog.Attribute{Key: kv[i], Value: kv[i+1]})
	}
	return e
}

func bankMove(id uint, phase, from, to, amount string) []BlockEvent {
	return []BlockEvent{
		blockEvent(id, phase, "coin_spent", "spender", from, "amount", amount),
		blockEvent(id+1, phase, "coin_received", "receiver", to, "amount", amount),
	}
}

func blockDetail(t *testing.T, e TaxableEvent) map[string]string {
	t.Helper()
	kv := map[string]string{}
	if err := json.Unmarshal([]byte(e.Detail), &kv); err != nil {
		t.Fatalf("detail %q: %v", e.Detail, err)
	}
	return kv
}

func onlyRow(t *testing.T, rows []TaxableEvent) TaxableEvent {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d: %+v", len(rows), rows)
	}
	return rows[0]
}

func TestBlockUnbondingComplete(t *testing.T) {
	events := append(bankMove(1, BlockPhaseEnd, notBonded, blockOwner, "2500000uatom"),
		blockEvent(3, BlockPhaseEnd, "complete_unbonding", "amount", "2500000uatom", "validator", "cosmosvaloper1v", "delegator", blockOwner))
	row := onlyRow(t, ClassifyBlockLifecycle(events, blockOwner, nil))
	if row.Category != string(CategoryUnbondingComplete) || row.Amount != "2500000" || row.Denom != "uatom" || row.ToAddr != blockOwner || row.ValidatorAddress != "cosmosvaloper1v" {
		t.Fatalf("row %+v", row)
	}
	if row.MessageID != 0 || row.SubIndex != 3 || row.TxHash != "" {
		t.Fatalf("identity %+v", row)
	}
	d := blockDetail(t, row)
	if d["source"] != "block_event" || d["phase"] != BlockPhaseEnd || d["review_required"] != "" {
		t.Fatalf("detail %v", d)
	}
	if CategoryUnbondingComplete.Taxable() {
		t.Fatal("unbonding_complete must be record-only")
	}
}

func TestBlockOtherWalletIgnored(t *testing.T) {
	events := append(bankMove(1, BlockPhaseEnd, notBonded, blockOther, "10uatom"),
		blockEvent(3, BlockPhaseEnd, "complete_unbonding", "amount", "10uatom", "validator", "cosmosvaloper1v", "delegator", blockOther))
	if rows := ClassifyBlockLifecycle(events, blockOwner, nil); len(rows) != 0 {
		t.Fatalf("rows %+v", rows)
	}
}

func TestBlockPreBankEventsChecksNothingElse(t *testing.T) {
	events := []BlockEvent{blockEvent(3, BlockPhaseEnd, "complete_unbonding", "amount", "10uatom", "validator", "cosmosvaloper1v", "delegator", blockOwner)}
	row := onlyRow(t, ClassifyBlockLifecycle(events, blockOwner, nil))
	if row.Category != string(CategoryUnbondingComplete) {
		t.Fatalf("row %+v", row)
	}
}

func TestBlockGovDepositRefunded(t *testing.T) {
	events := append(bankMove(1, BlockPhaseEnd, govModuleAddress, blockOwner, "1000000uatom"),
		blockEvent(3, BlockPhaseEnd, "transfer", "recipient", blockOwner, "sender", govModuleAddress, "amount", "1000000uatom"),
		blockEvent(4, BlockPhaseEnd, "active_proposal", "proposal_id", "1001", "proposal_result", "proposal_passed"))
	deposits := []GovDeposit{{ProposalID: "1001", Amount: "1000000", Denom: "uatom"}, {ProposalID: "999", Amount: "5", Denom: "uatom"}}
	row := onlyRow(t, ClassifyBlockLifecycle(events, blockOwner, deposits))
	if row.Category != string(CategoryGovDepositRefund) || row.Amount != "1000000" || row.FromAddr != govModuleAddress {
		t.Fatalf("row %+v", row)
	}
	if d := blockDetail(t, row); d["proposal_id"] != "1001" || d["proposal_result"] != "proposal_passed" {
		t.Fatalf("detail %v", d)
	}
}

func TestBlockGovDepositBurned(t *testing.T) {
	events := []BlockEvent{
		blockEvent(1, BlockPhaseEnd, "coin_spent", "spender", govModuleAddress, "amount", "1000000uatom"),
		blockEvent(2, BlockPhaseEnd, "burn", "burner", govModuleAddress, "amount", "1000000uatom"),
		blockEvent(3, BlockPhaseEnd, "active_proposal", "proposal_id", "1001", "proposal_result", "proposal_rejected"),
	}
	deposits := []GovDeposit{{ProposalID: "1001", Amount: "400000", Denom: "uatom"}, {ProposalID: "1001", Amount: "100000", Denom: "uatom"}}
	row := onlyRow(t, ClassifyBlockLifecycle(events, blockOwner, deposits))
	if row.Category != string(CategoryGovDepositBurn) || row.Amount != "500000" || row.FromAddr != blockOwner || row.ToAddr != govModuleAddress || row.SubIndex != 3 {
		t.Fatalf("row %+v", row)
	}
	d := blockDetail(t, row)
	if d["review_required"] != "" || d["proposal_id"] != "1001" {
		t.Fatalf("detail %v", d)
	}
	if !CategoryGovDepositBurn.Taxable() {
		t.Fatal("a burned deposit is a disposal")
	}
}

func TestBlockGovDepositWithoutEvidenceNeedsReview(t *testing.T) {
	events := []BlockEvent{blockEvent(3, BlockPhaseEnd, "active_proposal", "proposal_id", "1001", "proposal_result", "proposal_rejected")}
	deposits := []GovDeposit{{ProposalID: "1001", Amount: "500000", Denom: "uatom"}}
	row := onlyRow(t, ClassifyBlockLifecycle(events, blockOwner, deposits))
	d := blockDetail(t, row)
	if row.Category != string(CategoryGovDepositBurn) || d["review_required"] != "true" || d["review_reason"] != ReviewGovDepositOutcome {
		t.Fatalf("row %+v detail %v", row, d)
	}
}

func TestBlockGovDepositPartialRefundNeedsReview(t *testing.T) {
	events := append(bankMove(1, BlockPhaseEnd, govModuleAddress, blockOwner, "300000uatom"),
		blockEvent(3, BlockPhaseEnd, "transfer", "recipient", blockOwner, "sender", govModuleAddress, "amount", "300000uatom"),
		blockEvent(4, BlockPhaseEnd, "burn", "burner", govModuleAddress, "amount", "200000uatom"),
		blockEvent(5, BlockPhaseEnd, "active_proposal", "proposal_id", "1001", "proposal_result", "proposal_rejected"))
	deposits := []GovDeposit{{ProposalID: "1001", Amount: "500000", Denom: "uatom"}}
	rows := ClassifyBlockLifecycle(events, blockOwner, deposits)
	if len(rows) != 2 || rows[0].Category != string(CategoryGovDepositRefund) || rows[1].Category != string(CategoryGovDepositBurn) || rows[1].Amount != "200000" {
		t.Fatalf("rows %+v", rows)
	}
	if d := blockDetail(t, rows[1]); d["review_required"] != "true" {
		t.Fatalf("detail %v", d)
	}
}

func TestBlockCommunityPoolSpend(t *testing.T) {
	events := append(bankMove(1, BlockPhaseEnd, distributionModuleAddress, blockOwner, "7000000uatom"),
		blockEvent(3, BlockPhaseEnd, "transfer", "recipient", blockOwner, "sender", distributionModuleAddress, "amount", "7000000uatom"),
		blockEvent(4, BlockPhaseEnd, "active_proposal", "proposal_id", "1002", "proposal_result", "proposal_passed"))
	row := onlyRow(t, ClassifyBlockLifecycle(events, blockOwner, nil))
	if row.Category != string(CategoryIncome) || row.RewardTrigger != TriggerCommunityPoolSpend || row.Amount != "7000000" || row.FromAddr != distributionModuleAddress {
		t.Fatalf("row %+v", row)
	}
	if d := blockDetail(t, row); d["proposal_id"] != "1002" {
		t.Fatalf("detail %v", d)
	}
}

func TestBlockDistributionTransferWithoutProposalNeedsReview(t *testing.T) {
	events := append(bankMove(1, BlockPhaseBegin, distributionModuleAddress, blockOwner, "7uatom"),
		blockEvent(3, BlockPhaseBegin, "transfer", "recipient", blockOwner, "sender", distributionModuleAddress, "amount", "7uatom"))
	row := onlyRow(t, ClassifyBlockLifecycle(events, blockOwner, nil))
	d := blockDetail(t, row)
	if row.Category != string(CategoryTransfer) || row.ToAddr != blockOwner || row.Amount != "7" || d["review_reason"] != ReviewUnclassifiedBlockMovement {
		t.Fatalf("row %+v detail %v", row, d)
	}
}

func TestBlockUpgradeMoveNeedsReview(t *testing.T) {
	events := append(bankMove(1, BlockPhaseBegin, blockOther, blockOwner, "42uatom,9ibc/ABC"),
		bankMove(3, BlockPhaseBegin, blockOwner, blockOther, "5uatom")...)
	rows := ClassifyBlockLifecycle(events, blockOwner, nil)
	if len(rows) != 3 {
		t.Fatalf("rows %+v", rows)
	}
	for _, row := range rows {
		if d := blockDetail(t, row); d["review_required"] != "true" {
			t.Fatalf("row %+v not gated", row)
		}
	}
	if rows[0].Denom != "ibc/ABC" || rows[1].Denom != "uatom" || rows[1].Amount != "42" || rows[2].FromAddr != blockOwner || rows[2].Amount != "5" {
		t.Fatalf("rows %+v", rows)
	}
}

func TestBlockClassifiedMoreThanReceivedNeedsReview(t *testing.T) {
	events := append(bankMove(1, BlockPhaseEnd, notBonded, blockOwner, "10uatom"),
		blockEvent(3, BlockPhaseEnd, "complete_unbonding", "amount", "12uatom", "validator", "cosmosvaloper1v", "delegator", blockOwner))
	rows := ClassifyBlockLifecycle(events, blockOwner, nil)
	if len(rows) != 2 || rows[1].Amount != "2" {
		t.Fatalf("rows %+v", rows)
	}
	if d := blockDetail(t, rows[1]); d["classified_exceeds_received"] != "true" {
		t.Fatalf("detail %v", d)
	}
}
