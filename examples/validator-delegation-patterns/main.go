package main

import (
	"errors"
	"log"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/cmd"
	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/filter"
	"github.com/DefiantLabs/cosmos-indexer/parsers"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	dbTypes "github.com/DefiantLabs/cosmos-indexer/db"
	stdTypes "github.com/cosmos/cosmos-sdk/types"
	stakingTypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// This defines the custom message parser for the delegation and undelegation message type
// It implements the MessageParser interface
type MsgDelegateUndelegateParser struct {
	ID string
}

func (c *MsgDelegateUndelegateParser) Identifier() string {
	return c.ID
}

func (c *MsgDelegateUndelegateParser) ParseMessage(cosmosMsg stdTypes.Msg, log *indexerTxTypes.LogMessage, cfg config.IndexConfig) (*any, error) {
	msgDelegate, okMsgDelegate := cosmosMsg.(*stakingTypes.MsgDelegate)
	msgUndelegate, okMsgUndelegate := cosmosMsg.(*stakingTypes.MsgUndelegate)
	if !okMsgDelegate && !okMsgUndelegate {
		return nil, errors.New("not a delegation message")
	}

	if okMsgDelegate {
		delegator := models.Address{
			Address: msgDelegate.DelegatorAddress,
		}

		validator := Validator{
			ValidatorAddress: models.Address{
				Address: msgDelegate.ValidatorAddress,
			},
		}

		amount := msgDelegate.Amount.Amount.String()
		denom := models.Denom{
			Base: msgDelegate.Amount.Denom,
		}

		storageVal := any(DelegationEvent{
			Delegator:      delegator,
			Validator:      validator,
			Amount:         amount,
			Denom:          denom,
			DelegationType: Delegation,
		})

		return &storageVal, nil
	}

	delegator := models.Address{
		Address: msgUndelegate.DelegatorAddress,
	}

	validator := Validator{
		ValidatorAddress: models.Address{
			Address: msgUndelegate.ValidatorAddress,
		},
	}

	amount := msgUndelegate.Amount.Amount.String()
	denom := models.Denom{
		Base: msgUndelegate.Amount.Denom,
	}

	storageVal := any(DelegationEvent{
		Delegator:      delegator,
		Validator:      validator,
		Amount:         amount,
		Denom:          denom,
		DelegationType: Undelegation,
	})

	return &storageVal, nil
}

// This method is called during database insertion. It is responsible for storing the parsed data in the database.
// The gorm db is wrapped in a transaction, so any errors will cause a rollback.
// Any errors returned will be saved as a parser error in the database as well for later debugging.
func (c *MsgDelegateUndelegateParser) IndexMessage(dataset *any, db *gorm.DB, message models.Message, messageEvents []parsers.MessageEventWithAttributes, cfg config.IndexConfig) error {
	delegationEvent, ok := (*dataset).(DelegationEvent)
	if !ok {
		return errors.New("not a delegation event type")
	}

	// Save the delegator and validator addresses
	validatorAddress, err := dbTypes.FindOrCreateAddressByAddress(db, delegationEvent.Validator.ValidatorAddress.Address)
	if err != nil {
		return err
	}

	delegatorAddress, err := dbTypes.FindOrCreateAddressByAddress(db, delegationEvent.Delegator.Address)
	if err != nil {
		return err
	}

	// Save the denom of the delegation
	denom, err := dbTypes.FindOrCreateDenomByBase(db, delegationEvent.Denom.Base)
	if err != nil {
		return err
	}

	// Save the validator
	validator := Validator{
		ValidatorAddress:   validatorAddress,
		ValidatorAddressID: validatorAddress.ID,
	}

	err = db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "validator_address_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"validator_address_id"}),
	}).Create(&validator).Error
	if err != nil {
		return err
	}

	// Save the delegation event
	loadDelegationValues(&delegationEvent, message, validator, delegatorAddress, denom)

	err = db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "message_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"amount"}),
	}).Create(&delegationEvent).Error

	return err
}

func loadDelegationValues(initialDelegationEvent *DelegationEvent, message models.Message, validator Validator, delegator models.Address, delegationDenom models.Denom) *DelegationEvent {
	initialDelegationEvent.ValidatorID = validator.ID
	initialDelegationEvent.Validator = validator
	initialDelegationEvent.DelegatorID = delegator.ID
	initialDelegationEvent.Delegator = delegator
	initialDelegationEvent.DenomID = delegationDenom.ID
	initialDelegationEvent.Denom = delegationDenom
	initialDelegationEvent.MessageID = message.ID
	initialDelegationEvent.Message = message
	initialDelegationEvent.DelegationTime = message.Tx.Block.TimeStamp

	return initialDelegationEvent
}

// This defines the custom message parser for the undelegation message type
// It implements the MessageParser interface
type MsgUndelegateParser struct{}

// These are the indexer's custom models
// They are used to store the parsed data in the database
type Validator struct {
	ID                 uint
	ValidatorAddress   models.Address
	ValidatorAddressID uint `gorm:"uniqueIndex"`
}

type DelegationType int64

const (
	Delegation DelegationType = iota
	Undelegation
)

type DelegationEvent struct {
	ID             uint
	Delegator      models.Address
	DelegatorID    uint
	Validator      Validator
	ValidatorID    uint
	Amount         string
	Denom          models.Denom
	DenomID        uint
	Message        models.Message
	MessageID      uint `gorm:"uniqueIndex"`
	DelegationType DelegationType
	DelegationTime time.Time
}

func main() {
	// Register the custom database models. They will be migrated and included in the database when the indexer finishes setup.
	customModels := []any{
		&Validator{},
		&DelegationEvent{},
	}

	indexer := cmd.GetBuiltinIndexer()

	// Register the custom types that will modify the behavior of the indexer
	indexer.RegisterCustomModels(customModels)

	// This indexer is only concerned with delegate and undelegate messages, so we create regex filters to only index those messages.
	// This significantly reduces the size of the indexed dataset, saving space and processing time.
	stakingDelegateRegexMessageTypeFilter, err := filter.NewRegexMessageTypeFilter("^/cosmos\\.staking.*MsgDelegate$", false)
	if err != nil {
		log.Fatalf("Failed to create regex message type filter. Err: %v", err)
	}

	stakingUndelegateRegexMessageTypeFilter, err := filter.NewRegexMessageTypeFilter("^/cosmos\\.staking.*MsgUndelegate$", false)
	if err != nil {
		log.Fatalf("Failed to create regex message type filter. Err: %v", err)
	}

	indexer.RegisterMessageTypeFilter(stakingDelegateRegexMessageTypeFilter)
	indexer.RegisterMessageTypeFilter(stakingUndelegateRegexMessageTypeFilter)

	// Register the custom message parser for the delegation message types. Our parser can handle both delegate and undelegate messages.
	// However, they must be uniquely identified by the Identifier() method. This will make identifying any parser errors easier.
	delegateParser := &MsgDelegateUndelegateParser{ID: "delegate"}
	undelegateParser := &MsgDelegateUndelegateParser{ID: "undelegate"}
	indexer.RegisterCustomMessageParser("/cosmos.staking.v1beta1.MsgDelegate", delegateParser)
	indexer.RegisterCustomMessageParser("/cosmos.staking.v1beta1.MsgUndelegate", undelegateParser)

	err = cmd.Execute()
	if err != nil {
		log.Fatalf("Failed to execute. Err: %v", err)
	}
}
