package taxapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dbpkg "github.com/DefiantLabs/cosmos-indexer/db"
)

// TestAttestationScanner inserts attestation memos into the indexer tables the
// way the indexer writes them and checks that only the one signed by the
// operator account is recorded, that a later memo supersedes an earlier one,
// that country=none revokes, and that the registry handler reports it all.
func TestAttestationScanner(t *testing.T) {
	db := isolatedAPIFixtureDB(t)
	if err := dbpkg.MigrateModels(db); err != nil {
		t.Fatal(err)
	}
	if err := AttestationsMigrate(db); err != nil {
		t.Fatal(err)
	}

	operator := "cosmosvaloper130mdu9a0etmeuw52qfxk73pn0ga6gawkxsrlwf"
	account, err := OperatorAccount(operator)
	if err != nil {
		t.Fatal(err)
	}
	stranger := "cosmos1stranger"
	for _, q := range []string{
		"INSERT INTO addresses(id,address) VALUES(1,'" + account + "'),(2,'" + stranger + "'),(3,'cosmos1proposer')",
		"INSERT INTO chains(id,chain_id) VALUES(1,'cosmoshub-4')",
		"INSERT INTO blocks(id,chain_id,proposer_cons_address_id,height,time_stamp,tx_indexed) VALUES(1,1,3,33000001,'2026-10-01T00:00:00Z',true),(2,1,3,33000002,'2026-10-02T00:00:00Z',true),(3,1,3,33000003,'2026-10-03T00:00:00Z',true),(4,1,3,33000004,'2026-10-04T00:00:00Z',true),(5,1,3,33000005,'2026-10-05T00:00:00Z',true)",
		// 1: valid attestation by the operator account
		"INSERT INTO txes(id,block_id,hash,code,memo) VALUES(1,1,'ATTEST1',0,'bryanlabs-tax:jurisdiction v1 country=US entity=\"Strangelove Ventures\" source=https://strange.love/imprint')",
		"INSERT INTO tx_signer_addresses(tx_id,address_id) VALUES(1,1)",
		// 2: a stranger claims the same thing: ignored (its signer is not an operator account)
		"INSERT INTO txes(id,block_id,hash,code,memo) VALUES(2,2,'FAKE',0,'bryanlabs-tax:jurisdiction v1 country=CH')",
		"INSERT INTO tx_signer_addresses(tx_id,address_id) VALUES(2,2)",
		// 3: a failed transaction from the operator: ignored (code != 0)
		"INSERT INTO txes(id,block_id,hash,code,memo) VALUES(3,3,'FAILED',5,'bryanlabs-tax:jurisdiction v1 country=CH')",
		"INSERT INTO tx_signer_addresses(tx_id,address_id) VALUES(3,1)",
		// 4: a malformed memo from the operator: ignored
		"INSERT INTO txes(id,block_id,hash,code,memo) VALUES(4,4,'BAD',0,'bryanlabs-tax:jurisdiction v1 country=Switzerland')",
		"INSERT INTO tx_signer_addresses(tx_id,address_id) VALUES(4,1)",
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	rest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(apiContentType, apiJSONContentType)
		_, _ = w.Write([]byte(`{"validators":[{"operator_address":"` + operator + `","status":"BOND_STATUS_UNBONDED","jailed":true,"description":{"moniker":"strangelove"}}],"pagination":{"next_key":""}}`))
	}))
	defer rest.Close()
	s := NewServer(db, NewOracle("http://oracle.invalid", rest.URL), NativeAsset{})

	n, err := s.scanAttestationsOnce()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("recorded %d attestations, want 1", n)
	}
	if got := s.jurisdictionLabel(apiMainnet, operator); got != "US" {
		t.Fatalf("jurisdiction %q, want US", got)
	}
	if got := s.jurisdictionLabel(apiMainnet, "cosmosvaloper1other"); got != undisclosedJurisdiction {
		t.Fatalf("unattested operator %q, want undisclosed", got)
	}
	if got := s.jurisdictionLabel(apiTestnet, operator); got != undisclosedJurisdiction {
		t.Fatalf("attestation must not leak across chains: %q", got)
	}

	// A second scan records nothing new and keeps the cursor moving.
	if n, err := s.scanAttestationsOnce(); err != nil || n != 0 {
		t.Fatalf("second scan: n=%d err=%v", n, err)
	}

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(apiMethodGET, "/validators/jurisdictions?chain=cosmoshub", nil))
	if rr.Code != 200 {
		t.Fatalf("registry: %d %s", rr.Code, rr.Body.String())
	}
	var reg struct {
		ValidatorsDisclosed int `json:"validators_disclosed"`
		Jurisdictions       []struct {
			OperatorAddress string `json:"operator_address"`
			AccountAddress  string `json:"account_address"`
			Moniker         string `json:"moniker"`
			Status          string `json:"status"`
			Country         string `json:"country"`
			TxHash          string `json:"tx_hash"`
			Revoked         bool   `json:"revoked"`
		} `json:"jurisdictions"`
		HowToAttest map[string]any `json:"how_to_attest"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &reg); err != nil {
		t.Fatal(err)
	}
	if reg.ValidatorsDisclosed != 1 || len(reg.Jurisdictions) != 1 {
		t.Fatalf("registry %+v", reg)
	}
	j := reg.Jurisdictions[0]
	if j.OperatorAddress != operator || j.AccountAddress != account || j.Moniker != "strangelove" || j.Status != apiJailed || j.Country != "US" || j.TxHash != "ATTEST1" || j.Revoked {
		t.Fatalf("row %+v", j)
	}
	if reg.HowToAttest["memo_format"] == nil {
		t.Fatal("how_to_attest missing")
	}

	// A later attestation supersedes; country=none revokes.
	for _, q := range []string{
		"INSERT INTO txes(id,block_id,hash,code,memo) VALUES(5,5,'REVOKE',0,'bryanlabs-tax:jurisdiction v1 country=none')",
		"INSERT INTO tx_signer_addresses(tx_id,address_id) VALUES(5,1)",
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.scanAttestationsOnce(); err != nil || n != 1 {
		t.Fatalf("revoke scan: n=%d err=%v", n, err)
	}
	if got := s.jurisdictionLabel(apiMainnet, operator); got != undisclosedJurisdiction {
		t.Fatalf("after revoke %q, want undisclosed", got)
	}
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(apiMethodGET, "/validators/jurisdictions", nil))
	if err := json.Unmarshal(rr.Body.Bytes(), &reg); err != nil {
		t.Fatal(err)
	}
	if reg.ValidatorsDisclosed != 0 || len(reg.Jurisdictions) != 1 || !reg.Jurisdictions[0].Revoked || reg.Jurisdictions[0].TxHash != "REVOKE" {
		t.Fatalf("after revoke %+v", reg)
	}

	// The check endpoint explains a memo and names the signer.
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(apiMethodGET, "/validators/attest/check?memo=bryanlabs-tax:jurisdiction+v1+country=ch&operator="+operator, nil))
	var chk map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &chk); err != nil {
		t.Fatal(err)
	}
	if chk[apiValid] != true || chk["must_be_signed_by"] != account || chk[apiStatus] != apiJailed {
		t.Fatalf("check %+v", chk)
	}
}

// TestAttestationScanUsesTheMemoIndex checks that the scan can use the
// partial index: the planner matches a partial index only when the query
// carries its predicate literally, never from a bound parameter.
func TestAttestationScanUsesTheMemoIndex(t *testing.T) {
	if !strings.Contains(attestationScanQuery, "txes.memo LIKE 'bryanlabs-tax:%'") {
		t.Fatal("the scan query must repeat idx_txes_attestation_memo's predicate literally")
	}
	db := isolatedAPIFixtureDB(t)
	if err := dbpkg.MigrateModels(db); err != nil {
		t.Fatal(err)
	}
	if err := AttestationsMigrate(db); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO addresses(id,address) VALUES(1,'cosmos1proposer')",
		"INSERT INTO chains(id,chain_id) VALUES(1,'cosmoshub-4')",
		"INSERT INTO blocks(id,chain_id,proposer_cons_address_id,height,time_stamp,tx_indexed) VALUES(1,1,1,33000001,'2026-10-01T00:00:00Z',true)",
		"INSERT INTO txes(id,block_id,hash,code,memo) SELECT g, 1, 'H' || g, 0, 'ordinary memo ' || g FROM generate_series(1, 50000) g",
		"INSERT INTO txes(id,block_id,hash,code,memo) VALUES(50001,1,'ATTEST',0,'bryanlabs-tax:jurisdiction v1 country=US')",
		"ANALYZE txes",
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var plan []string
	if err := db.Raw("EXPLAIN (COSTS OFF) "+attestationScanQuery, AttestationMemoPrefix+"%", 0).Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	text := strings.Join(plan, "\n")
	if !strings.Contains(text, "idx_txes_attestation_memo") {
		t.Fatalf("the scan does not use idx_txes_attestation_memo:\n%s", text)
	}
}
