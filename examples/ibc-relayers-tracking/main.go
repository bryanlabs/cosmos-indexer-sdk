package main

import (
	"fmt"
	"log"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/cmd"
	"github.com/DefiantLabs/cosmos-indexer/config"
	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/filter"
	"github.com/DefiantLabs/cosmos-indexer/parsers"
	stdTypes "github.com/cosmos/cosmos-sdk/types"
	chanTypes "github.com/cosmos/ibc-go/v7/modules/core/04-channel/types"
	"gorm.io/gorm"
)

const (
	MsgRecvPacket      = "/ibc.core.channel.v1.MsgRecvPacket"
	MsgAcknowledgement = "/ibc.core.channel.v1.MsgAcknowledgement"
)

type RelayerTransaction struct {
	ID          uint
	Height      int64
	Timestamp   time.Time
	MessageType string
	Signer      string
	Memo        string
}

type IBCRelayerTrackingParser struct {
	UniqueID string
}

func (c *IBCRelayerTrackingParser) Identifier() string {
	return c.UniqueID
}

func (c *IBCRelayerTrackingParser) ParseMessage(cosmosMsg stdTypes.Msg, log *indexerTxTypes.LogMessage, cfg config.IndexConfig) (*any, error) {
	messageType := ""
	signer := ""

	msgAck, okMsgAck := cosmosMsg.(*chanTypes.MsgAcknowledgement)

	if okMsgAck {
		messageType = MsgAcknowledgement
		signer = msgAck.Signer
	}

	// Check if this is a MsgRecvPacket
	msgRecvPacket, okMsgRecvPacket := cosmosMsg.(*chanTypes.MsgRecvPacket)

	if okMsgRecvPacket {
		messageType = MsgRecvPacket
		signer = msgRecvPacket.Signer
	}

	if !okMsgAck && !okMsgRecvPacket {
		return nil, fmt.Errorf("unsupported message type passed to parser")
	}

	relayerTransaction := RelayerTransaction{
		MessageType: messageType,
		Signer:      signer,
	}

	anyCast := any(relayerTransaction)

	return &anyCast, nil
}

func (c *IBCRelayerTrackingParser) IndexMessage(dataset *any, db *gorm.DB, message models.Message, messageEvents []parsers.MessageEventWithAttributes, cfg config.IndexConfig) error {
	relayerTransaction, ok := (*dataset).(RelayerTransaction)

	if !ok {
		return fmt.Errorf("failed to cast dataset to RelayerTransaction")
	}

	relayerTransaction.Height = message.Tx.Block.Height
	relayerTransaction.Timestamp = message.Tx.Block.TimeStamp
	relayerTransaction.Memo = message.Tx.Memo

	// Check if the relayer transaction already exists and delete it, in case we are reindexing an already indexed block
	err := db.Delete(&RelayerTransaction{}, "signer = ? AND height = ?", relayerTransaction.Signer, relayerTransaction.Height).Error
	if err != nil {
		return fmt.Errorf("failed to delete existing relayer transaction. Err: %v", err)
	}

	return db.Create(&relayerTransaction).Error
}

func main() {
	indexer := cmd.GetBuiltinIndexer()

	indexer.RegisterCustomModels([]any{RelayerTransaction{}})

	ibcRegexMessageTypeFilter, err := filter.NewRegexMessageTypeFilter("^/ibc.core.channel.v1.Msg(RecvPacket|Acknowledgement)$")
	if err != nil {
		log.Fatalf("Failed to create regex message type filter. Err: %v", err)
	}

	indexer.RegisterMessageTypeFilter(ibcRegexMessageTypeFilter)

	ibcRelayerParserRecv := &IBCRelayerTrackingParser{
		UniqueID: "ibc-relayer-parser-recv",
	}
	ibcRelayerParserAck := &IBCRelayerTrackingParser{
		UniqueID: "ibc-relayer-parser-ack",
	}

	indexer.RegisterCustomMessageParser(MsgRecvPacket, ibcRelayerParserRecv)
	indexer.RegisterCustomMessageParser(MsgAcknowledgement, ibcRelayerParserAck)

	err = cmd.Execute()
	if err != nil {
		log.Fatalf("Failed to execute. Err: %v", err)
	}
}
