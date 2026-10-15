package tax

import "strings"

// ValidatorJurisdiction records a validator operator's publicly disclosed
// country of establishment. Only public self-disclosure is recorded (the
// operator's own website or published company registration). It is not legal
// advice and not a verification of tax residence; a validator that chose to
// stay anonymous is reported as undisclosed, never guessed.
type ValidatorJurisdiction struct {
	Moniker string `json:"moniker"`
	// Country is the ISO 3166-1 alpha-2 code of the disclosed establishment.
	Country string `json:"country"`
	Entity  string `json:"entity"`
	Source  string `json:"source"`
	// Reviewed is the date the disclosure was last checked.
	Reviewed string `json:"reviewed"`
	// Match lists the on-chain monikers that belong to this operator when they
	// differ from Moniker: exchanges run numbered nodes ("Kraken03"), some
	// operators decorate the moniker with a domain or an emoji. Comparison is
	// case-insensitive after trimming. A trailing "*" matches a prefix and a
	// leading "*" matches anywhere in the moniker.
	Match []string `json:"match,omitempty"`
}

// ValidatorJurisdictions is matched to operator addresses by current moniker
// at report time. Monikers are compared case-insensitively after trimming.
var ValidatorJurisdictions = []ValidatorJurisdiction{
	{Moniker: "Chorus One", Country: "CH", Entity: "Chorus One AG, Zug", Source: "https://chorus.one", Reviewed: "2026-10-14", Match: []string{"chorus one*"}},
	{Moniker: "Swiss Staking", Country: "CH", Entity: "Swiss Staking", Source: "https://swiss-staking.ch", Reviewed: "2026-10-14"},
	{Moniker: "SwissBorg", Country: "CH", Entity: "SwissBorg SA, Lausanne", Source: "https://swissborg.com", Reviewed: "2026-10-14", Match: []string{"swissborg*"}},
	{Moniker: "Luganodes", Country: "CH", Entity: "Luganodes, Lugano", Source: "https://luganodes.com", Reviewed: "2026-10-14"},
	{Moniker: "Validation Cloud", Country: "CH", Entity: "Validation Cloud AG, Zug", Source: "https://validationcloud.io", Reviewed: "2026-10-14", Match: []string{"validation cloud*"}},
	{Moniker: "Figment", Country: "CA", Entity: "Figment Inc., Toronto", Source: "https://figment.io", Reviewed: "2026-10-14"},
	{Moniker: "Informal Systems", Country: "CA", Entity: "Informal Systems Inc., Toronto", Source: "https://informal.systems", Reviewed: "2026-10-14"},
	{Moniker: "Coinbase", Country: "US", Entity: "Coinbase, Inc.", Source: "https://coinbase.com/staking", Reviewed: "2026-10-15", Match: []string{"coinbase*"}},
	{Moniker: "Kraken", Country: "US", Entity: "Payward, Inc.", Source: "https://kraken.com", Reviewed: "2026-10-15", Match: []string{"kraken*"}},
	{Moniker: "Allnodes", Country: "US", Entity: "Allnodes Inc.", Source: "https://allnodes.com", Reviewed: "2026-10-14"},
	{Moniker: "Blockdaemon", Country: "US", Entity: "Blockdaemon Inc., Los Angeles", Source: "https://blockdaemon.com", Reviewed: "2026-10-14"},
	{Moniker: "Strangelove", Country: "US", Entity: "Strangelove Ventures", Source: "https://strange.love", Reviewed: "2026-10-14"},
	{Moniker: "Polkachu", Country: "US", Entity: "Polkachu", Source: "https://polkachu.com", Reviewed: "2026-10-15", Match: []string{"polkachu*"}},
	{Moniker: "Lavender.Five Nodes", Country: "US", Entity: "Lavender.Five Nodes", Source: "https://lavenderfive.com", Reviewed: "2026-10-15", Match: []string{"lavender.five*"}},
	{Moniker: "Stakin", Country: "FR", Entity: "Stakin SAS, Paris", Source: "https://stakin.com", Reviewed: "2026-10-15", Match: []string{"stakin*"}},
	{Moniker: "Imperator.co", Country: "FR", Entity: "Imperator", Source: "https://imperator.co", Reviewed: "2026-10-14"},
	{Moniker: "StakeLab", Country: "FR", Entity: "StakeLab", Source: "https://stakelab.zone", Reviewed: "2026-10-15", Match: []string{"stakelab*"}},
	{Moniker: "Kiln", Country: "FR", Entity: "Kiln SAS, Paris", Source: "https://kiln.fi", Reviewed: "2026-10-15", Match: []string{"kiln*"}},
	{Moniker: "Meria", Country: "FR", Entity: "Meria SAS, Paris", Source: "https://meria.com", Reviewed: "2026-10-15"},
	{Moniker: "Staking Facilities", Country: "DE", Entity: "Staking Facilities GmbH, Munich", Source: "https://stakingfacilities.com", Reviewed: "2026-10-14"},
	{Moniker: "Twinstake", Country: "GB", Entity: "Twinstake Ltd, London", Source: "https://twinstake.io", Reviewed: "2026-10-14"},
	{Moniker: "Simply Staking", Country: "MT", Entity: "Simply VC Ltd", Source: "https://simply-vc.com.mt", Reviewed: "2026-10-15", Match: []string{"simply staking", "simply vc*"}},
	{Moniker: "Stakely", Country: "ES", Entity: "Stakely S.L., Spain", Source: "https://stakely.io", Reviewed: "2026-10-15", Match: []string{"stakely*"}},
	{Moniker: "Cosmostation", Country: "KR", Entity: "Cosmostation, Seoul", Source: "https://cosmostation.io", Reviewed: "2026-10-15", Match: []string{"*cosmostation*"}},
	{Moniker: "DSRV", Country: "KR", Entity: "DSRV Labs, Seoul", Source: "https://dsrvlabs.com", Reviewed: "2026-10-14"},
	{Moniker: "B-Harvest", Country: "KR", Entity: "B-Harvest, Seoul", Source: "https://bharvest.io", Reviewed: "2026-10-14"},
	{Moniker: "Upbit Staking", Country: "KR", Entity: "Dunamu Inc., Seoul", Source: "https://upbit.com", Reviewed: "2026-10-15", Match: []string{"upbit*"}},
	{Moniker: "Forbole", Country: "HK", Entity: "Forbole Limited", Source: "https://forbole.com", Reviewed: "2026-10-14"},
	{Moniker: "Crypto.com", Country: "SG", Entity: "Crypto.com", Source: "https://crypto.com", Reviewed: "2026-10-14", Match: []string{"crypto.com*"}},
}

// JurisdictionByMoniker resolves a current on-chain moniker to a disclosed
// jurisdiction. An exact match on a registry moniker wins over any pattern,
// so "Staking Facilities" is never claimed by the "stakin*" pattern; a prefix
// pattern only matches at a word boundary (the next character is not a
// letter), so "Kraken03" and "kiln-beta" match while "Kilnworks" does not.
// Comparison is case-insensitive after trimming.
func JurisdictionByMoniker(moniker string) (ValidatorJurisdiction, bool) {
	m := normalizeMoniker(moniker)
	if m == "" {
		return ValidatorJurisdiction{}, false
	}
	for _, j := range ValidatorJurisdictions {
		if normalizeMoniker(j.Moniker) == m {
			return j, true
		}
	}
	for _, j := range ValidatorJurisdictions {
		for _, pat := range j.Match {
			if monikerMatches(m, normalizeMoniker(pat)) {
				return j, true
			}
		}
	}
	return ValidatorJurisdiction{}, false
}

func normalizeMoniker(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func monikerMatches(m, pat string) bool {
	switch {
	case strings.HasPrefix(pat, "*") && strings.HasSuffix(pat, "*") && len(pat) > 2:
		return strings.Contains(m, pat[1:len(pat)-1])
	case strings.HasSuffix(pat, "*"):
		prefix := pat[:len(pat)-1]
		if !strings.HasPrefix(m, prefix) {
			return false
		}
		if len(m) == len(prefix) {
			return true
		}
		next := m[len(prefix)]
		return !(('a' <= next && next <= 'z') || ('A' <= next && next <= 'Z'))
	case strings.HasPrefix(pat, "*"):
		return strings.HasSuffix(m, pat[1:])
	default:
		return m == pat
	}
}
