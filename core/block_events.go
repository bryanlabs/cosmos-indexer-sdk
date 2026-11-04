package core

import (
	"encoding/base64"

	abci "github.com/cometbft/cometbft/abci/types"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/filter"
	"github.com/DefiantLabs/cosmos-indexer/parsers"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
)

func ProcessRPCBlockResults(conf config.IndexConfig, block models.Block, blockResults *rpc.CustomBlockResults, customBeginBlockParsers map[string][]parsers.BlockEventParser, customEndBlockParsers map[string][]parsers.BlockEventParser) (*db.BlockDBWrapper, error) {
	var blockDBWrapper db.BlockDBWrapper

	blockDBWrapper.Block = &block

	blockDBWrapper.UniqueBlockEventAttributeKeys = make(map[string]models.BlockEventAttributeKey)
	blockDBWrapper.UniqueBlockEventTypes = make(map[string]models.BlockEventType)

	var err error
	blockDBWrapper.BeginBlockEvents, err = ProcessRPCBlockEvents(blockDBWrapper.Block, blockResults.BeginBlockEvents, models.BeginBlockEvent, blockDBWrapper.UniqueBlockEventTypes, blockDBWrapper.UniqueBlockEventAttributeKeys, customBeginBlockParsers, conf)
	if err != nil {
		return nil, err
	}

	blockDBWrapper.EndBlockEvents, err = ProcessRPCBlockEvents(blockDBWrapper.Block, blockResults.EndBlockEvents, models.EndBlockEvent, blockDBWrapper.UniqueBlockEventTypes, blockDBWrapper.UniqueBlockEventAttributeKeys, customEndBlockParsers, conf)
	if err != nil {
		return nil, err
	}

	return &blockDBWrapper, nil
}

func ProcessRPCBlockEvents(block *models.Block, blockEvents []abci.Event, blockLifecyclePosition models.BlockLifecyclePosition, uniqueEventTypes map[string]models.BlockEventType, uniqueAttributeKeys map[string]models.BlockEventAttributeKey, customParsers map[string][]parsers.BlockEventParser, conf config.IndexConfig) ([]db.BlockEventDBWrapper, error) {
	beginBlockEvents := make([]db.BlockEventDBWrapper, len(blockEvents))

	for index, event := range blockEvents {
		eventType := models.BlockEventType{
			Type: event.Type,
		}
		beginBlockEvents[index].BlockEvent = models.BlockEvent{
			Index:             uint64(index),
			LifecyclePosition: blockLifecyclePosition,
			Block:             *block,
			BlockEventType:    eventType,
		}

		uniqueEventTypes[event.Type] = eventType

		beginBlockEvents[index].Attributes = make([]models.BlockEventAttribute, len(event.Attributes))

		for attrIndex, attribute := range event.Attributes {

			var value string
			var keyItem string
			if conf.Flags.BlockEventsBase64Encoded {
				// Should we even be decoding these from base64? What are the implications?
				valueBytes, err := base64.StdEncoding.DecodeString(attribute.Value)
				if err != nil {
					return nil, err
				}

				keyBytes, err := base64.StdEncoding.DecodeString(attribute.Key)
				if err != nil {
					return nil, err
				}

				value = string(valueBytes)
				keyItem = string(keyBytes)
			} else {
				value = attribute.Value
				keyItem = attribute.Key
			}

			key := models.BlockEventAttributeKey{
				Key: keyItem,
			}

			beginBlockEvents[index].Attributes[attrIndex] = models.BlockEventAttribute{
				Value:                  value,
				BlockEventAttributeKey: key,
				Index:                  uint64(attrIndex),
			}

			uniqueAttributeKeys[key.Key] = key

		}

		if customParsers != nil {
			if customBlockEventParsers, ok := customParsers[event.Type]; ok {
				for parserIndex, customParser := range customBlockEventParsers {
					// We deliberately ignore the error here, as we want to continue processing the block events even if a custom parser fails
					parsedData, err := customParser.ParseBlockEvent(event, conf)
					beginBlockEvents[index].BlockEventParsedDatasets = append(beginBlockEvents[index].BlockEventParsedDatasets, parsers.BlockEventParsedData{
						Data:   parsedData,
						Error:  err,
						Parser: &customBlockEventParsers[parserIndex],
					})
				}
			}
		}

	}

	return beginBlockEvents, nil
}

func FilterRPCBlockEvents(blockEvents []db.BlockEventDBWrapper, filterRegistry filter.StaticBlockEventFilterRegistry) ([]db.BlockEventDBWrapper, error) {
	// If there are no filters, just return the block events
	if len(filterRegistry.BlockEventFilters) == 0 && len(filterRegistry.RollingWindowEventFilters) == 0 {
		return blockEvents, nil
	}

	filterIndexes := make(map[int]bool)

	// If filters are defined, we treat filters as a whitelist, and only include block events that match the filters and are allowed
	// Filters are evaluated in order, and the first filter that matches is the one that is used. Single block event filters are preferred in ordering.
	for index, blockEvent := range blockEvents {
		filterEvent := filter.EventData{
			Event:      blockEvent.BlockEvent,
			Attributes: blockEvent.Attributes,
		}

		for _, filter := range filterRegistry.BlockEventFilters {
			patternMatch, err := filter.EventMatches(filterEvent)
			if err != nil {
				return nil, err
			}
			if patternMatch {
				filterIndexes[index] = filter.IncludeMatch()
			}
		}

		for _, rollingWindowFilter := range filterRegistry.RollingWindowEventFilters {
			if index+rollingWindowFilter.RollingWindowLength() <= len(blockEvents) {
				lastIndex := index + rollingWindowFilter.RollingWindowLength()
				blockEventSlice := blockEvents[index:lastIndex]

				filterEvents := make([]filter.EventData, len(blockEventSlice))

				for index, blockEvent := range blockEventSlice {
					filterEvents[index] = filter.EventData{
						Event:      blockEvent.BlockEvent,
						Attributes: blockEvent.Attributes,
					}
				}

				patternMatches, err := rollingWindowFilter.EventsMatch(filterEvents)
				if err != nil {
					return nil, err
				}

				if patternMatches {
					for i := index; i < lastIndex; i++ {
						filterIndexes[i] = rollingWindowFilter.IncludeMatches()
					}
				}
			}
		}
	}

	// Filter the block events based on the indexes that matched the registered patterns
	filteredBlockEvents := make([]db.BlockEventDBWrapper, 0)

	for index, blockEvent := range blockEvents {
		if filterIndexes[index] {
			filteredBlockEvents = append(filteredBlockEvents, blockEvent)
		}
	}

	return filteredBlockEvents, nil
}
