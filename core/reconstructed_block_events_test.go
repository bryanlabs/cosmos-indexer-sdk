package core

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
)

func gzipped(t *testing.T, s string) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, err := gz.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}

func TestParseReconstructedBlock(t *testing.T) {
	good := `{"chain_id":"c","height":5,"emitted_begin_block_events":2,"begin_block_events":[{"type":"burn","attributes":[{"key":"burner","value":"x"}]}]}`
	block, err := parseReconstructedBlock(gzipped(t, good))
	if err != nil {
		t.Fatal(err)
	}
	if block.Height != 5 || block.EmittedBeginBlockEvents != 2 || len(block.BeginBlockEvents) != 1 || block.BeginBlockEvents[0].Attributes[0].Value != "x" {
		t.Fatalf("parsed %+v", block)
	}
	for _, bad := range []string{
		`{"chain_id":"c","height":5,"emitted_begin_block_events":2,"begin_block_events":[]}`,
		`{"chain_id":"c","height":5,"emitted_begin_block_events":2,"begin_block_events":[{"type":"burn"}],"extra":1}`,
		`{"chain_id":"c","height":5,"emitted_begin_block_events":-1,"begin_block_events":[{"type":"burn"}]}`,
	} {
		if _, err := parseReconstructedBlock(gzipped(t, bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if _, err := parseReconstructedBlock(strings.NewReader(good)); err == nil {
		t.Fatal("accepted an uncompressed file")
	}
}

// The rebuilt events follow the chain's own BeginBlock events, keep plain
// text attributes when the chain's are base64, and refuse a block whose
// emitted count differs from the one they were rebuilt against.
func TestAppendReconstructedIndexesAfterEmittedEvents(t *testing.T) {
	rebuilt := &ReconstructedBlock{ChainID: "c", Height: 5, EmittedBeginBlockEvents: 3, BeginBlockEvents: []abci.Event{
		{Type: "withdraw_from_pool", Attributes: []abci.EventAttribute{{Key: "withdrawer", Value: "cosmos1a"}}},
		{Type: "burn", Attributes: []abci.EventAttribute{{Key: "burner", Value: "cosmos1b"}}},
	}}
	wrapper := &db.BlockDBWrapper{
		Block:                         &models.Block{Height: 5},
		BeginBlockEvents:              []db.BlockEventDBWrapper{{BlockEvent: models.BlockEvent{Index: 1}}},
		UniqueBlockEventTypes:         map[string]models.BlockEventType{},
		UniqueBlockEventAttributeKeys: map[string]models.BlockEventAttributeKey{},
	}
	var conf config.IndexConfig
	conf.Flags.BlockEventsBase64Encoded = true
	if err := appendReconstructed(conf, rebuilt, wrapper, 2); err == nil {
		t.Fatal("accepted a block with a different emitted count")
	}
	if len(wrapper.BeginBlockEvents) != 1 {
		t.Fatal("a refused block changed the wrapper")
	}
	if err := appendReconstructed(conf, rebuilt, wrapper, 3); err != nil {
		t.Fatal(err)
	}
	got := wrapper.BeginBlockEvents
	if len(got) != 3 {
		t.Fatalf("%d events, want 3", len(got))
	}
	for i, want := range []struct {
		index uint64
		typ   string
		value string
	}{{3, "withdraw_from_pool", "cosmos1a"}, {4, "burn", "cosmos1b"}} {
		e := got[i+1]
		if e.BlockEvent.Index != want.index || e.BlockEvent.LifecyclePosition != models.BeginBlockEvent || e.BlockEvent.BlockEventType.Type != want.typ || e.Attributes[0].Value != want.value {
			t.Fatalf("event %d = %+v", i, e.BlockEvent)
		}
	}
	if _, ok := wrapper.UniqueBlockEventTypes["burn"]; !ok {
		t.Fatal("event type not registered for upsert")
	}
	if _, ok := wrapper.UniqueBlockEventAttributeKeys["burner"]; !ok {
		t.Fatal("attribute key not registered for upsert")
	}
}

func TestReconstructedBeginBlockEventsUnknownBlock(t *testing.T) {
	block, err := ReconstructedBeginBlockEvents("cosmoshub-4", 16595999)
	if err != nil || block != nil {
		t.Fatalf("got %v, %v", block, err)
	}
}
