package core

import (
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	"gorm.io/gorm"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/filter"
	"github.com/DefiantLabs/cosmos-indexer/parsers"
)

type swapEventParser struct{}

func (swapEventParser) Identifier() string { return "swap" }

func (swapEventParser) ParseBlockEvent(abci.Event, config.IndexConfig) (*any, error) {
	var v any = "parsed"
	return &v, nil
}

func (swapEventParser) IndexBlockEvent(*any, *gorm.DB, models.Block, models.BlockEvent, []models.BlockEventAttribute, config.IndexConfig) error {
	return nil
}

func TestCustomBlockEventParserAttachesToMatchingEvent(t *testing.T) {
	events := []abci.Event{
		{Type: "complete_unbonding"},
		{Type: "complete_redelegation"},
		{Type: "swap_transacted"},
	}
	custom := map[string][]parsers.BlockEventParser{"swap_transacted": {swapEventParser{}}}
	got, err := ProcessRPCBlockEvents(&models.Block{Height: 7000000}, events, models.EndBlockEvent,
		map[string]models.BlockEventType{}, map[string]models.BlockEventAttributeKey{}, custom, config.IndexConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range got {
		want := 0
		if i == 2 {
			want = 1
		}
		if len(e.BlockEventParsedDatasets) != want {
			t.Fatalf("event %d (%s) has %d parsed datasets, want %d", i, events[i].Type, len(e.BlockEventParsedDatasets), want)
		}
	}
}

func filterEvent(eventType string, kv ...string) db.BlockEventDBWrapper {
	w := db.BlockEventDBWrapper{BlockEvent: models.BlockEvent{BlockEventType: models.BlockEventType{Type: eventType}}}
	for i := 0; i < len(kv); i += 2 {
		w.Attributes = append(w.Attributes, models.BlockEventAttribute{Value: kv[i+1], BlockEventAttributeKey: models.BlockEventAttributeKey{Key: kv[i]}})
	}
	return w
}

// The history filter keeps BeginBlock coin movements except routine module
// flows, by following an inclusive event_type filter with exclusive
// attribute-value filters.
func TestLaterExclusiveFilterCarvesOutOfInclusiveOne(t *testing.T) {
	const distribution = "cosmos1jv65s3grqf6v6jl3dp4t6c9t9rk99cd88lyufl"
	const feeCollector = "cosmos17xpfvakm2amg962yls6f84z3kell8c5lserqta"
	filters := []byte(`{"begin_block_filters": [
		{"type": "event_type", "event_type": "slash", "inclusive": true},
		{"type": "event_type", "event_type": "coin_received", "inclusive": true},
		{"type": "event_type_and_attribute_value", "event_type": "coin_received", "attribute_key": "receiver", "attribute_value": "` + distribution + `", "inclusive": false},
		{"type": "event_type", "event_type": "transfer", "inclusive": true},
		{"type": "event_type_and_attribute_value", "event_type": "transfer", "attribute_key": "recipient", "attribute_value": "` + feeCollector + `", "inclusive": false}
	]}`)
	begin, rolling, _, _, _, err := config.ParseJSONFilterConfig(filters)
	if err != nil {
		t.Fatal(err)
	}
	events := []db.BlockEventDBWrapper{
		filterEvent("coin_received", "receiver", distribution, "amount", "1uatom"),
		filterEvent("coin_received", "receiver", "cosmos1z8pq5cn7tdrwwzlwm0e44fgq07e43ner6vph64", "amount", "1uatom"),
		filterEvent("transfer", "recipient", feeCollector, "amount", "1uatom"),
		filterEvent("commission", "amount", "1uatom"),
		filterEvent("slash", "reason", "missing_signature"),
	}
	got, err := FilterRPCBlockEvents(events, filter.StaticBlockEventFilterRegistry{BlockEventFilters: begin, RollingWindowEventFilters: rolling})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Attributes[0].Value != "cosmos1z8pq5cn7tdrwwzlwm0e44fgq07e43ner6vph64" || got[1].BlockEvent.BlockEventType.Type != "slash" {
		t.Fatalf("kept %d events, want the user receipt and the slash: %+v", len(got), got)
	}
}
