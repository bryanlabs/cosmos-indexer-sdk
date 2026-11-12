// Package capture reads the retained block and block result captures of a
// Cosmos Hub 1 to 3 height from a cache directory. It makes no network calls.
package capture

import (
	"bufio"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/rpc"
	tmjson "github.com/cometbft/cometbft/libs/json"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	tm "github.com/cometbft/cometbft/types"
)

// DirSpan is the number of heights kept in one cache directory.
const DirSpan = 10000

func readCapture(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(bufio.NewReaderSize(f, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("capture is not gzip")
	}
	body, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, fmt.Errorf("capture gzip stream damaged")
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("capture exceeds decompressed limit")
	}
	return body, nil
}

// Height is one cached height: the block and its results in the shapes the
// decoder takes, plus the parts of the block the ledger keeps beside them.
type Height struct {
	Block   *coretypes.ResultBlock
	Results *rpc.CustomBlockResults
	// Hash is the block's own hash and Previous the hash it names as its
	// parent, both upper case hex. Proposer is the hex validator address.
	Hash, Previous, Proposer string
	// Evidence holds each piece of misbehaviour evidence the block carried,
	// as the JSON the node returned.
	Evidence []json.RawMessage
}

// Load reads one cached height into the shapes the decoder takes. A height
// with either capture absent returns os.ErrNotExist.
func Load(root, chain string, height int64, limit int64) (*coretypes.ResultBlock, *rpc.CustomBlockResults, error) {
	h, err := LoadHeight(root, chain, height, limit)
	if err != nil {
		return nil, nil, err
	}
	return h.Block, h.Results, nil
}

// LoadHeight reads one cached height. A height with either capture absent
// returns os.ErrNotExist.
func LoadHeight(root, chain string, height int64, limit int64) (*Height, error) {
	return load(root, chain, height, limit, false)
}

// LoadTransactions reads one cached height for a reader that only wants its
// transactions and their results. When the block holds no transaction the
// result capture is checked to be present and is not read, and Results is
// nil.
func LoadTransactions(root, chain string, height int64, limit int64) (*Height, error) {
	return load(root, chain, height, limit, true)
}

func load(root, chain string, height int64, limit int64, transactionsOnly bool) (*Height, error) {
	base := filepath.Join(root, chain, strconv.FormatInt(height/DirSpan, 10), strconv.FormatInt(height, 10))
	raw, err := readCapture(base+"-block.raw.gz", limit)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("block capture unreadable")
	}
	type blockID struct {
		Hash string `json:"hash"`
	}
	var block struct {
		Result struct {
			BlockMeta struct {
				BlockID blockID `json:"block_id"`
			} `json:"block_meta"`
			Block struct {
				Header struct {
					ChainID         string    `json:"chain_id"`
					Height          int64     `json:"height,string"`
					Time            time.Time `json:"time"`
					LastBlockID     blockID   `json:"last_block_id"`
					ProposerAddress string    `json:"proposer_address"`
				} `json:"header"`
				Data struct {
					Txs []string `json:"txs"`
				} `json:"data"`
				Evidence struct {
					Evidence []json.RawMessage `json:"evidence"`
				} `json:"evidence"`
			} `json:"block"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &block) != nil {
		return nil, fmt.Errorf("block JSON invalid")
	}
	header := block.Result.Block.Header
	if header.ChainID != chain || header.Height != height {
		return nil, fmt.Errorf("block identity does not match its file")
	}
	txs := make(tm.Txs, 0, len(block.Result.Block.Data.Txs))
	for _, encoded := range block.Result.Block.Data.Txs {
		bytes, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("transaction is not base64")
		}
		txs = append(txs, bytes)
	}
	found := &Height{
		Block:    &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: chain, Height: height, Time: header.Time}, Data: tm.Data{Txs: txs}}},
		Hash:     block.Result.BlockMeta.BlockID.Hash,
		Previous: header.LastBlockID.Hash,
		Proposer: header.ProposerAddress,
		Evidence: block.Result.Block.Evidence.Evidence,
	}
	if transactionsOnly && len(txs) == 0 {
		if _, err := os.Stat(base + "-block_results.raw.gz"); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, os.ErrNotExist
			}
			return nil, fmt.Errorf("result capture unreadable")
		}
		return found, nil
	}
	raw, err = readCapture(base+"-block_results.raw.gz", limit)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("result capture unreadable")
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &envelope) != nil || len(envelope.Result) == 0 {
		return nil, fmt.Errorf("result envelope invalid")
	}
	var results rpc.CustomBlockResults
	if err := tmjson.Unmarshal(envelope.Result, &results); err != nil {
		return nil, fmt.Errorf("result JSON invalid")
	}
	if results.Height != height {
		return nil, fmt.Errorf("result height does not match its file")
	}
	found.Results = &results
	return found, nil
}
