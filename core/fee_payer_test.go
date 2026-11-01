package core

import (
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/db/models"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
)

func TestFeePayerUnresolved(t *testing.T) {
	const verifiedSigner = "verified-signer"
	auth := txtypes.AuthInfo{Fee: &txtypes.Fee{Amount: sdk.NewCoins(sdk.NewInt64Coin("uatom", 7))}}
	if _, err := ProcessFees(nil, auth, nil); err == nil {
		t.Fatal("nonzero fee without a verified payer must defer the block, not create an empty address")
	}
	auth.Fee.Payer = "explicit-payer"
	fees, err := ProcessFees(nil, auth, nil)
	if err != nil || len(fees) != 1 || fees[0].PayerAddress.Address != "explicit-payer" {
		t.Fatalf("explicit payer: %v %v", fees, err)
	}
	auth.Fee.Payer = ""
	fees, err = ProcessFees(nil, auth, []models.Address{{Address: verifiedSigner}})
	if err != nil || len(fees) != 1 || fees[0].PayerAddress.Address != verifiedSigner {
		t.Fatalf("signer payer: %v %v", fees, err)
	}
	auth.Fee.Granter = "actual-fee-granter"
	fees, err = ProcessFees(nil, auth, []models.Address{{Address: verifiedSigner}})
	if err != nil || len(fees) != 1 || fees[0].PayerAddress.Address != "actual-fee-granter" {
		t.Fatalf("fee grant must use the account whose balance was deducted: %v %v", fees, err)
	}
	auth.Fee.Amount = sdk.NewCoins()
	if _, err := ProcessFees(nil, auth, nil); err != nil {
		t.Fatalf("zero fee needs no payer: %v", err)
	}
	if fees, err := ProcessFees(nil, txtypes.AuthInfo{}, nil); err != nil || len(fees) != 0 {
		t.Fatalf("absent fee is not an invented charge: %v %v", fees, err)
	}
}
