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
)

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
	Denom string

	FromAddr string `gorm:"index:idx_tax_from"`
	ToAddr   string `gorm:"index:idx_tax_to"`

	BlockHeight int64     `gorm:"index:idx_tax_height"`
	Timestamp   time.Time `gorm:"index:idx_tax_time"`
	TxHash      string    `gorm:"index:idx_tax_txhash"`
}
