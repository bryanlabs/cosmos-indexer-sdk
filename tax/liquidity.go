package tax

import (
	"fmt"
	"strings"

	sdkmath "cosmossdk.io/math"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// Gravity DEX is Cosmos Hub's x/liquidity module (Gravity-Devs/liquidity),
// live from Gaia v5 (July 2021) until Gaia v11 retired it. Its proto package
// is tendermint.liquidity.v1beta1, which this build does not link, so the
// messages are decoded here. Field numbers come from
// proto/tendermint/liquidity/v1beta1/tx.proto, identical in v1.2.9, v1.4.5 and
// v1.6.0-forced-withdrawal.
//
// Deposits, withdrawals and swaps are batch messages: the transaction only
// moves the sender's coins into the module's batch escrow, and the batch
// settles in EndBlock of the same block, which emits deposit_to_pool,
// withdraw_from_pool and swap_transacted with the settled amounts. The
// transaction therefore records nothing; the swap legs are read from those
// block events by liquidityBlockEvents. Creating a pool settles inside the
// transaction.

// #nosec G101 -- liquidity message identifiers are public on-chain type URLs.
const (
	TypeURLLiquidityCreatePool          = "/tendermint.liquidity.v1beta1.MsgCreatePool"
	TypeURLLiquidityDepositWithinBatch  = "/tendermint.liquidity.v1beta1.MsgDepositWithinBatch"
	TypeURLLiquidityWithdrawWithinBatch = "/tendermint.liquidity.v1beta1.MsgWithdrawWithinBatch"
	TypeURLLiquiditySwapWithinBatch     = "/tendermint.liquidity.v1beta1.MsgSwapWithinBatch"

	// liquidityEscrowAddress is the liquidity module account, which holds
	// batch escrow and receives newly minted pool coins.
	liquidityEscrowAddress = "cosmos1tx68a8k9yz54z06qfve9l2zxvgsz4ka3hr8962"

	protocolGravityDEX = "gravity_dex"

	TriggerGravityDEXSwap            = "gravity_dex_swap"
	TriggerGravityDEXDeposit         = "gravity_dex_deposit"
	TriggerGravityDEXWithdraw        = "gravity_dex_withdraw"
	TriggerGravityDEXCreatePool      = "gravity_dex_create_pool"
	TriggerGravityDEXPoolCreationFee = "gravity_dex_pool_creation_fee"

	liquiditySuccess      = "success"
	liquidityKindDeposit  = "deposit"
	liquidityKindWithdraw = "withdraw"
)

// liquidityInitPoolCoinMint is the module's InitPoolCoinMintAmount, the pool
// coins a new pool mints to its creator. The param store held 1000000 from
// the module's launch (6910001) to its removal, and pool 15's supply went
// from 0 to 1000000 in its creation block.
var liquidityInitPoolCoinMint = sdkmath.NewInt(1_000_000)

// LiquidityMsgTypes maps the Gravity DEX type URLs to registrable instances.
func LiquidityMsgTypes() map[string]sdk.Msg {
	return map[string]sdk.Msg{
		TypeURLLiquidityCreatePool:          &MsgLiquidityCreatePool{},
		TypeURLLiquidityDepositWithinBatch:  &MsgLiquidityDepositWithinBatch{},
		TypeURLLiquidityWithdrawWithinBatch: &MsgLiquidityWithdrawWithinBatch{},
		TypeURLLiquiditySwapWithinBatch:     &MsgLiquiditySwapWithinBatch{},
	}
}

func decodeCoin(b []byte) (sdk.Coin, error) {
	var denom, amount string
	if err := scanFields(b, func(w wireField) error {
		switch w.num {
		case 1:
			denom = string(w.bytes)
		case 2:
			amount = string(w.bytes)
		}
		return nil
	}); err != nil {
		return sdk.Coin{}, err
	}
	amt, ok := sdkmath.NewIntFromString(amount)
	if !ok {
		return sdk.Coin{}, fmt.Errorf("bad coin amount %q", amount)
	}
	return sdk.Coin{Denom: denom, Amount: amt}, nil
}

// MsgLiquidityCreatePool: pool_creator_address 1, pool_type_id 2,
// deposit_coins 4.
type MsgLiquidityCreatePool struct {
	rawMsg
	PoolCreator  string
	PoolTypeID   uint64
	DepositCoins sdk.Coins
}

func (m *MsgLiquidityCreatePool) Reset()                       { *m = MsgLiquidityCreatePool{} }
func (*MsgLiquidityCreatePool) ProtoMessage()                  {}
func (*MsgLiquidityCreatePool) ValidateBasic() error           { return nil }
func (m *MsgLiquidityCreatePool) GetSigners() []sdk.AccAddress { return signerOf(m.PoolCreator) }

func (m *MsgLiquidityCreatePool) String() string {
	return fmt.Sprintf("MsgCreatePool{%s %s}", m.PoolCreator, m.DepositCoins)
}

func (m *MsgLiquidityCreatePool) Unmarshal(b []byte) error {
	*m = MsgLiquidityCreatePool{}
	m.keep(b)
	return scanFields(b, func(w wireField) error {
		switch w.num {
		case 1:
			m.PoolCreator = string(w.bytes)
		case 2:
			m.PoolTypeID = w.varint
		case 4:
			c, err := decodeCoin(w.bytes)
			if err != nil {
				return err
			}
			m.DepositCoins = append(m.DepositCoins, c)
		}
		return nil
	})
}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*MsgLiquidityCreatePool) XXX_MessageName() string { return TypeURLLiquidityCreatePool[1:] }

// MsgLiquidityDepositWithinBatch: depositor_address 1, pool_id 2,
// deposit_coins 3.
type MsgLiquidityDepositWithinBatch struct {
	rawMsg
	Depositor    string
	PoolID       uint64
	DepositCoins sdk.Coins
}

func (m *MsgLiquidityDepositWithinBatch) Reset()                       { *m = MsgLiquidityDepositWithinBatch{} }
func (*MsgLiquidityDepositWithinBatch) ProtoMessage()                  {}
func (*MsgLiquidityDepositWithinBatch) ValidateBasic() error           { return nil }
func (m *MsgLiquidityDepositWithinBatch) GetSigners() []sdk.AccAddress { return signerOf(m.Depositor) }

func (m *MsgLiquidityDepositWithinBatch) String() string {
	return fmt.Sprintf("MsgDepositWithinBatch{%s %d %s}", m.Depositor, m.PoolID, m.DepositCoins)
}

func (m *MsgLiquidityDepositWithinBatch) Unmarshal(b []byte) error {
	*m = MsgLiquidityDepositWithinBatch{}
	m.keep(b)
	return scanFields(b, func(w wireField) error {
		switch w.num {
		case 1:
			m.Depositor = string(w.bytes)
		case 2:
			m.PoolID = w.varint
		case 3:
			c, err := decodeCoin(w.bytes)
			if err != nil {
				return err
			}
			m.DepositCoins = append(m.DepositCoins, c)
		}
		return nil
	})
}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*MsgLiquidityDepositWithinBatch) XXX_MessageName() string {
	return TypeURLLiquidityDepositWithinBatch[1:]
}

// MsgLiquidityWithdrawWithinBatch: withdrawer_address 1, pool_id 2,
// pool_coin 3.
type MsgLiquidityWithdrawWithinBatch struct {
	rawMsg
	Withdrawer string
	PoolID     uint64
	PoolCoin   sdk.Coin
}

func (m *MsgLiquidityWithdrawWithinBatch) Reset()             { *m = MsgLiquidityWithdrawWithinBatch{} }
func (*MsgLiquidityWithdrawWithinBatch) ProtoMessage()        {}
func (*MsgLiquidityWithdrawWithinBatch) ValidateBasic() error { return nil }
func (m *MsgLiquidityWithdrawWithinBatch) GetSigners() []sdk.AccAddress {
	return signerOf(m.Withdrawer)
}

func (m *MsgLiquidityWithdrawWithinBatch) String() string {
	return fmt.Sprintf("MsgWithdrawWithinBatch{%s %d %s}", m.Withdrawer, m.PoolID, m.PoolCoin)
}

func (m *MsgLiquidityWithdrawWithinBatch) Unmarshal(b []byte) error {
	*m = MsgLiquidityWithdrawWithinBatch{}
	m.keep(b)
	return scanFields(b, func(w wireField) error {
		switch w.num {
		case 1:
			m.Withdrawer = string(w.bytes)
		case 2:
			m.PoolID = w.varint
		case 3:
			c, err := decodeCoin(w.bytes)
			if err != nil {
				return err
			}
			m.PoolCoin = c
		}
		return nil
	})
}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*MsgLiquidityWithdrawWithinBatch) XXX_MessageName() string {
	return TypeURLLiquidityWithdrawWithinBatch[1:]
}

// MsgLiquiditySwapWithinBatch: swap_requester_address 1, pool_id 2,
// swap_type_id 3, offer_coin 4, demand_coin_denom 5, offer_coin_fee 6,
// order_price 7.
type MsgLiquiditySwapWithinBatch struct {
	rawMsg
	SwapRequester   string
	PoolID          uint64
	SwapTypeID      uint64
	OfferCoin       sdk.Coin
	DemandCoinDenom string
	OfferCoinFee    sdk.Coin
	OrderPrice      string
}

func (m *MsgLiquiditySwapWithinBatch) Reset()                       { *m = MsgLiquiditySwapWithinBatch{} }
func (*MsgLiquiditySwapWithinBatch) ProtoMessage()                  {}
func (*MsgLiquiditySwapWithinBatch) ValidateBasic() error           { return nil }
func (m *MsgLiquiditySwapWithinBatch) GetSigners() []sdk.AccAddress { return signerOf(m.SwapRequester) }

func (m *MsgLiquiditySwapWithinBatch) String() string {
	return fmt.Sprintf("MsgSwapWithinBatch{%s %d %s %s}", m.SwapRequester, m.PoolID, m.OfferCoin, m.DemandCoinDenom)
}

func (m *MsgLiquiditySwapWithinBatch) Unmarshal(b []byte) error {
	*m = MsgLiquiditySwapWithinBatch{}
	m.keep(b)
	return scanFields(b, func(w wireField) error {
		switch w.num {
		case 1:
			m.SwapRequester = string(w.bytes)
		case 2:
			m.PoolID = w.varint
		case 3:
			m.SwapTypeID = w.varint
		case 4, 6:
			c, err := decodeCoin(w.bytes)
			if err != nil {
				return err
			}
			if w.num == 4 {
				m.OfferCoin = c
			} else {
				m.OfferCoinFee = c
			}
		case 5:
			m.DemandCoinDenom = string(w.bytes)
		case 7:
			m.OrderPrice = string(w.bytes)
		}
		return nil
	})
}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*MsgLiquiditySwapWithinBatch) XXX_MessageName() string {
	return TypeURLLiquiditySwapWithinBatch[1:]
}

// liquidityCreatePoolEvents swaps the deposit for the pool coins minted to the
// creator and books the pool creation fee, paid to the community pool, as an
// expense.
func liquidityCreatePoolEvents(log *txlog.LogMessage, m *MsgLiquidityCreatePool) ([]TaxableEvent, []string) {
	if log == nil {
		return nil, nil
	}
	var created map[string]string
	for _, ev := range log.Events {
		if ev.Type == "create_pool" {
			created = ibcAttrs(ev)
		}
	}
	poolDenom := created["pool_coin_denom"]
	if poolDenom == "" {
		return nil, []string{"create_pool event missing pool coin denom"}
	}
	if m.DepositCoins.Empty() {
		return nil, []string{"pool creation without deposit coins"}
	}
	// On mainnet the module moved the deposit and minted the pool coins
	// without bank events (pool 15 at 9229836 has only create_pool and the
	// fee), so a missing receipt means the fixed initial mint.
	minted := ibcCredits(log)[m.PoolCreator].AmountOf(poolDenom)
	mintSource := "coin_received"
	if !minted.IsPositive() {
		minted, mintSource = liquidityInitPoolCoinMint, "init_pool_coin_mint_amount"
	}
	group := strings.Join([]string{protocolGravityDEX, "create", created[attributePoolID]}, ":")
	detail := detailJSON(map[string]string{
		detailProtocol: protocolGravityDEX, attributePoolID: created[attributePoolID], "swap_group": group, "pool_coin_mint": mintSource,
	})
	var out []TaxableEvent
	for _, c := range m.DepositCoins {
		out = append(out, TaxableEvent{
			Category: string(CategorySwap), FromAddr: m.PoolCreator, ToAddr: created["reserve_account"],
			Amount: c.Amount.String(), Denom: c.Denom, RewardTrigger: TriggerGravityDEXCreatePool, Detail: detail,
		})
	}
	out = append(out, TaxableEvent{
		Category: string(CategorySwap), ToAddr: m.PoolCreator,
		Amount: minted.String(), Denom: poolDenom, RewardTrigger: TriggerGravityDEXCreatePool, Detail: detail,
	})
	for _, c := range transfersFromSender(log, m.PoolCreator)[distributionModuleAddress] {
		out = append(out, TaxableEvent{
			Category: string(CategoryExpense), FromAddr: m.PoolCreator, ToAddr: distributionModuleAddress,
			Amount: c.Amount.String(), Denom: c.Denom, RewardTrigger: TriggerGravityDEXPoolCreationFee,
			Detail: detailJSON(map[string]string{detailProtocol: detailCommunityPool, attributePoolID: created[attributePoolID]}),
		})
	}
	return out, nil
}

// liquidityBlockEvents turns the owner's settled Gravity DEX batch results in
// one EndBlock into swap legs. Each result's legs share a swap_group, since
// block rows have no transaction hash to group them by. addIn records what the
// owner was credited, for the bank residual check.
//
// Refunds (unaccepted deposit coins, unmatched offer coins, a failed
// withdrawal's pool coins) are the owner's own escrowed coins coming back, so
// they get no row: liquidityEscrowReturns counts them as classified.
func liquidityBlockEvents(events []BlockEvent, owner string, addIn func(string, sdkmath.Int)) []TaxableEvent {
	var out []TaxableEvent
	escrowed := map[string]sdkmath.Int{}
	for _, e := range events {
		a := blockAttrs(e)
		var who, kind, trigger string
		switch e.Type {
		case "swap_transacted":
			who, kind, trigger = a["swap_requester"], "swap", TriggerGravityDEXSwap
		case "deposit_to_pool":
			who, kind, trigger = a["depositor"], liquidityKindDeposit, TriggerGravityDEXDeposit
		case "withdraw_from_pool":
			who, kind, trigger = a["withdrawer"], liquidityKindWithdraw, TriggerGravityDEXWithdraw
		default:
			continue
		}
		if who != owner || a[attributeSuccess] != liquiditySuccess {
			continue
		}
		kv := map[string]string{
			detailProtocol:  protocolGravityDEX,
			attributePoolID: a[attributePoolID],
			"batch_index":   a["batch_index"],
			"msg_index":     a["msg_index"],
			"swap_group":    strings.Join([]string{protocolGravityDEX, kind, a[attributePoolID], a["batch_index"], a["msg_index"]}, ":"),
		}
		var given, got []blockCoin
		switch kind {
		case "swap":
			kv["swap_price"] = a["swap_price"]
			offered := intAttr(a["transacted_coin_amount"]).Add(intAttr(a["offer_coin_fee_amount"]))
			if offered.IsPositive() {
				given = append(given, blockCoin{event: e, amount: offered, denom: a["offer_coin_denom"]})
			}
			if r := intAttr(a["exchanged_demand_coin_amount"]); r.IsPositive() {
				got = append(got, blockCoin{event: e, amount: r, denom: a["demand_coin_denom"]})
			}
		case liquidityKindDeposit:
			given = blockCoins(e, a["accepted_coins"])
			if p := intAttr(a["pool_coin_amount"]); p.IsPositive() {
				got = append(got, blockCoin{event: e, amount: p, denom: a["pool_coin_denom"]})
				escrowed[a["pool_coin_denom"]] = addInt(escrowed[a["pool_coin_denom"]], p)
			}
		case liquidityKindWithdraw:
			if p := intAttr(a["pool_coin_amount"]); p.IsPositive() {
				given = append(given, blockCoin{event: e, amount: p, denom: a["pool_coin_denom"]})
			}
			got = blockCoins(e, a["withdraw_coins"])
		}
		for _, c := range given {
			out = append(out, blockRow(c, CategorySwap, owner, "", "", trigger, kv))
		}
		for _, c := range got {
			addIn(c.denom, c.amount)
			out = append(out, blockRow(c, CategorySwap, "", owner, "", trigger, kv))
		}
	}
	for denom, amount := range liquidityEscrowReturns(events, owner) {
		if v, ok := escrowed[denom]; ok {
			amount = amount.Sub(v)
		}
		if amount.IsPositive() {
			addIn(denom, amount)
		}
	}
	return out
}

// liquidityEscrowReturns sums what the batch escrow paid the owner. The
// module pays out with InputOutputCoins, which emits every input's coin_spent
// before the outputs' coin_received, in matching order, so the n-th credit of
// a run came from the n-th debit. A pair whose amounts differ is not counted.
func liquidityEscrowReturns(events []BlockEvent, owner string) map[string]sdkmath.Int {
	out := map[string]sdkmath.Int{}
	type debit struct{ spender, amount string }
	var queue []debit
	inDebits := false
	for _, e := range events {
		a := blockAttrs(e)
		switch e.Type {
		case eventCoinSpent:
			if !inDebits {
				queue = nil
				inDebits = true
			}
			queue = append(queue, debit{a[attributeSpender], a[attributeAmount]})
		case eventBurn:
			// A burn's coin_spent has no matching credit.
			if len(queue) > 0 {
				queue = queue[:len(queue)-1]
			}
		case eventCoinReceived:
			inDebits = false
			if len(queue) == 0 {
				continue
			}
			d := queue[0]
			queue = queue[1:]
			if d.spender != liquidityEscrowAddress || a[attributeReceiver] != owner || d.amount != a[attributeAmount] {
				continue
			}
			for _, c := range blockCoins(e, a[attributeAmount]) {
				out[c.denom] = addInt(out[c.denom], c.amount)
			}
		}
	}
	return out
}

func intAttr(s string) sdkmath.Int {
	v, ok := sdkmath.NewIntFromString(s)
	if !ok || v.IsNegative() {
		return sdkmath.ZeroInt()
	}
	return v
}

func addInt(a, b sdkmath.Int) sdkmath.Int {
	if a.IsNil() {
		return b
	}
	return a.Add(b)
}
