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

// Load reads one cached height into the shapes the decoder takes. A height
// with either capture absent returns os.ErrNotExist.
func Load(root, chain string, height int64, limit int64) (*coretypes.ResultBlock, *rpc.CustomBlockResults, error) {
	base := filepath.Join(root, chain, strconv.FormatInt(height/DirSpan, 10), strconv.FormatInt(height, 10))
	raw, err := readCapture(base+"-block.raw.gz", limit)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, os.ErrNotExist
		}
		return nil, nil, fmt.Errorf("block capture unreadable")
	}
	var block struct {
		Result struct {
			Block struct {
				Header struct {
					ChainID string    `json:"chain_id"`
					Height  int64     `json:"height,string"`
					Time    time.Time `json:"time"`
				} `json:"header"`
				Data struct {
					Txs []string `json:"txs"`
				} `json:"data"`
			} `json:"block"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &block) != nil {
		return nil, nil, fmt.Errorf("block JSON invalid")
	}
	header := block.Result.Block.Header
	if header.ChainID != chain || header.Height != height {
		return nil, nil, fmt.Errorf("block identity does not match its file")
	}
	txs := make(tm.Txs, 0, len(block.Result.Block.Data.Txs))
	for _, encoded := range block.Result.Block.Data.Txs {
		bytes, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, nil, fmt.Errorf("transaction is not base64")
		}
		txs = append(txs, bytes)
	}
	raw, err = readCapture(base+"-block_results.raw.gz", limit)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, os.ErrNotExist
		}
		return nil, nil, fmt.Errorf("result capture unreadable")
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &envelope) != nil || len(envelope.Result) == 0 {
		return nil, nil, fmt.Errorf("result envelope invalid")
	}
	var results rpc.CustomBlockResults
	if err := tmjson.Unmarshal(envelope.Result, &results); err != nil {
		return nil, nil, fmt.Errorf("result JSON invalid")
	}
	if results.Height != height {
		return nil, nil, fmt.Errorf("result height does not match its file")
	}
	return &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: chain, Height: height, Time: header.Time}, Data: tm.Data{Txs: txs}}}, &results, nil
}
