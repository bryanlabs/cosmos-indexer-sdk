package tax

import (
	"encoding/json"
	"sort"
	"strings"

	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// This file holds the classification rules for protocol activity beyond plain
// bank, staking and IBC messages: NFT auctions, Hydro, Inflow vaults, DAO
// reward distributors, raffles, name services, the ICS721 bridge and Skip Go
// routing. Every rule reads events only, so the same code classifies a live
// message and a replay from the stored event log (taxrepair).

// detailJSON renders the Detail column. Keys are sorted so two runs over the
// same events produce byte-identical rows, which the repair relies on.
func detailJSON(kv map[string]string) string {
	if len(kv) == 0 {
		return ""
	}
	keys := make([]string, 0, len(kv))
	for k, v := range kv {
		if v != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(kv[k])
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.String()
}

// contractDetail is the standard Detail for a contract interaction.
func contractDetail(addr string, extra map[string]string) string {
	kv := map[string]string{"contract": addr}
	if c, ok := LookupContract(addr); ok {
		kv[detailProtocol] = c.Protocol
		kv["name"] = c.Name
	}
	for k, v := range extra {
		kv[k] = v
	}
	return detailJSON(kv)
}

// coinTransfer is one bank transfer event inside a message.
type coinTransfer struct {
	from, to string
	coins    sdk.Coins
}

// transfersIn lists the message's transfer events in order.
func transfersIn(log *indexerTxTypes.LogMessage) []coinTransfer {
	var out []coinTransfer
	for _, ev := range log.Events {
		if ev.Type != string(CategoryTransfer) {
			continue
		}
		for _, a := range ibcAttributeGroups(ev, attributeSender, attributeRecipient, attributeAmount) {
			coins, err := sdk.ParseCoinsNormalized(a[attributeAmount])
			if err == nil && a[attributeRecipient] != "" && !coins.Empty() {
				out = append(out, coinTransfer{from: a[attributeSender], to: a[attributeRecipient], coins: coins})
			}
		}
	}
	return out
}

// coinsBetween sums the coins moved from one address to another.
func coinsBetween(log *indexerTxTypes.LogMessage, from, to string) sdk.Coins {
	total := sdk.NewCoins()
	for _, t := range transfersIn(log) {
		if t.from == from && t.to == to {
			total = total.Add(t.coins...)
		}
	}
	return total
}

// coinsFrom sums everything one address paid out in the message, by recipient.
func coinsFrom(log *indexerTxTypes.LogMessage, from string) map[string]sdk.Coins {
	out := map[string]sdk.Coins{}
	for _, t := range transfersIn(log) {
		if t.from == from {
			out[t.to] = out[t.to].Add(t.coins...)
		}
	}
	return out
}

// messageSender is the signer recorded by baseapp's message event.
func messageSender(log *indexerTxTypes.LogMessage) string {
	for _, ev := range log.Events {
		if ev.Type != "message" {
			continue
		}
		a := attrMap(ev)
		if a[attributeAction] != "" && a[attributeSender] != "" {
			return a[attributeSender]
		}
	}
	return ""
}

func coinRows(category Category, coins sdk.Coins, from, to, trigger, detail string) []TaxableEvent {
	var out []TaxableEvent
	for _, c := range coins {
		if !c.IsPositive() {
			continue
		}
		out = append(out, TaxableEvent{
			Category: string(category), FromAddr: from, ToAddr: to,
			Amount: c.Amount.String(), Denom: c.Denom,
			RewardTrigger: trigger, Detail: detail,
		})
	}
	return out
}

// --- NFT auctions and inscription sales --------------------------------------

// auctionSaleEvents classifies the settlement events of the auction-style
// marketplaces on Cosmos Hub as nft_sale rows, the same shape the Stargaze
// Marketplace v2 finalize-sale rule produces: seller disposes, buyer acquires,
// Amount and Denom are the price.
func auctionSaleEvents(log *indexerTxTypes.LogMessage) []TaxableEvent {
	var out []TaxableEvent
	for _, ev := range log.Events {
		a := attrMap(ev)
		switch ev.Type {
		case eventSettleAuction:
			// Stargaze reserve auction: bid_amount is a bare integer, bid_denom
			// the denom. wasm-buy-now in the same message carries the same sale.
			if a["collection"] == "" || a["bid_amount"] == "" {
				continue
			}
			out = append(out, TaxableEvent{
				Category: string(CategoryNFTSale),
				FromAddr: a["seller"], ToAddr: a["bidder"],
				Amount: a["bid_amount"], Denom: a["bid_denom"],
				Asset:  a["collection"] + "/" + a["token_id"],
				Detail: contractDetail(a[attributeContractAddress], map[string]string{detailSale: contractRoleAuction}),
			})
		case "wasm-auction_settled":
			// Inscriptions auction: the asset is an inscription id on the
			// contract itself; amount is the winning bid in `denomination`.
			if a["inscription_id"] == "" || a[attributeAmount] == "" {
				continue
			}
			out = append(out, TaxableEvent{
				Category: string(CategoryNFTSale),
				FromAddr: a["seller"], ToAddr: a["winner"],
				Amount: a[attributeAmount], Denom: a["denomination"],
				Asset:  a[attributeContractAddress] + "/" + a["inscription_id"],
				Detail: contractDetail(a[attributeContractAddress], map[string]string{detailSale: contractRoleAuction, "platform_fee": a["platform_fee_amount"]}),
			})
		case "wasm-funded_offer_completed":
			// Inscriptions funded offer: gross is what the buyer paid.
			if a["inscription_id"] == "" || a["gross"] == "" {
				continue
			}
			out = append(out, TaxableEvent{
				Category: string(CategoryNFTSale),
				FromAddr: a["seller"], ToAddr: a["buyer"],
				Amount: a["gross"], Denom: a[attributeDenom],
				Asset:  a[attributeContractAddress] + "/" + a["inscription_id"],
				Detail: contractDetail(a[attributeContractAddress], map[string]string{detailSale: "offer", "platform_fee": a["fee"]}),
			})
		}
	}
	return out
}

// auctionEscrowEvents records bids placed into and refunded out of an auction
// contract. A bid is the bidder's own money held in escrow until the auction
// settles, so it is a protocol_deposit; an outbid bidder's refund is the
// matching protocol_withdrawal. Settlement itself is the nft_sale above.
func auctionEscrowEvents(log *indexerTxTypes.LogMessage) []TaxableEvent {
	var out []TaxableEvent
	for _, ev := range log.Events {
		if ev.Type != "wasm-place-bid" && ev.Type != "wasm-buy-now" && ev.Type != eventSettleAuction && ev.Type != "wasm-cancel-auction" {
			continue
		}
		a := attrMap(ev)
		contract := a[attributeContractAddress]
		if contract == "" {
			continue
		}
		detail := contractDetail(contract, map[string]string{"collection": a["collection"], "token_id": a["token_id"]})
		if ev.Type == "wasm-place-bid" && a["bidder"] != "" {
			out = append(out, coinRows(CategoryProtocolDeposit, coinsBetween(log, a["bidder"], contract), a["bidder"], contract, "auction_bid", detail)...)
		}
		// Anyone the contract paid other than the seller side of a settlement
		// is an outbid bidder getting their escrow back.
		winner := a["bidder"]
		if winner == "" {
			winner = a["buyer"]
		}
		for to, coins := range coinsFrom(log, contract) {
			if to == a["seller"] || to == winner || to == a["bidder"] {
				continue
			}
			if ev.Type == eventSettleAuction || ev.Type == "wasm-buy-now" {
				// Settlement pays the seller, royalties and the platform fee;
				// only the previous bidder's refund is a withdrawal of own funds.
				if to != a["previous_bidder"] {
					continue
				}
			}
			out = append(out, coinRows(CategoryProtocolWithdrawal, coins, contract, to, "auction_refund", detail)...)
		}
	}
	return out
}

// --- cw721 NFT movements ------------------------------------------------------

// nftTransferEvents records cw721 transfer_nft / send_nft / burn events that
// are not already explained by a sale or mint in the same message: gifts,
// marketplace escrow, raffle prizes, bridge deposits. These carry no price and
// are record-only; a sale of the same asset in the same message wins.
func nftTransferEvents(log *indexerTxTypes.LogMessage, sold map[string]bool, minted map[string]bool, trigger string) []TaxableEvent {
	var out []TaxableEvent
	owner := ""
	bridgeReceiver := ""
	bridgeContract := ""
	for _, ev := range log.Events {
		if ev.Type != eventWasm {
			continue
		}
		a := attrMap(ev)
		// The ICS721 outgoing proxy names the real owner and the destination
		// chain receiver before the collection emits its transfer event.
		if a["nft_owner"] != "" {
			owner = a["nft_owner"]
		}
		if a[attributeReceiver] != "" && a["channel_id"] != "" {
			bridgeReceiver = a[attributeReceiver]
		}
		if a["method"] == "execute_receive_nft" {
			bridgeContract = a[attributeContractAddress]
		}
	}
	for _, ev := range log.Events {
		if ev.Type != eventWasm {
			continue
		}
		a := attrMap(ev)
		action := a[attributeAction]
		if a["token_id"] == "" || a[attributeContractAddress] == "" {
			continue
		}
		asset := a[attributeContractAddress] + "/" + a["token_id"]
		switch action {
		case "transfer_nft", "send_nft":
			if sold[asset] || minted[asset] {
				continue
			}
			from, to := a[attributeSender], a[attributeRecipient]
			if owner != "" && contractKind(from) == "bridge_helper" {
				from = owner
			}
			// A contract moving a user's NFT into its own custody as an
			// approved operator (marketplace listing, game entry) reports
			// itself as both sender and recipient; the NFT came from the
			// message sender.
			if from == to && from != "" {
				if s := messageSender(log); s != "" {
					from = s
				}
			}
			t := trigger
			extra := map[string]string{}
			if bridgeContract != "" && to == bridgeContract {
				t = "ics721_out"
				extra["destination_receiver"] = bridgeReceiver
			}
			if k := contractKind(to); k == contractRoleMarketplace || k == contractRoleAuction || k == contractRoleRaffle {
				extra["escrow"] = k
			}
			if t == protocolPrize {
				extra["source"] = protocolPrize
			}
			out = append(out, TaxableEvent{
				Category: string(CategoryNFTTransfer),
				FromAddr: from, ToAddr: to, Asset: asset,
				RewardTrigger: t,
				Detail:        contractDetail(a[attributeContractAddress], extra),
			})
		case "burn":
			from := a["owner"]
			if from == "" {
				from = a[attributeSender]
			}
			out = append(out, TaxableEvent{
				Category: string(CategoryNFTBurn),
				FromAddr: from, Asset: asset,
				Detail: contractDetail(a[attributeContractAddress], nil),
			})
		}
	}
	return out
}

// --- protocol actions on wasm events ------------------------------------------

// protocolActionEvents classifies the generic `wasm` events whose action names
// a protocol operation with a known financial meaning.
func protocolActionEvents(log *indexerTxTypes.LogMessage) []TaxableEvent {
	var out []TaxableEvent
	sender := messageSender(log)
	unknownClaims := map[string]bool{}
	for _, ev := range log.Events {
		if ev.Type != eventWasm {
			continue
		}
		a := attrMap(ev)
		contract := a[attributeContractAddress]
		if contract == "" {
			continue
		}
		kind := contractKind(contract)
		switch a[attributeAction] {
		// Hydro: ATOM (as LSM shares) locked for voting power. Still the
		// user's asset, so a deposit, not a disposal.
		case "lock_tokens":
			if coins, err := sdk.ParseCoinsNormalized(a["locked_tokens"]); err == nil && a[attributeSender] != "" {
				detail := contractDetail(contract, map[string]string{"lock_id": a["lock_id"]})
				out = append(out, coinRows(CategoryProtocolDeposit, coins, a[attributeSender], contract, "hydro_lock", detail)...)
			}
		case "unlock_tokens":
			if coins, err := sdk.ParseCoinsNormalized(a["unlocked_tokens"]); err == nil && a[attributeSender] != "" {
				detail := contractDetail(contract, map[string]string{"lock_ids": a["unlocked_lock_ids"], "slashed_tokens": a["slashed_tokens"]})
				out = append(out, coinRows(CategoryProtocolWithdrawal, coins, contract, a[attributeSender], "hydro_unlock", detail)...)
			}
		// Hydro tribute: a project pays voters; the voter's claim is income,
		// the project's deposit is an expense.
		case "claim_tribute":
			if coins, err := sdk.ParseCoinsNormalized(a["tribute_amount"]); err == nil && a["tribute_receiver"] != "" {
				detail := contractDetail(contract, map[string]string{"round_id": a["round_id"], "tranche_id": a["tranche_id"], attributeProposalID: a[attributeProposalID], "tribute_id": a["tribute_id"]})
				out = append(out, coinRows(CategoryIncome, coins, contract, a["tribute_receiver"], protocolTribute, detail)...)
			}
		case "add_tribute":
			if coins, err := sdk.ParseCoinsNormalized(a["funds"]); err == nil && a["depositor"] != "" {
				detail := contractDetail(contract, map[string]string{"round_id": a["round_id"], "tranche_id": a["tranche_id"], attributeProposalID: a[attributeProposalID], "tribute_id": a["tribute_id"]})
				out = append(out, coinRows(CategoryExpense, coins, a["depositor"], contract, "tribute_paid", detail)...)
			}
		// Inflow vault: ATOM in, vault shares out (and back). A conversion
		// between two assets, so two swap legs, the same as a DEX trade.
		case "deposit":
			if a["vault_shares_minted"] != "" && a["deposit_amount"] != "" {
				out = append(out, vaultLegs(log, contract, a[attributeSender], a[attributeRecipient], a["deposit_amount"], a["vault_shares_minted"], true)...)
			}
		case "withdraw":
			if a["vault_shares_sent"] != "" && a["paid_out_amount"] != "" {
				payee := a["withdrawer"]
				if payee == "" {
					payee = a[attributeSender]
				}
				out = append(out, vaultLegs(log, contract, a[attributeSender], payee, a["paid_out_amount"], a["vault_shares_sent"], false)...)
			}
		// DAO token staking: stake and the later claim move the user's own
		// tokens; the distributor's claim pays rewards.
		case "stake":
			if kind == contractRoleDAOStaking && a["from"] != "" {
				coins := coinsBetween(log, a["from"], contract)
				out = append(out, coinRows(CategoryProtocolDeposit, coins, a["from"], contract, "dao_stake", contractDetail(contract, nil))...)
			}
		case actionClaim:
			switch {
			case kind == contractRoleDAOStaking && a["from"] != "":
				coins := coinsBetween(log, contract, a["from"])
				out = append(out, coinRows(CategoryProtocolWithdrawal, coins, contract, a["from"], "dao_unstake", contractDetail(contract, nil))...)
			case kind == attributeRewards:
				trigger := "dao_rewards"
				extra := map[string]string{"distribution_id": a["id"]}
				if a["user"] != "" {
					trigger = "governance_score"
				}
				for to, coins := range coinsFrom(log, contract) {
					if a["user"] != "" && to != a["user"] {
						continue
					}
					out = append(out, coinRows(CategoryIncome, coins, contract, to, trigger, contractDetail(contract, extra))...)
				}
			default:
				// A claim can return principal, pay rewards, settle vesting or
				// do something else. Its name is not evidence of taxable income.
				// Keep actual bank legs, with unresolved semantics made explicit.
				if unknownClaims[contract] {
					continue
				}
				unknownClaims[contract] = true
				detail := contractDetail(contract, map[string]string{
					attributeAction: actionClaim, "review_required": attributeTrue, "review_reason": "unknown_claim_semantics",
				})
				for _, transfer := range transfersIn(log) {
					if transfer.from == contract || transfer.to == contract {
						out = append(out, coinRows(CategoryTransfer, transfer.coins, transfer.from, transfer.to, "unknown_claim", detail)...)
					}
				}
			}
		// Raffles and packs: coins paid to the winner are prize income. NFT
		// prizes are recorded by nftTransferEvents with the prize trigger.
		case "claim_prize", "claim_pack":
			if a["winner"] != "" {
				coins := coinsBetween(log, contract, a["winner"])
				detail := contractDetail(contract, map[string]string{"week": a["week"], "pool_id": a["pool_id"]})
				out = append(out, coinRows(CategoryIncome, coins, contract, a["winner"], protocolPrize, detail)...)
			}
		// Name registration and renewal are a service purchase.
		case "register_subdomain", "register", "renew", "register_name", "renew_name":
			if kind == protocolNames && sender != "" {
				coins := coinsSpentBy(log, sender)
				detail := contractDetail(contract, map[string]string{"name": a["name"]})
				out = append(out, coinRows(CategoryExpense, coins, sender, contract, "name_registration", detail)...)
			}
		// Upload payment: a fee for storage.
		case "pay":
			if kind == "payment" && a["payer"] != "" {
				coins := coinsBetween(log, a["payer"], contract)
				detail := contractDetail(contract, map[string]string{"quote_id": a["quote_id"]})
				out = append(out, coinRows(CategoryExpense, coins, a["payer"], contract, "upload_fee", detail)...)
			}
		// Game entry fees.
		case "play":
			if kind == "game" && a["player"] != "" {
				coins := coinsSpentBy(log, a["player"])
				detail := contractDetail(contract, map[string]string{"offered_collection": a["offered_collection"], "offered_token_id": a["offered_token_id"]})
				out = append(out, coinRows(CategoryExpense, coins, a["player"], contract, "game_fee", detail)...)
			}
		}
	}
	return out
}

// vaultLegs builds the two swap legs of a vault deposit or withdrawal. The
// share denom is whichever tokenfactory coin the payee received (deposit) or
// the sender spent (withdrawal); on a deposit the ATOM leg is what the sender
// paid the vault.
func vaultLegs(log *indexerTxTypes.LogMessage, contract, sender, payee, atomAmount, shareAmount string, deposit bool) []TaxableEvent {
	if payee == "" {
		payee = sender
	}
	shareDenom := ""
	baseDenom := fixtureNativeDenom
	if deposit {
		for _, c := range coinsBetween(log, sender, contract) {
			if !strings.HasPrefix(c.Denom, "factory/") {
				baseDenom = c.Denom
			}
		}
		for _, t := range transfersIn(log) {
			if t.to != payee {
				continue
			}
			for _, c := range t.coins {
				if strings.HasPrefix(c.Denom, "factory/") {
					shareDenom = c.Denom
				}
			}
		}
	} else {
		for _, t := range transfersIn(log) {
			if t.to != contract {
				continue
			}
			for _, c := range t.coins {
				if strings.HasPrefix(c.Denom, "factory/") {
					shareDenom = c.Denom
				}
			}
		}
		for _, c := range coinsBetween(log, contract, payee) {
			if !strings.HasPrefix(c.Denom, "factory/") {
				baseDenom = c.Denom
			}
		}
	}
	if shareDenom == "" {
		return nil
	}
	detail := contractDetail(contract, map[string]string{"vault_shares": shareDenom})
	if deposit {
		return []TaxableEvent{
			{Category: string(CategorySwap), FromAddr: sender, Amount: atomAmount, Denom: baseDenom, RewardTrigger: actionVaultDeposit, Detail: detail},
			{Category: string(CategorySwap), ToAddr: payee, Amount: shareAmount, Denom: shareDenom, RewardTrigger: actionVaultDeposit, Detail: detail},
		}
	}
	return []TaxableEvent{
		{Category: string(CategorySwap), FromAddr: sender, Amount: shareAmount, Denom: shareDenom, RewardTrigger: actionVaultWithdraw, Detail: detail},
		{Category: string(CategorySwap), ToAddr: payee, Amount: atomAmount, Denom: baseDenom, RewardTrigger: actionVaultWithdraw, Detail: detail},
	}
}

// wasmExecuteEvents is the full classification of one MsgExecuteContract.
func wasmExecuteEvents(log *indexerTxTypes.LogMessage) []TaxableEvent {
	var out []TaxableEvent
	sales := nftSaleEvents(log)
	sales = append(sales, auctionSaleEvents(log)...)
	mints := nftMintEvents(log)
	sold := map[string]bool{}
	for _, s := range sales {
		sold[s.Asset] = true
	}
	minted := map[string]bool{}
	for _, m := range mints {
		minted[m.Asset] = true
	}
	out = append(out, sales...)
	out = append(out, swapEvents(log)...)
	out = append(out, mints...)
	out = append(out, auctionEscrowEvents(log)...)
	actions := protocolActionEvents(log)
	out = append(out, actions...)
	trigger := ""
	for _, e := range actions {
		if e.RewardTrigger == protocolPrize {
			trigger = protocolPrize
		}
	}
	if trigger == "" {
		for _, ev := range log.Events {
			if ev.Type == eventWasm {
				a := attrMap(ev)
				if (a[attributeAction] == "claim_prize" || a[attributeAction] == "claim_pack") && a["winner"] != "" {
					trigger = protocolPrize
				}
			}
		}
	}
	out = append(out, nftTransferEvents(log, sold, minted, trigger)...)
	return out
}

// --- staking principal, LSM and governance records ----------------------------

// stakingPrincipalEvents records the principal side of staking operations from
// the module's own events. These are record-only: bonding is not a disposal.
// The delegator comes from the event when the chain emits it and from the
// message otherwise.
func stakingPrincipalEvents(log *indexerTxTypes.LogMessage, delegator string) []TaxableEvent {
	var out []TaxableEvent
	if log == nil {
		return out
	}
	for _, ev := range log.Events {
		a := attrMap(ev)
		who := a[attributeDelegator]
		if who == "" {
			who = delegator
		}
		switch ev.Type {
		case "delegate":
			coins, err := sdk.ParseCoinsNormalized(a[attributeAmount])
			if err != nil || who == "" {
				continue
			}
			for _, c := range coins {
				out = append(out, TaxableEvent{Category: string(CategoryDelegate), FromAddr: who, Amount: c.Amount.String(), Denom: c.Denom, ValidatorAddress: a[attributeValidator]})
			}
		case "unbond":
			coins, err := sdk.ParseCoinsNormalized(a[attributeAmount])
			if err != nil || who == "" {
				continue
			}
			for _, c := range coins {
				out = append(out, TaxableEvent{Category: string(CategoryUndelegate), ToAddr: who, Amount: c.Amount.String(), Denom: c.Denom, ValidatorAddress: a[attributeValidator], Detail: detailJSON(map[string]string{"completion_time": a["completion_time"]})})
			}
		case "redelegate":
			coins, err := sdk.ParseCoinsNormalized(a[attributeAmount])
			if err != nil || who == "" {
				continue
			}
			for _, c := range coins {
				out = append(out, TaxableEvent{Category: string(CategoryRedelegate), FromAddr: who, ToAddr: who, Amount: c.Amount.String(), Denom: c.Denom, ValidatorAddress: a["destination_validator"], Detail: detailJSON(map[string]string{"source_validator": a["source_validator"], "destination_validator": a["destination_validator"], "completion_time": a["completion_time"]})})
			}
		case "cancel_unbonding_delegation":
			coins, err := sdk.ParseCoinsNormalized(a[attributeAmount])
			if err != nil || who == "" {
				continue
			}
			for _, c := range coins {
				out = append(out, TaxableEvent{Category: string(CategoryCancelUnbonding), FromAddr: who, Amount: c.Amount.String(), Denom: c.Denom, ValidatorAddress: a[attributeValidator], Detail: detailJSON(map[string]string{"creation_height": a["creation_height"]})})
			}
		}
	}
	return out
}

// lsmRecordEvents records the conversions the Liquid Staking Module performs.
// Tokenizing turns a delegation into a transferable share token and redeeming
// turns it back; both keep the same economic position, so they are record-only.
func lsmRecordEvents(log *indexerTxTypes.LogMessage, delegator string) []TaxableEvent {
	var out []TaxableEvent
	if log == nil {
		return out
	}
	for _, ev := range log.Events {
		a := attrMap(ev)
		switch ev.Type {
		case "tokenize_shares":
			who := a[attributeDelegator]
			if who == "" {
				who = delegator
			}
			coins, err := sdk.ParseCoinsNormalized(a["tokenized_shares"])
			if err != nil {
				coins, err = sdk.ParseCoinsNormalized(a[attributeAmount])
			}
			if err != nil || who == "" {
				continue
			}
			owner := a["share_owner"]
			if owner == "" {
				owner = who
			}
			for _, c := range coins {
				out = append(out, TaxableEvent{Category: string(CategoryTokenizeShares), FromAddr: who, ToAddr: owner, Amount: c.Amount.String(), Denom: c.Denom, ValidatorAddress: a[attributeValidator], Detail: detailJSON(map[string]string{"share_record_id": a["share_record_id"], "bonded_amount": a[attributeAmount]})})
			}
		case "redeem_shares":
			who := a[attributeDelegator]
			if who == "" {
				who = delegator
			}
			coins, err := sdk.ParseCoinsNormalized(a[attributeAmount])
			if err != nil || who == "" {
				continue
			}
			for _, c := range coins {
				out = append(out, TaxableEvent{Category: string(CategoryRedeemShares), FromAddr: who, ToAddr: who, Amount: c.Amount.String(), Denom: c.Denom, ValidatorAddress: a[attributeValidator]})
			}
		case "transfer_tokenize_share_record":
			if a[attributeSender] == "" || a["share_owner"] == "" {
				continue
			}
			out = append(out, TaxableEvent{Category: string(CategoryShareRecord), FromAddr: a[attributeSender], ToAddr: a["share_owner"], Detail: detailJSON(map[string]string{"share_record_id": a["share_record_id"]})})
		}
	}
	return out
}

// govDepositEvents records a proposal deposit. Deposits are refunded when a
// proposal passes or fails normally and burned on a veto, so the deposit
// itself is record-only and the refund arrives later as a bank transfer.
func govDepositEvents(log *indexerTxTypes.LogMessage, depositor string) []TaxableEvent {
	var out []TaxableEvent
	if log == nil {
		return out
	}
	for _, ev := range log.Events {
		if ev.Type != "proposal_deposit" {
			continue
		}
		a := attrMap(ev)
		who := a["depositor"]
		if who == "" {
			who = depositor
		}
		coins, err := sdk.ParseCoinsNormalized(a[attributeAmount])
		if err != nil || who == "" {
			continue
		}
		for _, c := range coins {
			out = append(out, TaxableEvent{Category: string(CategoryGovDeposit), FromAddr: who, Amount: c.Amount.String(), Denom: c.Denom, Detail: detailJSON(map[string]string{attributeProposalID: a[attributeProposalID]})})
		}
	}
	return out
}

// --- IBC memos: Skip Go, ibc-hooks and packet forwarding ----------------------

// ibcMemoIntent reads an ICS-20 memo and returns the routing trigger and
// detail for an outbound transfer. A Skip Go swap memo means the user is
// disposing of the sent asset on the destination chain; the outbound transfer
// itself is still recorded as ibc_out, with the route's destination attached
// so a reviewer can tag the disposal.
func ibcMemoIntent(memo string) (trigger string, detail map[string]string) {
	memo = strings.TrimSpace(memo)
	if memo == "" || memo[0] != '{' {
		return "", nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(memo), &m); err != nil {
		return "", nil
	}
	if fwd, ok := m["forward"]; ok {
		var f struct {
			Receiver string `json:"receiver"`
			Channel  string `json:"channel"`
			Port     string `json:"port"`
		}
		_ = json.Unmarshal(fwd, &f)
		return "pfm_forward", map[string]string{detailProtocol: "packet-forward-middleware", "forward_receiver": f.Receiver, "forward_channel": f.Channel}
	}
	wasm, ok := m[eventWasm]
	if !ok {
		if _, ok := m["dest_callback"]; ok {
			return "ibc_callback", map[string]string{detailProtocol: "ibc-callbacks"}
		}
		return "", nil
	}
	var w struct {
		Contract string                     `json:"contract"`
		Msg      map[string]json.RawMessage `json:"msg"`
	}
	if err := json.Unmarshal(wasm, &w); err != nil {
		return "", nil
	}
	detail = map[string]string{detailProtocol: "ibc-hooks", "hook_contract": w.Contract}
	for _, key := range []string{"swap_and_action", "swap_and_action_with_recover"} {
		raw, ok := w.Msg[key]
		if !ok {
			continue
		}
		detail[detailProtocol] = contractNameSkipGo
		var sa struct {
			UserSwap map[string]json.RawMessage `json:"user_swap"`
			MinAsset struct {
				Native struct {
					Denom  string `json:"denom"`
					Amount string `json:"amount"`
				} `json:"native"`
				CW20 struct {
					Address string `json:"address"`
					Amount  string `json:"amount"`
				} `json:"cw20"`
			} `json:"min_asset"`
			PostSwapAction struct {
				IBCTransfer struct {
					IBCInfo struct {
						Receiver       string `json:"receiver"`
						SourceChannel  string `json:"source_channel"`
						RecoverAddress string `json:"recover_address"`
					} `json:"ibc_info"`
				} `json:"ibc_transfer"`
				Transfer struct {
					ToAddress string `json:"to_address"`
				} `json:"transfer"`
				IBCEurekaTransfer struct {
					IBCInfo struct {
						Receiver string `json:"receiver"`
					} `json:"ibc_info"`
				} `json:"ibc_eureka_transfer"`
			} `json:"post_swap_action"`
		}
		if err := json.Unmarshal(raw, &sa); err == nil {
			for venueKind, v := range sa.UserSwap {
				detail["swap_kind"] = venueKind
				var venue struct {
					Name string `json:"swap_venue_name"`
				}
				_ = json.Unmarshal(v, &venue)
				detail["swap_venue"] = venue.Name
			}
			if sa.MinAsset.Native.Denom != "" {
				detail["destination_denom"] = sa.MinAsset.Native.Denom
				detail["min_destination_amount"] = sa.MinAsset.Native.Amount
			} else if sa.MinAsset.CW20.Address != "" {
				detail["destination_denom"] = "cw20:" + sa.MinAsset.CW20.Address
				detail["min_destination_amount"] = sa.MinAsset.CW20.Amount
			}
			switch {
			case sa.PostSwapAction.IBCTransfer.IBCInfo.Receiver != "":
				detail["destination_receiver"] = sa.PostSwapAction.IBCTransfer.IBCInfo.Receiver
				detail["recover_address"] = sa.PostSwapAction.IBCTransfer.IBCInfo.RecoverAddress
			case sa.PostSwapAction.Transfer.ToAddress != "":
				detail["destination_receiver"] = sa.PostSwapAction.Transfer.ToAddress
			case sa.PostSwapAction.IBCEurekaTransfer.IBCInfo.Receiver != "":
				detail["destination_receiver"] = sa.PostSwapAction.IBCEurekaTransfer.IBCInfo.Receiver
			}
		}
		return "skip_swap", detail
	}
	if _, ok := w.Msg[attributeAction]; ok {
		detail[detailProtocol] = contractNameSkipGo
		return "skip_action", detail
	}
	return "ibc_hook", detail
}

// skipForwardEvents records the pass-through the Skip Go entry point performs
// when a packet lands on the Hub only to be routed onward: the entry point's
// outbound transfer, attributed to the adapter contract that sent it, so the
// routed funds are visible end to end. The inbound leg is the ordinary ibc_in
// to the adapter, which the caller tags with the same trigger.
func skipForwardEvents(log *indexerTxTypes.LogMessage) []TaxableEvent {
	var out []TaxableEvent
	routed := false
	for _, ev := range log.Events {
		if ev.Type != eventWasm {
			continue
		}
		a := attrMap(ev)
		if contractKind(a[attributeContractAddress]) == contractRoleRouter && (a[attributeAction] == "dispatch_action_ibc_transfer" || a[attributeAction] == "execute_ibc_transfer" || a[attributeAction] == "dispatch_action_ibc_eureka_transfer") {
			routed = true
		}
	}
	if !routed {
		return out
	}
	for _, ev := range log.Events {
		if ev.Type != "ibc_transfer" {
			continue
		}
		a := attrMap(ev)
		if a[attributeAmount] == "" || a[attributeReceiver] == "" {
			continue
		}
		out = append(out, TaxableEvent{
			Category: string(CategoryIBCOut),
			FromAddr: a[attributeSender], ToAddr: a[attributeReceiver],
			Amount: a[attributeAmount], Denom: a[attributeDenom],
			RewardTrigger: triggerSkipForward,
			Detail:        detailJSON(map[string]string{detailProtocol: contractNameSkipGo, detailRoutedFromHub: attributeTrue}),
		})
	}
	return out
}

// isSkipRouterHop reports whether an inbound packet was received by a Skip Go
// adapter contract rather than an end user.
func isSkipRouterHop(log *indexerTxTypes.LogMessage, receiver string) bool {
	if contractKind(receiver) != contractRoleRouter {
		return false
	}
	return len(skipForwardEvents(log)) > 0
}

// --- ICS721 NFT bridge --------------------------------------------------------

// nftPacketData is the ICS721 NonFungibleTokenPacketData.
type nftPacketData struct {
	ClassID  string   `json:"classId"`
	TokenIDs []string `json:"tokenIds"`
	Sender   string   `json:"sender"`
	Receiver string   `json:"receiver"`
}

// ics721ReceiveEvents records NFTs arriving over the bridge: each cw721 mint or
// transfer to the packet's receiver becomes an nft_transfer in from the
// sender on the counterparty chain.
func ics721ReceiveEvents(log *indexerTxTypes.LogMessage, data nftPacketData) []TaxableEvent {
	var out []TaxableEvent
	if data.Receiver == "" {
		return out
	}
	bridge := ""
	for _, ev := range log.Events {
		if ev.Type == "wasm-ics721_receive_create_vouchers" || ev.Type == "wasm-ics721_receive_redeem_vouchers" {
			bridge = attrMap(ev)[attributeContractAddress]
		}
	}
	for _, ev := range log.Events {
		if ev.Type != eventWasm {
			continue
		}
		a := attrMap(ev)
		if a["token_id"] == "" || a[attributeContractAddress] == "" {
			continue
		}
		var to string
		switch a[attributeAction] {
		case "mint":
			to = a["owner"]
		case "transfer_nft":
			to = a[attributeRecipient]
		default:
			continue
		}
		if to != data.Receiver {
			continue
		}
		out = append(out, TaxableEvent{
			Category: string(CategoryNFTTransfer),
			FromAddr: data.Sender, ToAddr: to,
			Asset:         a[attributeContractAddress] + "/" + a["token_id"],
			RewardTrigger: "ics721_in",
			Detail:        contractDetail(bridge, map[string]string{"class_id": data.ClassID}),
		})
	}
	return out
}
