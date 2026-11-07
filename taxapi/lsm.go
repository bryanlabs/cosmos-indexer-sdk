package taxapi

import (
	"encoding/json"
	"strings"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

// lsmUnderlying returns the bonded coin behind a liquid staking share row.
// Tokenize and redeem rows move a share denom ("cosmosvaloper1.../N") that
// has no price; the parser records the tokens the shares were minted from
// (bonded_amount) or delegated back as (redeemed_amount), so the row can be
// shown and valued in the staking token. Rows without that detail keep the
// share denom and stay priceless rather than assume one token per share,
// which is wrong after a slash.
func lsmUnderlying(category, denom, detail string) (string, string, bool) {
	var key string
	switch category {
	case "tokenize_shares":
		key = "bonded_amount"
	case "redeem_shares":
		key = "redeemed_amount"
	default:
		return "", "", false
	}
	if !strings.HasPrefix(denom, "cosmosvaloper1") || detail == "" {
		return "", "", false
	}
	var d map[string]string
	if json.Unmarshal([]byte(detail), &d) != nil || d[key] == "" {
		return "", "", false
	}
	c, err := sdk.ParseCoinNormalized(d[key])
	if err != nil || strings.Contains(c.Denom, "/") || !c.Amount.IsPositive() {
		return "", "", false
	}
	return c.Denom, c.Amount.String(), true
}
