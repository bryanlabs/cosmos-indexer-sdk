package config

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/DefiantLabs/cosmos-indexer/filter"
)

const (
	EventTypeKey                  = "event_type"
	EventTypeAndAttributeValueKey = "event_type_and_attribute_value"
	RegexEventTypeKey             = "regex_event_type"
	RollingWindowKey              = "rolling_window"
	MessageTypeKey                = "message_type"
	MessageTypeRegex              = "message_type_regex"
)

var SingleBlockEventFilterKeys = []string{
	EventTypeKey,
	EventTypeAndAttributeValueKey,
	RegexEventTypeKey,
}

var MessageTypeFilterKeys = []string{
	MessageTypeKey,
	MessageTypeRegex,
}

func SingleBlockEventFilterIncludes(val string) bool {
	for _, key := range SingleBlockEventFilterKeys {
		if key == val {
			return true
		}
	}
	return false
}

type blockFilterConfigs struct {
	BeginBlockFilters  []json.RawMessage `json:"begin_block_filters,omitempty"`
	EndBlockFilters    []json.RawMessage `json:"end_block_filters,omitempty"`
	MessageTypeFilters []json.RawMessage `json:"message_type_filters,omitempty"`
}

type BlockEventFilterConfig struct {
	Type       string            `json:"type"`
	Subfilters []json.RawMessage `json:"subfilters"`
	Inclusive  bool              `json:"inclusive"`
}

type MessageTypeFilterConfig struct {
	Type    string `json:"type"`
	Pattern string `json:"pattern"`
}

func ParseJSONFilterConfig(configJSON []byte) ([]filter.BlockEventFilter, []filter.RollingWindowBlockEventFilter, []filter.BlockEventFilter, []filter.RollingWindowBlockEventFilter, []filter.MessageTypeFilter, error) {
	config := blockFilterConfigs{}
	err := json.Unmarshal(configJSON, &config)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	beginBlockSingleEventFilters, beginBlockRollingWindowFilters, err := ParseLifecycleConfig(config.BeginBlockFilters)
	if err != nil {
		newErr := fmt.Errorf("error parsing begin_block_filters: %s", err)
		return nil, nil, nil, nil, nil, newErr
	}
	endBlockSingleEventFilters, endBlockRollingWindowFilters, err := ParseLifecycleConfig(config.EndBlockFilters)
	if err != nil {
		newErr := fmt.Errorf("error parsing end_block_filters: %s", err)
		return nil, nil, nil, nil, nil, newErr
	}

	messageTypeFilters, err := ParseTXMessageTypeConfig(config.MessageTypeFilters)
	if err != nil {
		newErr := fmt.Errorf("error parsing message_type_filters: %s", err)
		return nil, nil, nil, nil, nil, newErr
	}

	return beginBlockSingleEventFilters, beginBlockRollingWindowFilters, endBlockSingleEventFilters, endBlockRollingWindowFilters, messageTypeFilters, nil
}

func ParseLifecycleConfig(lifecycleConfig []json.RawMessage) ([]filter.BlockEventFilter, []filter.RollingWindowBlockEventFilter, error) {
	rollingWindowFilters := []filter.RollingWindowBlockEventFilter{}
	singleEventFilters := []filter.BlockEventFilter{}
	for index, beginFilters := range lifecycleConfig {

		newFilter := BlockEventFilterConfig{}

		err := json.Unmarshal(beginFilters, &newFilter)
		if err != nil {
			parserError := fmt.Errorf("error parsing filter at index %d: %s", index, err)
			return nil, nil, parserError
		}

		err = validateBlockEventFilterConfig(newFilter)
		if err != nil {
			parserError := fmt.Errorf("error parsing filter at index %d: %s", index, err)
			return nil, nil, parserError
		}

		switch {
		case newFilter.Type == RollingWindowKey:
			eventPatterns := []filter.BlockEventFilter{}
			for subfilterIndex, subfilter := range newFilter.Subfilters {
				newSubFilter := BlockEventFilterConfig{}
				err := json.Unmarshal(subfilter, &newSubFilter)
				if err != nil {
					parserError := fmt.Errorf("error parsing rolling window filter at index %d and subfilter index %d: %s", index, subfilterIndex, err)
					return nil, nil, parserError
				}
				parsedFilter, err := ParseJSONFilterConfigFromType(newSubFilter.Type, subfilter)
				if err != nil {
					parserError := fmt.Errorf("error parsing rolling window filter at index %d and subfilter index %d: %s", index, subfilterIndex, err)
					return nil, nil, parserError
				}
				eventPatterns = append(eventPatterns, parsedFilter)
			}
			newRollingFilter := filter.NewDefaultRollingWindowBlockEventFilter(eventPatterns, newFilter.Inclusive)
			valid, err := newRollingFilter.Valid()
			if !valid || err != nil {
				parserError := fmt.Errorf("error parsing rolling window filter at index %d: %s", index, err)
				return nil, nil, parserError
			}
			rollingWindowFilters = append(rollingWindowFilters, newRollingFilter)
		case SingleBlockEventFilterIncludes(newFilter.Type):
			parsedFilter, err := ParseJSONFilterConfigFromType(newFilter.Type, beginFilters)
			if err != nil {
				parserError := fmt.Errorf("error parsing filter at index %d: %s", index, err)
				return nil, nil, parserError
			}
			valid, err := parsedFilter.Valid()
			if !valid || err != nil {
				parserError := fmt.Errorf("error parsing filter at index %d: %s", index, err)
				return nil, nil, parserError
			}
			singleEventFilters = append(singleEventFilters, parsedFilter)
		default:
			parserError := fmt.Errorf("error parsing filter at index %d: unknown filter type \"%s\"", index, newFilter.Type)
			return nil, nil, parserError
		}

		if err != nil {
			parserError := fmt.Errorf("error parsing filter at index %d: %s", index, err)
			return nil, nil, parserError
		}
	}

	return singleEventFilters, rollingWindowFilters, nil
}

func ParseJSONFilterConfigFromType(filterType string, configJSON []byte) (filter.BlockEventFilter, error) {
	switch filterType {
	case EventTypeKey:
		newFilter := filter.DefaultBlockEventTypeFilter{}

		err := json.Unmarshal(configJSON, &newFilter)
		if err != nil {
			return nil, err
		}
		return newFilter, nil
	case EventTypeAndAttributeValueKey:
		newFilter := filter.DefaultBlockEventTypeAndAttributeValueFilter{}

		err := json.Unmarshal(configJSON, &newFilter)
		if err != nil {
			return nil, err
		}
		return newFilter, nil
	case RegexEventTypeKey:
		newFilter := filter.RegexBlockEventTypeFilter{}

		err := json.Unmarshal(configJSON, &newFilter)
		if err != nil {
			return nil, err
		}

		// Reinit the filter so that regex compiles
		regexFilter, err := filter.NewRegexBlockEventFilter(newFilter.EventTypeRegexPattern, newFilter.Inclusive)
		if err != nil {
			return nil, err
		}
		return regexFilter, nil
	default:
		return nil, fmt.Errorf("unknown filter type %s", filterType)
	}
}

func ParseTXMessageTypeConfig(messageTypeConfigs []json.RawMessage) ([]filter.MessageTypeFilter, error) {
	messageTypeFilters := []filter.MessageTypeFilter{}
	for index, messageTypeConfig := range messageTypeConfigs {
		newFilter := MessageTypeFilterConfig{}

		err := json.Unmarshal(messageTypeConfig, &newFilter)
		if err != nil {
			parserError := fmt.Errorf("error parsing message type filter at index %d: %s", index, err)
			return nil, parserError
		}

		err = validateMessageTypeFilterConfig(newFilter)
		if err != nil {
			parserError := fmt.Errorf("error parsing filter at index %d: %s", index, err)
			return nil, parserError
		}

		switch newFilter.Type {
		case MessageTypeKey:
			newFilter := filter.DefaultMessageTypeFilter{}
			err := json.Unmarshal(messageTypeConfig, &newFilter)
			if err != nil {
				return nil, err
			}
			valid, err := newFilter.Valid()

			if !valid || err != nil {
				parserError := fmt.Errorf("error parsing filter at index %d: %s", index, err)
				return nil, parserError
			}
			messageTypeFilters = append(messageTypeFilters, newFilter)
		case MessageTypeRegex:
			newFilter := filter.MessageTypeRegexFilter{}
			err := json.Unmarshal(messageTypeConfig, &newFilter)
			if err != nil {
				return nil, err
			}

			newFilter, err = filter.NewRegexMessageTypeFilter(newFilter.MessageTypeRegexPattern, newFilter.ShouldIgnore)
			if err != nil {
				parserError := fmt.Errorf("error parsing filter at index %d: %s", index, err)
				return nil, parserError
			}

			valid, err := newFilter.Valid()

			if !valid || err != nil {
				parserError := fmt.Errorf("error parsing filter at index %d: %s", index, err)
				return nil, parserError
			}
			messageTypeFilters = append(messageTypeFilters, newFilter)
		default:
			parserError := fmt.Errorf("error parsing filter at index %d: unknown filter type \"%s\"", index, newFilter.Type)
			return nil, parserError
		}
	}
	return messageTypeFilters, nil
}

func validateBlockEventFilterConfig(config BlockEventFilterConfig) error {
	if config.Type == "" {
		return errors.New("filter config must have a type field")
	}
	return nil
}

func validateMessageTypeFilterConfig(config MessageTypeFilterConfig) error {
	if config.Type == "" {
		return errors.New("filter config must have a type field")
	}
	return nil
}
