package taxapi

import (
	"math/big"
	"sort"
)

// HeightHoldings is a wallet's holdings after one block, in base units by
// denom (liquid) and by validator (delegations and unbonding), or the status
// and reason the ledger could not rebuild it. The error maps bound how far a
// rebuilt figure may be from the chain's.
type HeightHoldings struct {
	Address         string            `json:"address"`
	Height          int64             `json:"height"`
	Status          string            `json:"status,omitempty"`
	Reason          string            `json:"reason,omitempty"`
	Liquid          map[string]string `json:"liquid,omitempty"`
	Delegations     map[string]string `json:"delegations,omitempty"`
	Unbonding       map[string]string `json:"unbonding,omitempty"`
	DelegationError map[string]string `json:"delegation_error,omitempty"`
	UnbondingError  map[string]string `json:"unbonding_error,omitempty"`
}

func holdingStrings(m map[string]*big.Int) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if v.Sign() != 0 {
			out[k] = v.String()
		}
	}
	return out
}

// HoldingsAtHeights walks addr back from the ledger tip to the state after
// each height, the way past-date balances are rebuilt, for comparison with
// archive state or a genesis export at those heights. A height below the
// first indexed block is the genesis state; the rebuild reads no block
// time for a target, so one with no block row is left without it.
func (s *Server) HoldingsAtHeights(chain, addr string, heights []int64) ([]HeightHoldings, error) {
	ids, err := s.chainRowIDs(chain)
	if err != nil {
		return nil, err
	}
	node, err := newLedgerNode()
	if err != nil {
		return nil, err
	}
	sorted := append([]int64(nil), heights...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	targets := make([]heightTime, len(sorted))
	for i, h := range sorted {
		var b heightTime
		if err := s.db.Table("blocks").Select("height, time_stamp").Where("chain_id IN ? AND height = ?", ids, h).Limit(1).Scan(&b).Error; err != nil {
			return nil, err
		}
		targets[i] = heightTime{Height: h, TimeStamp: b.TimeStamp}
	}
	rebuilt, _, err := s.rebuildHoldings(chain, ids, node, addr, targets)
	if err != nil {
		return nil, err
	}
	out := make([]HeightHoldings, len(rebuilt))
	for i, r := range rebuilt {
		out[i] = HeightHoldings{Address: addr, Height: sorted[i], Status: r.Status, Reason: r.Reason}
		if r.Status != "" {
			continue
		}
		out[i].Liquid = holdingStrings(r.Holdings.Liquid)
		out[i].Delegations = holdingStrings(r.Holdings.Delegations)
		out[i].Unbonding = holdingStrings(r.Holdings.Unbonding)
		out[i].DelegationError = holdingStrings(r.Holdings.DelegationError)
		out[i].UnbondingError = holdingStrings(r.Holdings.UnbondingError)
	}
	return out, nil
}

// HoldingsMismatch names what differs between a rebuilt and an expected
// state: the section (liquid, delegations, unbonding) and key, beyond the
// rebuild's own error bound. Empty when they agree.
func HoldingsMismatch(got, want HeightHoldings) []string {
	var out []string
	cmp := func(section string, g, w, bound map[string]string) {
		keys := map[string]bool{}
		for k := range g {
			keys[k] = true
		}
		for k := range w {
			keys[k] = true
		}
		for k := range keys {
			gv, _ := new(big.Int).SetString(orZero(g[k]), 10)
			wv, _ := new(big.Int).SetString(orZero(w[k]), 10)
			b, _ := new(big.Int).SetString(orZero(bound[k]), 10)
			if gv == nil || wv == nil || b == nil {
				out = append(out, section+" "+k+" unreadable")
				continue
			}
			if new(big.Int).Abs(new(big.Int).Sub(gv, wv)).Cmp(b) > 0 {
				out = append(out, section+" "+k)
			}
		}
	}
	cmp("liquid", got.Liquid, want.Liquid, nil)
	cmp("delegations", got.Delegations, want.Delegations, got.DelegationError)
	cmp("unbonding", got.Unbonding, want.Unbonding, got.UnbondingError)
	sort.Strings(out)
	return out
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}
