// Package taxapi serves taxable-event queries + exports (CSV for the major tax
// platforms, plus a basic IRS 8949) over the data the tax indexer writes,
// enriched with USD pricing + denom resolution from the wasm-indexer oracle.
package taxapi

import (
	"net/http"
	"net/url"
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
	bank        map[string]DenomMeta            // denom -> meta from the chain's bank module (cached, no TTL)
	prices      map[string]priceCacheEntry      // "chain|denom|date" -> price (cached; see PriceAt)
	denomTraces map[string]denomTraceCacheEntry // ibc hash -> trace (cached; see DenomTrace)

	validatorMu     sync.Mutex
	validators      map[string]string
	validatorStatus map[string]string // operator -> bonded | unbonding | unbonded | jailed
	validatorsAt    time.Time
}

// priceMissTTL bounds how long a "no price found" result is trusted before
// PriceAt retries the oracle: a miss might just mean the oracle had not caught
// up yet, so it is worth a retry rather than a permanent no.
const priceMissTTL = 5 * time.Minute

// priceUnsettledTTL bounds how long a found price that may still be revised
// is reused before PriceAt asks the oracle again. It is long enough for one
// report to reuse a day's price across its rows and short enough that a
// revised price reaches every later report.
const priceUnsettledTTL = 10 * time.Minute

// priceSettleAfter is how long after a day ends its price is taken as final.
// The oracle refetches each asset from two days before its newest stored day
// on every refresh (six hours apart), so a day is last rewritten by the first
// refresh three days after it. A week leaves room for an oracle that was down
// for several days.
const priceSettleAfter = 7 * 24 * time.Hour

// priceSettled reports whether a found price can be kept for the life of the
// process: the oracle returned it for the requested day itself (not an
// earlier day standing in for a day it has no price for, which a later
// refresh can fill), and that day ended at least priceSettleAfter ago.
func priceSettled(requested, returned string, now time.Time) bool {
	if requested == "" || returned != requested {
		return false
	}
	day, err := time.Parse(apiDateLayout, requested)
	if err != nil {
		return false
	}
	return !now.Before(day.AddDate(0, 0, 1).Add(priceSettleAfter))
}

type priceCacheEntry struct {
	usd     float64
	found   bool
	settled bool
	cached  time.Time
}

// fresh reports whether the entry can be served at now without asking the
// oracle again.
func (e priceCacheEntry) fresh(now time.Time) bool {
	switch {
	case e.found && e.settled:
		return true
	case e.found:
		return now.Sub(e.cached) < priceUnsettledTTL
	default:
		return now.Sub(e.cached) < priceMissTTL
	}
}

// denomTraceMissTTL bounds how long an unresolved trace is trusted before
// retrying. A resolved trace is permanent and cached indefinitely; some node
// REST gateways don't implement /ibc/apps/transfer/v1/denom_traces at all
// (501 Not Implemented) -- without caching that, every row for that denom
// repeats the same failing call, and a wallet with many distinct IBC assets
// can spend most of a report's time on calls that were never going to
// succeed.
const denomTraceMissTTL = 5 * time.Minute

type denomTraceCacheEntry struct {
	path   string
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
		denomTraces: map[string]denomTraceCacheEntry{},
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
	if err := o.get(o.base, "/denoms", url.Values{apiChain: {chain}}, &body); err == nil {
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

const cosmosHubNobleUSDCDenom = "ibc/27BCBC098A3AE31C80E18A3EA7A516F2530B7362F83D7992A4D7888DBB586D33"

// fixedUSDStablecoinPrice is deliberately an exact chain+denom allowlist, not a
// symbol check. Anyone can create a token called USDC, while this voucher's IBC
// trace is pinned to transfer/channel-1266/transfer/channel-2/uusdc. The oracle
// currently knows its symbol and decimals but has no CoinGecko mapping for it.
func fixedUSDStablecoinPrice(chain, denom string) (float64, bool) {
	if chain == apiMainnet && denom == cosmosHubNobleUSDCDenom {
		return 1, true
	}
	return 0, false
}

// PriceAt returns the USD price for a denom on (or most recently before) a date
// (YYYY-MM-DD). The oracle is authoritative when it has a value. If it does not,
// an exact allowlist supplies the documented $1 peg for verified USD stablecoin
// vouchers. found=false for every other miss, which callers must not treat as a
// confirmed $0 (INF-201). A found price is cached for the life of the process
// only once it is settled (see priceSettled); a recent day's price, an earlier
// day's price standing in for the requested day, and the $1 peg are reused for
// priceUnsettledTTL, and misses for priceMissTTL, so a revised or late price
// is picked up.
func (o *Oracle) PriceAt(chain, denom, date string) (float64, bool) {
	base, _ := ibcBaseDenom(denom)
	if isUnverifiedJunoFactoryUatom(chain, denom, base) {
		return 0, false
	}
	key := chain + "|" + denom + "|" + date

	o.mu.Lock()
	if e, ok := o.prices[key]; ok && e.fresh(nowUTC()) {
		o.mu.Unlock()
		return e.usd, e.found
	}
	o.mu.Unlock()

	var body struct {
		USD   float64 `json:"usd"`
		Found bool    `json:"found"`
		Date  string  `json:"date"`
	}
	usd, found, settled := 0.0, false, false
	if err := o.get(o.base, "/price", url.Values{apiChain: {chain}, apiDenom: {denom}, apiDate: {date}}, &body); err == nil {
		usd, found = body.USD, body.Found
		settled = found && priceSettled(date, body.Date, nowUTC())
	}
	if !found {
		usd, found = fixedUSDStablecoinPrice(chain, denom)
	}

	o.mu.Lock()
	o.prices[key] = priceCacheEntry{usd: usd, found: found, settled: settled, cached: nowUTC()}
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
	if err := o.get(o.nodeREST, "/cosmos/bank/v1beta1/denoms_metadata/"+denom, nil, &body); err != nil {
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
// prefix. A resolved trace is cached indefinitely (once registered on chain
// it never changes); an unresolved one is cached for denomTraceMissTTL so a
// wallet with many rows of the same never-going-to-resolve hash (e.g. this
// node's REST gateway not implementing the endpoint at all) doesn't repeat
// the same failing call per row. ok=false when the fallback is disabled,
// unreachable, or the hash is genuinely unknown to this chain.
func (o *Oracle) DenomTrace(hash string) (string, bool) {
	if o.nodeREST == "" {
		return "", false
	}

	o.mu.Lock()
	if e, ok := o.denomTraces[hash]; ok && (e.found || time.Since(e.cached) < denomTraceMissTTL) {
		o.mu.Unlock()
		return e.path, e.found
	}
	o.mu.Unlock()

	var body struct {
		DenomTrace struct {
			Path      string `json:"path"`
			BaseDenom string `json:"base_denom"`
		} `json:"denom_trace"`
	}
	path, found := "", false
	if err := o.get(o.nodeREST, "/ibc/apps/transfer/v1/denom_traces/"+hash, nil, &body); err == nil && body.DenomTrace.Path != "" && body.DenomTrace.BaseDenom != "" {
		path, found = body.DenomTrace.Path+"/"+body.DenomTrace.BaseDenom, true
	}

	o.mu.Lock()
	o.denomTraces[hash] = denomTraceCacheEntry{path: path, found: found, cached: nowUTC()}
	o.mu.Unlock()
	return path, found
}

func (o *Oracle) get(origin, path string, query url.Values, out any) error {
	return configuredJSONGet(o.http, origin, path, query, out)
}

func nowUTC() time.Time { return time.Now().UTC() }
