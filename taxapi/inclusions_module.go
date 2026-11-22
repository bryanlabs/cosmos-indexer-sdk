package taxapi

import (
	"fmt"
	"math/big"
	"strconv"

	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
)

// The fee collector and the distribution module move in every block outside
// any transaction: the mint pays the fee collector, the distribution module
// takes its whole balance and allocates it. The index keeps message events
// and transaction fees, not those block-level movements, so walking either
// account back through the ledger can never meet the chain. They are checked
// instead by balancing the block: what the index holds for the block plus the
// archive's own block events must equal the chain's change in balance.
const (
	moduleFeeCollector = authtypes.FeeCollectorName
	moduleDistribution = "distribution"
)

// checkedModule names the module account addr is, when it is one the block
// balance checks, or returns "".
func checkedModule(addr string) string {
	for _, name := range []string{moduleFeeCollector, moduleDistribution} {
		a, err := sdk.Bech32ifyAddressBytes(sdk.GetConfig().GetBech32AccountAddrPrefix(), authtypes.NewModuleAddress(name))
		if err == nil && a == addr {
			return name
		}
	}
	return ""
}

// coinFlow is a signed movement per denom.
type coinFlow map[string]*big.Int

func (f coinFlow) add(c sdk.Coins, sign int) {
	for _, coin := range c {
		v := new(big.Int).Set(coin.Amount.BigInt())
		if sign < 0 {
			v.Neg(v)
		}
		if f[coin.Denom] == nil {
			f[coin.Denom] = new(big.Int)
		}
		f[coin.Denom].Add(f[coin.Denom], v)
	}
}

// equal compares two flows, a missing denom counting as zero.
func (f coinFlow) equal(o coinFlow) bool {
	for _, pair := range [][2]coinFlow{{f, o}, {o, f}} {
		for d, v := range pair[0] {
			w := pair[1][d]
			if w == nil {
				w = new(big.Int)
			}
			if v.Cmp(w) != 0 {
				return false
			}
		}
	}
	return true
}

// addCoinEvent applies one coin_received or coin_spent event's movements of
// addr. An event may carry several receiver (or spender) and amount pairs.
func (f coinFlow) addCoinEvent(typ string, attrs [][2]string, addr string) error {
	sign := 1
	who := attrReceiver
	switch typ {
	case eventCoinReceived:
	case eventCoinSpent:
		sign, who = -1, attrSpender
	default:
		return nil
	}
	party := ""
	for _, kv := range attrs {
		switch kv[0] {
		case who:
			party = kv[1]
		case apiAmount:
			if party != addr {
				continue
			}
			c, err := sdk.ParseCoinsNormalized(kv[1])
			if err != nil {
				return fmt.Errorf("%s amount %q: %w", typ, kv[1], err)
			}
			f.add(c, sign)
		}
	}
	return nil
}

// moduleBlockFlows is what addr received less what it sent in block h's own
// events (BeginBlock and EndBlock), read from the node at that height.
func (n *ledgerNode) moduleBlockFlows(addr string, h int64) (coinFlow, error) {
	var res struct {
		Result struct {
			Height              string     `json:"height"`
			BeginBlockEvents    []rpcEvent `json:"begin_block_events"`
			EndBlockEvents      []rpcEvent `json:"end_block_events"`
			FinalizeBlockEvents []rpcEvent `json:"finalize_block_events"`
		} `json:"result"`
	}
	if err := n.rpcResult("block_results", h, &res); err != nil {
		return nil, err
	}
	if got := res.Result.Height; got != strconv.FormatInt(h, 10) {
		return nil, fmt.Errorf("block results %d: response is for height %q", h, got)
	}
	events := append(append([]rpcEvent{}, res.Result.BeginBlockEvents...), res.Result.EndBlockEvents...)
	if len(res.Result.FinalizeBlockEvents) > 0 {
		if len(events) > 0 {
			return nil, fmt.Errorf("block results %d carry both begin/end and finalize block events", h)
		}
		events = res.Result.FinalizeBlockEvents
	}
	out := coinFlow{}
	for _, e := range events {
		attrs := make([][2]string, len(e.Attributes))
		for i, a := range e.Attributes {
			k, v := eventKV(a)
			attrs[i] = [2]string{k, v}
		}
		if err := out.addCoinEvent(e.Type, attrs, addr); err != nil {
			return nil, fmt.Errorf("block results %d: %w", h, err)
		}
	}
	return out, nil
}

// moduleFeeRow is one fee denom of a transaction the ledger dates at a block.
type moduleFeeRow struct {
	TxID     uint
	Hash     string
	Code     int64
	Denom    string
	Amount   string
	Resolved bool
	Charged  string
}

// blockFees is what every transaction the ledger dates at block h paid the
// fee collector: a successful one its declared fee, a failed one its recorded
// charge. A failed one with no recorded charge paid nothing only when the
// chain shows the ante handler turned it away at h; otherwise the reason is
// returned.
func (s *Server) blockFees(chain string, ids []int64, node *ledgerNode, h int64) (coinFlow, string, error) {
	rel, rp := s.effectiveRel()
	var rows []moduleFeeRow
	if err := s.db.Raw(`SELECT * FROM (`+datedBranches(`
SELECT x.id AS tx_id, x.hash, COALESCE(p.code, x.code) AS code, COALESCE(d.base, '') AS denom, COALESCE(f.amount::text, '') AS amount,
       (p.tx_id IS NOT NULL OR o.tx_id IS NOT NULL) AS resolved,
       CASE WHEN p.tx_id IS NOT NULL THEN p.charged ELSE COALESCE(o.charged, '') END AS charged
FROM txes x
{{BLOCK}}
{{P}}
LEFT JOIN fees f ON f.tx_id = x.id
LEFT JOIN denoms d ON d.id = f.denomination_id
LEFT JOIN tax_failed_tx_fees o ON o.tx_id = x.id
WHERE b.chain_id IN @ids AND b.height = @h {{OWN}}`, rel)+`) u
ORDER BY tx_id`,
		withParams(rp, map[string]any{qpIDs: ids, "h": h})).Scan(&rows).Error; err != nil {
		return nil, "", fmt.Errorf("block fees: %w", err)
	}
	out := coinFlow{}
	failed := map[uint]moduleFeeRow{}
	for _, r := range rows {
		if r.Code != 0 {
			failed[r.TxID] = r
			continue
		}
		if r.Denom == "" {
			continue
		}
		c, err := sdk.ParseCoinsNormalized(r.Amount + r.Denom)
		if err != nil {
			return nil, "", fmt.Errorf("fee %q%s: %w", r.Amount, r.Denom, err)
		}
		out.add(c, 1)
	}
	var scan *scannedBlock
	for _, r := range failed {
		if r.Resolved {
			c, err := sdk.ParseCoinsNormalized(r.Charged)
			if err != nil {
				return nil, "", fmt.Errorf("failed transaction charge %q: %w", r.Charged, err)
			}
			out.add(c, 1)
			continue
		}
		if scan == nil {
			b, err := node.blockScan(h, false)
			if err != nil {
				return nil, "", err
			}
			scan = &b
		}
		turnedAway := false
		for i, t := range scan.txs {
			if t.Hash != r.Hash {
				continue
			}
			preEvents := chainParam(chain) == apiMainnet && h < hubFailedTxEventsFrom
			turnedAway = t.Rejected && (!preEvents || anteOnlyFailure(t.Code, scan.raw[i].Codespace))
			break
		}
		if !turnedAway {
			return nil, "a failed transaction has no recorded fee outcome", nil
		}
	}
	return out, "", nil
}

// checkModuleAcross balances block h for a module account: the index's
// message movements of the account, plus for the fee collector the fees of
// every transaction the ledger dates at h, plus the account's movements in
// the block's own events, must equal exactly the chain's change in the
// account's balance from h-1 to h. A transaction missing from the index shows
// as a shortfall in the fee collector's fees.
func (s *Server) checkModuleAcross(chain string, ids []int64, node *ledgerNode, addr, module string, h int64, rates *rateCache) (string, error) {
	at, err := rates.anchor(node, addr, h)
	if err != nil {
		return "", err
	}
	before, err := rates.anchor(node, addr, h-1)
	if err != nil {
		return "", err
	}
	for _, a := range []ledgerAnchor{at, before} {
		if len(pruneZero(copyInts(a.Delegations))) > 0 || len(a.Unbonding) > 0 {
			return "the module account holds stake", nil
		}
	}
	store := coinFlow{}
	for d, v := range at.Bank {
		store[d] = new(big.Int).Set(v)
	}
	for d, v := range before.Bank {
		if store[d] == nil {
			store[d] = new(big.Int)
		}
		store[d].Sub(store[d], v)
	}

	model := coinFlow{}
	operator, _ := AccountOperator(addr)
	events, _, err := s.ledgerEvents(ids, addr, operator, h-1, h)
	if err != nil {
		return "", err
	}
	for _, e := range events {
		// Block events come from the node below; the index keeps few.
		if e.Msg == 0 || e.Height != h {
			continue
		}
		attrs := make([][2]string, len(e.Attrs))
		for i, a := range e.Attrs {
			attrs[i] = [2]string{a.Key, a.Value}
		}
		if err := model.addCoinEvent(e.Type, attrs, addr); err != nil {
			return "", err
		}
	}
	if module == moduleFeeCollector {
		fees, reason, err := s.blockFees(chain, ids, node, h)
		if err != nil || reason != "" {
			return reason, err
		}
		for d, v := range fees {
			if model[d] == nil {
				model[d] = new(big.Int)
			}
			model[d].Add(model[d], v)
		}
	}
	block, err := node.moduleBlockFlows(addr, h)
	if err != nil {
		return "", err
	}
	for d, v := range block {
		if model[d] == nil {
			model[d] = new(big.Int)
		}
		model[d].Add(model[d], v)
	}
	if !model.equal(store) {
		return "the module account's balance change differs from the block's fees, messages and block events", nil
	}
	return "", nil
}
