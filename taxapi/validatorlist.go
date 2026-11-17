package taxapi

import (
	"net/http"
	"sort"
	"sync"
	"time"
)

// GET /validators: every operator the tax index recorded a stake sample for,
// with the current on-chain moniker when the chain still lists it. The website
// merges this with the chain's own validator list so a validator the chain
// has since removed can still be picked for a statement. The list is read once
// an hour per chain; the distinct scan over tax_validator_rates is cheap but
// not free.
type indexedValidator struct {
	OperatorAddress string `json:"operator_address"`
	Moniker         string `json:"moniker"`
	FirstHeight     int64  `json:"first_height"`
	LastHeight      int64  `json:"last_height"`
}

var validatorListCache = struct {
	sync.Mutex
	byChain map[string]validatorListEntry
}{byChain: map[string]validatorListEntry{}}

type validatorListEntry struct {
	at   time.Time
	rows []indexedValidator
}

func (s *Server) indexedValidators(chain string) ([]indexedValidator, error) {
	validatorListCache.Lock()
	defer validatorListCache.Unlock()
	if e, ok := validatorListCache.byChain[chain]; ok && time.Since(e.at) < time.Hour {
		return e.rows, nil
	}
	var rows []indexedValidator
	if err := s.db.Raw(`SELECT operator AS operator_address, min(height) AS first_height, max(height) AS last_height
FROM tax_validator_rates WHERE chain = ? GROUP BY operator`, chain).Scan(&rows).Error; err != nil {
		return nil, err
	}
	validatorListCache.byChain[chain] = validatorListEntry{at: time.Now(), rows: rows}
	return rows, nil
}

func (s *Server) handleValidatorList(w http.ResponseWriter, r *http.Request) {
	chain := chainParam(r.URL.Query().Get(apiChain))
	rows, err := s.indexedValidators(chain)
	if err != nil {
		http.Error(w, "validator list unavailable", http.StatusServiceUnavailable)
		return
	}
	monikers := s.oracle.ValidatorMonikers()
	out := make([]indexedValidator, len(rows))
	for i, v := range rows {
		v.Moniker = monikers[v.OperatorAddress]
		out[i] = v
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OperatorAddress < out[j].OperatorAddress })
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, map[string]any{apiChain: chain, "count": len(out), "validators": out})
}
