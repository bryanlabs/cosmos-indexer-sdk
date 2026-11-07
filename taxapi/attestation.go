package taxapi

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	"gorm.io/gorm"
)

// Validator jurisdiction attestations.
//
// A validator's country is only ever what the operator itself has attested
// on chain. The operator account (the cosmos1 address whose bytes are the
// cosmosvaloper1 operator address) broadcasts any transaction, normally a
// 1uatom send to itself, carrying a memo of the form
//
//	bryanlabs-tax:jurisdiction v1 country=CH entity="Chorus One AG" source=https://chorus.one/imprint
//
// The indexer stores every transaction with its memo and signers, so the
// scanner below finds these memos, checks that the operator account signed
// the transaction, and records the attestation with the transaction hash as
// its source. The latest attestation per operator wins; country=none revokes.
// Nothing is ever inferred from a moniker, a website or a company register.

const (
	AttestationMemoPrefix   = "bryanlabs-tax:jurisdiction"
	attestationVersion      = "v1"
	undisclosedJurisdiction = "undisclosed"
)

// ValidatorAttestation is one on-chain jurisdiction attestation.
type ValidatorAttestation struct {
	ID              uint      `gorm:"primaryKey" json:"-"`
	Chain           string    `gorm:"index:idx_attest_chain_op,priority:1" json:"chain"`
	OperatorAddress string    `gorm:"index:idx_attest_chain_op,priority:2" json:"operator_address"`
	AccountAddress  string    `json:"account_address"`
	Country         string    `json:"country"` // ISO 3166-1 alpha-2, "" when revoked
	Entity          string    `json:"entity,omitempty"`
	Source          string    `json:"source,omitempty"`
	TxHash          string    `gorm:"uniqueIndex" json:"tx_hash"`
	TxID            uint      `json:"-"`
	BlockHeight     int64     `json:"block_height"`
	BlockTime       time.Time `json:"block_time"`
	Memo            string    `json:"memo"`
	Revoked         bool      `json:"revoked"`
	CreatedAt       time.Time `json:"-"`
}

// AttestationMemo is a parsed attestation memo.
type AttestationMemo struct {
	Country string `json:"country"` // "" means revoke
	Entity  string `json:"entity,omitempty"`
	Source  string `json:"source,omitempty"`
	Revoke  bool   `json:"revoke"`
}

var memoToken = regexp.MustCompile(`([a-z_]+)=("([^"]*)"|(\S+))`)

// ParseAttestationMemo parses and validates a memo. It accepts the canonical
// form and tolerates extra whitespace. Unknown keys are an error so a typo in
// "country" cannot silently produce an attestation with no country.
func ParseAttestationMemo(memo string) (AttestationMemo, error) {
	m := strings.TrimSpace(memo)
	if !strings.HasPrefix(m, AttestationMemoPrefix) {
		return AttestationMemo{}, fmt.Errorf("memo must start with %q", AttestationMemoPrefix)
	}
	rest := strings.TrimSpace(strings.TrimPrefix(m, AttestationMemoPrefix))
	fields := strings.Fields(rest)
	if len(fields) == 0 || fields[0] != attestationVersion {
		return AttestationMemo{}, fmt.Errorf("memo must name the format version %q after the prefix", attestationVersion)
	}
	rest = strings.TrimSpace(strings.TrimPrefix(rest, attestationVersion))
	var out AttestationMemo
	seen := map[string]bool{}
	consumed := 0
	for _, match := range memoToken.FindAllStringSubmatchIndex(rest, -1) {
		key := rest[match[2]:match[3]]
		var val string
		if match[6] >= 0 {
			val = rest[match[6]:match[7]]
		} else {
			val = rest[match[8]:match[9]]
		}
		consumed += match[1] - match[0]
		if seen[key] {
			return AttestationMemo{}, fmt.Errorf("%s is given twice", key)
		}
		seen[key] = true
		val = strings.TrimSpace(val)
		switch key {
		case "country":
			c := strings.ToUpper(val)
			if c == "NONE" {
				out.Revoke = true
				continue
			}
			if !isoCountries[c] {
				return AttestationMemo{}, fmt.Errorf("country must be an ISO 3166-1 alpha-2 code such as CH or US, or none to revoke; got %q", val)
			}
			out.Country = c
		case "entity":
			if len(val) > 120 {
				return AttestationMemo{}, fmt.Errorf("entity is longer than 120 characters")
			}
			out.Entity = val
		case "source":
			if !strings.HasPrefix(val, "https://") && !strings.HasPrefix(val, "http://") {
				return AttestationMemo{}, fmt.Errorf("source must be an http(s) URL")
			}
			if len(val) > 200 {
				return AttestationMemo{}, fmt.Errorf("source is longer than 200 characters")
			}
			out.Source = val
		default:
			return AttestationMemo{}, fmt.Errorf("unknown key %q (allowed: country, entity, source)", key)
		}
	}
	if strings.TrimSpace(strings.Join(strings.Fields(rest), " ")) != "" && consumed == 0 {
		return AttestationMemo{}, fmt.Errorf("no key=value pairs found after the version")
	}
	if leftover := strings.TrimSpace(memoToken.ReplaceAllString(rest, "")); leftover != "" {
		return AttestationMemo{}, fmt.Errorf("unexpected text %q; quote values that contain spaces", leftover)
	}
	if !seen["country"] {
		return AttestationMemo{}, fmt.Errorf("country is required (an ISO 3166-1 alpha-2 code, or none to revoke)")
	}
	return out, nil
}

// OperatorAccount converts a cosmosvaloper1 operator address to the cosmos1
// account that controls it (same bytes, different prefix), and back.
func OperatorAccount(operator string) (string, error) {
	hrp, bz, err := bech32.DecodeAndConvert(operator)
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(hrp, apiValoper) {
		return "", fmt.Errorf("%q is not an operator address", operator)
	}
	return bech32.ConvertAndEncode(strings.TrimSuffix(hrp, apiValoper), bz)
}

// AccountOperator is the inverse of OperatorAccount.
func AccountOperator(account string) (string, error) {
	hrp, bz, err := bech32.DecodeAndConvert(account)
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(hrp, apiValoper) || strings.HasSuffix(hrp, "valcons") {
		return "", fmt.Errorf("%q is not an account address", account)
	}
	return bech32.ConvertAndEncode(hrp+apiValoper, bz)
}

// attestationCache is the current attestation per operator address, rebuilt
// by the scanner and on demand.
type attestationCache struct {
	mu      sync.RWMutex
	byOp    map[string]ValidatorAttestation // chain|operator -> latest
	loaded  time.Time
	scanned uint
}

func (s *Server) attestations() *attestationCache {
	s.attestOnce.Do(func() { s.attest = &attestationCache{byOp: map[string]ValidatorAttestation{}} })
	return s.attest
}

// scanAttestationsOnce finds new attestation memos in txes past the cursor
// and records the valid ones. It returns how many rows it recorded.
func (s *Server) scanAttestationsOnce() (int, error) {
	c := s.attestations()
	c.mu.RLock()
	cursor := c.scanned
	c.mu.RUnlock()
	if cursor == 0 {
		var last ValidatorAttestation
		if s.db.Order("tx_id desc").Limit(1).Find(&last).RowsAffected > 0 {
			cursor = last.TxID
		}
	}
	type hit struct {
		TxID    uint
		Hash    string
		Memo    string
		Height  int64
		Time    time.Time
		ChainID string
		Signer  string
	}
	var hits []hit
	err := s.db.Raw(`
		SELECT txes.id AS tx_id, txes.hash, txes.memo, blocks.height, blocks.time_stamp AS time,
		       chains.chain_id, addresses.address AS signer
		FROM txes
		JOIN blocks ON blocks.id = txes.block_id
		JOIN chains ON chains.id = blocks.chain_id
		JOIN tx_signer_addresses tsa ON tsa.tx_id = txes.id
		JOIN addresses ON addresses.id = tsa.address_id
		WHERE txes.code = 0 AND txes.memo LIKE ? AND txes.id > ?
		ORDER BY txes.id`, AttestationMemoPrefix+"%", cursor).Scan(&hits).Error
	if err != nil {
		return 0, err
	}
	recorded := 0
	maxID := cursor
	for _, h := range hits {
		if h.TxID > maxID {
			maxID = h.TxID
		}
		parsed, perr := ParseAttestationMemo(h.Memo)
		if perr != nil {
			continue
		}
		operator, oerr := AccountOperator(h.Signer)
		if oerr != nil {
			continue
		}
		row := ValidatorAttestation{
			Chain: chainOfID(h.ChainID), OperatorAddress: operator, AccountAddress: h.Signer,
			Country: parsed.Country, Entity: parsed.Entity, Source: parsed.Source, Revoked: parsed.Revoke,
			TxHash: h.Hash, TxID: h.TxID, BlockHeight: h.Height, BlockTime: h.Time.UTC(), Memo: strings.TrimSpace(h.Memo),
		}
		res := s.db.Where("tx_hash = ?", h.Hash).FirstOrCreate(&row)
		if res.Error != nil {
			return recorded, res.Error
		}
		if res.RowsAffected > 0 {
			recorded++
		}
	}
	if err := s.reloadAttestations(); err != nil {
		return recorded, err
	}
	c.mu.Lock()
	c.scanned = maxID
	c.mu.Unlock()
	return recorded, nil
}

// chainOfID maps an indexed chain id to the API's chain name.
func chainOfID(chainID string) string {
	if strings.HasPrefix(chainID, apiProvider) || strings.Contains(chainID, apiTestnet) {
		return apiTestnet
	}
	return apiMainnet
}

func (s *Server) reloadAttestations() error {
	var rows []ValidatorAttestation
	if err := s.db.Order("block_height asc, tx_id asc").Find(&rows).Error; err != nil {
		return err
	}
	by := make(map[string]ValidatorAttestation, len(rows))
	for _, r := range rows {
		by[r.Chain+"|"+r.OperatorAddress] = r // later rows overwrite earlier ones
	}
	c := s.attestations()
	c.mu.Lock()
	c.byOp = by
	c.loaded = nowUTC()
	c.mu.Unlock()
	return nil
}

// StartAttestationScanner scans on start and then every interval.
func (s *Server) StartAttestationScanner(interval time.Duration, logf func(string, ...any)) {
	run := func() {
		n, err := s.scanAttestationsOnce()
		if err != nil {
			logf("attestation scan: %v", err)
		} else if n > 0 {
			logf("attestation scan: recorded %d new attestation(s)", n)
		}
	}
	go func() {
		run()
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			run()
		}
	}()
}

// attestationFor returns the current attestation for an operator, if any
// non-revoked one exists.
func (s *Server) attestationFor(chain, operator string) (ValidatorAttestation, bool) {
	c := s.attestations()
	c.mu.RLock()
	if c.loaded.IsZero() {
		c.mu.RUnlock()
		_ = s.reloadAttestations()
		c.mu.RLock()
	}
	a, ok := c.byOp[chain+"|"+operator]
	c.mu.RUnlock()
	if !ok || a.Revoked || a.Country == "" {
		return ValidatorAttestation{}, false
	}
	return a, true
}

// JurisdictionFunc maps a validator operator address to its attested ISO
// country code or "undisclosed". Report builders take one so they stay pure.
type JurisdictionFunc func(operator string) string

// undisclosedAlways is the JurisdictionFunc with no attestations (tests).
func undisclosedAlways(string) string { return undisclosedJurisdiction }

// jurisdictionLabel is the attested ISO country code for a validator operator
// address, or "undisclosed".
func (s *Server) jurisdictionLabel(chain, operator string) string {
	if a, ok := s.attestationFor(chain, operator); ok {
		return a.Country
	}
	return undisclosedJurisdiction
}

// jurisdictionFunc binds jurisdictionLabel to a chain.
func (s *Server) jurisdictionFunc(chain string) JurisdictionFunc {
	return func(operator string) string { return s.jurisdictionLabel(chain, operator) }
}

// AttestationHowTo is the operator-facing recipe, served with the registry
// and on /tax/coverage so an operator can attest without reading code.
var AttestationHowTo = map[string]any{
	"memo_format":  AttestationMemoPrefix + " v1 country=<ISO 3166-1 alpha-2> entity=\"<legal entity, optional>\" source=<https URL of the public evidence, optional>",
	"revoke":       AttestationMemoPrefix + " v1 country=none",
	"who_signs":    "The operator account: the cosmos1 address with the same bytes as the cosmosvaloper1 operator address. A transaction signed by any other account is ignored. Multisig operator accounts use the normal multisign flow.",
	"what_to_send": "Any successful transaction with that memo; the simplest is a 1uatom send from the operator account to itself.",
	"example":      "gaiad tx bank send <operator-account> <operator-account> 1uatom --note 'bryanlabs-tax:jurisdiction v1 country=CH entity=\"Example AG\" source=https://example.com/imprint' --chain-id cosmoshub-4 --fees 2000uatom --from <key>",
	"effect":       "The latest attestation per operator wins and applies from the block it was included in. It appears in the registry within a few minutes of being indexed; the transaction hash is the source. country=none revokes.",
	"check":        "GET /api/tax/v1/validators/attest/check?memo=<memo> validates a memo before sending.",
}

// GET /validators/jurisdictions: every operator with a current attestation.
func (s *Server) handleValidatorJurisdictions(w http.ResponseWriter, r *http.Request) {
	chain := chainParam(r.URL.Query().Get(apiChain))
	if err := s.reloadAttestations(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	monikers := s.oracle.ValidatorMonikers()
	statuses := s.oracle.ValidatorStatuses()
	c := s.attestations()
	c.mu.RLock()
	type row struct {
		OperatorAddress string    `json:"operator_address"`
		AccountAddress  string    `json:"account_address"`
		Moniker         string    `json:"moniker"`
		Status          string    `json:"status"`
		Country         string    `json:"country"`
		Entity          string    `json:"entity,omitempty"`
		Source          string    `json:"source,omitempty"`
		TxHash          string    `json:"tx_hash"`
		BlockHeight     int64     `json:"block_height"`
		DisclosedAt     time.Time `json:"disclosed_at"`
		Revoked         bool      `json:"revoked"`
	}
	out := make([]row, 0, len(c.byOp))
	disclosed := 0
	for key, a := range c.byOp {
		if !strings.HasPrefix(key, chain+"|") {
			continue
		}
		status := statuses[a.OperatorAddress]
		if status == "" {
			if _, known := monikers[a.OperatorAddress]; known {
				status = apiUnknown
			} else {
				status = "not a validator"
			}
		}
		out = append(out, row{
			OperatorAddress: a.OperatorAddress, AccountAddress: a.AccountAddress, Moniker: monikers[a.OperatorAddress],
			Status: status, Country: a.Country, Entity: a.Entity, Source: a.Source, TxHash: a.TxHash,
			BlockHeight: a.BlockHeight, DisclosedAt: a.BlockTime, Revoked: a.Revoked,
		})
		if !a.Revoked && a.Country != "" {
			disclosed++
		}
	}
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Country != out[j].Country {
			return out[i].Country < out[j].Country
		}
		return out[i].Moniker < out[j].Moniker
	})
	writeJSON(w, map[string]any{
		apiBasis:               "On-chain self-attestation only. Each row is a transaction signed by the validator's operator account carrying a bryanlabs-tax:jurisdiction memo; the transaction hash is the source. Nothing is inferred from monikers, websites or company registers. It records what the operator attested, not a verification of tax residence. Validators without an attestation are reported as undisclosed, never guessed.",
		apiChain:               chain,
		"validators_on_chain":  len(monikers),
		"validators_disclosed": disclosed,
		"jurisdictions":        out,
		"undisclosed_label":    undisclosedJurisdiction,
		"how_to_attest":        AttestationHowTo,
	})
}

// GET /validators/attest/check?memo=...: validate a memo before broadcasting.
// Optionally ?operator=cosmosvaloper1... returns the account that must sign.
func (s *Server) handleAttestationCheck(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	memo := q.Get(apiMemo)
	resp := map[string]any{apiMemo: memo}
	parsed, err := ParseAttestationMemo(memo)
	if err != nil {
		resp[apiValid] = false
		resp[apiError] = err.Error()
	} else {
		resp[apiValid] = true
		resp["parsed"] = parsed
	}
	if op := strings.TrimSpace(q.Get("operator")); op != "" {
		acct, aerr := OperatorAccount(op)
		if aerr != nil {
			resp["operator_error"] = "not a valid operator address: " + aerr.Error()
		} else {
			resp["operator_address"] = op
			resp["must_be_signed_by"] = acct
			if name, ok := s.oracle.ValidatorMonikers()[op]; ok {
				resp[apiMoniker] = name
				resp[apiStatus] = s.oracle.ValidatorStatuses()[op]
			} else {
				resp[apiStatus] = "not a validator on this chain"
			}
		}
	}
	writeJSON(w, resp)
}

// attestationsMigrate owns the table and the partial index that makes the
// scanner's memo-prefix query cheap on a multi-million-row txes table.
func AttestationsMigrate(db *gorm.DB) error {
	if err := db.AutoMigrate(&ValidatorAttestation{}); err != nil {
		return err
	}
	return db.Exec(`CREATE INDEX IF NOT EXISTS idx_txes_attestation_memo ON txes (id) WHERE memo LIKE 'bryanlabs-tax:%'`).Error
}
