package core

import (
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	"gorm.io/gorm"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
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
