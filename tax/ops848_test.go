package tax

import (
	"testing"

	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	govv1beta1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1beta1"
)

// legacyDepositLog is the stored log of a v1beta1 MsgDeposit on proposal 39
// (cosmoshub-4 height 5413682, message 102873042) with the addresses replaced.
// proposal_deposit, and the transfer to the gov module, both carry an empty
// amount, and the stored message body holds no coins: the deposit was zero.
func legacyDepositLog(transferAmount string) *indexerTxTypes.LogMessage {
	return &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("message", [2]string{attributeAction, "deposit"}, [2]string{attributeSender, del}, [2]string{"module", "governance"}, [2]string{attributeSender, del}),
		ev("proposal_deposit", [2]string{attributeAmount, ""}, [2]string{attributeProposalID, "39"}),
		ev(string(CategoryTransfer), [2]string{attributeRecipient, govModuleAddress}, [2]string{attributeSender, del}, [2]string{attributeAmount, transferAmount}),
	}}
}

func TestGovDepositEmptyAmountZeroDepositRecordsNothing(t *testing.T) {
	out := classify(&govv1beta1.MsgDeposit{ProposalId: 39, Depositor: del}, legacyDepositLog(""))
	if len(out) != 0 {
		t.Fatalf("a zero deposit moves no coins and must record nothing, got %+v", out)
	}
}

func TestGovDepositEmptyAmountFallsBackToGovModuleTransfer(t *testing.T) {
	log := legacyDepositLog("5000000uatom")
	for name, msg := range map[string]sdk.Msg{
		"deposit": &govv1beta1.MsgDeposit{ProposalId: 39, Depositor: del},
		"submit":  &govv1beta1.MsgSubmitProposal{Proposer: del},
	} {
		out := classify(msg, log)
		if len(out) != 1 || out[0].Category != string(CategoryGovDeposit) || out[0].FromAddr != del ||
			out[0].Amount != "5000000" || out[0].Denom != fixtureNativeDenom || out[0].Detail != `{"proposal_id":"39"}` {
			t.Fatalf("%s: empty proposal_deposit amount should take the transfer to the gov module, got %+v", name, out)
		}
	}
	// A transfer from someone else, or to an account other than the gov module,
	// is not the deposit.
	stranger := legacyDepositLog("")
	stranger.Events = append(stranger.Events,
		ev(string(CategoryTransfer), [2]string{attributeRecipient, govModuleAddress}, [2]string{attributeSender, other}, [2]string{attributeAmount, "7uatom"}),
		ev(string(CategoryTransfer), [2]string{attributeRecipient, other}, [2]string{attributeSender, del}, [2]string{attributeAmount, "9uatom"}))
	if out := classify(&govv1beta1.MsgDeposit{ProposalId: 39, Depositor: del}, stranger); len(out) != 0 {
		t.Fatalf("only the depositor's transfer to the gov module is the deposit, got %+v", out)
	}
}

func TestGovDepositKeepsEventAmountWhenPresent(t *testing.T) {
	log := legacyDepositLog("5000000uatom")
	log.Events[1].Attributes[0].Value = "4000000uatom"
	out := classify(&govv1beta1.MsgDeposit{ProposalId: 39, Depositor: del}, log)
	if len(out) != 1 || out[0].Amount != "4000000" {
		t.Fatalf("a non-empty proposal_deposit amount stays authoritative, got %+v", out)
	}
}

// soleTransferLog is the stored shape of a community pool funding or vesting
// account creation from hub4 2026 (messages 1797787, 5195108 and 9760992):
// message action and sender, coin_spent, coin_received, one transfer.
func soleTransferLog(typ, to, amount string) *indexerTxTypes.LogMessage {
	return &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("message", [2]string{attributeAction, typ}, [2]string{attributeSender, del}, [2]string{"module", "x"}, [2]string{"msg_index", "0"}),
		ev("coin_spent", [2]string{"spender", del}, [2]string{attributeAmount, amount}, [2]string{"msg_index", "0"}),
		ev("coin_received", [2]string{attributeReceiver, to}, [2]string{attributeAmount, amount}, [2]string{"msg_index", "0"}),
		ev(string(CategoryTransfer), [2]string{attributeRecipient, to}, [2]string{attributeSender, del}, [2]string{attributeAmount, amount}, [2]string{"msg_index", "0"}),
		ev("message", [2]string{attributeSender, del}, [2]string{"msg_index", "0"}),
	}}
}

func replayStored(t *testing.T, typ string, log *indexerTxTypes.LogMessage) ([]TaxableEvent, error) {
	t.Helper()
	msg, err := ReconstructMsg(typ, log)
	if err != nil {
		return nil, err
	}
	out, err := (&Parser{ID: "test"}).ParseMessage(msg, log, cfgForTests())
	if err != nil {
		t.Fatalf("%s: %v", typ, err)
	}
	return (*out).([]TaxableEvent), nil
}

func TestReconstructFundCommunityPoolAndVestingFromStoredLog(t *testing.T) {
	pool, err := replayStored(t, typeURLFundCommunityPool, soleTransferLog(typeURLFundCommunityPool, distributionModuleAddress, "24000000000uatom"))
	if err != nil || len(pool) != 1 || pool[0].Category != string(CategoryTransfer) || pool[0].FromAddr != del ||
		pool[0].ToAddr != distributionModuleAddress || pool[0].Amount != "24000000000" || pool[0].Denom != fixtureNativeDenom ||
		pool[0].Detail != `{"protocol":"community_pool"}` {
		t.Fatalf("community pool funding should replay as a transfer to the distribution module, got %+v %v", pool, err)
	}
	for _, typ := range []string{typeURLCreateVestingAccount, typeURLCreatePermanentLockedAccount} {
		grant, err := replayStored(t, typ, soleTransferLog(typ, other, "10000uatom"))
		if err != nil || len(grant) != 1 || grant[0].Category != string(CategoryTransfer) || grant[0].FromAddr != del ||
			grant[0].ToAddr != other || grant[0].Amount != "10000" || grant[0].Denom != fixtureNativeDenom {
			t.Fatalf("%s should replay as the grant transfer, got %+v %v", typ, grant, err)
		}
	}
}

func TestReconstructSoleTransferRefusesAmbiguousLogs(t *testing.T) {
	if _, err := replayStored(t, typeURLFundCommunityPool, soleTransferLog(typeURLFundCommunityPool, other, "5uatom")); err == nil {
		t.Fatal("community pool funding that does not pay the distribution module must be refused")
	}
	two := soleTransferLog(typeURLCreateVestingAccount, other, "5uatom")
	two.Events = append(two.Events, ev(string(CategoryTransfer), [2]string{attributeRecipient, other}, [2]string{attributeSender, del}, [2]string{attributeAmount, "6uatom"}))
	if _, err := replayStored(t, typeURLCreateVestingAccount, two); err == nil {
		t.Fatal("two transfers from the signer must be refused, not summed or guessed")
	}
	if _, err := replayStored(t, typeURLCreateVestingAccount, soleTransferLog(typeURLCreateVestingAccount, other, "")); err == nil {
		t.Fatal("an empty transfer must be refused")
	}
	noSender := soleTransferLog(typeURLCreatePermanentLockedAccount, other, "5uatom")
	noSender.Events[0].Attributes[0].Value = "something-else"
	if _, err := replayStored(t, typeURLCreatePermanentLockedAccount, noSender); err == nil {
		t.Fatal("a log without the message action and sender must be refused")
	}
}
