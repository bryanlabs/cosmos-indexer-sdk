package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	amino "github.com/tendermint/go-amino"
	abci "github.com/tendermint/tendermint/abci/types"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	"github.com/tendermint/tendermint/types"
)

const dirSpan = 10000

var cdc = func() *amino.Codec {
	c := amino.NewCodec()
	ctypes.RegisterAmino(c)
	return c
}()

// cached is one height as the archive node returned it.
type cached struct {
	height  int64
	block   *types.Block
	metaID  []byte
	results *cachedResults
	err     error
}

type cachedResults struct {
	deliver []*abci.ResponseDeliverTx
	begin   *abci.ResponseBeginBlock
	end     *abci.ResponseEndBlock
}

func readGzip(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(bufio.NewReaderSize(f, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("not gzip")
	}
	body, err := io.ReadAll(io.LimitReader(zr, 512<<20))
	if err != nil {
		return nil, fmt.Errorf("gzip stream damaged")
	}
	return body, nil
}

func basePath(root, chain string, height int64) string {
	return filepath.Join(root, chain, strconv.FormatInt(height/dirSpan, 10), strconv.FormatInt(height, 10))
}

// loadBlock reads the block capture of one height and checks that the file
// holds the height and chain it is named for.
func loadBlock(root, chain string, height int64) (*types.Block, []byte, error) {
	raw, err := readGzip(basePath(root, chain, height) + "-block.raw.gz")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("block capture absent")
		}
		return nil, nil, fmt.Errorf("block capture unreadable: %v", err)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &envelope) != nil || len(envelope.Result) == 0 {
		return nil, nil, fmt.Errorf("block envelope invalid")
	}
	var result ctypes.ResultBlock
	if err := cdc.UnmarshalJSON(envelope.Result, &result); err != nil {
		return nil, nil, fmt.Errorf("block does not decode")
	}
	if result.Block == nil || result.BlockMeta == nil {
		return nil, nil, fmt.Errorf("block or its meta absent")
	}
	if result.Block.ChainID != chain || result.Block.Height != height {
		return nil, nil, fmt.Errorf("block identity does not match its file")
	}
	return result.Block, result.BlockMeta.BlockID.Hash, nil
}

func loadResults(root, chain string, height int64) (*cachedResults, error) {
	raw, err := readGzip(basePath(root, chain, height) + "-block_results.raw.gz")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("result capture absent")
		}
		return nil, fmt.Errorf("result capture unreadable: %v", err)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &envelope) != nil || len(envelope.Result) == 0 {
		return nil, fmt.Errorf("result envelope invalid")
	}
	var result ctypes.ResultBlockResults
	if err := cdc.UnmarshalJSON(envelope.Result, &result); err != nil {
		return nil, fmt.Errorf("result does not decode")
	}
	if result.Height != height || result.Results == nil {
		return nil, fmt.Errorf("result identity does not match its file")
	}
	return &cachedResults{deliver: result.Results.DeliverTx, begin: result.Results.BeginBlock, end: result.Results.EndBlock}, nil
}

func load(root, chain string, height int64, withResults bool) cached {
	c := cached{height: height}
	c.block, c.metaID, c.err = loadBlock(root, chain, height)
	if c.err == nil {
		c.err = checkBlock(c)
	}
	if c.err == nil && withResults {
		c.results, c.err = loadResults(root, chain, height)
	}
	return c
}

// checkBlock proves the parts of a cached block against its own header and
// the header against the hash the node gave for the block. A block that
// passes cannot have a vote, a piece of evidence, a transaction or a header
// field altered or missing without the block hash changing.
func checkBlock(c cached) error {
	b := c.block
	if !bytes.Equal(b.Header.Hash(), c.metaID) || len(c.metaID) == 0 {
		return fmt.Errorf("header does not hash to the block hash")
	}
	if err := b.ValidateBasic(); err != nil {
		return fmt.Errorf("block parts do not match the header")
	}
	if len(b.ProposerAddress) != 20 {
		return fmt.Errorf("proposer absent")
	}
	if b.Height > 1 {
		if b.LastCommit == nil || len(b.LastCommit.Precommits) == 0 {
			return fmt.Errorf("previous block votes absent")
		}
		if !b.LastCommit.BlockID.Equals(b.LastBlockID) {
			return fmt.Errorf("previous block votes are for another block")
		}
	}
	return nil
}

// feed reads heights from..to in order through a pool of readers.
func feed(root, chain string, from, to int64, workers int, withResults bool) <-chan cached {
	type slot chan cached
	order := make(chan slot, workers*64)
	jobs := make(chan struct {
		h int64
		s slot
	}, workers*64)
	for i := 0; i < workers; i++ {
		go func() {
			for j := range jobs {
				j.s <- load(root, chain, j.h, withResults)
			}
		}()
	}
	go func() {
		for h := from; h <= to; h++ {
			s := make(slot, 1)
			order <- s
			jobs <- struct {
				h int64
				s slot
			}{h, s}
		}
		close(order)
		close(jobs)
	}()
	out := make(chan cached, workers*64)
	go func() {
		for s := range order {
			out <- <-s
		}
		close(out)
	}()
	return out
}
