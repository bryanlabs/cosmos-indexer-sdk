package taxapi

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/DefiantLabs/cosmos-indexer/taxapi/internal/servicehttp"
)

// FailedTxFee records what the ante handler charged a failed transaction. The
// index keeps a failed transaction's declared fee but not whether the ante
// handler passed: it charges the fee when it passes (the message then fails)
// and nothing when it rejects the transaction. Rows come from the chain's own
// block results, never from the declared fee.
type FailedTxFee struct {
	TxID uint `gorm:"primaryKey;autoIncrement:false"`
	// Charged is the coins the fee deduction moved, empty when nothing was charged.
	Charged string
}

func (FailedTxFee) TableName() string { return "tax_failed_tx_fees" }

// FeeGrantUse records a transaction whose fee a fee grant paid. The ante
// handler then deducts the granter's balance, but blocks indexed before the
// indexer read the granter stored the signer as the payer, so these rows,
// taken from the chain's use_feegrant events, override the fee payer.
type FeeGrantUse struct {
	TxID    uint   `gorm:"primaryKey;autoIncrement:false"`
	Granter string `gorm:"index"`
}

func (FeeGrantUse) TableName() string { return "tax_fee_grants" }

// LedgerScan is the height span whose blocks were scanned for fee grant use
// and for transactions missing from the index, or indexed without their
// messages. Blocks above it were indexed with the granter as the payer; the
// sweeper extends FeeGrantsTo block by block, and balances are rebuilt only
// up to it.
type LedgerScan struct {
	Chain         string `gorm:"primaryKey"`
	FeeGrantsFrom int64
	FeeGrantsTo   int64
}

func (LedgerScan) TableName() string { return "tax_ledger_scans" }

// ValidatorConsensus maps a validator's consensus address to its operator, so
// a slash of a validator that has since been removed can still be attributed.
type ValidatorConsensus struct {
	ConsAddress string `gorm:"primaryKey"`
	Operator    string
}

func (ValidatorConsensus) TableName() string { return "tax_validator_consensus" }

// IndexHole is a block marked indexed that is missing a transaction that moved
// coins, or holds a successful transaction indexed without its messages (and
// so without the coin events they emitted). A copy of a transaction the chain
// included twice is not one: the later copy fails before moving anything and
// shares its hash with the copy the index keeps. A balance whose window covers
// a hole cannot be rebuilt exactly. When AddressesKnown is set, IndexHoleAddress lists every account
// the missing transactions name (in their bytes or any of their events), and
// only those wallets are affected; otherwise every wallet is. The sweeper
// removes the row once the block holds ChainTxs transactions again and each
// successful one has its messages.
type IndexHole struct {
	Chain          string `gorm:"primaryKey"`
	Height         int64  `gorm:"primaryKey;autoIncrement:false"`
	ChainTxs       int
	AddressesKnown bool `gorm:"not null;default:false"`
}

func (IndexHole) TableName() string { return "tax_index_holes" }

// IndexHoleAddress is an account a hole's missing transactions name.
type IndexHoleAddress struct {
	Chain   string `gorm:"primaryKey"`
	Height  int64  `gorm:"primaryKey;autoIncrement:false"`
	Address string `gorm:"primaryKey"`
}

func (IndexHoleAddress) TableName() string { return "tax_index_hole_addresses" }

// ValidatorRateSample is a validator's bonded tokens and delegator shares at
// the last block of a UTC day, read from chain state. A past delegation is
// shown at the rate of its date, and the samples around a movement bound the
// rate it was made at.
type ValidatorRateSample struct {
	Chain    string `gorm:"primaryKey"`
	Operator string `gorm:"primaryKey"`
	Height   int64  `gorm:"primaryKey;autoIncrement:false"`
	Tokens   string `gorm:"type:numeric;not null"`
	Shares   string `gorm:"type:numeric;not null"`
}

func (ValidatorRateSample) TableName() string { return "tax_validator_rates" }

// BalanceHistoryMigrate creates the tables the balance ledger reads.
// The index scan and the hole sweeper read a block's transactions, which the
// indexer's schema does not index.
func BalanceHistoryMigrate(db *gorm.DB) error {
	if err := db.AutoMigrate(&FailedTxFee{}, &FeeGrantUse{}, &LedgerScan{}, &ValidatorConsensus{}, &IndexHole{}, &IndexHoleAddress{}, &ValidatorRateSample{}); err != nil {
		return err
	}
	return db.Exec("CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_txes_block_id ON txes (block_id)").Error
}

const heightHeader = "x-cosmos-block-height"

// ledgerNode reads state at a height from the chain's REST API and block
// results from its RPC.
type ledgerNode struct {
	rest *servicehttp.Endpoint
	rpc  *servicehttp.Endpoint
	// cache, when set, is a block cache directory that RPC reads come from
	// instead of the node.
	cache string
}

func newLedgerNode() (*ledgerNode, error) {
	restOrigin := os.Getenv("NODE_REST_API")
	if restOrigin == "" {
		return nil, errors.New("NODE_REST_API is not configured")
	}
	rpcOrigin := os.Getenv("NODE_RPC_API")
	if rpcOrigin == "" {
		u, err := url.Parse(restOrigin)
		if err != nil {
			return nil, err
		}
		u.Host = u.Hostname() + ":26657"
		rpcOrigin = u.String()
	}
	client := &http.Client{Timeout: 30 * time.Second}
	rest, err := servicehttp.New(client, restOrigin)
	if err != nil {
		return nil, err
	}
	rpc, err := servicehttp.New(client, rpcOrigin)
	if err != nil {
		return nil, err
	}
	return &ledgerNode{rest: rest, rpc: rpc}, nil
}

func (n *ledgerNode) at(path string, q url.Values, height int64, out any) error {
	return n.rest.GetJSONWithHeaders(path, q, map[string]string{heightHeader: strconv.FormatInt(height, 10)}, out)
}

func (n *ledgerNode) latestHeight() (int64, error) {
	var body struct {
		Block struct {
			Header struct {
				Height string `json:"height"`
			} `json:"header"`
		} `json:"block"`
	}
	if err := n.rest.GetJSON("/cosmos/base/tendermint/v1beta1/blocks/latest", nil, &body); err != nil {
		return 0, err
	}
	return strconv.ParseInt(body.Block.Header.Height, 10, 64)
}

type pageResp struct {
	NextKey string `json:"next_key"`
}

// paged follows next_key until the listing ends.
func (n *ledgerNode) paged(path string, height int64, each func(raw func(out any) error) (pageResp, error)) error {
	key := ""
	for i := 0; i < 1000; i++ {
		q := url.Values{apiPaginationLimit: {"1000"}}
		if key != "" {
			q.Set("pagination.key", key)
		}
		page, err := each(func(out any) error { return n.at(path, q, height, out) })
		if err != nil {
			return err
		}
		if page.NextKey == "" {
			return nil
		}
		key = page.NextKey
	}
	return fmt.Errorf("%s did not finish paging", path)
}

func parseInt(v string) (*big.Int, error) {
	n, ok := new(big.Int).SetString(strings.TrimSpace(v), 10)
	if !ok {
		return nil, fmt.Errorf("bad integer %q", v)
	}
	return n, nil
}

// anchor reads the wallet's bank balances, delegations and unbonding entries
// at height.
func (n *ledgerNode) anchor(addr string, height int64) (ledgerAnchor, error) {
	a := ledgerAnchor{Height: height, Bank: map[string]*big.Int{}, Delegations: map[string]*big.Int{}, Shares: map[string]decimal.Decimal{}}
	err := n.paged("/cosmos/bank/v1beta1/balances/"+addr, height, func(get func(any) error) (pageResp, error) {
		var body struct {
			Balances   []balCoin `json:"balances"`
			Pagination pageResp  `json:"pagination"`
		}
		if err := get(&body); err != nil {
			return pageResp{}, err
		}
		for _, c := range body.Balances {
			v, err := parseInt(c.Amount)
			if err != nil {
				return pageResp{}, err
			}
			addInt(a.Bank, c.Denom, v)
		}
		return body.Pagination, nil
	})
	if err != nil {
		return a, fmt.Errorf("bank balances: %w", err)
	}
	err = n.paged("/cosmos/staking/v1beta1/delegations/"+addr, height, func(get func(any) error) (pageResp, error) {
		var body struct {
			DelegationResponses []struct {
				Delegation struct {
					ValidatorAddress string `json:"validator_address"`
					Shares           string `json:"shares"`
				} `json:"delegation"`
				Balance balCoin `json:"balance"`
			} `json:"delegation_responses"`
			Pagination pageResp `json:"pagination"`
		}
		if err := get(&body); err != nil {
			return pageResp{}, err
		}
		for _, d := range body.DelegationResponses {
			v, err := parseInt(d.Balance.Amount)
			if err != nil {
				return pageResp{}, err
			}
			sh, err := decimal.NewFromString(d.Delegation.Shares)
			if err != nil {
				return pageResp{}, fmt.Errorf("delegation shares %q: %w", d.Delegation.Shares, err)
			}
			addInt(a.Delegations, d.Delegation.ValidatorAddress, v)
			a.Shares[d.Delegation.ValidatorAddress] = a.Shares[d.Delegation.ValidatorAddress].Add(sh)
		}
		return body.Pagination, nil
	})
	if err != nil {
		return a, fmt.Errorf("delegations: %w", err)
	}
	err = n.paged("/cosmos/staking/v1beta1/delegators/"+addr+"/unbonding_delegations", height, func(get func(any) error) (pageResp, error) {
		var body struct {
			UnbondingResponses []struct {
				ValidatorAddress string `json:"validator_address"`
				Entries          []struct {
					CreationHeight string `json:"creation_height"`
					Balance        string `json:"balance"`
				} `json:"entries"`
			} `json:"unbonding_responses"`
			Pagination pageResp `json:"pagination"`
		}
		if err := get(&body); err != nil {
			return pageResp{}, err
		}
		for _, u := range body.UnbondingResponses {
			for _, e := range u.Entries {
				ch, err := strconv.ParseInt(e.CreationHeight, 10, 64)
				if err != nil {
					return pageResp{}, err
				}
				v, err := parseInt(e.Balance)
				if err != nil {
					return pageResp{}, err
				}
				a.Unbonding = append(a.Unbonding, unbondingEntry{Validator: u.ValidatorAddress, CreationHeight: ch, Balance: v})
			}
		}
		return body.Pagination, nil
	})
	if err != nil {
		return a, fmt.Errorf("unbonding delegations: %w", err)
	}
	return a, nil
}

// validatorRates reads every validator's bonded tokens and delegator shares.
func (n *ledgerNode) validatorRates(height int64) (map[string]validatorRate, error) {
	out := map[string]validatorRate{}
	err := n.paged("/cosmos/staking/v1beta1/validators", height, func(get func(any) error) (pageResp, error) {
		var body struct {
			Validators []struct {
				OperatorAddress string `json:"operator_address"`
				Tokens          string `json:"tokens"`
				DelegatorShares string `json:"delegator_shares"`
			} `json:"validators"`
			Pagination pageResp `json:"pagination"`
		}
		if err := get(&body); err != nil {
			return pageResp{}, err
		}
		for _, v := range body.Validators {
			tokens, err := parseInt(v.Tokens)
			if err != nil {
				return pageResp{}, err
			}
			shares, err := decimal.NewFromString(v.DelegatorShares)
			if err != nil {
				return pageResp{}, err
			}
			out[v.OperatorAddress] = validatorRate{Tokens: tokens, Shares: shares}
		}
		return body.Pagination, nil
	})
	return out, err
}

// consensusOperators maps every validator's consensus address to its operator.
func (n *ledgerNode) consensusOperators(height int64) (map[string]string, error) {
	out := map[string]string{}
	err := n.paged("/cosmos/staking/v1beta1/validators", height, func(get func(any) error) (pageResp, error) {
		var body struct {
			Validators []struct {
				OperatorAddress string `json:"operator_address"`
				ConsensusPubkey struct {
					Type string `json:"@type"`
					Key  string `json:"key"`
				} `json:"consensus_pubkey"`
			} `json:"validators"`
			Pagination pageResp `json:"pagination"`
		}
		if err := get(&body); err != nil {
			return pageResp{}, err
		}
		for _, v := range body.Validators {
			key, err := base64.StdEncoding.DecodeString(v.ConsensusPubkey.Key)
			if err != nil || !strings.HasSuffix(v.ConsensusPubkey.Type, "ed25519.PubKey") {
				continue
			}
			sum := sha256.Sum256(key)
			hrp := strings.Replace(strings.SplitN(v.OperatorAddress, "1", 2)[0], apiValoper, "valcons", 1)
			cons, err := bech32.ConvertAndEncode(hrp, sum[:20])
			if err == nil {
				out[cons] = v.OperatorAddress
			}
		}
		return body.Pagination, nil
	})
	return out, err
}

func (n *ledgerNode) slashFractions(height int64) (downtime, doubleSign decimal.Decimal, err error) {
	var body struct {
		Params struct {
			Downtime   string `json:"slash_fraction_downtime"`
			DoubleSign string `json:"slash_fraction_double_sign"`
		} `json:"params"`
	}
	if err = n.at("/cosmos/slashing/v1beta1/params", nil, height, &body); err != nil {
		return
	}
	if downtime, err = decimal.NewFromString(body.Params.Downtime); err != nil {
		return
	}
	doubleSign, err = decimal.NewFromString(body.Params.DoubleSign)
	return
}

type rpcAttr struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// decoded undoes the base64 attribute encoding of older CometBFT releases.
func (a rpcAttr) decoded() (string, string) {
	if a.Key == "amount" || a.Key == "spender" || a.Key == "granter" || a.Key == "fee" || a.Key == "msg_index" {
		return a.Key, a.Value
	}
	k, err1 := base64.StdEncoding.DecodeString(a.Key)
	v, err2 := base64.StdEncoding.DecodeString(a.Value)
	if err1 == nil && err2 == nil && (string(k) == "amount" || string(k) == "spender" || string(k) == "granter") {
		return string(k), string(v)
	}
	return a.Key, a.Value
}

// failedTxCharges reads one block's failed transactions from the node and
// returns, by hash, the coins each one's fee deduction moved.
func (n *ledgerNode) failedTxCharges(height int64) (map[string]string, error) {
	txs, err := n.blockTxs(height)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, t := range txs {
		if t.Code != 0 {
			out[t.Hash] = t.Charged
		}
	}
	return out, nil
}

// --- index reads ------------------------------------------------------------

func (s *Server) chainRowIDs(chain string) ([]int64, error) {
	var ids []int64
	err := s.db.Table("chains").Where("chain_id IN ?", chainIDs(chain)).Pluck("id", &ids).Error
	if err == nil && len(ids) == 0 {
		err = fmt.Errorf("chain %s is not indexed", chain)
	}
	return ids, err
}

type heightTime struct {
	Height    int64
	TimeStamp time.Time
}

// dayEndBlock finds the last block before the end of day (UTC). It needs the
// first block of the next day and the block row before it, so the boundary is
// known rather than guessed across a gap. The day's last block itself need not
// be indexed: the balance undoes only the blocks after it.
func (s *Server) dayEndBlock(ids []int64, day time.Time) (heightTime, error) {
	cutoff := day.UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	first := func(from int64) (heightTime, bool, error) {
		var r heightTime
		res := s.db.Table("blocks").Select("height, time_stamp").
			Where("chain_id IN ? AND height >= ?", ids, from).Order("height").Limit(1).Scan(&r)
		return r, res.RowsAffected > 0, res.Error
	}
	var tip heightTime
	if err := s.db.Table("blocks").Select("height, time_stamp").Where("chain_id IN ?", ids).Order("height DESC").Limit(1).Scan(&tip).Error; err != nil {
		return heightTime{}, err
	}
	if tip.TimeStamp.Before(cutoff) {
		return heightTime{}, errBalanceNotReached
	}
	lo, _, err := first(0)
	if err != nil {
		return heightTime{}, err
	}
	if !lo.TimeStamp.Before(cutoff) {
		return heightTime{}, errBalanceBeforeIndex
	}
	// Smallest height whose first indexed block is at or after the cutoff.
	l, r := lo.Height, tip.Height
	for l < r {
		mid := l + (r-l)/2
		b, ok, err := first(mid)
		if err != nil {
			return heightTime{}, err
		}
		if ok && !b.TimeStamp.Before(cutoff) {
			r = mid
		} else {
			l = mid + 1
		}
	}
	next, ok, err := first(l)
	if err != nil || !ok {
		return heightTime{}, fmt.Errorf("day boundary lookup: %v", err)
	}
	var h heightTime
	res := s.db.Table("blocks").Select("height, time_stamp").
		Where("chain_id IN ? AND height = ? AND time_stamp IS NOT NULL", ids, next.Height-1).Limit(1).Scan(&h)
	if res.Error != nil {
		return heightTime{}, res.Error
	}
	if res.RowsAffected == 0 || !h.TimeStamp.Before(cutoff) {
		return heightTime{}, balanceIncomplete{fmt.Sprintf("block %d, the last block of %s, is not in the index", next.Height-1, day.Format(apiDateLayout))}
	}
	return h, nil
}

var (
	errBalanceNotReached  = errors.New("that day has not ended on chain yet")
	errBalanceBeforeIndex = errors.New("that day is before the first indexed block")
)

// balanceIncomplete means the index cannot rebuild the date exactly.
type balanceIncomplete struct{ reason string }

func (e balanceIncomplete) Error() string { return e.reason }

// ledgerTip is the newest block with transactions and block events indexed
// that the node can still answer state queries for.
func (s *Server) ledgerTip(ids []int64, node *ledgerNode, scannedTo int64) (heightTime, error) {
	latest, err := node.latestHeight()
	if err != nil {
		return heightTime{}, fmt.Errorf("node latest height: %w", err)
	}
	if scannedTo > 0 && scannedTo < latest {
		latest = scannedTo
	}
	var t heightTime
	res := s.db.Table("blocks").Select("height, time_stamp").
		Where("chain_id IN ? AND height <= ? AND tx_indexed AND block_events_indexed", ids, latest).
		Order("height DESC").Limit(1).Scan(&t)
	if res.Error != nil {
		return heightTime{}, res.Error
	}
	if res.RowsAffected == 0 {
		return heightTime{}, errors.New("no indexed block the node can answer for")
	}
	return t, nil
}

type ledgerCoverage struct {
	mu       sync.Mutex
	verified map[int64]int64
}

// windowIndexed reports whether every block in (lo, hi] has its transactions
// and block events indexed. Verified spans are remembered per lo.
func (s *Server) windowIndexed(ids []int64, lo, hi int64) (int64, error) {
	s.ledgerCov.mu.Lock()
	if s.ledgerCov.verified == nil {
		s.ledgerCov.verified = map[int64]int64{}
	}
	from := lo
	if v, ok := s.ledgerCov.verified[lo]; ok && v <= hi {
		from = v
	}
	s.ledgerCov.mu.Unlock()
	if from >= hi {
		return 0, nil
	}
	var n int64
	if err := s.db.Table("blocks").Where("chain_id IN ? AND height > ? AND height <= ? AND tx_indexed AND block_events_indexed", ids, from, hi).Count(&n).Error; err != nil {
		return 0, err
	}
	missing := (hi - from) - n
	if missing == 0 {
		s.ledgerCov.mu.Lock()
		s.ledgerCov.verified[lo] = hi
		s.ledgerCov.mu.Unlock()
	}
	return missing, nil
}

// indexHoles counts the recorded index holes in (lo, hi] that may touch addr
// and returns the lowest.
func (s *Server) indexHoles(chain, addr string, lo, hi int64) (int64, int64, error) {
	heights, err := s.holeHeights(chain, addr, lo, hi)
	if err != nil || len(heights) == 0 {
		return 0, 0, err
	}
	return int64(len(heights)), heights[0], nil
}

// holeHeights lists, ascending, the recorded index holes in (lo, hi] that may
// touch addr.
func (s *Server) holeHeights(chain, addr string, lo, hi int64) ([]int64, error) {
	var heights []int64
	err := s.db.Raw(`
SELECT h.height FROM tax_index_holes h
WHERE h.chain = @chain AND h.height > @lo AND h.height <= @hi
AND (NOT h.addresses_known OR EXISTS (SELECT 1 FROM tax_index_hole_addresses a
     WHERE a.chain = h.chain AND a.height = h.height AND a.address = @addr))
ORDER BY h.height`,
		map[string]any{"chain": chain, "addr": addr, "lo": lo, "hi": hi}).Scan(&heights).Error
	return heights, err
}

// clearIndexHoles drops the holes whose block now holds every transaction.
func (s *Server) clearIndexHoles(chain string, ids []int64) (int64, error) {
	var before int64
	if err := s.db.Model(&IndexHole{}).Where("chain = ?", chain).Count(&before).Error; err != nil {
		return 0, err
	}
	res := s.db.Exec(`
WITH done AS (
  DELETE FROM tax_index_holes h
  WHERE h.chain = @chain AND h.chain_txs <= (
    SELECT count(*) FROM txes x JOIN blocks b ON b.id = x.block_id
    WHERE b.chain_id IN @ids AND b.height = h.height)
  AND NOT EXISTS (
    SELECT 1 FROM txes x JOIN blocks b ON b.id = x.block_id
    WHERE b.chain_id IN @ids AND b.height = h.height AND x.code = 0
    AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.tx_id = x.id))
  RETURNING h.chain, h.height)
DELETE FROM tax_index_hole_addresses a USING done
WHERE a.chain = done.chain AND a.height = done.height`, map[string]any{"chain": chain, "ids": ids})
	if res.Error != nil {
		return 0, res.Error
	}
	var left int64
	if err := s.db.Model(&IndexHole{}).Where("chain = ?", chain).Count(&left).Error; err != nil {
		return 0, err
	}
	return before - left, nil
}

// ledgerScan is the span whose fee grant use and index holes are recorded,
// zero when no scan has run for the chain.
func (s *Server) ledgerScan(chain string) (LedgerScan, error) {
	var scan LedgerScan
	if err := s.db.Where("chain = ?", chain).Limit(1).Find(&scan).Error; err != nil {
		return scan, fmt.Errorf("ledger scan span: %w", err)
	}
	return scan, nil
}

type ledgerAttrRow struct {
	EventID   uint64
	MessageID uint64
	Height    int64
	TimeStamp time.Time
	Phase     int
	Type      string
	Key       string
	Value     string
}

var (
	ledgerMessageTypes = []string{"coin_received", "coin_spent", "delegate", "unbond", "redelegate", "cancel_unbonding_delegation", "tokenize_shares", "redeem_shares"}
	ledgerBlockTypes   = []string{"coin_received", "coin_spent", "complete_unbonding"}
)

func groupLedgerRows(rows []ledgerAttrRow) []ledgerEvent {
	var out []ledgerEvent
	for _, r := range rows {
		if len(out) == 0 || out[len(out)-1].ID != r.EventID || out[len(out)-1].Phase != r.Phase {
			out = append(out, ledgerEvent{ID: r.EventID, Msg: r.MessageID, Height: r.Height, Time: r.TimeStamp, Phase: r.Phase, Type: r.Type})
		}
		e := &out[len(out)-1]
		e.Attrs = append(e.Attrs, ledgerAttr{Key: r.Key, Value: r.Value})
	}
	return out
}

// ledgerEvents loads every message and block event in (lo, hi] that names
// addr (and the wallet's own create_validator), with all their attributes,
// plus the wallet's redelegations, and the highest height of a redelegation
// that cannot be attributed.
func (s *Server) ledgerEvents(ids []int64, addr, operator string, lo, hi int64) ([]ledgerEvent, int64, error) {
	var msgRows []ledgerAttrRow
	if err := s.db.Raw(`
SELECT me.id AS event_id, me.message_id, b.height, b.time_stamp, 1 AS phase, t.type, k.key, a.value
FROM message_events me
JOIN message_event_types t ON t.id = me.message_event_type_id
JOIN messages m ON m.id = me.message_id
JOIN txes x ON x.id = m.tx_id
JOIN blocks b ON b.id = x.block_id
JOIN message_event_attributes a ON a.message_event_id = me.id
JOIN message_event_attribute_keys k ON k.id = a.message_event_attribute_key_id
WHERE me.id IN (
  SELECT a0.message_event_id FROM message_event_attributes a0
  WHERE left(a0.value, 100) = left(@addr, 100) AND a0.value = @addr
  UNION
  SELECT a1.message_event_id FROM message_event_attributes a1
  JOIN message_events me1 ON me1.id = a1.message_event_id
  JOIN message_event_types t1 ON t1.id = me1.message_event_type_id
  WHERE left(a1.value, 100) = left(@op, 100) AND a1.value = @op AND t1.type = 'create_validator')
AND (t.type IN @types OR t.type = 'create_validator')
AND b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi
ORDER BY b.height, me.id, a.index`,
		map[string]any{"addr": addr, "op": operator, "types": ledgerMessageTypes, "ids": ids, "lo": lo, "hi": hi}).
		Scan(&msgRows).Error; err != nil {
		return nil, 0, fmt.Errorf("message events: %w", err)
	}
	var blockRows []ledgerAttrRow
	if err := s.db.Raw(`
SELECT be.id AS event_id, b.height, b.time_stamp, CASE WHEN be.lifecycle_position = 0 THEN 0 ELSE 2 END AS phase, t.type, k.key, a.value
FROM block_events be
JOIN block_event_types t ON t.id = be.block_event_type_id
JOIN blocks b ON b.id = be.block_id
JOIN block_event_attributes a ON a.block_event_id = be.id
JOIN block_event_attribute_keys k ON k.id = a.block_event_attribute_key_id
WHERE be.id IN (
  SELECT a0.block_event_id FROM block_event_attributes a0
  WHERE left(a0.value, 100) = left(@addr, 100) AND a0.value = @addr)
AND t.type IN @types
AND b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi
ORDER BY b.height, be.id, a.index`,
		map[string]any{"addr": addr, "types": ledgerBlockTypes, "ids": ids, "lo": lo, "hi": hi}).
		Scan(&blockRows).Error; err != nil {
		return nil, 0, fmt.Errorf("block events: %w", err)
	}
	var redelMsgs []uint64
	if err := s.db.Raw(`
SELECT DISTINCT me0.message_id FROM message_event_attributes a0
JOIN message_events me0 ON me0.id = a0.message_event_id
JOIN message_event_types t0 ON t0.id = me0.message_event_type_id
JOIN messages m ON m.id = me0.message_id
JOIN txes x ON x.id = m.tx_id
JOIN blocks b ON b.id = x.block_id
WHERE left(a0.value, 100) = left(@addr, 100) AND a0.value = @addr AND t0.type IN ('withdraw_rewards', 'message')
AND b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi
AND EXISTS (SELECT 1 FROM message_events r JOIN message_event_types rt ON rt.id = r.message_event_type_id
            WHERE r.message_id = me0.message_id AND rt.type = 'redelegate')`,
		map[string]any{"addr": addr, "ids": ids, "lo": lo, "hi": hi}).
		Scan(&redelMsgs).Error; err != nil {
		return nil, 0, fmt.Errorf("redelegation messages: %w", err)
	}
	var redelRows []ledgerAttrRow
	if len(redelMsgs) > 0 {
		if err := s.db.Raw(`
SELECT me.id AS event_id, me.message_id, b.height, b.time_stamp, 1 AS phase, t.type, k.key, a.value
FROM message_events me
JOIN message_event_types t ON t.id = me.message_event_type_id
JOIN messages m ON m.id = me.message_id
JOIN txes x ON x.id = m.tx_id
JOIN blocks b ON b.id = x.block_id
JOIN message_event_attributes a ON a.message_event_id = me.id
JOIN message_event_attribute_keys k ON k.id = a.message_event_attribute_key_id
WHERE me.message_id IN @msgs AND t.type IN ('redelegate', 'withdraw_rewards', 'message')
ORDER BY b.height, me.message_id, me.index, a.index`,
			map[string]any{"msgs": redelMsgs}).
			Scan(&redelRows).Error; err != nil {
			return nil, 0, fmt.Errorf("redelegation events: %w", err)
		}
	}
	redelegations, unattributed := attributeRedelegations(redelRows, addr)
	events := append(groupLedgerRows(msgRows), groupLedgerRows(blockRows)...)
	return append(events, redelegations...), unattributed, nil
}

// attributeRedelegations gives the wallet its redelegate events. Gaia never
// puts the delegator on a redelegate event, so it is read from the same
// message: the delegators its withdraw_rewards events name, or the sender of
// its staking message event on SDKs that leave the delegator off those too.
// A message with one such delegator is that delegator's. In one with several
// (an authz exec for many granters), a redelegation is matched to the reward
// withdrawal from its source validator that precedes it. The highest height
// of a redelegation that may be the wallet's but cannot be matched is
// returned, so dates before it are refused.
func attributeRedelegations(rows []ledgerAttrRow, addr string) ([]ledgerEvent, int64) {
	var out []ledgerEvent
	var unattributed int64
	for start := 0; start < len(rows); {
		end := start
		for end < len(rows) && rows[end].MessageID == rows[start].MessageID {
			end++
		}
		events := groupLedgerRows(rows[start:end])
		start = end
		delegators := map[string]bool{}
		for _, ev := range events {
			for _, rec := range ev.records() {
				switch {
				case ev.Type == "withdraw_rewards" && rec["delegator"] != "":
					delegators[rec["delegator"]] = true
				case ev.Type == "message" && rec["module"] == "staking" && rec["sender"] != "":
					delegators[rec["sender"]] = true
				}
			}
		}
		if !delegators[addr] {
			continue
		}
		withdrawn := map[string]map[string]bool{}
		for _, ev := range events {
			switch ev.Type {
			case "withdraw_rewards":
				for _, rec := range ev.records() {
					if rec["delegator"] != "" {
						if withdrawn[rec["validator"]] == nil {
							withdrawn[rec["validator"]] = map[string]bool{}
						}
						withdrawn[rec["validator"]][rec["delegator"]] = true
					}
				}
			case "redelegate":
				var attrs []ledgerAttr
				for _, rec := range ev.records() {
					if rec["delegator"] != "" {
						continue
					}
					owner := ""
					if len(delegators) == 1 {
						owner = addr
					} else if w := withdrawn[rec["source_validator"]]; len(w) == 1 {
						for d := range w {
							owner = d
						}
					}
					if owner == "" {
						if ev.Height > unattributed {
							unattributed = ev.Height
						}
						continue
					}
					withdrawn = map[string]map[string]bool{}
					if owner != addr {
						continue
					}
					for _, k := range []string{"source_validator", "destination_validator", "amount", "completion_time"} {
						if v, ok := rec[k]; ok {
							attrs = append(attrs, ledgerAttr{Key: k, Value: v})
						}
					}
					attrs = append(attrs, ledgerAttr{Key: "delegator", Value: addr})
				}
				if len(attrs) > 0 {
					out = append(out, ledgerEvent{ID: ev.ID, Msg: ev.Msg, Height: ev.Height, Time: ev.Time, Phase: ev.Phase, Type: ev.Type, Attrs: attrs})
				}
			}
		}
	}
	return out, unattributed
}

type ledgerFeeRow struct {
	TxID     uint
	Hash     string
	Height   int64
	Code     int64
	Denom    string
	Amount   string
	Resolved bool
	Charged  string
}

// ledgerFees loads the fees addr paid in (lo, hi]: the fees of its own
// transactions that no fee grant covered, and those of transactions its fee
// grants paid for. A successful transaction paid its declared fee. A failed
// one paid what its block results show; ones not yet recorded are read from
// the node, and the count still unknown is returned.
func (s *Server) ledgerFees(ids []int64, addr string, lo, hi int64, node *ledgerNode) ([]ledgerFee, int, error) {
	var rows []ledgerFeeRow
	if err := s.db.Raw(`
SELECT x.id AS tx_id, x.hash, b.height, x.code, d.base AS denom, f.amount::text AS amount,
       o.tx_id IS NOT NULL AS resolved, COALESCE(o.charged, '') AS charged
FROM (
  SELECT f.tx_id, f.denomination_id, f.amount FROM fees f
  JOIN addresses ad ON ad.id = f.payer_address_id
  WHERE ad.address = @addr AND NOT EXISTS (SELECT 1 FROM tax_fee_grants g WHERE g.tx_id = f.tx_id)
  UNION ALL
  SELECT f.tx_id, f.denomination_id, f.amount FROM tax_fee_grants g
  JOIN fees f ON f.tx_id = g.tx_id
  WHERE g.granter = @addr
) f
JOIN txes x ON x.id = f.tx_id
JOIN blocks b ON b.id = x.block_id
JOIN denoms d ON d.id = f.denomination_id
LEFT JOIN tax_failed_tx_fees o ON o.tx_id = x.id
WHERE b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi
ORDER BY b.height, x.id`,
		map[string]any{"addr": addr, "ids": ids, "lo": lo, "hi": hi}).
		Scan(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("fees: %w", err)
	}
	var out []ledgerFee
	failed := map[uint]ledgerFeeRow{}
	for _, r := range rows {
		if r.Code == 0 {
			amt, err := parseInt(r.Amount)
			if err != nil {
				return nil, 0, err
			}
			out = append(out, ledgerFee{Height: r.Height, Coins: sdk.NewCoins(sdk.NewCoin(r.Denom, sdk.NewIntFromBigInt(amt)))})
			continue
		}
		failed[r.TxID] = r
	}
	var pending []ledgerFeeRow
	for _, r := range failed {
		if !r.Resolved {
			pending = append(pending, r)
		}
	}
	if len(pending) > 0 && node != nil {
		resolved := s.resolveFailedFees(pending, node)
		for id, charged := range resolved {
			r := failed[id]
			r.Resolved, r.Charged = true, charged
			failed[id] = r
		}
	}
	unknown := 0
	for _, r := range failed {
		if !r.Resolved {
			unknown++
			continue
		}
		if r.Charged == "" {
			continue
		}
		coins, err := ledgerCoins(r.Charged, s.native.Denom)
		if err != nil {
			return nil, 0, fmt.Errorf("charged fee of tx %d: %w", r.TxID, err)
		}
		out = append(out, ledgerFee{Height: r.Height, Coins: coins})
	}
	return out, unknown, nil
}

// resolveFailedFees reads failed transactions' fee outcomes from the node's
// block results and records them.
func (s *Server) resolveFailedFees(rows []ledgerFeeRow, node *ledgerNode) map[uint]string {
	byHeight := map[int64][]ledgerFeeRow{}
	for _, r := range rows {
		byHeight[r.Height] = append(byHeight[r.Height], r)
	}
	out := map[uint]string{}
	for h, rs := range byHeight {
		charges, err := node.failedTxCharges(h)
		if err != nil {
			continue
		}
		for _, r := range rs {
			charged, ok := charges[strings.ToUpper(r.Hash)]
			if !ok {
				continue
			}
			if err := s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&FailedTxFee{TxID: r.TxID, Charged: charged}).Error; err != nil {
				continue
			}
			out[r.TxID] = charged
		}
	}
	return out
}

// SweepFailedTxFees records fee outcomes for failed transactions in the
// newest blocks while the node still keeps their block results.
func (s *Server) SweepFailedTxFees(chain string, depth int64, logf func(string, ...any)) {
	node, err := newLedgerNode()
	if err != nil {
		return
	}
	ids, err := s.chainRowIDs(chain)
	if err != nil {
		return
	}
	latest, err := node.latestHeight()
	if err != nil {
		logf("failed-fee sweep: node height: %v", err)
		return
	}
	var rows []ledgerFeeRow
	if err := s.db.Raw(`
SELECT DISTINCT x.id AS tx_id, x.hash, b.height
FROM txes x
JOIN blocks b ON b.id = x.block_id
JOIN fees f ON f.tx_id = x.id
LEFT JOIN tax_failed_tx_fees o ON o.tx_id = x.id
WHERE b.chain_id IN @ids AND b.height > @lo AND x.code <> 0 AND o.tx_id IS NULL`,
		map[string]any{"ids": ids, "lo": latest - depth}).Scan(&rows).Error; err != nil {
		logf("failed-fee sweep: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	done := s.resolveFailedFees(rows, node)
	logf("failed-fee sweep: recorded %d of %d failed transactions", len(done), len(rows))
}

// StartBalanceLedgerSweeper keeps the failed-transaction fee table, the index
// holes and the recorded validator set current.
func (s *Server) StartBalanceLedgerSweeper(chain string, every time.Duration, logf func(string, ...any)) {
	go func() {
		for {
			s.SweepFailedTxFees(chain, 50000, logf)
			if ids, err := s.chainRowIDs(chain); err == nil {
				s.SweepIndexScan(chain, ids, 2000, logf)
				if node, err := newLedgerNode(); err == nil {
					s.SweepValidatorRates(chain, ids, node, 3, logf)
				}
				if n, err := s.clearIndexHoles(chain, ids); err != nil {
					logf("index hole sweep: %v", err)
				} else if n > 0 {
					logf("index hole sweep: %d blocks now fully indexed", n)
				}
			}
			if node, err := newLedgerNode(); err == nil {
				if h, err := node.latestHeight(); err == nil {
					if err := s.recordValidators(node, h); err != nil {
						logf("validator record: %v", err)
					}
				}
			}
			time.Sleep(every)
		}
	}()
}

// consensusOperators maps consensus addresses to operators from the validator
// set at height, plus validators recorded earlier that have since been removed.
func (s *Server) consensusOperators(node *ledgerNode, height int64) (map[string]string, error) {
	cons, err := node.consensusOperators(height)
	if err != nil {
		return nil, fmt.Errorf("validator set: %w", err)
	}
	var known []ValidatorConsensus
	if err := s.db.Find(&known).Error; err != nil {
		return nil, fmt.Errorf("recorded validators: %w", err)
	}
	for _, k := range known {
		if _, ok := cons[k.ConsAddress]; !ok {
			cons[k.ConsAddress] = k.Operator
		}
	}
	return cons, nil
}

// recordValidators keeps the consensus addresses of the validator set at
// height, so slashes stay attributable after a validator is removed.
func (s *Server) recordValidators(node *ledgerNode, height int64) error {
	cons, err := node.consensusOperators(height)
	if err != nil {
		return err
	}
	rows := make([]ValidatorConsensus, 0, len(cons))
	for c, op := range cons {
		rows = append(rows, ValidatorConsensus{ConsAddress: c, Operator: op})
	}
	if len(rows) == 0 {
		return nil
	}
	return s.db.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 500).Error
}

// ledgerSlashes loads validator slashes in (lo, hi]. Slash events are found by
// their reason attribute, which the attribute value index covers.
func (s *Server) ledgerSlashes(ids []int64, lo, hi int64, node *ledgerNode, anchor int64, touched map[string]bool) ([]ledgerSlash, error) {
	reasons := []string{"missing_signature", "double_sign"}
	var rows []ledgerAttrRow
	if err := s.db.Raw(`
SELECT be.id AS event_id, b.height, b.time_stamp, 0 AS phase, t.type, k.key, a.value
FROM block_events be
JOIN block_event_types t ON t.id = be.block_event_type_id
JOIN blocks b ON b.id = be.block_id
JOIN block_event_attributes a ON a.block_event_id = be.id
JOIN block_event_attribute_keys k ON k.id = a.block_event_attribute_key_id
WHERE be.id IN (
  SELECT a0.block_event_id FROM block_event_attributes a0
  WHERE left(a0.value, 100) IN @reasons AND a0.value IN @reasons)
AND t.type = 'slash' AND b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi
ORDER BY b.height, be.id, a.index`,
		map[string]any{"reasons": reasons, "ids": ids, "lo": lo, "hi": hi}).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("slashes: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	cons, err := s.consensusOperators(node, anchor)
	if err != nil {
		return nil, err
	}
	mapped := map[string]bool{}
	for _, op := range cons {
		mapped[op] = true
	}
	downtime, doubleSign, err := node.slashFractions(anchor)
	if err != nil {
		return nil, fmt.Errorf("slashing params: %w", err)
	}
	var out []ledgerSlash
	for _, ev := range groupLedgerRows(rows) {
		for _, rec := range ev.records() {
			op, ok := cons[rec["address"]]
			if !ok {
				// Consensus addresses map one to one onto operators, so an
				// unknown one cannot be a validator this wallet touched when
				// every validator it touched is mapped.
				for v := range touched {
					if !mapped[v] {
						return nil, balanceIncomplete{fmt.Sprintf("slashed validator %s at height %d cannot be matched to an operator", rec["address"], ev.Height)}
					}
				}
				continue
			}
			sl := ledgerSlash{Height: ev.Height, Validator: op, Fraction: downtime}
			if rec["reason"] == "double_sign" {
				sl.Fraction, sl.DoubleSign = doubleSign, true
			}
			out = append(out, sl)
		}
	}
	return out, nil
}

// --- the rebuilt balance ----------------------------------------------------

// BalanceLine is one holding on a date: a denom in the bank, or ATOM bonded
// to or unbonding from one validator.
type BalanceLine struct {
	Bucket          string           `json:"bucket"`
	Validator       string           `json:"validator,omitempty"`
	Denom           string           `json:"denom"`
	Symbol          string           `json:"symbol"`
	AmountBase      string           `json:"amount_base"`
	Quantity        decimal.Decimal  `json:"quantity"`
	PriceUSD        *decimal.Decimal `json:"price_usd"`
	ValueUSD        *decimal.Decimal `json:"value_usd"`
	PriceMissing    bool             `json:"price_missing"`
	DecimalsAssumed bool             `json:"decimals_assumed,omitempty"`
	Approximate     bool             `json:"approximate,omitempty"`
	// MaxErrorBase bounds, in base units, how far an approximate amount can
	// be from the chain's.
	MaxErrorBase string `json:"max_error_base,omitempty"`
}

// HistoricalBalance is a wallet's holdings at the end of one UTC day.
type HistoricalBalance struct {
	Address string `json:"address"`
	Date    string `json:"date"`
	Label   string `json:"label,omitempty"`
	// Status is exact, approximate (a staked figure is known only to within
	// a few base units, given per line as max_error_base),
	// incomplete (the index cannot rebuild this date) or not_reached.
	Status       string        `json:"status"`
	Reason       string        `json:"reason,omitempty"`
	Height       int64         `json:"height,omitempty"`
	BlockTime    *time.Time    `json:"block_time,omitempty"`
	AnchorHeight int64         `json:"anchor_height,omitempty"`
	Lines        []BalanceLine `json:"holdings"`
	// Native totals in display units of the chain's own asset.
	Liquid    decimal.Decimal `json:"liquid"`
	Staked    decimal.Decimal `json:"staked"`
	Unbonding decimal.Decimal `json:"unbonding"`
	TotalUSD  decimal.Decimal `json:"total_usd"`
	// UnpricedLines counts holdings with no USD price on that date; they are
	// not in total_usd.
	UnpricedLines int `json:"unpriced_lines"`
}

const (
	balanceExact       = "exact"
	balanceApproximate = "approximate"
	balanceIncompleteS = "incomplete"
	balanceNotReached  = "not_reached"
)

// balanceMaxError is the most an approximate line may be off by, in base
// units. A wider bound refuses the date rather than show the figure.
var balanceMaxError = big.NewInt(100)

// rewardsNote explains why unclaimed rewards are not in a rebuilt balance.
const rewardsNote = "Unclaimed staking rewards are not included in a past-date balance. The chain does not emit an event when rewards accrue, only when they are withdrawn, so the amount owed on a past date cannot be rebuilt exactly from indexed events; it is omitted rather than estimated. Rewards count once withdrawn, in liquid."

// HistoricalBalances rebuilds addr at the end of each day. One anchor and one
// event read serve every day.
func (s *Server) HistoricalBalances(chain, addr string, days []time.Time) ([]HistoricalBalance, error) {
	out := make([]HistoricalBalance, len(days))
	for i, d := range days {
		out[i] = HistoricalBalance{Address: addr, Date: d.UTC().Format(apiDateLayout), Lines: []BalanceLine{}}
	}
	ids, err := s.chainRowIDs(chain)
	if err != nil {
		return nil, err
	}
	node, err := newLedgerNode()
	if err != nil {
		return nil, err
	}
	var idx []int
	var targets []heightTime
	for i, d := range days {
		ht, err := s.dayEndBlock(ids, d)
		if err != nil {
			out[i].Status, out[i].Reason = balanceStatusFor(err)
			if out[i].Status == "" {
				return nil, err
			}
			continue
		}
		idx, targets = append(idx, i), append(targets, ht)
	}
	if len(targets) == 0 {
		return out, nil
	}
	rebuilt, anchor, err := s.rebuildHoldings(chain, ids, node, addr, targets)
	if err != nil {
		return nil, err
	}
	meta := s.oracle.Denoms(chain)
	for j, r := range rebuilt {
		hb := &out[idx[j]]
		if r.Status != "" {
			hb.Status, hb.Reason = r.Status, r.Reason
			continue
		}
		bt := targets[j].TimeStamp.UTC()
		hb.Height, hb.BlockTime, hb.AnchorHeight = targets[j].Height, &bt, anchor
		s.fillBalanceLines(chain, meta, hb, r.Holdings, days[idx[j]])
	}
	return out, nil
}

// walletDay is a wallet's rebuilt holdings at the end of one target block, or
// the status and reason it cannot be rebuilt.
type walletDay struct {
	Status, Reason string
	Holdings       ledgerHoldings
}

// rebuildHoldings walks addr back from the ledger tip to each target block,
// with one anchor and one event read for all of them. It returns one entry
// per target, in order, and the anchor height.
func (s *Server) rebuildHoldings(chain string, ids []int64, node *ledgerNode, addr string, targets []heightTime) ([]walletDay, int64, error) {
	out := make([]walletDay, len(targets))
	operator, _ := AccountOperator(addr)
	scan, err := s.ledgerScan(chain)
	if err != nil {
		return nil, 0, err
	}
	grantsFrom := scan.FeeGrantsFrom
	tip, err := s.ledgerTip(ids, node, scan.FeeGrantsTo)
	if err != nil {
		return nil, 0, err
	}
	lowest := tip.Height
	for _, t := range targets {
		lowest = min(lowest, t.Height)
	}
	// Coverage up to the tip is monotone, so when the lowest target's window
	// is complete every target's is.
	allIndexed := false
	if lowest < tip.Height {
		missing, err := s.windowIndexed(ids, lowest, tip.Height)
		if err != nil {
			return nil, 0, err
		}
		allIndexed = missing == 0
	}
	holes, err := s.holeHeights(chain, addr, lowest, tip.Height)
	if err != nil {
		return nil, 0, err
	}
	lowest = tip.Height
	var usable []int
	for i, t := range targets {
		if t.Height > tip.Height {
			out[i].Status, out[i].Reason = balanceNotReached, "the index has not reached the end of this day yet"
			continue
		}
		if grantsFrom == 0 || t.Height+1 < grantsFrom {
			out[i].Status = balanceIncompleteS
			out[i].Reason = fmt.Sprintf("fee grant use before height %d has not been scanned yet, so fees paid through grants after this date cannot be attributed", grantsFrom)
			continue
		}
		if !allIndexed {
			missing, err := s.windowIndexed(ids, t.Height, tip.Height)
			if err != nil {
				return nil, 0, err
			}
			if missing > 0 {
				out[i].Status = balanceIncompleteS
				out[i].Reason = fmt.Sprintf("%d blocks between this date (height %d) and the index tip (height %d) are not indexed yet, so the balance cannot be rebuilt exactly", missing, t.Height, tip.Height)
				continue
			}
		}
		// holes is ascending, so the first above the target is the lowest.
		if k := sort.Search(len(holes), func(k int) bool { return holes[k] > t.Height }); k < len(holes) {
			out[i].Status = balanceIncompleteS
			out[i].Reason = fmt.Sprintf("%d blocks between this date and the index tip, the first at height %d, are missing transactions in the index, so the balance cannot be rebuilt exactly", len(holes)-k, holes[k])
			continue
		}
		usable = append(usable, i)
		lowest = min(lowest, t.Height)
	}
	if len(usable) == 0 {
		return out, tip.Height, nil
	}
	refuse := func(reason string) ([]walletDay, int64, error) {
		for _, i := range usable {
			out[i].Status, out[i].Reason = balanceIncompleteS, reason
		}
		return out, tip.Height, nil
	}

	anchor, err := node.anchor(addr, tip.Height)
	if err != nil {
		return nil, 0, fmt.Errorf("anchor state at height %d: %w", tip.Height, err)
	}
	anchor.Time = tip.TimeStamp
	events, unattributed, err := s.ledgerEvents(ids, addr, operator, lowest, tip.Height)
	if err != nil {
		return nil, 0, err
	}
	if unattributed > 0 {
		kept := usable[:0]
		for _, i := range usable {
			if targets[i].Height < unattributed {
				out[i].Status = balanceIncompleteS
				out[i].Reason = fmt.Sprintf("a redelegation at height %d after this date cannot be attributed to one delegator", unattributed)
				continue
			}
			kept = append(kept, i)
		}
		if usable = kept; len(usable) == 0 {
			return out, tip.Height, nil
		}
	}
	fees, unknownFees, err := s.ledgerFees(ids, addr, lowest, tip.Height, node)
	if err != nil {
		return nil, 0, err
	}
	if unknownFees > 0 {
		return refuse(fmt.Sprintf("%d failed transactions this wallet paid for after this date have no recorded fee outcome yet", unknownFees))
	}
	var slashes []ledgerSlash
	if needsSlashes(anchor, events) {
		if anchor.Rates, err = node.validatorRates(tip.Height); err != nil {
			return nil, 0, fmt.Errorf("validator rates at height %d: %w", tip.Height, err)
		}
		if slashes, err = s.ledgerSlashes(ids, lowest, tip.Height, node, tip.Height, touchedValidators(anchor, events)); err != nil {
			var inc balanceIncomplete
			if errors.As(err, &inc) {
				return refuse(inc.reason)
			}
			return nil, 0, err
		}
	}
	samples, err := s.rateSamples(chain, sampledValidators(anchor, events, slashes), lowest, tip.Height)
	if err != nil {
		return nil, 0, err
	}
	heights := make([]int64, 0, len(usable))
	for _, i := range usable {
		heights = append(heights, targets[i].Height)
	}
	held, err := walkBack(ledgerInput{Address: addr, Operator: operator, BondDenom: s.native.Denom, Anchor: anchor, Events: events, Fees: fees, Slashes: slashes, Samples: samples}, heights)
	if err != nil {
		var inc ledgerInconsistent
		if errors.As(err, &inc) {
			return refuse("the indexed history does not reconcile with chain state: " + inc.msg)
		}
		var unk ledgerUnknown
		if errors.As(err, &unk) {
			return refuse(unk.msg)
		}
		return nil, 0, err
	}
	for _, i := range usable {
		out[i].Holdings = held[targets[i].Height]
	}
	return out, tip.Height, nil
}

// touchedValidators is every operator the wallet held stake with at the
// anchor or moved stake with in its events.
func touchedValidators(anchor ledgerAnchor, events []ledgerEvent) map[string]bool {
	out := map[string]bool{}
	for v := range anchor.Delegations {
		out[v] = true
	}
	for _, u := range anchor.Unbonding {
		out[u.Validator] = true
	}
	for _, e := range events {
		for _, a := range e.Attrs {
			if hrp, _, ok := strings.Cut(a.Value, "1"); ok && strings.HasSuffix(hrp, apiValoper) {
				out[a.Value] = true
			}
		}
	}
	return out
}

// sampledValidators is every validator the wallet touched whose rate is not
// known to be one throughout the window.
func sampledValidators(anchor ledgerAnchor, events []ledgerEvent, slashes []ledgerSlash) []string {
	if anchor.Rates == nil {
		return nil
	}
	slashed := map[string]bool{}
	for _, sl := range slashes {
		slashed[sl.Validator] = true
	}
	var out []string
	for v := range touchedValidators(anchor, events) {
		if r, ok := anchor.Rates[v]; !ok || !r.unit() || slashed[v] {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// rateSamples loads the recorded rates of validators from each one's last
// record at or before lo up to the anchor hi.
func (s *Server) rateSamples(chain string, validators []string, lo, hi int64) (map[string][]rateSample, error) {
	if len(validators) == 0 {
		return nil, nil
	}
	var rows []ValidatorRateSample
	if err := s.db.Raw(`
SELECT r.chain, r.operator, r.height, r.tokens::text AS tokens, r.shares::text AS shares
FROM tax_validator_rates r
WHERE r.chain = @chain AND r.operator IN @ops AND r.height < @hi
AND r.height >= COALESCE((SELECT max(p.height) FROM tax_validator_rates p
  WHERE p.chain = @chain AND p.operator = r.operator AND p.height <= @lo), @lo)
ORDER BY r.operator, r.height`, map[string]any{"chain": chain, "ops": validators, "lo": lo, "hi": hi}).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("validator rates: %w", err)
	}
	out := map[string][]rateSample{}
	for _, r := range rows {
		tokens, err := parseInt(r.Tokens)
		if err != nil {
			return nil, fmt.Errorf("validator rate tokens %q: %w", r.Tokens, err)
		}
		shares, err := decimal.NewFromString(r.Shares)
		if err != nil {
			return nil, fmt.Errorf("validator rate shares %q: %w", r.Shares, err)
		}
		out[r.Operator] = append(out[r.Operator], rateSample{Height: r.Height, Rate: validatorRate{Tokens: tokens, Shares: shares}})
	}
	return out, nil
}

// SweepValidatorRates records every validator's tokens and shares at the end
// of each finished UTC day after the newest recorded one, from the first day
// the ledger scan covers, while the node still holds that state. A day the
// node can no longer serve is skipped once it is a week old.
func (s *Server) SweepValidatorRates(chain string, ids []int64, node *ledgerNode, maxDays int, logf func(string, ...any)) {
	var last struct{ Height int64 }
	if err := s.db.Raw(`SELECT COALESCE(max(height), 0) AS height FROM tax_validator_rates WHERE chain = ?`, chain).Scan(&last).Error; err != nil {
		logf("validator rates: %v", err)
		return
	}
	from := last.Height
	if from == 0 {
		scan, err := s.ledgerScan(chain)
		if err != nil || scan.FeeGrantsFrom == 0 {
			return
		}
		from = scan.FeeGrantsFrom
	}
	var b heightTime
	if err := s.db.Table("blocks").Select("height, time_stamp").Where("chain_id IN ? AND height >= ?", ids, from).Order("height").Limit(1).Scan(&b).Error; err != nil || b.Height == 0 {
		return
	}
	day := b.TimeStamp.UTC().Truncate(24 * time.Hour)
	if last.Height != 0 {
		day = day.AddDate(0, 0, 1)
	}
	for n := 0; n < maxDays; n, day = n+1, day.AddDate(0, 0, 1) {
		ht, err := s.dayEndBlock(ids, day)
		if errors.Is(err, errBalanceNotReached) {
			return
		}
		if err != nil {
			logf("validator rates for %s: %v", day.Format(apiDateLayout), err)
			continue
		}
		heights, err := s.slashBlocksOn(ids, day, ht.Height)
		if err != nil {
			logf("validator rates for %s: %v", day.Format(apiDateLayout), err)
			return
		}
		for _, h := range append(heights, ht.Height) {
			if err := s.recordValidatorRates(chain, node, h); err != nil {
				if nowUTC().Sub(day) > 7*24*time.Hour {
					logf("validator rates at height %d skipped: %v", h, err)
					continue
				}
				logf("validator rates at height %d: %v", h, err)
				return
			}
		}
	}
}

// slashBlocksOn lists, for each block of day that slashed a validator, the
// block before it and the block itself, so the rate on either side of every
// slash is recorded. A slash burns a share of the validator's power at the
// infraction, rounded to whole tokens of power and net of entries it also
// slashed, so its effect on the rate is known only from these records.
func (s *Server) slashBlocksOn(ids []int64, day time.Time, end int64) ([]int64, error) {
	lo := end - 20000
	if prev, err := s.dayEndBlock(ids, day.AddDate(0, 0, -1)); err == nil {
		lo = prev.Height
	}
	var slashed []int64
	if err := s.db.Raw(`
SELECT DISTINCT b.height FROM block_events be
JOIN block_event_types t ON t.id = be.block_event_type_id
JOIN blocks b ON b.id = be.block_id
WHERE t.type = 'slash' AND b.chain_id IN ? AND b.height > ? AND b.height <= ?
ORDER BY b.height`, ids, lo, end).Scan(&slashed).Error; err != nil {
		return nil, err
	}
	var out []int64
	for _, h := range slashed {
		out = append(out, h-1, h)
	}
	return out, nil
}

// recordValidatorRates stores every validator's tokens and shares at the end
// of block h.
func (s *Server) recordValidatorRates(chain string, node *ledgerNode, h int64) error {
	rates, err := node.validatorRates(h)
	if err != nil {
		return err
	}
	rows := make([]ValidatorRateSample, 0, len(rates))
	for op, r := range rates {
		rows = append(rows, ValidatorRateSample{Chain: chain, Operator: op, Height: h, Tokens: r.Tokens.String(), Shares: r.Shares.String()})
	}
	return s.db.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 500).Error
}

func needsSlashes(anchor ledgerAnchor, events []ledgerEvent) bool {
	if len(anchor.Delegations) > 0 || len(anchor.Unbonding) > 0 {
		return true
	}
	for _, e := range events {
		switch e.Type {
		case "delegate", "unbond", "redelegate", "cancel_unbonding_delegation", "tokenize_shares", "redeem_shares", "create_validator":
			return true
		}
	}
	return false
}

func balanceStatusFor(err error) (string, string) {
	var inc balanceIncomplete
	switch {
	case errors.Is(err, errBalanceNotReached):
		return balanceNotReached, err.Error()
	case errors.Is(err, errBalanceBeforeIndex):
		return balanceIncompleteS, err.Error()
	case errors.As(err, &inc):
		return balanceIncompleteS, inc.reason
	}
	return "", ""
}

// fillBalanceLines prices each holding at the day's close and totals them.
func (s *Server) fillBalanceLines(chain string, meta map[string]DenomMeta, hb *HistoricalBalance, h ledgerHoldings, day time.Time) {
	for _, m := range []map[string]*big.Int{h.DelegationError, h.UnbondingError} {
		for _, v := range sortedKeys(m) {
			if m[v].Cmp(balanceMaxError) > 0 {
				hb.Status = balanceIncompleteS
				hb.Reason = fmt.Sprintf("the stake with %s can be rebuilt only to within %s base units, more than the %s a balance may be off by", v, m[v], balanceMaxError)
				return
			}
		}
	}
	hb.Status = balanceExact
	add := func(bucket, validator, denom string, amt, bound *big.Int) {
		row := s.buildRow(chain, meta, day, "", "balance", "", denom, amt.String(), "", "")
		approx := bound != nil && bound.Sign() > 0
		line := BalanceLine{
			Bucket: bucket, Validator: validator, Denom: denom, Symbol: row.Symbol,
			AmountBase: amt.String(), Quantity: row.Amount, PriceMissing: row.PriceMissing,
			DecimalsAssumed: row.DecimalsAssumed, Approximate: approx,
		}
		if approx {
			line.MaxErrorBase = bound.String()
		}
		if !row.PriceMissing {
			p, v := row.PriceUSD, row.ValueUSD.Round(2)
			line.PriceUSD, line.ValueUSD = &p, &v
			hb.TotalUSD = hb.TotalUSD.Add(v)
		} else {
			hb.UnpricedLines++
		}
		if approx {
			hb.Status = balanceApproximate
		}
		if denom == s.native.Denom {
			q := decimal.NewFromBigInt(amt, int32(-s.native.Decimals))
			switch bucket {
			case "liquid":
				hb.Liquid = hb.Liquid.Add(q)
			case "staked":
				hb.Staked = hb.Staked.Add(q)
			case "unbonding":
				hb.Unbonding = hb.Unbonding.Add(q)
			}
		}
		hb.Lines = append(hb.Lines, line)
	}
	for _, d := range sortedKeys(h.Liquid) {
		add("liquid", "", d, h.Liquid[d], nil)
	}
	for _, v := range sortedKeys(h.Delegations) {
		add("staked", v, s.native.Denom, h.Delegations[v], h.DelegationError[v])
	}
	for _, v := range sortedKeys(h.Unbonding) {
		add("unbonding", v, s.native.Denom, h.Unbonding[v], h.UnbondingError[v])
	}
}

func sortedKeys(m map[string]*big.Int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
