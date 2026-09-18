package models

import (
	"time"
)

type Block struct {
	ID                    uint
	TimeStamp             time.Time
	Height                int64 `gorm:"uniqueIndex:chainheight"`
	ChainID               uint  `gorm:"uniqueIndex:chainheight"`
	Chain                 Chain
	ProposerConsAddress   Address
	ProposerConsAddressID uint
	TxIndexed             bool
	// TODO: Should block event indexing be split out or rolled up?
	BlockEventsIndexed bool
}

// Used to keep track of BeginBlock and EndBlock events
type BlockLifecyclePosition int

const (
	BeginBlockEvent BlockLifecyclePosition = iota
	EndBlockEvent
)

type BlockEvent struct {
	ID uint
	// These fields uniquely identify every block event
	// Index refers to the position of the event in the block event lifecycle array
	// LifecyclePosition refers to whether the event is a BeginBlock or EndBlock event
	Index             uint64                 `gorm:"uniqueIndex:eventBlockPositionIndex,priority:3"`
	LifecyclePosition BlockLifecyclePosition `gorm:"uniqueIndex:eventBlockPositionIndex,priority:2"`
	BlockID           uint                   `gorm:"uniqueIndex:eventBlockPositionIndex,priority:1"`
	Block             Block
	BlockEventTypeID  uint
	BlockEventType    BlockEventType
}

type BlockEventType struct {
	ID   uint
	Type string `gorm:"uniqueIndex"`
}

type BlockEventAttribute struct {
	ID           uint
	BlockEvent   BlockEvent
	BlockEventID uint `gorm:"uniqueIndex:eventAttributeIndex,priority:1"`
	Value        string
	Index        uint64 `gorm:"uniqueIndex:eventAttributeIndex,priority:2"`
	// Keys are limited to a smallish subset of string values set by the Cosmos SDK and external modules
	// Save DB space by storing the key as a foreign key
	BlockEventAttributeKeyID uint
	BlockEventAttributeKey   BlockEventAttributeKey
}

type BlockEventAttributeKey struct {
	ID  uint
	Key string `gorm:"uniqueIndex"`
}

type FailedBlock struct {
	ID           uint
	Height       int64 `gorm:"uniqueIndex:failedchainheight"`
	BlockchainID uint  `gorm:"uniqueIndex:failedchainheight"`
	Chain        Chain `gorm:"foreignKey:BlockchainID"`
	// Bookkeeping for automatic retries. Attempts counts every processing
	// failure recorded for the height; LastAttemptedAt drives the retry
	// backoff; LastError preserves why the last attempt failed.
	Attempts        int `gorm:"default:0"`
	LastError       string
	LastAttemptedAt *time.Time
}

type FailedEventBlock struct {
	ID           uint
	Height       int64 `gorm:"uniqueIndex:failedchaineventheight"`
	BlockchainID uint  `gorm:"uniqueIndex:failedchaineventheight"`
	Chain        Chain `gorm:"foreignKey:BlockchainID"`
}
