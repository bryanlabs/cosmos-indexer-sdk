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

	mu          sync.Mutex
	denoms      map[string]map[string]DenomMeta // chain -> denom -> meta (cached)
	at          map[string]time.Time
	bank        map[string]DenomMeta       // denom -> meta from the chain's bank module (cached, no TTL)
	prices      map[string]priceCacheEntry // "chain|denom|date" -> price (cached; see PriceAt)
	denomTraces map[string]string          // ibc hash -> full trace path (cached, no TTL; see DenomTrace)
}

// priceMissTTL bounds how long a "no price found" result is trusted before
// PriceAt retries the oracle. A found price for a historical date never
// changes and is cached indefinitely; a miss might just mean the oracle
// hadn't caught up yet, so it's worth a retry rather than a permanent no.
const priceMissTTL = 5 * time.Minute

type priceCacheEntry struct {
	usd    float64
	found  bool
	cached time.Time
}

// NewOracle constructs a client for the wasm-indexer oracle at base. nodeREST is
// the chain's own REST API host (e.g. the same host balances.go's NODE_REST_API
// points at), used as a fallback for denom metadata the oracle hasn't indexed;
// pass "" to disable the fallback (decimals then go straight to "unknown").
func NewOracle(base, nodeREST string) *Oracle {
	return &Oracle{
		base:        base,
		nodeREST:    nodeREST,
		http:        &http.Client{Timeout: 8 * time.Second},
		denoms:      map[string]map[string]DenomMeta{},
		at:          map[string]time.Time{},
		bank:        map[string]DenomMeta{},
		prices:      map[string]priceCacheEntry{},
		denomTraces: map[string]string{},
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
// (YYYY-MM-DD). found=false when the oracle has no price — callers must not
// treat that as a confirmed $0 (INF-201). A found price is cached indefinitely
// (a historical day's price is immutable); a miss is cached for priceMissTTL so
// one report's many rows for the same denom+date don't hammer the oracle, but a
// transient gap still gets retried.
func (o *Oracle) PriceAt(chain, denom, date string) (float64, bool) {
	key := chain + "|" + denom + "|" + date

	o.mu.Lock()
	if e, ok := o.prices[key]; ok && (e.found || time.Since(e.cached) < priceMissTTL) {
		o.mu.Unlock()
		return e.usd, e.found
	}
	o.mu.Unlock()

	var body struct {
		USD   float64 `json:"usd"`
		Found bool    `json:"found"`
	}
	url := fmt.Sprintf("%s/price?chain=%s&denom=%s&date=%s", o.base, chain, denom, date)
	usd, found := 0.0, false
	if err := o.get(url, &body); err == nil {
		usd, found = body.USD, body.Found
	}

	o.mu.Lock()
	o.prices[key] = priceCacheEntry{usd: usd, found: found, cached: nowUTC()}
	o.mu.Unlock()
	return usd, found
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

// DenomTrace resolves an "ibc/<HASH>" voucher denom to its full trace path
// (e.g. "transfer/channel-141/uosmo") via the chain's own IBC transfer
// module, for vouchers the oracle hasn't already resolved — most useful for
// an asset that arrived from a chain we've only recently started indexing
// (cross-chain IBC, INF-208). hash is the bare hash without the "ibc/"
// prefix. Cached indefinitely: once a denom trace is registered on chain it
// never changes. ok=false when the fallback is disabled, unreachable, or the
// hash is genuinely unknown to this chain.
func (o *Oracle) DenomTrace(hash string) (string, bool) {
	if o.nodeREST == "" {
		return "", false
	}

	o.mu.Lock()
	if p, ok := o.denomTraces[hash]; ok {
		o.mu.Unlock()
		return p, true
	}
	o.mu.Unlock()

	var body struct {
		DenomTrace struct {
			Path      string `json:"path"`
			BaseDenom string `json:"base_denom"`
		} `json:"denom_trace"`
	}
	url := fmt.Sprintf("%s/ibc/apps/transfer/v1/denom_traces/%s", o.nodeREST, hash)
	if err := o.get(url, &body); err != nil {
		return "", false
	}
	if body.DenomTrace.Path == "" || body.DenomTrace.BaseDenom == "" {
		return "", false
	}
	full := body.DenomTrace.Path + "/" + body.DenomTrace.BaseDenom

	o.mu.Lock()
	o.denomTraces[hash] = full
	o.mu.Unlock()
	return full, true
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
