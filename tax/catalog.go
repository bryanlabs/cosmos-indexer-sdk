package tax

// CategoryInfo documents one classification for the public catalog and the
// "What we track" page. Descriptions are written for a controller, not a
// developer: what the row means for the wallet owner's books.
type CategoryInfo struct {
	Category    string `json:"category"`
	Taxable     bool   `json:"taxable"`
	Group       string `json:"group"`
	Description string `json:"description"`
}

// Catalog lists every category the parser can emit with its tax meaning.
func Catalog() []CategoryInfo {
	desc := map[Category][2]string{
		CategoryTransfer:           {catalogGroupTransfers, "A bank send or receive between two addresses. A send is a disposal at fair market value unless you tag it as an internal transfer or a gift; a receive is an acquisition whose cost basis we cannot see."},
		CategoryReward:             {"staking", "Staking rewards actually withdrawn from the distribution module: an explicit claim, the auto-withdrawal that happens on delegate, undelegate and redelegate, or an LSM tokenize or redeem. Ordinary income at the USD price that day, split by validator."},
		CategoryCommission:         {"staking", "Validator commission withdrawn by the operator. Ordinary income at the USD price that day."},
		CategoryIBCOut:             {catalogGroupTransfers, "Coins sent to another chain over IBC. Treated as a send unless the memo shows a Skip Go swap, in which case the row carries the swap intent so you can match the receipt on the destination chain."},
		CategoryIBCIn:              {catalogGroupTransfers, "Coins received from another chain over IBC, including the Hub hop of a Skip Go route."},
		CategoryNFTSale:            {catalogGroupNFTs, "An NFT sold through a marketplace, auction or offer. A disposal of the NFT for the seller and an acquisition for the buyer, priced in the coin that changed hands."},
		CategoryNFTMint:            {catalogGroupNFTs, "An NFT minted for a price. The mint cost is the NFT's cost basis."},
		CategorySwap:               {"defi", "One leg of a swap or vault movement: the coin given up is a disposal, the coin received is an acquisition. Vault deposits and withdrawals on Inflow are recorded as swaps between ATOM and vault shares."},
		CategoryIncome:             {string(CategoryIncome), "Protocol income that is not a staking reward: Hydro tributes, DAO reward distributions, governance-score rewards, raffle and game prizes paid in coins. Ordinary income at the USD price that day; the source is named on the row."},
		CategoryExpense:            {string(CategoryIncome), "A payment to a protocol for a service with nothing coming back: a name registration, an upload fee, a game entry fee, a tribute paid into Hydro. A disposal of the coins paid."},
		CategoryProtocolDeposit:    {catalogGroupRecords, "Coins you still own moved into a contract: a Hydro lock, DAO staking, an auction bid held in escrow. Not a disposal. Kept out of tax-software imports so it is never read as a sale."},
		CategoryProtocolWithdrawal: {catalogGroupRecords, "The same coins coming back out of a contract: an unlock, an unstake, a refunded bid. Not income."},
		CategoryDelegate:           {catalogGroupRecords, "Staking principal bonded to a validator. Not a disposal; the wallet still owns the ATOM."},
		CategoryUndelegate:         {catalogGroupRecords, "Staking principal entering the 21-day unbonding queue, with the completion time."},
		CategoryRedelegate:         {catalogGroupRecords, "Staking principal moved between validators."},
		CategoryCancelUnbonding:    {catalogGroupRecords, "Unbonding principal re-bonded before the unbonding period finished."},
		CategoryTokenizeShares:     {catalogGroupRecords, "A delegation converted into transferable liquid staking share tokens. The tokens represent the same stake."},
		CategoryRedeemShares:       {catalogGroupRecords, "Liquid staking share tokens converted back into a native delegation."},
		CategoryShareRecord:        {catalogGroupRecords, "Ownership of a tokenize-share record transferred to another address."},
		CategoryGovDeposit:         {catalogGroupRecords, "A refundable governance proposal deposit. Not a disposal while the deposit can still be returned."},
		CategoryNFTTransfer:        {catalogGroupNFTs, "An NFT moved without a payment: a gift, a listing moved into marketplace escrow, a raffle entry or prize, a bridge transfer over ICS721. Review and tag if it was actually a sale off chain."},
		CategoryNFTBurn:            {catalogGroupNFTs, "An NFT destroyed by its owner."},
	}
	out := make([]CategoryInfo, 0, len(desc))
	for _, c := range AllCategories() {
		d := desc[c]
		out = append(out, CategoryInfo{Category: string(c), Taxable: c.Taxable(), Group: d[0], Description: d[1]})
	}
	return out
}

// TriggerInfo documents a RewardTrigger value: the specific source of an
// income, expense or record row.
type TriggerInfo struct {
	Trigger     string `json:"trigger"`
	Categories  string `json:"categories"`
	Description string `json:"description"`
}

// Triggers lists every RewardTrigger the parser emits.
func Triggers() []TriggerInfo {
	return []TriggerInfo{
		{actionClaim, "reward, commission", "An explicit MsgWithdrawDelegatorReward or MsgWithdrawValidatorCommission."},
		{"delegate", string(CategoryReward), "Reward auto-withdrawn when the delegator added stake to the same validator."},
		{"undelegate", string(CategoryReward), "Reward auto-withdrawn when the delegator started unbonding."},
		{"redelegate", string(CategoryReward), "Reward auto-withdrawn from both validators when stake moved between them."},
		{"authz", string(CategoryReward), "Reward withdrawn by a grantee on the delegator's behalf, typically REStake auto-compounding."},
		{"tokenize", string(CategoryReward), "Reward auto-withdrawn when a delegation was tokenized into liquid staking shares."},
		{"redeem", string(CategoryReward), "Reward paid out when liquid staking shares were redeemed, including the tokenize-share-record reward."},
		{protocolTribute, string(CategoryIncome), "A Hydro tribute claimed by a voter."},
		{"tribute_paid", string(CategoryExpense), "A tribute added to a Hydro proposal by its sponsor."},
		{"dao_rewards", string(CategoryIncome), "A DAO DAO rewards distributor payout."},
		{"governance_score", string(CategoryIncome), "A Governance Score reward claim."},
		{protocolPrize, "income, nft_transfer", "A raffle, pack or game prize."},
		{"name_registration", string(CategoryExpense), "An atom.names registration or renewal fee."},
		{"upload_fee", string(CategoryExpense), "A payment to the Hub upload payment contract."},
		{"game_fee", string(CategoryExpense), "A Mad Gumball play fee."},
		{"auction_bid", string(CategoryProtocolDeposit), "A bid placed into a reserve auction's escrow."},
		{"auction_refund", string(CategoryProtocolWithdrawal), "An outbid bidder's escrow returned."},
		{"hydro_lock", string(CategoryProtocolDeposit), "ATOM or liquid staking tokens locked in Hydro."},
		{"hydro_unlock", string(CategoryProtocolWithdrawal), "A Hydro lock released."},
		{"dao_stake", string(CategoryProtocolDeposit), "Tokens staked in a DAO voting module."},
		{"dao_unstake", string(CategoryProtocolWithdrawal), "Tokens claimed back from a DAO voting module."},
		{actionVaultDeposit, string(CategorySwap), "An Inflow vault deposit: ATOM out, vault shares in."},
		{actionVaultWithdraw, string(CategorySwap), "An Inflow vault withdrawal: vault shares out, ATOM in."},
		{"skip_swap", string(CategoryIBCOut), "An IBC transfer whose memo instructs a Skip Go swap on the destination chain. Detail carries the venue, destination asset, minimum amount and final receiver."},
		{"skip_action", string(CategoryIBCOut), "An IBC transfer whose memo runs another Skip Go action on arrival."},
		{triggerSkipForward, "ibc_in, ibc_out", "A Skip Go route that passed through the Hub: the adapter received the coins and forwarded them on."},
		{"ibc_hook", string(CategoryIBCOut), "An IBC transfer that calls a contract on arrival (ibc-hooks)."},
		{"pfm_forward", string(CategoryIBCOut), "An IBC transfer forwarded onward by packet-forward middleware."},
		{"ibc_callback", string(CategoryIBCOut), "An IBC transfer with a callback contract."},
		{"ics721_out", "nft_transfer", "An NFT sent to another chain over the ICS721 bridge."},
		{"ics721_in", "nft_transfer", "An NFT received from another chain over the ICS721 bridge."},
	}
}
