package taxapi

import (
	"fmt"
	"net/url"
	"time"
)

// ValidatorMonikers returns current on-chain display labels, not historical
// identities or jurisdictions. Operator addresses remain the canonical keys.
// A failed lookup leaves names blank; it never drops a reward or fabricates a
// validator. One bounded paginated refresh serves all rows in a report.
func (o *Oracle) ValidatorMonikers() map[string]string {
	o.validatorMu.Lock()
	defer o.validatorMu.Unlock()
	if o.nodeREST == "" {
		return nil
	}
	if !o.validatorsAt.IsZero() && time.Since(o.validatorsAt) < time.Hour {
		return o.validators
	}
	names := map[string]string{}
	status := map[string]string{}
	key := ""
	seen := map[string]bool{}
	for page := 0; page < 10; page++ {
		var body struct {
			Validators []struct {
				Address     string `json:"operator_address"`
				Status      string `json:"status"`
				Jailed      bool   `json:"jailed"`
				Description struct {
					Moniker string `json:"moniker"`
				} `json:"description"`
			} `json:"validators"`
			Pagination struct {
				NextKey string `json:"next_key"`
			} `json:"pagination"`
		}
		query := url.Values{"pagination.limit": {"1000"}}
		if key != "" {
			query.Set("pagination.key", key)
		}
		if err := o.get(fmt.Sprintf("%s/cosmos/staking/v1beta1/validators?%s", o.nodeREST, query.Encode()), &body); err != nil {
			// Retry after five minutes, retaining the last successful labels.
			o.validatorsAt = nowUTC().Add(-55 * time.Minute)
			return o.validators
		}
		for _, v := range body.Validators {
			if v.Address != "" {
				names[v.Address] = v.Description.Moniker
				status[v.Address] = validatorStatusLabel(v.Status, v.Jailed)
			}
		}
		key = body.Pagination.NextKey
		if key == "" {
			o.validators = names
			o.validatorStatus = status
			o.validatorsAt = nowUTC()
			return names
		}
		if seen[key] {
			break
		}
		seen[key] = true
	}
	o.validatorsAt = nowUTC().Add(-55 * time.Minute)
	return o.validators
}

// ValidatorStatuses returns the current bonding status per operator address:
// "bonded", "unbonding", "unbonded", or "jailed" (jailed overrides the
// bonding state). Same refresh and fallback as ValidatorMonikers.
func (o *Oracle) ValidatorStatuses() map[string]string {
	o.ValidatorMonikers()
	o.validatorMu.Lock()
	defer o.validatorMu.Unlock()
	return o.validatorStatus
}

func validatorStatusLabel(status string, jailed bool) string {
	if jailed {
		return "jailed"
	}
	switch status {
	case "BOND_STATUS_BONDED":
		return "bonded"
	case "BOND_STATUS_UNBONDING":
		return "unbonding"
	case "BOND_STATUS_UNBONDED":
		return "unbonded"
	}
	return "unknown"
}
