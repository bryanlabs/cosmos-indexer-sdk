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

// Oracle is a thin client for the wasm-indexer price/denom API.
type Oracle struct {
	base string
	http *http.Client

	mu     sync.Mutex
	denoms map[string]map[string]DenomMeta // chain -> denom -> meta (cached)
	at     map[string]time.Time
}

func NewOracle(base string) *Oracle {
	return &Oracle{
		base:   base,
		http:   &http.Client{Timeout: 8 * time.Second},
		denoms: map[string]map[string]DenomMeta{},
		at:     map[string]time.Time{},
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
