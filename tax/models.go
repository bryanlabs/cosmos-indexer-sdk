// Package tax implements a taxable-event classification layer on top of the
// cosmos-indexer SDK. It registers custom message parsers that turn raw Cosmos
// messages/events into TaxableEvent rows, reaching parity with the legacy
// cosmos-tax-cli for the events that matter on Cosmos Hub: bank transfers,
// staking rewards, validator commission, and IBC in/out. Fees are already
// captured generically by the SDK's Fee model, so they're read from there at
// export time rather than duplicated here.
package tax

import (
	"time"

	"github.com/DefiantLabs/cosmos-indexer/db/models"
)

// Category is the taxable classification of an event.
type Category string

const (
	CategoryTransfer   Category = "transfer"   // bank send/receive (direction derived per-address at export)
	CategoryReward     Category = "reward"     // staking/distribution delegator reward (income)
	CategoryCommission Category = "commission" // validator commission (income)
	CategoryIBCOut     Category = "ibc_out"    // outbound IBC transfer
	CategoryIBCIn      Category = "ibc_in"     // inbound IBC receive
	CategoryNFTSale    Category = "nft_sale"   // CosmWasm NFT marketplace sale (disposal for seller, acquisition for buyer)
	CategoryNFTMint    Category = "nft_mint"   // CosmWasm NFT mint (acquisition; basis = mint cost)
	CategorySwap       Category = "swap"       // CosmWasm DEX swap leg (offer = disposal, return = acquisition)

	// Non-staking protocol income: Hydro tributes, DAO reward distributors,
	// governance-score rewards, raffle and game prizes. RewardTrigger names the
	// source (tribute, dao_rewards, governance_score, prize) and FromAddr is the
	// paying contract.
	CategoryIncome Category = "income"
	// A payment to a protocol for a service with nothing coming back: name
	// registration, upload fees, game entry fees, tributes paid into Hydro.
	// A disposal of the coins at fair market value for the payer.
	CategoryExpense Category = "expense"

	// Record-only categories. These are not taxable events but a controller
	// needs them to reconcile a wallet: coins the user still owns moved into or
	// out of a protocol, or changed form inside the staking system.
	CategoryProtocolDeposit    Category = "protocol_deposit"    // coins locked in a contract the user still owns (Hydro lock, DAO stake, auction bid escrow)
	CategoryProtocolWithdrawal Category = "protocol_withdrawal" // the same coins coming back (unlock, claim of staked tokens, bid refund)
	CategoryDelegate           Category = "delegate"            // staking principal bonded to a validator
	CategoryUndelegate         Category = "undelegate"          // staking principal entering the unbonding queue
	CategoryRedelegate         Category = "redelegate"          // staking principal moved between validators
	CategoryCancelUnbonding    Category = "cancel_unbonding"    // unbonding principal re-bonded
	CategoryTokenizeShares     Category = "tokenize_shares"     // LSM: a delegation converted into transferable share tokens
	CategoryRedeemShares       Category = "redeem_shares"       // LSM: share tokens converted back into a delegation
	CategoryShareRecord        Category = "share_record"        // LSM: ownership of a tokenize-share record transferred
	CategoryGovDeposit         Category = "gov_deposit"         // refundable governance proposal deposit
	CategoryNFTTransfer        Category = "nft_transfer"        // NFT moved without payment (gift, escrow, bridge, prize)
	CategoryNFTBurn            Category = "nft_burn"            // NFT destroyed
)

// Taxable reports whether a category can change a tax result on its own. The
// record-only categories describe where the user's own assets went; exporters
// keep them out of tax-software imports so they are never read as disposals.
func (c Category) Taxable() bool {
	switch c {
	case CategoryTransfer, CategoryReward, CategoryCommission, CategoryIBCOut, CategoryIBCIn,
		CategoryNFTSale, CategoryNFTMint, CategorySwap, CategoryIncome, CategoryExpense:
		return true
	}
	return false
}

// AllCategories lists every category the parser can emit, taxable first.
func AllCategories() []Category {
	return []Category{
		CategoryTransfer, CategoryReward, CategoryCommission, CategoryIBCOut, CategoryIBCIn,
		CategoryNFTSale, CategoryNFTMint, CategorySwap, CategoryIncome, CategoryExpense,
		CategoryProtocolDeposit, CategoryProtocolWithdrawal, CategoryDelegate, CategoryUndelegate,
		CategoryRedelegate, CategoryCancelUnbonding, CategoryTokenizeShares, CategoryRedeemShares,
		CategoryShareRecord, CategoryGovDeposit, CategoryNFTTransfer, CategoryNFTBurn,
	}
}

// TaxableEvent is one classified coin movement, denormalized for fast
// address+date-range export queries. A single message can produce several
// (multi-send, multi-coin rewards), distinguished by SubIndex.
type TaxableEvent struct {
	ID        uint           `gorm:"primaryKey"`
	MessageID uint           `gorm:"uniqueIndex:tax_msg_sub,priority:1"`
	Message   models.Message `gorm:"foreignKey:MessageID"`
	SubIndex  int            `gorm:"uniqueIndex:tax_msg_sub,priority:2"`

	Category string `gorm:"index:idx_tax_category"`
	// Base-denom integer amount as a string (e.g. "1234567" uatom).
	Amount string
	// On-chain base denom (e.g. "uatom", "ibc/<hash>"); resolved to a symbol at export.
	// For nft_sale, this is the denom the NFT was priced/paid in; Amount is the price.
	Denom string

	// Asset identifies a non-fungible asset for nft_sale events as
	// "<collection>/<token_id>"; empty for fungible coin movements.
	Asset string `gorm:"index:idx_tax_asset"`

	FromAddr string `gorm:"index:idx_tax_from"`
	ToAddr   string `gorm:"index:idx_tax_to"`

	// ValidatorAddress is the operator address on the reward withdrawal event,
	// or the validator a staking-principal record concerns.
	// RewardTrigger distinguishes explicit claims from staking auto-withdrawals;
	// for income, expense and protocol records it names the source
	// (tribute, dao_rewards, prize, skip_swap, vault_deposit, ...).
	ValidatorAddress string
	RewardTrigger    string

	// Detail is a small JSON object of protocol context the category alone
	// does not carry: the contract and its name, a Skip route's destination,
	// a Hydro lock id, a proposal id, an unbonding completion time. Empty when
	// there is nothing beyond the other columns.
	Detail string

	BlockHeight int64     `gorm:"index:idx_tax_height"`
	Timestamp   time.Time `gorm:"index:idx_tax_time"`
	TxHash      string    `gorm:"index:idx_tax_txhash"`
}
