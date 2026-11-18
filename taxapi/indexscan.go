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
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// nodeTx is one transaction of a block as the chain has it.
type nodeTx struct {
	Hash     string
	Code     uint32
	Accounts []string
	// Charged is the coins a failed transaction's fee deduction moved, and
	// ChargeKnown says the block results settle it. Only a failed
	// transaction has either.
	Charged     string
	ChargeKnown bool
	// Granter is the account whose fee grant paid the fee, if any.
	Granter string
	// Rejected is a failed transaction with no events: the ante handler
	// turned it away, so it changed nothing, not even the fee, and the same
	// bytes can be included again in a later block.
	Rejected bool
}

// failedTxResult is what the block results say about one failed
// transaction, read where the SDK emitted no coin events (before Gaia v6 on
// cosmoshub-4).
type failedTxResult struct {
	Code      uint32
	Codespace string
	Log       string
	GasWanted int64
	TxBytes   int
	Signers   int
	// Fee is the fee the transaction declared, "" for none, and FeeRead
	// says the transaction's bytes decoded.
	Fee     string
	FeeRead bool
}

// Before SDK 0.44 a failed transaction's result carries no events at all, so
// the fee it paid follows from where it failed. runTx in SDK 0.41 and 0.42
// decodes the transaction and checks its messages' ValidateBasic first, then
// runs the ante handler, which deducts the fee, on a cached store that is
// written only when the whole ante chain passes, and runs the messages after
// that. So a transaction turned away before or inside the ante handler paid
// nothing, and one whose message failed paid its declared fee. runMsgs names
// the failing message's index in every error it returns, and nothing before
// it does, so the log tells the two apart, except for three cases: an
// out-of-gas panic, which reads the same in the ante handler and in a
// message; a panic, whose log is redacted; and an error with no ABCI code,
// which is redacted to "internal".
const (
	msgFailurePrefix  = "failed to execute message; message index: "
	msgUnroutedPrefix = "unrecognized message "
	msgIndexMarker    = "; message index: "
	outOfGasPrefix    = "out of gas in location: "
	// sdkInternalCode and sdkPanicCode are the redacted errors.
	sdkInternalCode = 1
	sdkPanicCode    = 111222
	// preCoinEventsAnteGas bounds the gas the SDK 0.42 ante handler spends
	// on one signature beyond the transaction size charge of ten gas a
	// byte. An out-of-gas panic in a transaction whose gas limit reaches
	// that for every signature came from a message: the ante handler had
	// finished within the limit.
	preCoinEventsAnteGas = 100000
)

// preCoinEventsCharge settles a failed transaction's fee from its result in
// a block without coin events. ok is false when the result does not settle
// it.
func preCoinEventsCharge(r failedTxResult) (charged string, ok bool) {
	switch {
	case strings.HasPrefix(r.Log, msgFailurePrefix),
		strings.HasPrefix(r.Log, msgUnroutedPrefix) && strings.Contains(r.Log, msgIndexMarker):
		return r.Fee, r.FeeRead
	case r.Codespace == "sdk" && strings.HasPrefix(r.Log, outOfGasPrefix):
		limit := int64(10*r.TxBytes) + int64(max(r.Signers, 1))*preCoinEventsAnteGas
		if r.FeeRead && (r.Fee == "" || r.GasWanted >= limit) {
			return r.Fee, true
		}
		return "", false
	case r.Codespace == "undefined" && r.Code == sdkInternalCode,
		r.Codespace == "undefined" && r.Code == sdkPanicCode:
		return "", false
	}
	return "", true
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
				Code      uint32 `json:"code"`
				Codespace string `json:"codespace"`
				Log       string `json:"log"`
				GasWanted string `json:"gas_wanted"`
				Events    []struct {
					Type       string    `json:"type"`
					Attributes []rpcAttr `json:"attributes"`
				} `json:"events"`
			} `json:"txs_results"`
			BeginBlockEvents    []struct{ Type string } `json:"begin_block_events"`
			FinalizeBlockEvents []struct{ Type string } `json:"finalize_block_events"`
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
	// SDK 0.44 and later emit coin events in every block's own events (the
	// mint at least) and on every failed transaction that paid its fee.
	coinEvents := false
	for _, e := range append(res.Result.BeginBlockEvents, res.Result.FinalizeBlockEvents...) {
		if e.Type == "coin_spent" || e.Type == "coin_received" || e.Type == "coinbase" {
			coinEvents = true
			break
		}
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
		switch {
		case t.Code == 0:
		case coinEvents || len(t.Events) > 0:
			tx.Charged, tx.ChargeKnown = strings.Join(spent, ","), true
		default:
			gw, _ := strconv.ParseInt(t.GasWanted, 10, 64)
			fee, signers, read := declaredFee(raw)
			tx.Charged, tx.ChargeKnown = preCoinEventsCharge(failedTxResult{
				Code: t.Code, Codespace: t.Codespace, Log: t.Log, GasWanted: gw,
				TxBytes: len(raw), Signers: signers, Fee: fee, FeeRead: read,
			})
		}
		for a := range found {
			tx.Accounts = append(tx.Accounts, a)
		}
		sort.Strings(tx.Accounts)
		out = append(out, tx)
	}
	return out, nil
}

// declaredFee reads the fee, without zero coins, and the number of
// signatures from a transaction's bytes. ok is false for bytes that do not
// decode.
func declaredFee(raw []byte) (fee string, signers int, ok bool) {
	var tx txtypes.TxRaw
	if err := tx.Unmarshal(raw); err != nil {
		return "", 0, false
	}
	var auth txtypes.AuthInfo
	if err := auth.Unmarshal(tx.AuthInfoBytes); err != nil {
		return "", 0, false
	}
	var coins sdk.Coins
	if auth.Fee != nil {
		for _, c := range auth.Fee.Amount {
			if c.Amount.IsNil() || !c.Amount.IsPositive() {
				continue
			}
			coins = append(coins, c)
		}
	}
	return coins.String(), len(tx.Signatures), true
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
