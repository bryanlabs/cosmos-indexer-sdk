package models

import (
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Tx struct {
	ID              uint
	Hash            string `gorm:"uniqueIndex"`
	Code            uint32
	BlockID         uint
	Block           Block
	Memo            string
	SignerAddresses []Address `gorm:"many2many:tx_signer_addresses;"`
	Fees            []Fee
}

type FailedTx struct {
	ID      uint
	Hash    string `gorm:"uniqueIndex"`
	BlockID uint
	Block   Block
}

type Fee struct {
	ID             uint            `gorm:"primaryKey"`
	TxID           uint            `gorm:"uniqueIndex:txDenomFee"`
	Amount         decimal.Decimal `gorm:"type:decimal(78,0);"`
	DenominationID uint            `gorm:"uniqueIndex:txDenomFee"`
	Denomination   Denom           `gorm:"foreignKey:DenominationID"`
	PayerAddressID uint            `gorm:"index:idx_payer_addr"`
	PayerAddress   Address         `gorm:"foreignKey:PayerAddressID"`
}

// This lifecycle function ensures the on conflict statement is added for Fees which are associated to Txes by the Gorm slice association method for has_many
func (b *Fee) BeforeCreate(tx *gorm.DB) (err error) {
	tx.Statement.AddClause(clause.OnConflict{
		Columns:   []clause.Column{{Name: "tx_id"}, {Name: "denomination_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"amount", "payer_address_id"}),
	})
	return nil
}

type MessageType struct {
	ID          uint   `gorm:"primaryKey"`
	MessageType string `gorm:"uniqueIndex;not null"`
}

type Message struct {
	ID            uint
	TxID          uint `gorm:"uniqueIndex:messageIndex,priority:1"`
	Tx            Tx
	MessageTypeID uint `gorm:"foreignKey:MessageTypeID,index:idx_txid_typeid"`
	MessageType   MessageType
	MessageIndex  int `gorm:"uniqueIndex:messageIndex,priority:2"`
	MessageBytes  []byte
	// Opaque marks retained payloads the SDK could not decode. Older rows need
	// replay before absence of this marker can establish decoding completeness.
	Opaque bool `gorm:"not null;default:false;index:idx_message_opaque,where:opaque = true,option:CONCURRENTLY"`
}

type FailedMessage struct {
	ID           uint
	MessageIndex int
	TxID         uint
	Tx           Tx
}

type MessageEvent struct {
	ID uint
	// These fields uniquely identify every message event
	// Index refers to the position of the event in the message event array
	Index              uint64 `gorm:"uniqueIndex:messageEventIndex,priority:2"`
	MessageID          uint   `gorm:"uniqueIndex:messageEventIndex,priority:1"`
	Message            Message
	MessageEventTypeID uint
	MessageEventType   MessageEventType
}

type MessageEventType struct {
	ID   uint
	Type string `gorm:"uniqueIndex"`
}

type MessageEventAttribute struct {
	ID             uint
	MessageEvent   MessageEvent
	MessageEventID uint `gorm:"uniqueIndex:messageAttributeIndex,priority:1"`
	Value          string
	Index          uint64 `gorm:"uniqueIndex:messageAttributeIndex,priority:2"`
	// Keys are limited to a smallish subset of string values set by the Cosmos SDK and external modules
	// Save DB space by storing the key as a foreign key
	MessageEventAttributeKeyID uint
	MessageEventAttributeKey   MessageEventAttributeKey
}

type MessageEventAttributeKey struct {
	ID  uint
	Key string `gorm:"uniqueIndex"`
}
