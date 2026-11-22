package taxapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// legacyTestSchema creates the tables of the legacy schema the API reads,
// under a schema of its own, with three short chains whose block times
// match the real restarts.
func legacyTestSchema(t *testing.T, s *Server) string {
	t.Helper()
	schema := fmt.Sprintf("legacy_t%d", time.Now().UnixNano())
	q := func(sql string) {
		t.Helper()
		require.NoError(t, s.db.Exec(strings.ReplaceAll(sql, "L.", schema+".")).Error)
	}
	q("CREATE SCHEMA " + schema)
	t.Cleanup(func() { _ = s.db.WithContext(context.Background()).Exec("DROP SCHEMA " + schema + " CASCADE").Error })
	q("CREATE TABLE L.eras (chain_id text PRIMARY KEY, era smallint UNIQUE, first_height bigint, export_height bigint, export_paid_rewards boolean NOT NULL DEFAULT false, published boolean NOT NULL DEFAULT false)")
	q("CREATE TABLE L.blocks (chain_id text, height bigint, time timestamptz, PRIMARY KEY (chain_id, height))")
	q("CREATE TABLE L.import_ranges (chain_id text, from_height bigint, to_height bigint, status text, tax_rows bigint DEFAULT 0, updated_at timestamptz DEFAULT now(), PRIMARY KEY (chain_id, from_height))")
	q("CREATE TABLE L.reconciliation (chain_id text, address text, component text, status text, PRIMARY KEY (chain_id, address, component))")
	q(`CREATE TABLE L.taxable_events (chain_id text, height bigint, phase smallint, tx_index int, msg_index int, sub_index int, category text, amount text, denom text,
		asset text DEFAULT '', from_addr text DEFAULT '', to_addr text DEFAULT '', validator_address text DEFAULT '', reward_trigger text DEFAULT '', detail text DEFAULT '',
		"timestamp" timestamptz, tx_hash text DEFAULT '', amount_source text DEFAULT 'message', PRIMARY KEY (chain_id, height, phase, tx_index, msg_index, sub_index))`)
	q("CREATE TABLE L.tx_inclusions (chain_id text, height bigint, tx_index int, hash text, fee text, fee_payer text, fee_charged boolean, PRIMARY KEY (chain_id, height, tx_index))")
	q("CREATE TABLE L.balance_days (chain_id text, address text, day date, height bigint, holdings jsonb, PRIMARY KEY (chain_id, address, day))")
	q("INSERT INTO L.eras VALUES ('cosmoshub-1',1,1,10,true,false),('cosmoshub-2',2,1,10,true,false),('cosmoshub-3',3,1,10,false,false),('cosmoshub-4',4,5200791,NULL,false,false)")
	times := map[string][]string{
		"cosmoshub-1": {"2019-03-13T23:00:00Z", "2019-03-20T00:00:00Z", "2019-03-25T00:00:00Z", "2019-04-01T00:00:00Z", "2019-04-05T00:00:00Z", "2019-04-10T00:00:00Z", "2019-04-15T00:00:00Z", "2019-04-20T00:00:00Z", "2019-04-22T10:00:00Z", "2019-04-22T14:02:00Z"},
		"cosmoshub-2": {"2019-04-22T17:00:00Z", "2019-05-01T00:00:00Z", "2019-06-01T00:00:00Z", "2019-07-01T00:00:00Z", "2019-08-01T00:00:00Z", "2019-09-01T00:00:00Z", "2019-10-01T00:00:00Z", "2019-11-01T00:00:00Z", "2019-12-11T15:00:00Z", "2019-12-11T15:11:49Z"},
		"cosmoshub-3": {"2019-12-11T16:11:34Z", "2019-12-20T00:00:00Z", "2019-12-31T23:00:00Z", "2020-01-01T01:00:00Z", "2020-06-15T12:00:00Z", "2020-12-31T23:00:00Z", "2021-01-01T01:00:00Z", "2021-02-01T00:00:00Z", "2021-02-18T05:00:00Z", "2021-02-18T05:59:57Z"},
	}
	for chain, ts := range times {
		for i, at := range ts {
			q(fmt.Sprintf("INSERT INTO L.blocks VALUES ('%s',%d,'%s')", chain, i+1, at))
		}
		q(fmt.Sprintf("INSERT INTO L.import_ranges (chain_id, from_height, to_height, status, updated_at) VALUES ('%s',1,10,'done','2026-11-18T00:00:00Z')", chain))
	}
	s.legacySchema = schema
	return schema
}

func legacyExec(t *testing.T, s *Server, schema string, sqls ...string) {
	t.Helper()
	for _, sql := range sqls {
		require.NoError(t, s.db.Exec(strings.ReplaceAll(sql, "L.", schema+".")).Error)
	}
	require.NoError(t, s.RefreshLegacyEras(context.Background()))
}

// legacyRows writes one wallet's history across the restarts. Heights restart
// on every chain: the cosmoshub-2 rows sit at heights above the cosmoshub-3
// rows that follow them.
func legacyRows(t *testing.T, s *Server, schema string) {
	legacyExec(t, s, schema,
		`INSERT INTO L.taxable_events (chain_id, height, phase, tx_index, msg_index, sub_index, category, amount, denom, to_addr, from_addr, validator_address, reward_trigger, "timestamp", tx_hash, amount_source) VALUES
		 ('cosmoshub-1',5,2,0,0,0,'reward','1000000','uatom','wallet','','cosmosvaloper1fixture','claim','2019-04-05T00:00:00Z','AA01','event'),
		 ('cosmoshub-1',10,4,0,0,0,'reward','2000000','uatom','wallet','','cosmosvaloper1fixture','export_payout','2019-04-22T14:02:00Z','','export_payout'),
		 ('cosmoshub-2',9,2,0,0,0,'reward','3000000','uatom','wallet','','cosmosvaloper1fixture','claim','2019-12-11T15:00:00Z','BB09','event'),
		 ('cosmoshub-2',10,4,0,0,0,'reward','4000000','uatom','wallet','','cosmosvaloper1fixture','export_payout','2019-12-11T15:11:49Z','','export_payout'),
		 ('cosmoshub-3',0,0,0,0,0,'transfer','9','uatom','wallet','','','','2019-12-11T16:11:34Z','','genesis'),
		 ('cosmoshub-3',2,2,0,0,1,'transfer','1500000','uatom','merchant','wallet','','','2019-12-20T00:00:00Z','CC02','message'),
		 ('cosmoshub-3',2,2,0,0,0,'transfer','500000','uatom','merchant','wallet','','','2019-12-20T00:00:00Z','CC02','message'),
		 ('cosmoshub-3',5,2,0,0,0,'transfer','1000000','uatom','merchant','wallet','','','2020-06-15T12:00:00Z','CC05','message'),
		 ('cosmoshub-3',10,4,0,0,0,'prop29_genesis_recovery','7','uatom','wallet','','','','2021-02-18T05:59:57Z','','genesis'),
		 ('cosmoshub-3',4,2,0,0,0,'transfer','1','uatom','other-wallet','someone','','','2020-01-01T01:00:00Z','CC04','message')`,
		`INSERT INTO L.tx_inclusions VALUES
		 ('cosmoshub-3',2,0,'CC02','5000uatom','wallet',true),
		 ('cosmoshub-3',5,0,'CC05','0uatom,0stake','wallet',true),
		 ('cosmoshub-3',6,0,'CC06','7000uatom','wallet',false),
		 ('cosmoshub-2',9,0,'BB09','2500uatom','wallet',true)`,
	)
	o := s.oracle
	for _, day := range []string{"2019-04-05", "2019-04-22", "2019-12-11", "2019-12-20", "2020-06-15", "2021-02-18", "2020-01-01", "2020-03-31", "2020-12-31"} {
		o.prices[apiMainnet+"|"+apiUatom+"|"+day] = priceCacheEntry{usd: 4, found: true, cached: nowUTC()}
	}
}

func legacyGet(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr
}

// neutralPaths are the report, export and balance responses that must not
// change while no earlier Hub chain is published.
var neutralPaths = []string{
	"/events?address=wallet&chain=mainnet&format=generic",
	"/events?address=wallet&chain=mainnet&format=koinly&start=2019-01-01&end=2027-01-01",
	"/events?address=wallet&chain=mainnet&format=cryptotaxcalculator&preview=1&start=2019-01-01&end=2027-01-01",
	"/events?address=wallet&chain=mainnet&format=summ&preview=1",
	"/8949?address=wallet&chain=mainnet&start=2019-01-01&end=2027-01-01",
	"/schedule-d?address=wallet&chain=mainnet",
	"/income?address=wallet&chain=mainnet",
	"/journal?address=wallet&chain=mainnet&format=csv",
	"/journal?address=wallet&chain=mainnet",
	"/positions?address=wallet&chain=mainnet&date=2026-06-30",
	"/positions?address=wallet&chain=mainnet&date=2026-06-30&format=csv",
	"/index-coverage",
	"/index-coverage?start=2019-01-01&end=2020-01-01",
	"/index-coverage?start=2020-06-01&end=2026-01-01",
}

// response is a handler's status, headers that carry data, and body.
func response(rr *httptest.ResponseRecorder) string {
	var h []string
	for _, k := range []string{apiContentType, headerIndexCoverage, headerIndexCoverageWarning, headerIndexCoverageYears, headerIndexCoverageNote, "X-Basis-Unknown-Count", "X-Proceeds-Unknown-Count"} {
		h = append(h, k+": "+rr.Header().Get(k))
	}
	return fmt.Sprintf("%d\n%s\n%s", rr.Code, strings.Join(h, "\n"), rr.Body.String())
}

// withoutUnreportedEras removes what the coverage statement adds for each
// legacy era that reports do not read yet: its chain entry, its open item and
// both of its lines in the statement text. The statement lists those eras on
// purpose, so a response is otherwise unchanged byte for byte.
func withoutUnreportedEras(t *testing.T, s string) string {
	t.Helper()
	const key = `"statement":`
	for from := 0; ; {
		i := strings.Index(s[from:], key)
		if i < 0 {
			return s
		}
		at := from + i + len(key)
		var st CoverageStatement
		require.NoError(t, json.NewDecoder(strings.NewReader(s[at:])).Decode(&st))
		for _, c := range st.Chains {
			if c.InReports || c.ChainID == apiHubChainID {
				continue
			}
			chain, err := json.Marshal(c)
			require.NoError(t, err)
			cut := []string{string(chain) + ","}
			for _, item := range st.OpenItems {
				if strings.HasPrefix(item, c.ChainID+" (") {
					q, err := json.Marshal(item)
					require.NoError(t, err)
					cut = append(cut, string(q)+",")
					q, err = json.Marshal("- " + item + "\n")
					require.NoError(t, err)
					cut = append(cut, string(q[1:len(q)-1]))
				}
			}
			for _, line := range strings.Split(st.Text, "\n") {
				if strings.HasPrefix(line, c.ChainID+":") {
					q, err := json.Marshal(line + "\n")
					require.NoError(t, err)
					cut = append(cut, string(q[1:len(q)-1]))
				}
			}
			require.Len(t, cut, 4, "%s: chain, open item and both text lines", c.ChainID)
			for _, part := range cut {
				j := strings.Index(s[at:], part)
				require.GreaterOrEqual(t, j, 0, "%s: %s", c.ChainID, part)
				s = s[:at+j] + s[at+j+len(part):]
			}
		}
		from = at
	}
}

// withoutStatement cuts the coverage statement from a response body, which
// the old /index-coverage handler did not write, and returns it on its own.
func withoutStatement(t *testing.T, body string) (string, string) {
	t.Helper()
	const key = `,"statement":`
	i := strings.Index(body, key)
	if i < 0 {
		return body, ""
	}
	dec := json.NewDecoder(strings.NewReader(body[i+len(key):]))
	var raw json.RawMessage
	require.NoError(t, dec.Decode(&raw))
	end := i + len(key) + int(dec.InputOffset())
	return body[:i] + body[end:], `"statement":` + body[i+len(key):end]
}

// withoutEras removes the eras field from an /index-coverage body. It must
// sit right before period (or at the end), so the rest of the body is the
// response as it was before eras existed, byte for byte.
func withoutEras(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `,"eras":`)
	require.GreaterOrEqual(t, i, 0, body)
	dec := json.NewDecoder(strings.NewReader(body[i+len(`,"eras":`):]))
	var raw json.RawMessage
	require.NoError(t, dec.Decode(&raw))
	return body[:i] + body[i+len(`,"eras":`)+int(dec.InputOffset()):]
}

func legacyCoverageFixture(s *Server) {
	sc := fixtureScanner(&fakeCoverageSource{}, false)
	sc.snapshot = assembleCoverage(apiMainnet, coverageCriterionTx, 5200791, hubGenesisDate, 29500000, time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC), hubYearStarts, []heightRun{{Lo: 5200791, Hi: 29500000}}, []heightRun{{Lo: 5200791, Hi: 29500000}})
	s.coverageScanner = sc
}

func hub4Rows(t *testing.T, s *Server) {
	at := time.Date(2025, 12, 30, 12, 0, 0, 0, time.UTC)
	insertNativeFinancialEvent(t, s, 1, apiReward, "", apiWallet, "claim", "", at)
	insertNativeFinancialEvent(t, s, 2, apiTransfer, apiWallet, "merchant", "", "", at.Add(24*time.Hour))
}

// While no earlier Hub chain is published every response is what it was
// before eras existed, byte for byte, apart from eras[] on /index-coverage
// and the coverage statement naming each era reports do not read yet: with
// the legacy schema present and holding rows, and with it missing or
// unreadable.
func TestLegacyErasUnpublishedChangeNothingPostgres(t *testing.T) {
	s := refundCachePGFixture(t)
	hub4Rows(t, s)
	legacyCoverageFixture(s)
	t.Setenv("NODE_REST_API", "")

	before := map[string]string{}
	for _, p := range neutralPaths {
		before[p] = response(legacyGet(t, s, p))
	}
	// /index-coverage as the old handler wrote it.
	oldCoverage := func(path string) string {
		rr := httptest.NewRecorder()
		snap := s.coverageScanner.Snapshot()
		rr.Header().Set(apiContentType, apiJSONContentType)
		u := httptest.NewRequest(http.MethodGet, path, nil).URL.Query()
		if u.Get(apiStart) == "" && u.Get(apiEnd) == "" {
			writeJSON(rr, snap)
		} else {
			writeJSON(rr, struct {
				*IndexCoverage
				Period PeriodCoverage `json:"period"`
			}{snap, snap.Period(dateParam(u.Get(apiStart), time.Time{}), dateParam(u.Get(apiEnd), nowUTC().AddDate(0, 0, 1)))})
		}
		return rr.Body.String()
	}

	schema := legacyTestSchema(t, s)
	legacyRows(t, s, schema)
	legacyExec(t, s, schema,
		`INSERT INTO L.balance_days VALUES ('cosmoshub-3','wallet','2019-12-11',1,'{"liquid":{"uatom":"5"}}')`,
		`INSERT INTO L.reconciliation VALUES ('cosmoshub-3','wallet','liquid','closed')`)
	st := s.legacyEras(apiMainnet)
	require.True(t, st.readable)
	require.False(t, st.active())

	check := func(label string) {
		t.Helper()
		for _, p := range neutralPaths {
			got := response(legacyGet(t, s, p))
			if strings.HasPrefix(p, "/index-coverage") {
				parts := strings.SplitN(got, "\n", 9)
				body := parts[len(parts)-1]
				rest, statement := withoutStatement(t, withoutEras(t, body))
				_, beforeStatement := withoutStatement(t, strings.SplitN(before[p], "\n", 9)[8])
				require.Equal(t, oldCoverage(p), rest, "%s %s", label, p)
				require.Equal(t, withoutStatementGeneratedAt(beforeStatement), withoutStatementGeneratedAt(withoutUnreportedEras(t, statement)), "%s %s statement", label, p)
				require.Equal(t, strings.SplitN(before[p], "\n", 9)[:8], parts[:8], "%s %s headers", label, p)
				continue
			}
			require.Equal(t, withoutStatementGeneratedAt(before[p]), withoutStatementGeneratedAt(withoutUnreportedEras(t, got)), "%s %s", label, p)
		}
	}
	check("unpublished")
	// The coverage statement names each era that reports do not read yet.
	positions := response(legacyGet(t, s, "/positions?address=wallet&chain=mainnet&date=2026-06-30"))
	for _, era := range []string{"cosmoshub-1", "cosmoshub-2", "cosmoshub-3"} {
		require.Contains(t, positions, era+" (heights 1 to 10) is not read by reports yet")
	}

	// Every era imported but only cosmoshub-1 and 2 published: an earlier
	// era waits for every later one, so nothing joins reports.
	legacyExec(t, s, schema, "UPDATE L.eras SET published = true WHERE era IN (1,2)")
	require.False(t, s.legacyEras(apiMainnet).active())
	check("published without cosmoshub-3")

	// A schema the role cannot read leaves reports as they are.
	legacyExec(t, s, schema, "UPDATE L.eras SET published = true")
	require.True(t, s.legacyEras(apiMainnet).active())
	s.legacySchema = schema + "_missing"
	require.NoError(t, s.RefreshLegacyEras(context.Background()))
	require.False(t, s.legacyEras(apiMainnet).readable)
	check("unreadable")

	// eras[] lists the earlier chains while they are unpublished.
	s.legacySchema = schema
	legacyExec(t, s, schema, "UPDATE L.eras SET published = false")
	var body struct {
		Eras []EraCoverage `json:"eras"`
	}
	require.NoError(t, json.Unmarshal(legacyGet(t, s, "/index-coverage").Body.Bytes(), &body))
	require.Len(t, body.Eras, 4)
	for i, e := range body.Eras[:3] {
		require.Equal(t, fmt.Sprintf("cosmoshub-%d", i+1), e.ChainID)
		require.Equal(t, eraStatusVerifying, e.Status)
		require.False(t, e.InReports)
		require.Equal(t, int64(10), e.HeightsIndexed)
		require.NotNil(t, e.WalletsNotFullyVerified)
	}
	require.Equal(t, apiHubChainID, body.Eras[3].ChainID)
	require.Equal(t, eraStatusLive, body.Eras[3].Status)
	require.True(t, body.Eras[3].InReports)
	require.Nil(t, body.Eras[3].EndTime)
	require.Nil(t, body.Eras[3].WalletsNotFullyVerified)
}

func csvLines(body string) []string { return strings.Split(strings.TrimSpace(body), "\n") }

// Published eras join reports in ledger order across the restarts, each row
// naming its chain.
func TestLegacyErasPublishedJoinReportsPostgres(t *testing.T) {
	s := refundCachePGFixture(t)
	hub4Rows(t, s)
	legacyCoverageFixture(s)
	schema := legacyTestSchema(t, s)
	legacyRows(t, s, schema)

	// cosmoshub-3 alone: its genesis row opens the history; cosmoshub-2 and
	// earlier stay out.
	legacyExec(t, s, schema, "UPDATE L.eras SET published = true WHERE era = 3")
	st := s.legacyEras(apiMainnet)
	require.Equal(t, []string{"cosmoshub-3"}, st.chainIDs())
	lines := csvLines(legacyGet(t, s, "/events?address=wallet&chain=mainnet&format=generic").Body.String())
	var chains []string
	for _, l := range lines[1:] {
		f := strings.Split(l, ",")
		chains = append(chains, f[12]+"|"+f[2]+"|"+f[6])
	}
	require.Equal(t, []string{
		"cosmoshub-3|transfer|0.000009",
		"cosmoshub-3|transfer|0.5",
		"cosmoshub-3|transfer|1.5",
		"cosmoshub-3|transfer|1",
		"cosmoshub-3|prop29_genesis_recovery|0.000007",
		"cosmoshub-4|reward|100",
		"cosmoshub-4|transfer|100",
		"cosmoshub-3|fee|0.005",
	}, chains)

	// All three: rows run cosmoshub-1, 2, 3, 4. The cosmoshub-3 genesis row
	// restates coins cosmoshub-2 already holds and is left out; cosmoshub-2's
	// height 9 and 10 rows come before cosmoshub-3's height 2.
	legacyExec(t, s, schema, "UPDATE L.eras SET published = true")
	require.Equal(t, []string{"cosmoshub-1", "cosmoshub-2", "cosmoshub-3"}, s.legacyEras(apiMainnet).chainIDs())
	lines = csvLines(legacyGet(t, s, "/events?address=wallet&chain=mainnet&format=generic").Body.String())
	chains = nil
	for _, l := range lines[1:] {
		f := strings.Split(l, ",")
		chains = append(chains, f[12]+"|"+f[2]+"|"+f[17])
	}
	require.Equal(t, []string{
		"cosmoshub-1|reward|claim", "cosmoshub-1|reward|export_payout",
		"cosmoshub-2|reward|claim", "cosmoshub-2|reward|export_payout",
		"cosmoshub-3|transfer|", "cosmoshub-3|transfer|", "cosmoshub-3|transfer|", "cosmoshub-3|prop29_genesis_recovery|",
		"cosmoshub-4|reward|claim", "cosmoshub-4|transfer|",
		"cosmoshub-2|fee|", "cosmoshub-3|fee|",
	}, chains)

	// Form 8949: the cosmoshub-3 sales draw FIFO on the cosmoshub-1 and 2
	// reward lots, and every line names its chain.
	lines = csvLines(legacyGet(t, s, "/8949?address=wallet&chain=mainnet&start=2019-01-01&end=2021-01-01").Body.String())
	require.Equal(t, "Address,Part,Description of property,Date acquired,Date sold,Proceeds (USD),Cost basis (USD),Gain or loss (USD),Basis unknown,Proceeds unknown,Chain ID", lines[0])
	// Fees are disposals too: the cosmoshub-2 fee sells first, on its own
	// chain.
	require.Contains(t, lines[1], "04/05/2019,12/11/2019")
	require.True(t, strings.HasSuffix(lines[1], ",cosmoshub-2"), lines[1])
	sold := 0
	for _, l := range lines[1:] {
		require.True(t, strings.HasSuffix(l, ",cosmoshub-2") || strings.HasSuffix(l, ",cosmoshub-3"), l)
		require.Contains(t, l, ",false,false,", "basis known from the earlier chains' lots: %s", l)
		if strings.Contains(l, "04/05/2019,12/20/2019") {
			sold++
		}
	}
	require.Positive(t, sold, "the cosmoshub-3 sale draws on the cosmoshub-1 lot: %v", lines)

	// Journal and preview carry the chain column; positions keep the
	// cosmoshub-1 lot left after the sales.
	j := legacyGet(t, s, "/journal?address=wallet&chain=mainnet&format=csv").Body.String()
	require.True(t, strings.HasSuffix(csvLines(j)[0], ",chain_id"))
	require.Contains(t, j, ",cosmoshub-1\n")
	require.Contains(t, j, ",cosmoshub-4\n")
	p := legacyGet(t, s, "/events?address=wallet&chain=mainnet&format=cryptotaxcalculator&preview=1").Body.String()
	require.True(t, strings.HasSuffix(csvLines(p)[0], ",Chain ID"), csvLines(p)[0])
	require.Contains(t, p, ",cosmoshub-2\n")
	var pos struct {
		Note      string     `json:"note"`
		Positions []Position `json:"positions"`
	}
	require.NoError(t, json.Unmarshal(legacyGet(t, s, "/positions?address=wallet&chain=mainnet&date=2020-12-31").Body.Bytes(), &pos))
	require.Contains(t, pos.Note, "from cosmoshub-1 (2019-03-13)")
	require.Len(t, pos.Positions, 1)
	// 1+2+3+4 ATOM of rewards less 3 sold and 0.0075 of fees.
	require.Equal(t, "6.9925", pos.Positions[0].Quantity.String())

	// An era that has not reached reports yet, or was taken back out, drops
	// its rows and the chain column again.
	legacyExec(t, s, schema, "UPDATE L.eras SET published = false WHERE era = 3")
	require.False(t, s.legacyEras(apiMainnet).active())
	lines = csvLines(legacyGet(t, s, "/8949?address=wallet&chain=mainnet&start=2019-01-01&end=2027-01-01").Body.String())
	require.False(t, strings.HasSuffix(lines[0], "Chain ID"))
}

// Era ordering never uses height alone and puts the later chain last on a
// restart day.
func TestLegacyRowOrderAcrossRestartPostgres(t *testing.T) {
	s := refundCachePGFixture(t)
	schema := legacyTestSchema(t, s)
	legacyRows(t, s, schema)
	legacyExec(t, s, schema, "UPDATE L.eras SET published = true WHERE era >= 2")
	st := s.legacyEras(apiMainnet)
	events, fees, err := s.legacyWalletRows(st, "wallet", time.Date(2019, 12, 11, 0, 0, 0, 0, time.UTC), time.Date(2019, 12, 21, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	var got []string
	for _, e := range events {
		got = append(got, fmt.Sprintf("%s@%d/%d", e.ChainID, e.BlockHeight, e.SubIndex))
	}
	// cosmoshub-2 is the earliest era in reports, so cosmoshub-3's genesis
	// row is left out; within a transaction rows follow sub_index.
	require.Equal(t, []string{"cosmoshub-2@9/0", "cosmoshub-2@10/0", "cosmoshub-3@2/0", "cosmoshub-3@2/1"}, got)
	require.Len(t, fees, 2)
	require.Equal(t, "cosmoshub-2", fees[0].ChainID)
	require.Equal(t, "2500", fees[0].Amount)
	require.Equal(t, "5000", fees[1].Amount)

	// Equal times on two chains still order by era.
	a := Row{ChainID: "cosmoshub-3", Time: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	b := Row{ChainID: "cosmoshub-2", Time: a.Time.Add(time.Hour)}
	c := Row{ChainID: "", Time: a.Time.Add(-time.Hour)}
	require.True(t, rowBefore(b, a))
	require.True(t, rowBefore(a, c))
	require.True(t, rowBefore(c, Row{Time: a.Time}))
}

func TestLegacyFeeParse(t *testing.T) {
	got, err := parseLegacyFee("5000uatom,0stake, 12uatom")
	require.NoError(t, err)
	require.Equal(t, [][2]string{{"5000", "uatom"}, {"12", "uatom"}}, got)
	got, err = parseLegacyFee("0uatom,0uatom")
	require.NoError(t, err)
	require.Empty(t, got)
	_, err = parseLegacyFee("uatom")
	require.NoError(t, err)
	_, err = parseLegacyFee("12")
	require.Error(t, err)
}

// Coverage, years and the date floor follow the eras in reports.
func TestLegacyErasCoveragePostgres(t *testing.T) {
	s := refundCachePGFixture(t)
	legacyCoverageFixture(s)
	schema := legacyTestSchema(t, s)
	legacyExec(t, s, schema, "UPDATE L.eras SET published = true WHERE era = 3")
	snap := s.coverageSnapshot()
	require.Equal(t, "2019-12-11", snap.GenesisDate)
	require.Equal(t, int64(5200791), snap.GenesisHeight)
	require.True(t, snap.Complete)
	// 2019 is partial: cosmoshub-1 and 2 ran in it and are not in reports.
	y2019 := snap.Years[0]
	require.Equal(t, 2019, y2019.Year)
	require.False(t, y2019.Complete)
	require.False(t, y2019.TxComplete)
	require.Len(t, y2019.Chains, 3)
	require.Equal(t, "cosmoshub-1", y2019.Chains[0].ChainID)
	require.True(t, y2019.Chains[0].NotInReports)
	require.Equal(t, int64(0), y2019.Chains[1].IndexedBlocks)
	require.Equal(t, "2019-04-22", y2019.Chains[1].From)
	require.Equal(t, "cosmoshub-3", y2019.Chains[2].ChainID)
	require.False(t, y2019.Chains[2].NotInReports)
	require.Equal(t, int64(23), y2019.ExpectedBlocks)
	require.Equal(t, int64(3), y2019.IndexedBlocks)
	require.Equal(t, "2019-12-11", *y2019.EarliestCoveredDate)
	require.True(t, snap.Years[1].Complete, "2020 is cosmoshub-3 only")
	require.Contains(t, snap.Note, "2019 is partial")
	y2021 := snap.Years[2]
	require.Equal(t, 2021, y2021.Year)
	require.Len(t, y2021.Chains, 2)
	require.Equal(t, int64(5200791), y2021.FirstHeight)
	require.Equal(t, "2021-01-01", *y2021.EarliestCoveredDate)
	require.Contains(t, snap.Note, "from cosmoshub-3 genesis (2019-12-11)")
	require.Contains(t, snap.Note, "cosmoshub-1 and cosmoshub-2")

	p := snap.Period(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC))
	require.True(t, p.Complete, "%+v", p)
	// A 2019 report reaches back into cosmoshub-1 and 2: incomplete, not a
	// quiet start at cosmoshub-3 genesis.
	p = snap.Period(time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	require.True(t, p.BeforeGenesis)
	require.False(t, p.Complete)
	require.Equal(t, coverageStatusIncomplete, p.Status)
	require.Empty(t, p.Note)
	require.Len(t, p.IncompleteYears, 1)
	require.Equal(t, 2019, p.IncompleteYears[0].Year)
	require.Contains(t, p.Warning, "from 2019-03-13 to that day is on earlier Hub chains (cosmoshub-1 and cosmoshub-2), which are not in reports yet")
	require.NotContains(t, p.Warning, "backfill")
	p = snap.Period(time.Date(2019, 12, 1, 0, 0, 0, 0, time.UTC), time.Date(2020, 7, 1, 0, 0, 0, 0, time.UTC))
	require.False(t, p.Complete)
	// From cosmoshub-3 genesis on it is complete, 2019 included.
	p = snap.Period(time.Date(2019, 12, 11, 0, 0, 0, 0, time.UTC), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	require.True(t, p.Complete, "%+v", p)
	// All history starts at the first day in reports, as before.
	p = snap.Period(time.Time{}, time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC))
	require.True(t, p.Complete, "%+v", p)
	p = snap.Period(time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2019, 6, 1, 0, 0, 0, 0, time.UTC))
	require.False(t, p.Complete)
	require.Contains(t, p.Warning, "entirely before cosmoshub-3 genesis")

	legacyExec(t, s, schema, "UPDATE L.eras SET published = true")
	snap = s.coverageSnapshot()
	require.Equal(t, "2019-03-13", snap.GenesisDate)
	require.Len(t, snap.Years[0].Chains, 3)
	require.Equal(t, int64(23), snap.Years[0].ExpectedBlocks)
	p = snap.Period(time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	require.True(t, p.Complete, "%+v", p)
	require.Contains(t, p.Note, "before the Cosmos Hub launched")
	require.Equal(t, "2019-03-13", s.coverageSnapshot().ranges[0].From.Format(apiDateLayout))

	// A published era missing imported heights makes its year incomplete.
	legacyExec(t, s, schema, "UPDATE L.import_ranges SET to_height = 8 WHERE chain_id = 'cosmoshub-2'")
	snap = s.coverageSnapshot()
	require.False(t, snap.Complete)
	require.False(t, snap.Years[0].Complete)
	p = snap.Period(time.Date(2019, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2019, 7, 1, 0, 0, 0, 0, time.UTC))
	require.False(t, p.Complete)

	var body struct {
		Eras []EraCoverage `json:"eras"`
	}
	require.NoError(t, json.Unmarshal(legacyGet(t, s, "/index-coverage").Body.Bytes(), &body))
	require.Equal(t, eraStatusVerified, body.Eras[0].Status)
	require.True(t, body.Eras[0].InReports)
	require.Equal(t, int64(8), body.Eras[1].HeightsIndexed)
	require.Equal(t, "2019-04-22T17:00:00Z", body.Eras[1].StartTime.Format(time.RFC3339))
	// cosmoshub-4 counts only its own heights, not the earlier chains'.
	var hub4 int64
	for _, y := range snap.Years {
		for _, c := range y.Chains {
			if c.ChainID == apiHubChainID {
				hub4 += c.IndexedBlocks
			}
		}
		if len(y.Chains) == 0 {
			hub4 += y.IndexedBlocks
		}
	}
	require.Equal(t, apiHubChainID, body.Eras[3].ChainID)
	require.Equal(t, hub4, body.Eras[3].HeightsIndexed)
}

// Balances on a published earlier chain come from its stored daily
// holdings, with the end-of-day block of that chain.
func TestLegacyBalanceDaysPostgres(t *testing.T) {
	s := refundCachePGFixture(t)
	legacyCoverageFixture(s)
	schema := legacyTestSchema(t, s)
	legacyRows(t, s, schema)
	t.Setenv("NODE_REST_API", "")
	day := func(d string) time.Time { v, _ := time.Parse(apiDateLayout, d); return v }

	// Unpublished: the day is refused as before (no live rebuild here).
	_, err := s.HistoricalBalances(apiMainnet, "wallet", []time.Time{day("2020-03-31")})
	require.Error(t, err)

	legacyExec(t, s, schema, "UPDATE L.eras SET published = true WHERE era = 3")
	out, err := s.HistoricalBalances(apiMainnet, "wallet", []time.Time{day("2020-03-31")})
	require.NoError(t, err)
	require.Equal(t, balanceIncompleteS, out[0].Status)
	require.Contains(t, out[0].Reason, "still being built")

	legacyExec(t, s, schema,
		`INSERT INTO L.balance_days VALUES
		 ('cosmoshub-3','wallet','2019-12-11',1,'{"liquid":{"uatom":"9000000"},"delegations":{"cosmosvaloper1fixture":"1000000"}}'),
		 ('cosmoshub-3','wallet','2020-01-01',4,'{"liquid":{"uatom":"8000000"},"delegations":{"cosmosvaloper1fixture":"1000000"}}')`,
		`INSERT INTO L.reconciliation VALUES ('cosmoshub-3','wallet','liquid','closed'),('cosmoshub-3','other-wallet','delegations','open')`)
	out, err = s.HistoricalBalances(apiMainnet, "wallet", []time.Time{day("2019-12-15"), day("2020-03-31"), day("2019-12-11")})
	require.NoError(t, err)
	require.Equal(t, balanceExact, out[0].Status, out[0].Reason)
	require.Equal(t, "cosmoshub-3", out[0].ChainID)
	require.Equal(t, "9", out[0].Liquid.String())
	require.Equal(t, "1", out[0].Staked.String())
	require.Equal(t, int64(1), out[0].Height)
	require.Equal(t, "8", out[1].Liquid.String())
	require.Equal(t, int64(4), out[1].Height, "the last cosmoshub-3 block of 2020-03-31")
	// The restart day ends on the later chain.
	require.Equal(t, "cosmoshub-3", out[2].ChainID)

	// A wallet whose history did not reconcile is refused; one with no row
	// held nothing.
	out, err = s.HistoricalBalances(apiMainnet, "other-wallet", []time.Time{day("2020-03-31")})
	require.NoError(t, err)
	require.Equal(t, balanceIncompleteS, out[0].Status)
	require.Contains(t, out[0].Reason, "does not reconcile")
	out, err = s.HistoricalBalances(apiMainnet, "nobody", []time.Time{day("2020-03-31")})
	require.NoError(t, err)
	require.Equal(t, balanceExact, out[0].Status)
	require.True(t, out[0].Liquid.IsZero())

	// cosmoshub-2 days stay out until cosmoshub-2 is published.
	_, err = s.HistoricalBalances(apiMainnet, "wallet", []time.Time{day("2019-06-30")})
	require.Error(t, err)

	// The statement names each day's chain; without eras it has no chain
	// column, as before.
	out, err = s.HistoricalBalances(apiMainnet, "wallet", []time.Time{day("2020-03-31")})
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	writeBalanceStatementCSV(rr, out)
	require.True(t, strings.HasSuffix(csvLines(rr.Body.String())[0], ",chain_id"))
	require.True(t, bytes.Contains(rr.Body.Bytes(), []byte(",cosmoshub-3\n")))
	js, err := json.Marshal(s.historicalBalanceJSON(out[0]))
	require.NoError(t, err)
	require.Contains(t, string(js), `"chain_id":"cosmoshub-3"`)
	out[0].ChainID = ""
	rr = httptest.NewRecorder()
	writeBalanceStatementCSV(rr, out)
	require.NotContains(t, csvLines(rr.Body.String())[0], "chain_id")
	js, err = json.Marshal(s.historicalBalanceJSON(out[0]))
	require.NoError(t, err)
	require.NotContains(t, string(js), "chain_id")
}
