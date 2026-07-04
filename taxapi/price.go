// Package taxapi serves taxable-event queries + exports (CSV for the major tax
// platforms, plus a basic IRS 8949) over the data the tax indexer writes,
// enriched with USD pricing + denom resolution from the wasm-indexer oracle.
package taxapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// DenomMeta is the symbol + decimals for a base denom, from the oracle's
// /denoms endpoint (chain-registry + scraped IBC traces).
type DenomMeta struct {
	Symbol   string
	Decimals int
}

// Oracle is a thin client for the wasm-indexer price/denom API, with a fallback
// to the chain's own bank module for denom metadata the oracle doesn't have.
type Oracle struct {
	base     string
	nodeREST string // chain REST API host for the denoms_metadata fallback; "" disables it
	http     *http.Client

	mu     sync.Mutex
	denoms map[string]map[string]DenomMeta // chain -> denom -> meta (cached)
	at     map[string]time.Time
	bank   map[string]DenomMeta // denom -> meta from the chain's bank module (cached, no TTL)
}

// NewOracle constructs a client for the wasm-indexer oracle at base. nodeREST is
// the chain's own REST API host (e.g. the same host balances.go's NODE_REST_API
// points at), used as a fallback for denom metadata the oracle hasn't indexed;
// pass "" to disable the fallback (decimals then go straight to "unknown").
func NewOracle(base, nodeREST string) *Oracle {
	return &Oracle{
		base:     base,
		nodeREST: nodeREST,
		http:     &http.Client{Timeout: 8 * time.Second},
		denoms:   map[string]map[string]DenomMeta{},
		at:       map[string]time.Time{},
		bank:     map[string]DenomMeta{},
	}
}

// Denoms returns the chain's denom->meta map, cached for 1h.
func (o *Oracle) Denoms(chain string) map[string]DenomMeta {
	o.mu.Lock()
	if m, ok := o.denoms[chain]; ok && time.Since(o.at[chain]) < time.Hour {
		o.mu.Unlock()
		return m
	}
	o.mu.Unlock()

	m := map[string]DenomMeta{}
	var body struct {
		Denoms map[string]struct {
			Symbol   string `json:"symbol"`
			Decimals int    `json:"decimals"`
		} `json:"denoms"`
	}
	if err := o.get(fmt.Sprintf("%s/denoms?chain=%s", o.base, chain), &body); err == nil {
		for d, v := range body.Denoms {
			m[d] = DenomMeta{Symbol: v.Symbol, Decimals: v.Decimals}
		}
	}

	o.mu.Lock()
	o.denoms[chain] = m
	o.at[chain] = nowUTC()
	o.mu.Unlock()
	return m
}

// PriceAt returns the USD price for a denom on (or most recently before) a date
// (YYYY-MM-DD). found=false when the oracle has no price.
func (o *Oracle) PriceAt(chain, denom, date string) (float64, bool) {
	var body struct {
		USD   float64 `json:"usd"`
		Found bool    `json:"found"`
	}
	url := fmt.Sprintf("%s/price?chain=%s&denom=%s&date=%s", o.base, chain, denom, date)
	if err := o.get(url, &body); err != nil {
		return 0, false
	}
	return body.USD, body.Found
}

// BankMetaFallback resolves a denom's symbol+decimals directly from the chain's
// bank module when the oracle has no entry for it (nonstandard IBC assets, new
// factory denoms the wasm-indexer hasn't scraped yet). Cached indefinitely per
// denom: once a chain registers a denom's metadata it doesn't change. ok=false
// when the fallback is disabled, the node is unreachable, or the chain itself
// has no metadata for this denom (a genuinely unregistered/unknown asset).
func (o *Oracle) BankMetaFallback(denom string) (DenomMeta, bool) {
	if o.nodeREST == "" {
		return DenomMeta{}, false
	}

	o.mu.Lock()
	if m, ok := o.bank[denom]; ok {
		o.mu.Unlock()
		return m, true
	}
	o.mu.Unlock()

	var body struct {
		Metadata struct {
			Symbol     string `json:"symbol"`
			DenomUnits []struct {
				Denom    string `json:"denom"`
				Exponent int    `json:"exponent"`
			} `json:"denom_units"`
		} `json:"metadata"`
	}
	url := fmt.Sprintf("%s/cosmos/bank/v1beta1/denoms_metadata/%s", o.nodeREST, denom)
	if err := o.get(url, &body); err != nil {
		return DenomMeta{}, false
	}
	decimals := 0
	for _, u := range body.Metadata.DenomUnits {
		if u.Exponent > decimals {
			decimals = u.Exponent
		}
	}
	if decimals == 0 {
		// Metadata present but no display unit exponent found; treat as unknown
		// rather than caching a guess.
		return DenomMeta{}, false
	}
	m := DenomMeta{Symbol: body.Metadata.Symbol, Decimals: decimals}
	if m.Symbol == "" {
		m.Symbol = denom
	}

	o.mu.Lock()
	o.bank[denom] = m
	o.mu.Unlock()
	return m, true
}

func (o *Oracle) get(url string, out any) error {
	resp, err := o.http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oracle %s: %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func nowUTC() time.Time { return time.Now().UTC() }
