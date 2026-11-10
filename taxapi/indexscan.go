package taxapi

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// nodeTx is one transaction of a block as the chain has it.
type nodeTx struct {
	Hash     string
	Code     uint32
	Accounts []string
	// Charged is the coins a failed transaction's fee deduction moved.
	Charged string
	// Granter is the account whose fee grant paid the fee, if any.
	Granter string
	// Rejected is a failed transaction with no events: the ante handler
	// turned it away, so it changed nothing, not even the fee, and the same
	// bytes can be included again in a later block.
	Rejected bool
}

// rpcResult reads one RPC response at height, from the block cache when the
// node has one. The cache keeps each response whole, gzipped, under the
// height's ten-thousand-block folder.
func (n *ledgerNode) rpcResult(method string, height int64, out any) error {
	if n.cache == "" {
		return n.rpc.GetJSON("/"+method, url.Values{"height": {strconv.FormatInt(height, 10)}}, out)
	}
	f, err := os.Open(filepath.Join(n.cache, strconv.FormatInt(height/10000, 10), fmt.Sprintf("%d-%s.raw.gz", height, method)))
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s %d: %w", method, height, err)
	}
	if err := json.NewDecoder(gz).Decode(out); err != nil {
		return fmt.Errorf("%s %d: %w", method, height, err)
	}
	return nil
}

// blockTxs reads a block's transactions from the node with every account
// each one names in its bytes or any of its events, the fee a failed one was
// charged and the granter of a fee grant that paid.
func (n *ledgerNode) blockTxs(height int64) ([]nodeTx, error) {
	var blk struct {
		Result struct {
			Block struct {
				Header struct {
					Height string `json:"height"`
				} `json:"header"`
				Data struct {
					Txs []string `json:"txs"`
				} `json:"data"`
			} `json:"block"`
		} `json:"result"`
	}
	if err := n.rpcResult("block", height, &blk); err != nil {
		return nil, err
	}
	if h := blk.Result.Block.Header.Height; h != strconv.FormatInt(height, 10) {
		return nil, fmt.Errorf("block %d: response is for height %q", height, h)
	}
	var res struct {
		Result struct {
			Height     string `json:"height"`
			TxsResults []struct {
				Code   uint32 `json:"code"`
				Events []struct {
					Type       string    `json:"type"`
					Attributes []rpcAttr `json:"attributes"`
				} `json:"events"`
			} `json:"txs_results"`
		} `json:"result"`
	}
	if err := n.rpcResult("block_results", height, &res); err != nil {
		return nil, err
	}
	if h := res.Result.Height; h != strconv.FormatInt(height, 10) {
		return nil, fmt.Errorf("block results %d: response is for height %q", height, h)
	}
	txs, results := blk.Result.Block.Data.Txs, res.Result.TxsResults
	if len(txs) != len(results) {
		return nil, fmt.Errorf("block %d has %d txs and %d results", height, len(txs), len(results))
	}
	pat := regexp.MustCompile(sdk.GetConfig().GetBech32AccountAddrPrefix() + `1[02-9ac-hj-np-z]{38,58}`)
	out := make([]nodeTx, 0, len(txs))
	for i, t := range results {
		raw, err := base64.StdEncoding.DecodeString(txs[i])
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		found := map[string]bool{}
		for _, m := range pat.FindAll(raw, -1) {
			found[string(m)] = true
		}
		var spent []string
		granter := ""
		for _, e := range t.Events {
			for _, a := range e.Attributes {
				vals := []string{a.Value}
				if v, err := base64.StdEncoding.DecodeString(a.Value); err == nil {
					vals = append(vals, string(v))
				}
				for _, v := range vals {
					for _, m := range pat.FindAllString(v, -1) {
						found[m] = true
					}
				}
				switch k, v := a.decoded(); {
				case e.Type == "coin_spent" && k == "amount":
					spent = append(spent, v)
				case e.Type == "use_feegrant" && k == "granter":
					granter = v
				}
			}
		}
		tx := nodeTx{Hash: strings.ToUpper(hex.EncodeToString(sum[:])), Code: t.Code, Granter: granter, Rejected: t.Code != 0 && len(t.Events) == 0}
		if t.Code != 0 {
			tx.Charged = strings.Join(spent, ",")
		}
		for a := range found {
			tx.Accounts = append(tx.Accounts, a)
		}
		sort.Strings(tx.Accounts)
		out = append(out, tx)
	}
	return out, nil
}

type indexedTx struct {
	Height   int64
	Indexed  bool
	Hash     string
	Code     uint32
	Messages bool
}

// scanHole compares one block's transactions on chain with the index. It
// returns the hole to record, if any: the distinct transactions the chain
// holds, and every account named by a transaction the index lacks or holds
// without its messages. A rejected transaction needs no index row, since it
// changed nothing; the index keeps its hash at the block that included it
// again.
func scanHole(chain []nodeTx, index []indexedTx) (int, []string) {
	have := map[string]indexedTx{}
	for _, x := range index {
		if x.Hash != "" {
			have[strings.ToUpper(x.Hash)] = x
		}
	}
	distinct := map[string]bool{}
	accounts := map[string]bool{}
	short := false
	for _, t := range chain {
		if t.Rejected || distinct[t.Hash] {
			continue
		}
		distinct[t.Hash] = true
		x, ok := have[t.Hash]
		if ok && (x.Code != 0 || x.Messages) {
			continue
		}
		short = true
		for _, a := range t.Accounts {
			accounts[a] = true
		}
	}
	if !short {
		return 0, nil
	}
	out := make([]string, 0, len(accounts))
	for a := range accounts {
		out = append(out, a)
	}
	sort.Strings(out)
	return len(distinct), out
}

// SweepIndexScan extends the scanned span over up to limit newly indexed
// blocks, recording a hole for each block missing a transaction or holding a
// successful one without its messages, so balances can be rebuilt up to the
// newest scanned block. It stops at the first block not yet indexed.
func (s *Server) SweepIndexScan(chain string, ids []int64, limit int64, logf func(string, ...any)) {
	scan, err := s.ledgerScan(chain)
	if err != nil || scan.FeeGrantsTo == 0 {
		return
	}
	node, err := newLedgerNode()
	if err != nil {
		return
	}
	latest, err := node.latestHeight()
	if err != nil {
		logf("index scan: node height: %v", err)
		return
	}
	lo, hi := scan.FeeGrantsTo+1, min(scan.FeeGrantsTo+limit, latest)
	if lo > hi {
		return
	}
	var rows []indexedTx
	if err := s.db.Raw(`
SELECT b.height, b.tx_indexed AND b.block_events_indexed AS indexed, COALESCE(x.hash, '') AS hash, COALESCE(x.code, 0) AS code,
       EXISTS (SELECT 1 FROM messages m WHERE m.tx_id = x.id) AS messages
FROM blocks b LEFT JOIN txes x ON x.block_id = b.id
WHERE b.chain_id IN @ids AND b.height BETWEEN @lo AND @hi
ORDER BY b.height`, map[string]any{"ids": ids, "lo": lo, "hi": hi}).Scan(&rows).Error; err != nil {
		logf("index scan: %v", err)
		return
	}
	byHeight := map[int64][]indexedTx{}
	for _, r := range rows {
		byHeight[r.Height] = append(byHeight[r.Height], r)
	}
	done, holes := scan.FeeGrantsTo, 0
	for h := lo; h <= hi; h++ {
		idx, ok := byHeight[h]
		if !ok || !idx[0].Indexed {
			break
		}
		txs, err := node.blockTxs(h)
		if err != nil {
			logf("index scan: block %d: %v", h, err)
			break
		}
		if n, accounts := scanHole(txs, idx); n > 0 {
			if err := s.recordIndexHole(chain, h, n, accounts); err != nil {
				logf("index scan: record hole at %d: %v", h, err)
				break
			}
			holes++
		}
		done = h
	}
	if done == scan.FeeGrantsTo {
		return
	}
	if err := s.db.Model(&LedgerScan{}).Where("chain = ?", chain).Update("fee_grants_to", done).Error; err != nil {
		logf("index scan: advance span: %v", err)
		return
	}
	if holes > 0 {
		logf("index scan: scanned to height %d, %d blocks with missing transactions", done, holes)
	}
}

func (s *Server) recordIndexHole(chain string, height int64, chainTxs int, accounts []string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&IndexHole{Chain: chain, Height: height, ChainTxs: chainTxs, AddressesKnown: true}).Error; err != nil {
			return err
		}
		if len(accounts) == 0 {
			return nil
		}
		rows := make([]IndexHoleAddress, 0, len(accounts))
		for _, a := range accounts {
			rows = append(rows, IndexHoleAddress{Chain: chain, Height: height, Address: a})
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 500).Error
	})
}
