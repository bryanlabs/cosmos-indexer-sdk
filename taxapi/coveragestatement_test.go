package taxapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dbpkg "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/tax"
)

func statementSnapshot() *IndexCoverage {
	return &IndexCoverage{Chain: apiMainnet, GenesisHeight: 5200791, TipHeight: 33984407, TipTime: time.Date(2026, 11, 21, 18, 7, 0, 0, time.UTC), Complete: true}
}

func TestCoverageStatementWithoutDatabase(t *testing.T) {
	s := &Server{}
	period := PeriodCoverage{Status: coverageStatusComplete, Complete: true, Start: "2024-01-01", End: "2025-01-01"}
	st := s.coverageStatement(statementSnapshot(), period)
	if st.IndexedThroughHeight != 33984407 || st.IndexedThroughTime == nil || len(st.Chains) != 1 || st.Chains[0].ChainID != apiHubChainID {
		t.Fatalf("statement facts: %+v", st)
	}
	if len(st.RecordOnlyMessageTypes) != len(tax.NoTaxEffectTypeURLs) {
		t.Fatalf("record-only types %d, want %d", len(st.RecordOnlyMessageTypes), len(tax.NoTaxEffectTypeURLs))
	}
	for _, want := range []string{
		"COVERAGE STATEMENT",
		"Period: 2024-01-01 to 2025-01-01, every block indexed.",
		"cosmoshub-4: heights 5200791 to 33984407, indexed through 2026-11-21 18:07 UTC, read by reports.",
		"could not be read when this was generated",
		"message types are recorded with no tax effect",
	} {
		if !strings.Contains(st.Text, want) {
			t.Fatalf("statement text lacks %q:\n%s", want, st.Text)
		}
	}
	h := st.Summary
	if strings.ContainsAny(h, "\r\n") || !strings.Contains(h, "cosmoshub-4 5200791-33984407") {
		t.Fatalf("header %q", h)
	}
}

func TestCoverageStatementIncompletePeriod(t *testing.T) {
	s := &Server{}
	period := PeriodCoverage{Status: "incomplete", Start: "2021-01-01", End: "2022-01-01", Warning: "This period is not fully indexed yet."}
	st := s.coverageStatement(statementSnapshot(), period)
	if !strings.Contains(st.OpenItems[0], "not fully indexed yet") || !strings.Contains(st.Text, "not fully indexed.") {
		t.Fatalf("incomplete period not stated: %v", st.OpenItems)
	}
	if st := s.coverageStatement(nil, unknownPeriodCoverage()); !strings.Contains(st.OpenItems[0], "still being computed") {
		t.Fatalf("unknown coverage not stated: %v", st.OpenItems)
	}
}

// Uses the disposable Docker Postgres fixture, never a live tax DB.
func TestCoverageStatementHolesAndHeaders(t *testing.T) {
	db := isolatedAPIFixtureDB(t)
	if err := dbpkg.MigrateModels(db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&IndexHole{}, &LedgerScan{}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO addresses(id,address) VALUES(1,'proposer')",
		"INSERT INTO chains(id,chain_id) VALUES(1,'cosmoshub-4')",
		"INSERT INTO blocks(id,chain_id,proposer_cons_address_id,height,time_stamp,tx_indexed) VALUES(1,1,1,9041674,'2022-01-10T00:00:00Z',true),(2,1,1,9041675,'2022-01-10T00:00:06Z',true),(3,1,1,16584595,'2023-08-01T00:00:00Z',true)",
		"INSERT INTO tax_index_holes(chain,height,chain_txs,addresses_known) VALUES('mainnet',9041674,1,true),('mainnet',9041675,1,true),('mainnet',16584595,2,false)",
		"INSERT INTO tax_ledger_scans(chain,fee_grants_from,fee_grants_to) VALUES('mainnet',8695000,33984410)",
		"INSERT INTO message_types(id,message_type) VALUES(1,'/cosmos.bank.v1beta1.MsgSend'),(2,'/example.v1.MsgNew')",
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	s := NewServer(db, nil, DefaultNativeAsset)
	st := s.coverageStatement(statementSnapshot(), PeriodCoverage{Status: coverageStatusComplete, Complete: true})
	h := st.IndexHoles
	if h == nil || h.Flagged != 3 || h.Cleared != 0 || h.Open != 3 || h.ByYear[2022] != 2 || h.ByYear[2023] != 1 || h.ClearancesRead {
		t.Fatalf("holes before clearances: %+v", h)
	}
	if st.LedgerScanFrom != 8695000 || st.UnreviewedMessageTypes != 1 {
		t.Fatalf("ledger scan %d, unreviewed %d", st.LedgerScanFrom, st.UnreviewedMessageTypes)
	}
	for _, want := range []string{
		"3 cosmoshub-4 blocks (2 in 2022, 1 in 2023)",
		"all flagged blocks are counted",
		"heights 5200791 to 8694999 are not checked",
		"1 indexed message types have no tax decision yet",
	} {
		if !strings.Contains(st.Text, want) {
			t.Fatalf("statement text lacks %q:\n%s", want, st.Text)
		}
	}

	// A source-verified clearance stays recorded as a hole but no longer
	// counts as open, once the clearances are readable.
	for _, q := range []string{
		"CREATE TABLE tax_index_hole_clearances (chain text, height bigint, chain_txs bigint, placed bigint, accounts bigint, cleared_at timestamptz)",
		"INSERT INTO tax_index_hole_clearances VALUES('mainnet',9041675,1,1,1,now())",
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	s.stmtCache.facts = nil
	st = s.coverageStatement(statementSnapshot(), PeriodCoverage{Status: coverageStatusComplete, Complete: true})
	if h := st.IndexHoles; h.Flagged != 3 || h.Cleared != 1 || h.Open != 2 || h.ByYear[2022] != 1 || !h.ClearancesRead {
		t.Fatalf("holes after a clearance: %+v", h)
	}
	if strings.Contains(st.Text, "all flagged blocks are counted") || !strings.Contains(st.Text, "2 cosmoshub-4 blocks (1 in 2022, 1 in 2023)") {
		t.Fatalf("statement after a clearance:\n%s", st.Text)
	}

	// Every wrapped report carries the header, and the endpoint serves the
	// same statement as JSON and text.
	rr := httptest.NewRecorder()
	s.withPeriodCoverage(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })(rr, httptest.NewRequest(http.MethodGet, "/events?start=2024-01-01&end=2025-01-01", nil))
	// No scanner runs in this fixture, so coverage is unknown, yet the
	// statement still rides along.
	if rr.Header().Get(headerCoverageStatement) == "" || rr.Header().Get(headerIndexCoverage) != coverageStatusUnknown {
		t.Fatalf("no %s header: %v", headerCoverageStatement, rr.Header())
	}
	if !strings.Contains(rr.Header().Get("Access-Control-Expose-Headers"), headerCoverageStatement) {
		t.Fatalf("statement header not exposed: %v", rr.Header())
	}
	rr = httptest.NewRecorder()
	s.handleCoverageStatement(rr, httptest.NewRequest(http.MethodGet, "/coverage-statement?start=2024-01-01&end=2025-01-01", nil))
	var body CoverageStatement
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.IndexHoles == nil || body.IndexHoles.Open != 2 || body.Text == "" {
		t.Fatalf("endpoint JSON %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	s.handleCoverageStatement(rr, httptest.NewRequest(http.MethodGet, "/coverage-statement?format=text", nil))
	if !strings.HasPrefix(rr.Body.String(), "COVERAGE STATEMENT\n") || !strings.HasPrefix(rr.Header().Get(apiContentType), "text/plain") {
		t.Fatalf("endpoint text %q", rr.Body.String())
	}
}
