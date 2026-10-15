package tax

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
}

// ValidatorJurisdictions is matched to operator addresses by current moniker
// at report time. Monikers are compared case-insensitively.
var ValidatorJurisdictions = []ValidatorJurisdiction{
	{"Chorus One", "CH", "Chorus One AG, Zug", "https://chorus.one", "2026-10-14"},
	{"Swiss Staking", "CH", "Swiss Staking", "https://swiss-staking.ch", "2026-10-14"},
	{"SwissBorg", "CH", "SwissBorg SA, Lausanne", "https://swissborg.com", "2026-10-14"},
	{"Luganodes", "CH", "Luganodes, Lugano", "https://luganodes.com", "2026-10-14"},
	{"Validation Cloud", "CH", "Validation Cloud AG, Zug", "https://validationcloud.io", "2026-10-14"},
	{"Figment", "CA", "Figment Inc., Toronto", "https://figment.io", "2026-10-14"},
	{"Informal Systems", "CA", "Informal Systems Inc., Toronto", "https://informal.systems", "2026-10-14"},
	{"Coinbase Cloud", "US", "Coinbase, Inc.", "https://coinbase.com/cloud", "2026-10-14"},
	{"Kraken", "US", "Payward, Inc.", "https://kraken.com", "2026-10-14"},
	{"Allnodes", "US", "Allnodes Inc.", "https://allnodes.com", "2026-10-14"},
	{"Blockdaemon", "US", "Blockdaemon Inc., Los Angeles", "https://blockdaemon.com", "2026-10-14"},
	{"Strangelove", "US", "Strangelove Ventures", "https://strange.love", "2026-10-14"},
	{"Polkachu", "US", "Polkachu", "https://polkachu.com", "2026-10-14"},
	{"Lavender.Five Nodes", "US", "Lavender.Five Nodes", "https://lavenderfive.com", "2026-10-14"},
	{"Stakin", "FR", "Stakin SAS, Paris", "https://stakin.com", "2026-10-14"},
	{"Imperator.co", "FR", "Imperator", "https://imperator.co", "2026-10-14"},
	{"StakeLab", "FR", "StakeLab", "https://stakelab.zone", "2026-10-14"},
	{"Staking Facilities", "DE", "Staking Facilities GmbH, Munich", "https://stakingfacilities.com", "2026-10-14"},
	{"Twinstake", "GB", "Twinstake Ltd, London", "https://twinstake.io", "2026-10-14"},
	{"Simply Staking", "MT", "Simply VC Ltd", "https://simply-vc.com.mt", "2026-10-14"},
	{"Cosmostation", "KR", "Cosmostation, Seoul", "https://cosmostation.io", "2026-10-14"},
	{"DSRV", "KR", "DSRV Labs, Seoul", "https://dsrvlabs.com", "2026-10-14"},
	{"B-Harvest", "KR", "B-Harvest, Seoul", "https://bharvest.io", "2026-10-14"},
	{"Forbole", "HK", "Forbole Limited", "https://forbole.com", "2026-10-14"},
	{"Crypto.com", "SG", "Crypto.com", "https://crypto.com", "2026-10-14"},
}

// JurisdictionByMoniker resolves a current moniker to a disclosed
// jurisdiction. Case-insensitive, exact match only.
func JurisdictionByMoniker(moniker string) (ValidatorJurisdiction, bool) {
	for _, j := range ValidatorJurisdictions {
		if equalFold(j.Moniker, moniker) {
			return j, true
		}
	}
	return ValidatorJurisdiction{}, false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
