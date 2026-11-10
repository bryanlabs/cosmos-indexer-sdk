package core

import (
	"compress/gzip"
	"embed"
	"encoding/json"
	"fmt"
	"io"

	abci "github.com/cometbft/cometbft/abci/types"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
)

// Some state changes reached the chain without events. The Gaia v11 upgrade at
// cosmoshub-4 16596000 withdrew every Gravity DEX pool coin inside a cache
// context whose events SDK 0.45 discarded, so block_results shows pool coins
// burned and reserves paid out with nothing saying who got what. The events it
// would have emitted are rebuilt from archive balances, kept here, and stored
// after the block's own BeginBlock events (the upgrade ran first in BeginBlock,
// but the emitted events already hold those indexes).

//go:embed reconstructed
var reconstructedFS embed.FS

// reconstructedFiles maps chain ID and height to the rebuilt events.
var reconstructedFiles = map[string]map[int64]string{}

// ReconstructedBlock is one block's rebuilt BeginBlock events.
// EmittedBeginBlockEvents is how many BeginBlock events the chain itself
// returned for the block; the rebuilt events are indexed after them.
type ReconstructedBlock struct {
	ChainID                 string       `json:"chain_id"`
	Height                  int64        `json:"height"`
	EmittedBeginBlockEvents int          `json:"emitted_begin_block_events"`
	BeginBlockEvents        []abci.Event `json:"begin_block_events"`
}

// ReconstructedBeginBlockEvents returns the rebuilt events for a block, or nil
// when the block has none.
func ReconstructedBeginBlockEvents(chainID string, height int64) (*ReconstructedBlock, error) {
	name, ok := reconstructedFiles[chainID][height]
	if !ok {
		return nil, nil
	}
	f, err := reconstructedFS.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	block, err := parseReconstructedBlock(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if block.ChainID != chainID || block.Height != height {
		return nil, fmt.Errorf("%s holds %s %d", name, block.ChainID, block.Height)
	}
	return block, nil
}

func parseReconstructedBlock(r io.Reader) (*ReconstructedBlock, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	dec := json.NewDecoder(gz)
	dec.DisallowUnknownFields()
	var block ReconstructedBlock
	if err := dec.Decode(&block); err != nil {
		return nil, err
	}
	if len(block.BeginBlockEvents) == 0 || block.EmittedBeginBlockEvents < 0 {
		return nil, fmt.Errorf("no rebuilt events")
	}
	return &block, nil
}

// AppendReconstructedBlockEvents adds a block's rebuilt BeginBlock events to
// the wrapper after its filters ran, so a filter cannot drop them. emitted is
// the number of BeginBlock events the chain returned before filtering; it must
// match the count the events were rebuilt against, or the indexes would
// collide with the chain's own.
func AppendReconstructedBlockEvents(conf config.IndexConfig, chainID string, wrapper *db.BlockDBWrapper, emitted int) error {
	block, err := ReconstructedBeginBlockEvents(chainID, wrapper.Block.Height)
	if err != nil || block == nil {
		return err
	}
	return appendReconstructed(conf, block, wrapper, emitted)
}

func appendReconstructed(conf config.IndexConfig, block *ReconstructedBlock, wrapper *db.BlockDBWrapper, emitted int) error {
	if emitted < 0 || emitted != block.EmittedBeginBlockEvents {
		return fmt.Errorf("block %d returned %d BeginBlock events, rebuilt events expect %d", block.Height, emitted, block.EmittedBeginBlockEvents)
	}
	// The rebuilt attributes are plain text whatever the chain's encoding.
	conf.Flags.BlockEventsBase64Encoded = false
	events, err := ProcessRPCBlockEvents(wrapper.Block, block.BeginBlockEvents, models.BeginBlockEvent, wrapper.UniqueBlockEventTypes, wrapper.UniqueBlockEventAttributeKeys, nil, conf)
	if err != nil {
		return err
	}
	for i := range events {
		events[i].BlockEvent.Index += uint64(emitted)
	}
	wrapper.BeginBlockEvents = append(wrapper.BeginBlockEvents, events...)
	return nil
}
