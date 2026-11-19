package taxapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	flowServeBuild   = "sha256:build-a"
	flowServeProfile = "profile-a"
	flowServeFrom    = "2023-01-01"
	flowServeAsOf    = "2025-03-31"
	flowServeHalf    = "0.5"
)

var (
	flowServeDrifted = flowPGValoper(3)
	flowServeRetried = flowPGValoper(4)
	flowServeErrored = flowPGValoper(5)
)

func flowServeSource(version string) commissionServingSource {
	return commissionServingSource{Version: version, Build: flowServeBuild, Profile: flowServeProfile, FromDay: flowServeFrom, AsOf: flowServeAsOf}
}

// flowServeSchema creates the versioned result table as production has it.
func flowServeSchema(t *testing.T, s *Server) {
	t.Helper()
	require.NoError(t, s.db.Exec(`CREATE TABLE tax_commission_flow_versions (
		version text NOT NULL, chain text NOT NULL, validator text NOT NULL, build text NOT NULL,
		computed_at timestamptz, run_json jsonb NOT NULL, rows_json jsonb NOT NULL, evidence_json jsonb NOT NULL,
		private_profile_sha256 text, PRIMARY KEY (version, chain, validator))`).Error)
}

func flowServeInsert(t *testing.T, s *Server, version, build, profile string, run CommissionFlowRun, rows []CommissionFlow) {
	t.Helper()
	if rows == nil {
		rows = []CommissionFlow{}
	}
	r, err := json.Marshal(run)
	require.NoError(t, err)
	w, err := json.Marshal(rows)
	require.NoError(t, err)
	flowServeInsertRaw(t, s, version, build, profile, run.Validator, string(r), string(w))
}

func flowServeInsertRaw(t *testing.T, s *Server, version, build, profile, validator, run, rows string) {
	t.Helper()
	require.NoError(t, s.db.Exec(`INSERT INTO tax_commission_flow_versions
		(version, chain, validator, build, computed_at, run_json, rows_json, evidence_json, private_profile_sha256)
		VALUES (?, ?, ?, ?, ?, ?::jsonb, ?::jsonb, '{"private":"evidence"}'::jsonb, ?)`,
		version, apiMainnet, validator, build, time.Now().UTC(), run, rows, profile).Error)
}

func TestCommissionFlowsServeAcceptedVersionsPostgres(t *testing.T) {
	s := flowPGServer(t)
	flowServeSchema(t, s)
	retry := flowServeSource("retry")
	retry.Root = flowValidatorRoot(flowServeRetried)
	s.flowSources = map[string][]commissionServingSource{apiMainnet: {retry, flowServeSource("shard"), flowServeSource("source"), flowServeSource("late")}}
	chain := chainParam("cosmoshub")
	h := s.handleValidatorCommissionFlows

	// Legacy rows: the main validator reconciled, the other drifted, and one
	// that errored in its versioned attempt.
	require.NoError(t, s.saveCommissionFlows(flowPGRun(flowPGValidator, flowStatusReconciled, `{"2025-01":"1000000","2025-02":"2500000"}`), flowPGRows(flowPGValidator,
		[3]string{flowPGJan, flowExchange, "1000000"}, [3]string{flowPGFeb, flowHeld, "2500000"})))
	require.NoError(t, s.saveCommissionFlows(flowPGRun(flowPGOther, flowStatusDrift, `{"2025-02":"300000"}`), flowPGRows(flowPGOther,
		[3]string{flowPGFeb, flowExchange, "300000"})))
	require.NoError(t, s.saveCommissionFlows(flowPGRun(flowServeErrored, flowStatusReconciled, `{"2025-02":"100000"}`), flowPGRows(flowServeErrored,
		[3]string{flowPGFeb, flowHeld, "100000"})))

	// The main validator's accepted versioned result replaces its legacy one.
	main := flowPGRun(flowPGValidator, flowStatusReconciled, `{"2025-01":"1000000","2025-02":"2500000"}`)
	main.Wallets = 9
	flowServeInsert(t, s, "shard", flowServeBuild, flowServeProfile, main, flowPGRows(flowPGValidator,
		[3]string{flowPGJan, flowExchange, "1000000"}, [3]string{flowPGFeb, flowHeld, "2000000"}, [3]string{flowPGFeb, flowExchangeLike, "500000"}))
	// The same result adopted from the source collection is not counted twice.
	flowServeInsert(t, s, "source", flowServeBuild, flowServeProfile, main, flowPGRows(flowPGValidator, [3]string{flowPGJan, flowFees, "1000000"}))
	// A stored error is not a result: the legacy run stays.
	failed := flowPGRun(flowServeErrored, flowStatusError, "{}")
	failed.Reason = "deadline"
	flowServeInsert(t, s, "shard", flowServeBuild, flowServeProfile, failed, nil)
	// A validator only the versioned collections traced, with drift.
	drift := flowPGRun(flowServeDrifted, flowStatusDrift, `{"2025-02":"700000"}`)
	drift.DriftBase = "99000000"
	flowServeInsert(t, s, "shard", flowServeBuild, flowServeProfile, drift, flowPGRows(flowServeDrifted, [3]string{flowPGFeb, flowExchange, "700000"}))
	// Results the API must not serve: another build, another profile, another
	// window, a pinned retry for another validator, a run stored under another
	// validator, a chunked payload, and an unlisted collection.
	other := flowPGRun(flowServeRetried, flowStatusReconciled, `{"2025-02":"400000"}`)
	flowServeInsert(t, s, "source", "sha256:other", flowServeProfile, other, nil)
	flowServeInsert(t, s, "shard", flowServeBuild, "profile-b", other, nil)
	window := other
	window.AsOf = time.Date(2025, 4, 30, 0, 0, 0, 0, time.UTC)
	flowServeInsert(t, s, "late", flowServeBuild, flowServeProfile, window, nil)
	wrongRoot := flowPGRun(flowPGOther, flowStatusReconciled, `{"2025-02":"300000"}`)
	flowServeInsert(t, s, "retry", flowServeBuild, flowServeProfile, wrongRoot, nil)
	r, err := json.Marshal(other)
	require.NoError(t, err)
	flowServeInsertRaw(t, s, "shard", flowServeBuild, flowServeProfile, flowPGOther, string(r), "[]")
	flowServeInsertRaw(t, s, "shard", flowServeBuild, flowServeProfile, "cosmosvaloper1chunked", `"cosmos-commission-payload/v1:run:abc"`, "[]")
	flowServeInsert(t, s, "unlisted", flowServeBuild, flowServeProfile, other, nil)

	served, err := s.servedCommissionFlows(apiMainnet, "")
	require.NoError(t, err)
	require.Len(t, served, 4, "main, other, errored and drifted; nothing else is accepted")
	require.True(t, served[flowPGValidator].Versioned)
	require.False(t, served[flowPGOther].Versioned, "no accepted versioned result keeps the legacy run")
	require.False(t, served[flowServeErrored].Versioned)
	require.True(t, served[flowServeDrifted].Versioned)

	var got ValidatorCommissionFlows
	flowPGGet(t, h, "/validator-commission-flows?validator="+flowPGValidator, http.StatusOK, &got)
	require.Equal(t, flowStatusReconciled, got.Status)
	require.EqualValues(t, 9, got.Reconciliation["wallets"])
	require.Equal(t, "3.5", got.Total.Withdrawn.String(), "commission is the same in both results")
	classes := map[string]string{}
	for _, l := range got.Total.Flows {
		classes[l.Class] = l.Amount.String()
	}
	require.Equal(t, map[string]string{flowExchange: "1", flowHeld: "2", flowExchangeLike: flowServeHalf}, classes, "flows come from the shard's result, not the source's")

	var errored ValidatorCommissionFlows
	flowPGGet(t, h, "/validator-commission-flows?validator="+flowServeErrored, http.StatusOK, &errored)
	require.Equal(t, flowStatusReconciled, errored.Status, "a stored versioned error never replaces a finished legacy result")
	require.Equal(t, "0.1", errored.Total.Withdrawn.String())

	var drifted ValidatorCommissionFlows
	flowPGGet(t, h, "/validator-commission-flows?validator="+flowServeDrifted, http.StatusOK, &drifted)
	require.Equal(t, flowStatusDrift, drifted.Status, "a drift result is served and labelled as drift")
	require.Equal(t, "99", drifted.Reconciliation["drift"])
	require.Equal(t, "0.7", drifted.Total.Withdrawn.String())
	flowPGGet(t, h, "/validator-commission-flows?validator="+flowServeRetried, http.StatusNotFound, nil)

	// The Hub counts each validator once and keeps drift out of its totals.
	flowPGExpect(s, chain, flowPGValidator, flowPGOther, flowServeErrored, flowServeDrifted, flowServeRetried)
	var hub CommissionFlowsHub
	flowPGGet(t, s.handleCommissionFlowsHub, "/commission-flows", http.StatusOK, &hub)
	require.Equal(t, 4, hub.Validators)
	require.Equal(t, 4, hub.Traced)
	require.False(t, hub.Complete)
	require.Empty(t, hub.Months)
	require.Equal(t, map[string]int{flowStatusReconciled: 2, flowStatusDrift: 2}, hub.Statuses)
	require.Equal(t, 2, hub.Unreconciled.Validators)
	require.Equal(t, "1", hub.Unreconciled.Withdrawn.String(), "the legacy and the versioned drift results")

	// The retry collection's pinned validator completes the set.
	retried := flowPGRun(flowServeRetried, flowStatusOK, `{"2025-02":"400000"}`)
	flowServeInsert(t, s, "retry", flowServeBuild, flowServeProfile, retried, flowPGRows(flowServeRetried, [3]string{flowPGFeb, flowFees, "400000"}))
	hub = CommissionFlowsHub{}
	flowPGGet(t, s.handleCommissionFlowsHub, "/commission-flows", http.StatusOK, &hub)
	require.True(t, hub.Complete, "a newly inserted result is read, not a cached set")
	require.Equal(t, 5, hub.Traced)
	require.Len(t, hub.Months, 2)
	require.Equal(t, "1", hub.Months[0].Withdrawn.String())
	require.Equal(t, "3", hub.Months[1].Withdrawn.String(), "main 2.5, errored legacy 0.1 and retried 0.4; drift left out")
	feb := map[string]string{}
	for _, l := range hub.Months[1].Flows {
		feb[l.Class] = l.Amount.String()
	}
	require.Equal(t, map[string]string{flowHeld: "2.1", flowExchangeLike: "0.5", flowFees: "0.4"}, feb, "neither drift result's exchange line is summed")
	require.Equal(t, "4", hub.Total.Withdrawn.String())

	// Run as a role holding only the columns the API is granted: the API never
	// needs the private evidence column.
	role := fmt.Sprintf("flow_serve_api_%d", time.Now().UnixNano())
	var schema string
	require.NoError(t, s.db.Raw("SELECT current_schema()").Scan(&schema).Error)
	for _, sql := range []string{
		"CREATE ROLE " + role + " NOLOGIN",
		"GRANT USAGE ON SCHEMA " + schema + " TO " + role,
		"GRANT SELECT ON ALL TABLES IN SCHEMA " + schema + " TO " + role,
		"REVOKE SELECT ON tax_commission_flow_versions FROM " + role,
		"GRANT SELECT (version, chain, validator, build, private_profile_sha256, computed_at, run_json, rows_json) ON tax_commission_flow_versions TO " + role,
		"SET ROLE " + role,
	} {
		require.NoError(t, s.db.Exec(sql).Error, sql)
	}
	t.Cleanup(func() {
		_ = s.db.Exec("RESET ROLE").Error
		_ = s.db.Exec("DROP OWNED BY " + role).Error
		_ = s.db.Exec("DROP ROLE " + role).Error
	})
	require.Error(t, s.db.Exec("SELECT evidence_json FROM tax_commission_flow_versions").Error)
	s.flowVer = nil
	hub = CommissionFlowsHub{}
	flowPGGet(t, s.handleCommissionFlowsHub, "/commission-flows", http.StatusOK, &hub)
	require.Equal(t, "4", hub.Total.Withdrawn.String())
	got = ValidatorCommissionFlows{}
	flowPGGet(t, h, "/validator-commission-flows?validator="+flowPGValidator, http.StatusOK, &got)
	require.EqualValues(t, 9, got.Reconciliation["wallets"])

	// Before the role is granted the versioned columns, the page serves the
	// legacy runs exactly as it did before versioned results existed.
	require.NoError(t, s.db.Exec("RESET ROLE").Error)
	require.NoError(t, s.db.Exec("REVOKE SELECT (version, chain, validator, build, private_profile_sha256, computed_at, run_json, rows_json) ON tax_commission_flow_versions FROM "+role).Error)
	require.NoError(t, s.db.Exec("SET ROLE "+role).Error)
	s.flowVer = nil
	hub = CommissionFlowsHub{}
	flowPGGet(t, s.handleCommissionFlowsHub, "/commission-flows", http.StatusOK, &hub)
	require.Equal(t, 3, hub.Validators)
	require.Equal(t, map[string]int{flowStatusReconciled: 2, flowStatusDrift: 1}, hub.Statuses)
	got = ValidatorCommissionFlows{}
	flowPGGet(t, h, "/validator-commission-flows?validator="+flowPGValidator, http.StatusOK, &got)
	require.EqualValues(t, 3, got.Reconciliation["wallets"], "the legacy run")
	flowPGGet(t, h, "/validator-commission-flows?validator="+flowServeDrifted, http.StatusNotFound, nil)
	require.NoError(t, s.db.Exec("RESET ROLE").Error)
	require.NoError(t, s.db.Exec("DROP TABLE tax_commission_flow_versions").Error)
	s.flowVer = nil
	served, err = s.servedCommissionFlows(apiMainnet, "")
	require.NoError(t, err, "a database without the versioned table serves its legacy runs")
	require.Len(t, served, 3)
	require.NoError(t, s.db.Exec("CREATE VIEW tax_commission_flow_versions AS SELECT 1 AS other").Error)
	_, err = s.servedCommissionFlows(apiMainnet, "")
	require.Error(t, err, "any other read error fails the request")
}

func TestAcceptFlowVersionRules(t *testing.T) {
	src := flowServeSource("v")
	run := flowPGRun(flowPGValidator, flowStatusReconciled, "{}")
	encode := func(r CommissionFlowRun, rows []CommissionFlow) flowVersionRow {
		a, _ := json.Marshal(r)
		b, _ := json.Marshal(rows)
		return flowVersionRow{Version: "v", Chain: apiMainnet, Validator: r.Validator, Build: flowServeBuild, PrivateProfileSHA256: flowServeProfile, RunJSON: string(a), RowsJSON: string(b)}
	}
	_, ok := acceptFlowVersion(src, encode(run, []CommissionFlow{}), apiMainnet)
	require.True(t, ok)
	for _, status := range []string{flowStatusOK, flowStatusDrift} {
		r := run
		r.Status = status
		_, ok = acceptFlowVersion(src, encode(r, nil), apiMainnet)
		require.True(t, ok, status)
	}
	for _, status := range []string{flowStatusError, "", "unknown"} {
		r := run
		r.Status = status
		_, ok = acceptFlowVersion(src, encode(r, nil), apiMainnet)
		require.False(t, ok, status)
	}
	_, ok = acceptFlowVersion(src, encode(run, flowPGRows(flowPGOther, [3]string{flowPGJan, flowHeld, "1"})), apiMainnet)
	require.False(t, ok, "rows stored for another validator")
	_, ok = acceptFlowVersion(src, encode(run, flowPGRows(flowPGValidator, [3]string{flowPGJan, flowHeld, "x"})), apiMainnet)
	require.False(t, ok, "an amount that is not a number")
	row := encode(run, nil)
	row.RowsJSON = `"cosmos-commission-payload/v1:rows:abc"`
	_, ok = acceptFlowVersion(src, row, apiMainnet)
	require.False(t, ok, "chunked rows")
	r := run
	r.FromDay = time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC)
	_, ok = acceptFlowVersion(src, encode(r, nil), apiMainnet)
	require.False(t, ok, "another window")
	_, ok = acceptFlowVersion(src, encode(run, nil), apiTestnet)
	require.False(t, ok, "another chain")
}

func TestDefaultCommissionServingSources(t *testing.T) {
	// The giant reruns share a version across several pinned roots, so a
	// source is its version plus its root.
	seen := map[string]bool{}
	for _, src := range defaultCommissionServingSources[apiMainnet] {
		key := src.Version + "/" + src.Root
		require.False(t, seen[key], key)
		seen[key] = true
		require.Equal(t, flowCorrectedFrom, src.FromDay)
		require.Equal(t, flowCorrectedAsOf, src.AsOf)
		require.NotEmpty(t, src.Build)
		require.NotEmpty(t, src.Profile)
	}
	require.Len(t, seen, 75)
	require.Empty(t, defaultCommissionServingSources[apiTestnet])
}

// Each retry and giant entry serves only its one pinned root, and no root is
// pinned by two of them: later-s02 has two deadline retries (v2 and v3),
// each under its own version.
func TestCommissionServingRetriesPinOneRootEach(t *testing.T) {
	roots := map[string]string{}
	for _, src := range defaultCommissionServingSources[apiMainnet] {
		if !strings.Contains(src.Version, "-retry-") && src.Build != flowGiantBuild && src.Build != flowWithdrawFixBuild {
			require.Empty(t, src.Root, src.Version)
			continue
		}
		require.Len(t, src.Root, 64, src.Version)
		// A withdraw-address rerun outranks the retry it replaces, which
		// stays pinned as the fallback.
		if prev, ok := roots[src.Root]; ok {
			require.Contains(t, prev, "accel-withdraw-fix-", src.Version)
			require.NotContains(t, src.Version, "accel-withdraw-fix-", src.Version)
			continue
		}
		roots[src.Root] = src.Version
	}
	require.Len(t, roots, 54)
	require.Equal(t, flowCorrectedPrefix+"accel-later-s02-20261115-deadline-retry-v2", roots["2d473f4858d9987775254ce4bff8939f812bfefeb04eb1e6f0a5ebde72969fd8"])
	require.Equal(t, flowCorrectedPrefix+"accel-later-s02-20261115-deadline-retry-v3", roots["50c56a74de37966c61278d7381538fe0d8fa3746d87eadae5d633a471f017de1"])
	require.Equal(t, flowCorrectedPrefix+"accel-four-s05-20261115-deadline-retry-v3", roots["5187c6ad32a6209ebd2f8b36e2f78b95950a96e046c474db04ca64b8c981f885"])
	require.Equal(t, flowCorrectedPrefix+"accel-four-s04-20261115-deadline-retry-v3", roots["6e79e2a8e3e5b77e7d96bc45c28592f36610c3992d54bb6e828ad44e375f1302"])
	// The Cosmos Hub v29.1.0 upgrade stopped the earlier retries of these roots with stored errors, so the next version of each replaces it.
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-ab12977c1a21-20261118-fanout-v1", roots["ab12977c1a2135f7aeea69534171ee3c2bec9a03f3650cbf5dff76caefb19345"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-0a11a4c00904-20261118-fanout-v1", roots["0a11a4c0090436746a12555ec8f07a503c467e849bccf9ef550a8e2985f2b5ab"])
	require.Equal(t, flowCorrectedPrefix+"accel-four-s03-20261115-deadline-retry-v3", roots["290eefd100eb62044e1da01618881c70ac9aa783672b9547415766c16bcf06ff"])
	require.Equal(t, flowLSMPrefix+"accel-four-s01-20261115-lsm-retry-v1", roots["bc0089661f770388eeb5acd70a947bbc9c47ab0808b6fda90df95ff57800f15a"])
	require.Equal(t, flowLSMPrefix+"accel-four-s06-20261115-lsm-retry-v1", roots["85f6d8206eebe56d592d5c82aa2978a04e7edf1ad6fcfb8165e6b15020298fd6"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-52836aab8ac6-20261118-fanout-v1", roots["52836aab8ac6250910ea4e77539c7ef6ef8fa98050b057271b80919434a13e42"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-d2d7b3c161a2-20261118-fanout-v1", roots["d2d7b3c161a280621a0b05189e3746f1d785eab94cc4de6096a76a621c80d2b5"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-001308de5051-20261118-fanout-v1", roots["001308de505138aa91ea460b148382f1dd7b6528a4a307e6333dff716f08a7ae"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-a5b85d99e09a-20261118-fanout-v1", roots["a5b85d99e09a1b9844bbb4209119b7b1d730b83b37c8e45bae11e21c09428965"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-06d2a6fb6513-20261118-fanout-v1", roots["06d2a6fb65137dc2b6730a227548da0ab3d738cc6bf2a561c68ed9154c293c1b"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-32608eb7f728-20261118-fanout-v1", roots["32608eb7f7286cab627246cc922e7b874857acf5a5dce90ebff5d5623f184eb2"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-6381eb3d4d63-20261118-fanout-v1", roots["6381eb3d4d63ccfe34abfde32b00c5692b94fb1081ba8628fb55a32d9b822201"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-7745d8ffeac6-20261118-fanout-v1", roots["7745d8ffeac67f746b31f6cbdc249829bb77eb1708c12f798a9dd9bb45ba9ce1"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-17fa80ffb90c-20261118-fanout-v1", roots["17fa80ffb90c55459967e90d18555042a950e8b95b05f19ff2f922da458baa7e"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-2453c25e0fd9-20261118-fanout-v1", roots["2453c25e0fd99d77519b1f1ddc9805b827da1c1df87bc099d511a3626065633a"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-f54f5e725ac7-20261118-fanout-v1", roots["f54f5e725ac75b7c1be4fcaee9d2c5ac24fb41b08fb7f8838dca9fb79580ee74"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-a3e91bffa7c0-20261118-fanout-v1", roots["a3e91bffa7c05edeb6836f0d37d7bd8d8ed18788e8dd4af9b806caa5d7b2de71"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-d2c321cc8c4e-20261118-fanout-v1", roots["d2c321cc8c4ec629077ecc8e006f372bc361c918ef0fcb540372fd347746cbcc"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-8df708d2db2f-20261118-fanout-v1", roots["8df708d2db2f5583431b8f9ce8a6e3ba9ae7840e0f78b38366948a69c0d24716"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-1e6691e3985a-20261118-fanout-v1", roots["1e6691e3985a8f008990dbebf047b578651e022a4f608e7f48b939f571d4e3ab"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-fda70bc2ca4c-20261118-fanout-v1", roots["fda70bc2ca4ce5311f719f17a2e26c8339c2e727ce3ad9309ecf27f8c039e02b"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-7f2f32dfb1d5-20261118-fanout-v1", roots["7f2f32dfb1d5d391d70b85d967e90ae248d7aa4dd109656f633a893c98febea6"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-75f92cb68a7f-20261118-fanout-v1", roots["75f92cb68a7f639c979be19a1d7ca4b7ce6ad63a2341562f3c966f82c7ab920d"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-f467f62240db-20261118-fanout-v1", roots["f467f62240db0faad0c3394d0f0be34ec1ed8a1310d287c04cd46b648525b571"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-007bd61b16ac-20261118-fanout-v1", roots["007bd61b16ac3792987956d92c01c689478dac00d39979d0e8acb010a07c50b3"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-c07d4e7c6b6e-20261118-fanout-v2", roots["c07d4e7c6b6e288cc7c23111d04eca5c50872c5b58fdb9b5f2ec67ae2093cdad"])
}

// The LSM reward retries bind the reward-free corrections manifest: the
// version names its digest and the profile is the base profile combined with
// it, never the six-reward manifest the corrected shards used.
func TestCommissionServingLSMRetriesBindRewardFreeManifest(t *testing.T) {
	const base = "53da2904e3a6893e92745b1db9766b356da09042f52ec5384d0beb11a46aa913"
	combined := func(corrections string) string {
		b, _ := json.Marshal([]string{base, corrections})
		h := sha256.Sum256(b)
		return hex.EncodeToString(h[:])
	}
	require.Equal(t, flowCorrectedProfile, combined("ade534f8e5f78bd9dc9c52945d0116059e36496d3a4394f09143a3670c659f6b"))
	require.Equal(t, flowLSMProfile, combined("40d6961721383adb2aa8bf4ad19ac9b8df7f834a3687dd1cd63a144bef654eb4"))
	require.Equal(t, "ops818-page82a-profile"+base[:12]+"-sources40d696172138-", flowLSMPrefix)
	lsm := 0
	for _, src := range defaultCommissionServingSources[apiMainnet] {
		if !strings.HasPrefix(src.Version, flowLSMPrefix) {
			require.NotEqual(t, flowLSMProfile, src.Profile, src.Version)
			continue
		}
		lsm++
		require.Equal(t, flowLSMBuild, src.Build, src.Version)
		require.Equal(t, flowLSMProfile, src.Profile, src.Version)
		require.Len(t, src.Root, 64, src.Version)
	}
	require.Equal(t, 2, lsm)
}
