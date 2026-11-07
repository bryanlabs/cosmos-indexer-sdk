package db

import (
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/parsers"
)

const (
	OsmosisRewardDistribution uint = iota
	TendermintLiquidityDepositCoinsToPool
	TendermintLiquidityDepositPoolCoinReceived
	TendermintLiquiditySwapTransactedCoinIn
	TendermintLiquiditySwapTransactedCoinOut
	TendermintLiquiditySwapTransactedFee
	TendermintLiquidityWithdrawPoolCoinSent
	TendermintLiquidityWithdrawCoinReceived
	TendermintLiquidityWithdrawFee
	OsmosisProtorevDeveloperRewardDistribution
)

type BlockDBWrapper struct {
	Block                         *models.Block
	BeginBlockEvents              []BlockEventDBWrapper
	EndBlockEvents                []BlockEventDBWrapper
	UniqueBlockEventTypes         map[string]models.BlockEventType
	UniqueBlockEventAttributeKeys map[string]models.BlockEventAttributeKey
}

type BlockEventDBWrapper struct {
	BlockEvent               models.BlockEvent
	Attributes               []models.BlockEventAttribute
	BlockEventParsedDatasets []parsers.BlockEventParsedData
}

// Store transactions with their messages for easy database creation
type TxDBWrapper struct {
	Tx                         models.Tx
	Messages                   []MessageDBWrapper
	UniqueMessageTypes         map[string]models.MessageType
	UniqueMessageEventTypes    map[string]models.MessageEventType
	UniqueMessageAttributeKeys map[string]models.MessageEventAttributeKey
}

type MessageDBWrapper struct {
	// PreserveOpaqueEvidence bypasses optional raw/event storage for undecodable messages.
	PreserveOpaqueEvidence bool
	Message                models.Message
	MessageEvents          []MessageEventDBWrapper
	MessageParsedDatasets  []parsers.MessageParsedData
}

type MessageEventDBWrapper struct {
	MessageEvent models.MessageEvent
	Attributes   []models.MessageEventAttribute
}

type DenomDBWrapper struct {
	Denom models.Denom
}
