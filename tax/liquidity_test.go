package tax

import (
	"bytes"
	"strings"
	"testing"

	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
)

const (
	liqOffer    = "ibc/14F9BC3E44B8A9C1BE1FB08980FAB87034C9905EF17CF2F5008FC085218811CC"
	liqPoolCoin = "poolDFB8434D5A80B4EAFA94B6878BD5B85265AC6C5D37204AB899B1C3C52543DA7E"
	liqReserve  = "cosmos1reserve00000000000000000000000000000"

	liqCreationFee = "40000000uatom"
)

func coinField(num int, denom, amount string) []byte {
	return nested(num, append(fieldBytes(1, denom), fieldBytes(2, amount)...))
}

func TestLiquidityEscrowIsTheModuleAccount(t *testing.T) {
	if got := authtypes.NewModuleAddress("liquidity").String(); got != liquidityEscrowAddress {
		t.Fatalf("liquidity module address %s, constant %s", got, liquidityEscrowAddress)
	}
}

// Field layouts as seen on mainnet swaps, deposits and withdrawals in 2022.
func TestLiquidityMessagesDecode(t *testing.T) {
	signer := sdk.AccAddress(bytes.Repeat([]byte{7}, 20))
	swap := append(fieldBytes(1, signer.String()), fieldVarint(2, 1)...)
	swap = append(swap, fieldVarint(3, 1)...)
	swap = append(swap, coinField(4, liqOffer, "4337370")...)
	swap = append(swap, fieldBytes(5, fixtureNativeDenom)...)
	swap = append(swap, coinField(6, liqOffer, "6507")...)
	swap = append(swap, fieldBytes(7, "4428506089351284203")...)
	var s MsgLiquiditySwapWithinBatch
	if err := s.Unmarshal(swap); err != nil {
		t.Fatal(err)
	}
	if s.PoolID != 1 || s.SwapTypeID != 1 || s.OfferCoin.String() != "4337370"+liqOffer || s.DemandCoinDenom != fixtureNativeDenom || s.OfferCoinFee.Amount.Int64() != 6507 || s.OrderPrice == "" {
		t.Fatalf("swap %+v", s)
	}

	deposit := append(fieldBytes(1, signer.String()), fieldVarint(2, 1)...)
	deposit = append(deposit, coinField(3, liqOffer, "898168")...)
	deposit = append(deposit, coinField(3, fixtureNativeDenom, "200000")...)
	var d MsgLiquidityDepositWithinBatch
	if err := d.Unmarshal(deposit); err != nil {
		t.Fatal(err)
	}
	if d.PoolID != 1 || len(d.DepositCoins) != 2 || d.DepositCoins[1].String() != "200000uatom" {
		t.Fatalf("deposit %+v", d)
	}

	withdraw := append(fieldBytes(1, signer.String()), fieldVarint(2, 1)...)
	withdraw = append(withdraw, coinField(3, liqPoolCoin, "2444946")...)
	var w MsgLiquidityWithdrawWithinBatch
	if err := w.Unmarshal(withdraw); err != nil {
		t.Fatal(err)
	}
	if w.PoolID != 1 || w.PoolCoin.String() != "2444946"+liqPoolCoin {
		t.Fatalf("withdraw %+v", w)
	}

	create := append(fieldBytes(1, signer.String()), fieldVarint(2, 1)...)
	create = append(create, coinField(4, liqOffer, "1000000")...)
	create = append(create, coinField(4, fixtureNativeDenom, "2000000")...)
	var c MsgLiquidityCreatePool
	if err := c.Unmarshal(create); err != nil {
		t.Fatal(err)
	}
	if c.PoolTypeID != 1 || len(c.DepositCoins) != 2 {
		t.Fatalf("create %+v", c)
	}

	for _, m := range []sdk.Msg{&s, &d, &w, &c} {
		got := m.GetSigners()
		if len(got) != 1 || !got[0].Equals(signer) {
			t.Fatalf("%T signers %v", m, got)
		}
	}
}

// A batch message only escrows coins, so its transaction records nothing.
func TestLiquidityBatchMessagesRecordNothing(t *testing.T) {
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev("transfer", [2]string{attributeRecipient, liquidityEscrowAddress}, [2]string{attributeSender, del}, [2]string{attributeAmount, "4343877" + liqOffer}),
	}}
	msg := &MsgLiquiditySwapWithinBatch{SwapRequester: del, OfferCoin: sdk.NewInt64Coin(liqOffer, 4337370)}
	if out := classify(msg, log); len(out) != 0 {
		t.Fatalf("rows %+v", out)
	}
}

// Swap settled in EndBlock of height 9000001 (pool 1, batch 42629, msg 35229).
// The escrow pays the reserve the transacted offer and the fee, and the
// reserve pays the requester, in one InputOutputCoins call.
func TestBlockGravityDEXSwap(t *testing.T) {
	events := []BlockEvent{
		blockEvent(317, BlockPhaseEnd, "swap_transacted", "pool_id", "1", "batch_index", "42629", "msg_index", "35229",
			"swap_requester", blockOwner, "swap_type_id", "1", "offer_coin_denom", liqOffer, "offer_coin_amount", "4337370",
			"demand_coin_denom", fixtureNativeDenom, "swap_price", "4.417290716791536022", "transacted_coin_amount", "4337370",
			"remaining_offer_coin_amount", "0", "exchanged_demand_coin_amount", "980434", "offer_coin_fee_amount", "6507",
			"reserved_offer_coin_fee_amount", "0", "success", "success"),
		blockEvent(318, BlockPhaseEnd, "coin_spent", attributeSpender, liquidityEscrowAddress, "amount", "4337370"+liqOffer),
		blockEvent(319, BlockPhaseEnd, "message", "sender", liquidityEscrowAddress),
		blockEvent(320, BlockPhaseEnd, "coin_spent", attributeSpender, liqReserve, "amount", "980434uatom"),
		blockEvent(321, BlockPhaseEnd, "message", "sender", liqReserve),
		blockEvent(322, BlockPhaseEnd, "coin_spent", attributeSpender, liquidityEscrowAddress, "amount", "6507"+liqOffer),
		blockEvent(323, BlockPhaseEnd, "message", "sender", liquidityEscrowAddress),
		blockEvent(324, BlockPhaseEnd, "coin_received", "receiver", liqReserve, "amount", "4337370"+liqOffer),
		blockEvent(325, BlockPhaseEnd, "transfer", "recipient", liqReserve, "amount", "4337370"+liqOffer),
		blockEvent(326, BlockPhaseEnd, "coin_received", "receiver", blockOwner, "amount", "980434uatom"),
		blockEvent(327, BlockPhaseEnd, "transfer", "recipient", blockOwner, "amount", "980434uatom"),
		blockEvent(328, BlockPhaseEnd, "coin_received", "receiver", liqReserve, "amount", "6507"+liqOffer),
		blockEvent(329, BlockPhaseEnd, "transfer", "recipient", liqReserve, "amount", "6507"+liqOffer),
	}
	rows := ClassifyBlockLifecycle(events, blockOwner, nil)
	if len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
	out, in := rows[0], rows[1]
	if out.Category != string(CategorySwap) || out.FromAddr != blockOwner || out.Amount != "4343877" || out.Denom != liqOffer || out.RewardTrigger != TriggerGravityDEXSwap {
		t.Fatalf("disposal %+v", out)
	}
	if in.Category != string(CategorySwap) || in.ToAddr != blockOwner || in.Amount != "980434" || in.Denom != fixtureNativeDenom {
		t.Fatalf("acquisition %+v", in)
	}
	do, di := blockDetail(t, out), blockDetail(t, in)
	if do["swap_group"] == "" || do["swap_group"] != di["swap_group"] || do["pool_id"] != "1" || do["review_required"] != "" || di["review_required"] != "" {
		t.Fatalf("detail %v %v", do, di)
	}
}

// Deposit settled at height 9000108 (batch 42636, msg 7700): part of the
// deposit is accepted for pool coins and the rest is returned from escrow.
// The return is the owner's own coins and gets no row.
func TestBlockGravityDEXDepositWithRefund(t *testing.T) {
	refund := "15119" + liqOffer + ",1uatom"
	accepted := "883049" + liqOffer + ",199999uatom"
	events := []BlockEvent{
		blockEvent(314, BlockPhaseEnd, "coin_received", "receiver", liquidityEscrowAddress, "amount", "143801"+liqPoolCoin),
		blockEvent(315, BlockPhaseEnd, "coinbase", "minter", liquidityEscrowAddress, "amount", "143801"+liqPoolCoin),
		blockEvent(316, BlockPhaseEnd, "coin_spent", attributeSpender, liquidityEscrowAddress, "amount", refund),
		blockEvent(317, BlockPhaseEnd, "message", "sender", liquidityEscrowAddress),
		blockEvent(318, BlockPhaseEnd, "coin_spent", attributeSpender, liquidityEscrowAddress, "amount", accepted),
		blockEvent(319, BlockPhaseEnd, "message", "sender", liquidityEscrowAddress),
		blockEvent(320, BlockPhaseEnd, "coin_spent", attributeSpender, liquidityEscrowAddress, "amount", "143801"+liqPoolCoin),
		blockEvent(321, BlockPhaseEnd, "message", "sender", liquidityEscrowAddress),
		blockEvent(322, BlockPhaseEnd, "coin_received", "receiver", blockOwner, "amount", refund),
		blockEvent(323, BlockPhaseEnd, "transfer", "recipient", blockOwner, "amount", refund),
		blockEvent(324, BlockPhaseEnd, "coin_received", "receiver", liqReserve, "amount", accepted),
		blockEvent(325, BlockPhaseEnd, "transfer", "recipient", liqReserve, "amount", accepted),
		blockEvent(326, BlockPhaseEnd, "coin_received", "receiver", blockOwner, "amount", "143801"+liqPoolCoin),
		blockEvent(327, BlockPhaseEnd, "transfer", "recipient", blockOwner, "amount", "143801"+liqPoolCoin),
		blockEvent(328, BlockPhaseEnd, "deposit_to_pool", "pool_id", "1", "batch_index", "42636", "msg_index", "7700",
			"depositor", blockOwner, "accepted_coins", accepted, "refunded_coins", refund,
			"pool_coin_denom", liqPoolCoin, "pool_coin_amount", "143801", "success", "success"),
	}
	rows := ClassifyBlockLifecycle(events, blockOwner, nil)
	if len(rows) != 3 {
		t.Fatalf("rows %+v", rows)
	}
	want := []struct{ from, to, amount, denom string }{
		{blockOwner, "", "883049", liqOffer},
		{blockOwner, "", "199999", fixtureNativeDenom},
		{"", blockOwner, "143801", liqPoolCoin},
	}
	for i, w := range want {
		r := rows[i]
		if r.Category != string(CategorySwap) || r.FromAddr != w.from || r.ToAddr != w.to || r.Amount != w.amount || r.Denom != w.denom || r.RewardTrigger != TriggerGravityDEXDeposit {
			t.Fatalf("row %d %+v", i, r)
		}
		if blockDetail(t, r)["review_required"] != "" {
			t.Fatalf("row %d needs review", i)
		}
	}
}

// Withdrawal settled at height 9000503 (batch 42665, msg 2006): the reserve
// pays the owner and the escrowed pool coins are burned.
func TestBlockGravityDEXWithdraw(t *testing.T) {
	paid := "14999995" + liqOffer + ",3403606uatom"
	events := []BlockEvent{
		blockEvent(314, BlockPhaseEnd, "coin_spent", attributeSpender, liqReserve, "amount", paid),
		blockEvent(315, BlockPhaseEnd, "message", "sender", liqReserve),
		blockEvent(316, BlockPhaseEnd, "coin_received", "receiver", blockOwner, "amount", paid),
		blockEvent(317, BlockPhaseEnd, "transfer", "recipient", blockOwner, "amount", paid),
		blockEvent(318, BlockPhaseEnd, "coin_spent", attributeSpender, liquidityEscrowAddress, "amount", "2444946"+liqPoolCoin),
		blockEvent(319, BlockPhaseEnd, "burn", "burner", liquidityEscrowAddress, "amount", "2444946"+liqPoolCoin),
		blockEvent(320, BlockPhaseEnd, "withdraw_from_pool", "pool_id", "1", "batch_index", "42665", "msg_index", "2006",
			"withdrawer", blockOwner, "pool_coin_denom", liqPoolCoin, "pool_coin_amount", "2444946",
			"withdraw_coins", paid, "withdraw_fee_coins", "0"+liqOffer+",0uatom", "success", "success"),
	}
	rows := ClassifyBlockLifecycle(events, blockOwner, nil)
	if len(rows) != 3 {
		t.Fatalf("rows %+v", rows)
	}
	if r := rows[0]; r.FromAddr != blockOwner || r.Amount != "2444946" || r.Denom != liqPoolCoin || r.RewardTrigger != TriggerGravityDEXWithdraw {
		t.Fatalf("disposal %+v", r)
	}
	if r := rows[1]; r.ToAddr != blockOwner || r.Amount != "14999995" || r.Denom != liqOffer {
		t.Fatalf("acquisition %+v", r)
	}
	if r := rows[2]; r.ToAddr != blockOwner || r.Amount != "3403606" || r.Denom != fixtureNativeDenom {
		t.Fatalf("acquisition %+v", r)
	}
}

// An unmatched order's escrow comes back with a failure event: no swap rows,
// and the return is not left for review.
func TestBlockGravityDEXUnmatchedSwapRefund(t *testing.T) {
	events := []BlockEvent{
		blockEvent(1, BlockPhaseEnd, "swap_transacted", "pool_id", "1", "batch_index", "7", "msg_index", "3",
			"swap_requester", blockOwner, "offer_coin_denom", fixtureNativeDenom, "demand_coin_denom", liqOffer,
			"remaining_offer_coin_amount", "1000", "reserved_offer_coin_fee_amount", "2", "success", "failure"),
		blockEvent(2, BlockPhaseEnd, "coin_spent", attributeSpender, liquidityEscrowAddress, "amount", "1002uatom"),
		blockEvent(3, BlockPhaseEnd, "coin_received", "receiver", blockOwner, "amount", "1002uatom"),
	}
	if rows := ClassifyBlockLifecycle(events, blockOwner, nil); len(rows) != 0 {
		t.Fatalf("rows %+v", rows)
	}
}

// A credit from the escrow that pairs with a different debit is not taken as
// a refund, so it still goes to review.
func TestBlockGravityDEXMismatchedEscrowCreditNeedsReview(t *testing.T) {
	events := []BlockEvent{
		blockEvent(1, BlockPhaseEnd, "coin_spent", attributeSpender, liqReserve, "amount", "5uatom"),
		blockEvent(2, BlockPhaseEnd, "coin_spent", attributeSpender, liquidityEscrowAddress, "amount", "7uatom"),
		blockEvent(3, BlockPhaseEnd, "coin_received", "receiver", blockOwner, "amount", "7uatom"),
		blockEvent(4, BlockPhaseEnd, "coin_received", "receiver", blockOther, "amount", "5uatom"),
	}
	row := onlyRow(t, ClassifyBlockLifecycle(events, blockOwner, nil))
	if blockDetail(t, row)["review_reason"] != ReviewUnclassifiedBlockMovement || row.Amount != "7" {
		t.Fatalf("row %+v", row)
	}
}

// Pool creation settles in the transaction. When the deposit and minted pool
// coins carry bank events (one InputOutputCoins, then FundCommunityPool for the
// fee), the pool coin amount comes from the creator's receipt.
func TestLiquidityCreatePoolSwapsDepositForPoolCoins(t *testing.T) {
	deposit := "1000000" + liqOffer + ",2000000uatom"
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev(eventCoinReceived, [2]string{attributeReceiver, liquidityEscrowAddress}, [2]string{attributeAmount, "1000000" + liqPoolCoin}),
		ev("coinbase", [2]string{"minter", liquidityEscrowAddress}, [2]string{attributeAmount, "1000000" + liqPoolCoin}),
		ev("coin_spent", [2]string{attributeSpender, del}, [2]string{attributeAmount, deposit}),
		ev("coin_spent", [2]string{attributeSpender, liquidityEscrowAddress}, [2]string{attributeAmount, "1000000" + liqPoolCoin}),
		ev(eventCoinReceived, [2]string{attributeReceiver, liqReserve}, [2]string{attributeAmount, deposit}),
		ev("transfer", [2]string{attributeRecipient, liqReserve}, [2]string{attributeAmount, deposit}),
		ev(eventCoinReceived, [2]string{attributeReceiver, del}, [2]string{attributeAmount, "1000000" + liqPoolCoin}),
		ev("transfer", [2]string{attributeRecipient, del}, [2]string{attributeAmount, "1000000" + liqPoolCoin}),
		ev("coin_spent", [2]string{attributeSpender, del}, [2]string{attributeAmount, liqCreationFee}),
		ev(eventCoinReceived, [2]string{attributeReceiver, distributionModuleAddress}, [2]string{attributeAmount, liqCreationFee}),
		ev("transfer", [2]string{attributeRecipient, distributionModuleAddress}, [2]string{attributeSender, del}, [2]string{attributeAmount, liqCreationFee}),
		ev("create_pool", [2]string{"pool_id", "5"}, [2]string{"pool_type_id", "1"}, [2]string{"reserve_account", liqReserve},
			[2]string{"deposit_coins", deposit}, [2]string{"pool_coin_denom", liqPoolCoin}),
	}}
	msg := &MsgLiquidityCreatePool{PoolCreator: del, PoolTypeID: 1, DepositCoins: sdk.NewCoins(sdk.NewInt64Coin(liqOffer, 1000000), sdk.NewInt64Coin(fixtureNativeDenom, 2000000))}
	out := classify(msg, log)
	if len(out) != 4 {
		t.Fatalf("rows %+v", out)
	}
	for i := 0; i < 2; i++ {
		if out[i].Category != string(CategorySwap) || out[i].FromAddr != del || out[i].RewardTrigger != TriggerGravityDEXCreatePool {
			t.Fatalf("deposit leg %+v", out[i])
		}
	}
	if r := out[2]; r.Category != string(CategorySwap) || r.ToAddr != del || r.Amount != "1000000" || r.Denom != liqPoolCoin {
		t.Fatalf("pool coin leg %+v", r)
	}
	if r := out[3]; r.Category != string(CategoryExpense) || r.Amount != "40000000" || r.ToAddr != distributionModuleAddress || r.RewardTrigger != TriggerGravityDEXPoolCreationFee {
		t.Fatalf("fee %+v", r)
	}
}

// Mainnet pool 15 at 9229836: the module emitted no bank events for the
// deposit or the minted pool coins, only the fee transfer and create_pool.
func TestLiquidityCreatePoolMainnetShapeUsesInitialMint(t *testing.T) {
	const (
		poolDenom  = "poolFD005C5AB01714A4B62E87F5213F5D5CDE357773D70712916A93664BCE5A6931"
		depositIBC = "ibc/ADBEC1A7AC2FEF73E06B066A1C94DAB6C27924EF7EA3F5A43378150009620284"
		txFee      = "5000uatom"
	)
	feeCollector := authtypes.NewModuleAddress(authtypes.FeeCollectorName).String()
	log := &indexerTxTypes.LogMessage{Events: []indexerTxTypes.LogMessageEvent{
		ev(eventCoinSpent, [2]string{attributeSpender, del}, [2]string{attributeAmount, txFee}),
		ev(eventCoinReceived, [2]string{attributeReceiver, feeCollector}, [2]string{attributeAmount, txFee}),
		ev("transfer", [2]string{attributeRecipient, feeCollector}, [2]string{attributeSender, del}, [2]string{attributeAmount, txFee}),
		ev(eventCoinSpent, [2]string{attributeSpender, del}, [2]string{attributeAmount, liqCreationFee}),
		ev(eventCoinReceived, [2]string{attributeReceiver, distributionModuleAddress}, [2]string{attributeAmount, liqCreationFee}),
		ev("transfer", [2]string{attributeRecipient, distributionModuleAddress}, [2]string{attributeSender, del}, [2]string{attributeAmount, liqCreationFee}),
		ev("create_pool", [2]string{attributePoolID, "15"}, [2]string{"pool_type_id", "1"}, [2]string{"reserve_account", liqReserve},
			[2]string{"deposit_coins", "299000000" + depositIBC + ",1100000uatom"}, [2]string{"pool_coin_denom", poolDenom}),
	}}
	msg := &MsgLiquidityCreatePool{PoolCreator: del, PoolTypeID: 1, DepositCoins: sdk.NewCoins(sdk.NewInt64Coin(depositIBC, 299000000), sdk.NewInt64Coin(fixtureNativeDenom, 1100000))}
	out := classify(msg, log)
	if len(out) != 4 {
		t.Fatalf("rows %+v", out)
	}
	if r := out[2]; r.Category != string(CategorySwap) || r.ToAddr != del || r.Amount != "1000000" || r.Denom != poolDenom ||
		!strings.Contains(r.Detail, `"pool_coin_mint":"init_pool_coin_mint_amount"`) {
		t.Fatalf("pool coin leg %+v", r)
	}
	if r := out[3]; r.Category != string(CategoryExpense) || r.Amount != "40000000" || r.Denom != fixtureNativeDenom {
		t.Fatalf("fee %+v", r)
	}
}
