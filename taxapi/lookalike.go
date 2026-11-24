package taxapi

import (
	"reflect"
	"strings"
)

// A lookalike is a receipt whose label is the symbol of a listed asset (a
// registry asset with a price mapping) while the token is not that asset: its
// denom is none of the listed asset's denoms, and its base denom on the origin
// chain is derived (an issuer path) and matches none of theirs. Tax software that imports by symbol would value
// it as the listed asset, so it is flagged for review like an ATOM lookalike,
// and every export and download renders it under its own name. The same base
// over another IBC route (USDC sent the long way round) is the same token and
// stays as it is.

type officialSymbol struct {
	denoms      map[string]bool
	bases       map[string]bool
	unknownBase bool
}

// officialIndex maps an upper-cased listed symbol to the registry assets that
// carry it.
type officialIndex map[string]*officialSymbol

type officialIndexEntry struct {
	meta  uintptr
	index officialIndex
}

func newOfficialIndex(meta map[string]DenomMeta) officialIndex {
	out := officialIndex{}
	// A symbol is listed when a registry asset with a price mapping carries
	// it. Every registry asset with that symbol, priced or not (USDC over
	// Wormhole or Gravity), then contributes its denom and base, so another
	// route of any registry asset is never taken for a lookalike.
	for _, m := range meta {
		if m.Verified && m.CoingeckoID != "" && strings.TrimSpace(m.Symbol) != "" {
			key := strings.ToUpper(strings.TrimSpace(m.Symbol))
			if out[key] == nil {
				out[key] = &officialSymbol{denoms: map[string]bool{}, bases: map[string]bool{}}
			}
		}
	}
	for denom, m := range meta {
		if !m.Verified {
			continue
		}
		o := out[strings.ToUpper(strings.TrimSpace(m.Symbol))]
		if o == nil {
			continue
		}
		o.denoms[denom] = true
		switch {
		case m.Base != "":
			o.bases[m.Base] = true
		case !strings.HasPrefix(denom, "ibc/"):
			o.bases[denom] = true
		default:
			o.unknownBase = true
		}
	}
	return out
}

func mapIdentity(m map[string]DenomMeta) uintptr {
	return reflect.ValueOf(m).Pointer()
}

// officialIndexFor returns the listed-symbol index of meta: the cached one when
// meta is the oracle's cached map for chain, else one built from meta.
func (s *Server) officialIndexFor(chain string, meta map[string]DenomMeta) officialIndex {
	if s.oracle != nil {
		s.oracle.mu.Lock()
		entry, ok := s.oracle.official[chain]
		s.oracle.mu.Unlock()
		if ok && entry.meta == mapIdentity(meta) {
			return entry.index
		}
	}
	return newOfficialIndex(meta)
}

// lookalikeOf reports the listed symbol a receipt only looks like. denom is
// the on-chain denom, base the denom-trace resolution buildRow looked up, and
// symbol the label it found. It answers "" whenever the token is, or could be,
// the listed asset: a registry asset, a listed denom, a shared base, or a base
// that cannot be told.
func (ix officialIndex) lookalikeOf(meta map[string]DenomMeta, denom, base, symbol string) string {
	key := strings.ToUpper(strings.TrimSpace(symbol))
	o := ix[key]
	if o == nil || o.unknownBase || o.denoms[denom] || o.denoms[base] {
		return ""
	}
	entry, inMeta := meta[base]
	if !inMeta {
		entry, inMeta = meta[denom]
	}
	if inMeta && entry.Verified {
		return ""
	}
	candidate := ""
	switch {
	case inMeta && entry.Base != "":
		candidate = entry.Base
	case base != denom:
		candidate = base
	case !strings.HasPrefix(denom, "ibc/"):
		candidate = denom
	}
	// Only a derived base (a tokenfactory or other issuer path) is judged. A
	// plain base such as uusdc, ustars or cro can be the listed token under a
	// registry spelling this index lacks, so it is left as it is.
	if candidate == "" || !strings.Contains(candidate, "/") || o.bases[candidate] {
		return ""
	}
	return key
}

// lookalikeExportLabel is the name exports render for a lookalike: the
// token's own name when it has one that is not itself a listed symbol, else
// the raw denom.
func (ix officialIndex) lookalikeExportLabel(tokenName, denom string) string {
	name := strings.TrimSpace(tokenName)
	if name != "" && ix[strings.ToUpper(name)] == nil && !strings.EqualFold(name, apiUatom) {
		return name
	}
	return denom
}

// exportSymbol is the asset label every export and download renders. A
// lookalike renders under its own name, unless the user overrode the receipt
// with a token of their own; the reported label stays in the identity
// metadata and the API's Symbol.
func (r Row) exportSymbol() string {
	if r.AssetIdentity == nil || r.AssetIdentity.ExportLabel == "" {
		return r.Symbol
	}
	if r.AssetDecision != nil && r.AssetDecision.Mode == AssetDecisionOverride {
		return r.Symbol
	}
	return r.AssetIdentity.ExportLabel
}

// lookalikeBase is the base token shown for a lookalike: the oracle's trace
// base when it has one, else the denom-trace resolution.
func lookalikeBase(meta map[string]DenomMeta, denom, base string) string {
	for _, key := range []string{base, denom} {
		if m, ok := meta[key]; ok && m.Base != "" {
			return m.Base
		}
	}
	return base
}
