package taxapi

import (
	"bytes"
	"compress/gzip"
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
	require.Len(t, seen, 245)
	require.Empty(t, defaultCommissionServingSources[apiTestnet])
}

// Each retry and giant entry serves only its one pinned root, and no root is
// pinned by two of them: later-s02 has two deadline retries (v2 and v3),
// each under its own version.
func TestCommissionServingRetriesPinOneRootEach(t *testing.T) {
	roots := map[string]string{}
	for _, src := range defaultCommissionServingSources[apiMainnet] {
		if !strings.Contains(src.Version, "-retry-") && src.Build != flowGiantBuild && src.Build != flowWithdrawFixBuild && src.Build != flowSlashBuild && src.Build != flowClientTraceBuild && src.Build != flowSelfBondBuild {
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
	require.Len(t, roots, 212)
	require.Equal(t, flowCorrectedPrefix+"accel-later-s02-20261115-deadline-retry-v3", roots["50c56a74de37966c61278d7381538fe0d8fa3746d87eadae5d633a471f017de1"])
	require.Equal(t, flowCorrectedPrefix+"accel-four-s04-20261115-deadline-retry-v3", roots["6e79e2a8e3e5b77e7d96bc45c28592f36610c3992d54bb6e828ad44e375f1302"])
	// The Cosmos Hub v29.1.0 upgrade stopped the earlier retries of these roots with stored errors, so the next version of each replaces it.
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-ab12977c1a21-20261118-fanout-v3", roots["ab12977c1a2135f7aeea69534171ee3c2bec9a03f3650cbf5dff76caefb19345"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-0a11a4c00904-20261118-fanout-v3", roots["0a11a4c0090436746a12555ec8f07a503c467e849bccf9ef550a8e2985f2b5ab"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-290eefd100eb-20261118-fanout-v1", roots["290eefd100eb62044e1da01618881c70ac9aa783672b9547415766c16bcf06ff"])
	require.Equal(t, flowLSMPrefix+"accel-withdraw-fix-bc0089661f77-20261118-fanout-v4", roots["bc0089661f770388eeb5acd70a947bbc9c47ab0808b6fda90df95ff57800f15a"])
	require.Equal(t, flowLSMPrefix+"accel-four-s06-20261115-lsm-retry-v1", roots["85f6d8206eebe56d592d5c82aa2978a04e7edf1ad6fcfb8165e6b15020298fd6"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-52836aab8ac6-20261118-fanout-v3", roots["52836aab8ac6250910ea4e77539c7ef6ef8fa98050b057271b80919434a13e42"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-d2d7b3c161a2-20261118-fanout-v3", roots["d2d7b3c161a280621a0b05189e3746f1d785eab94cc4de6096a76a621c80d2b5"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-001308de5051-20261118-fanout-v3", roots["001308de505138aa91ea460b148382f1dd7b6528a4a307e6333dff716f08a7ae"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-a5b85d99e09a-20261118-fanout-v3", roots["a5b85d99e09a1b9844bbb4209119b7b1d730b83b37c8e45bae11e21c09428965"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-06d2a6fb6513-20261118-fanout-v1", roots["06d2a6fb65137dc2b6730a227548da0ab3d738cc6bf2a561c68ed9154c293c1b"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-32608eb7f728-20261118-fanout-v3", roots["32608eb7f7286cab627246cc922e7b874857acf5a5dce90ebff5d5623f184eb2"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-0d96280b5e9a-20261118-fanout-v3", roots["0d96280b5e9a7d7e720633d37a252722db055189337b778f1ae5d805a80eab84"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-1456c0b64ef4-20261118-fanout-v1", roots["1456c0b64ef40bd1073983d272a32eb28333805b6470a6903d4f776ec2eeb9fc"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-6381eb3d4d63-20261118-fanout-v3", roots["6381eb3d4d63ccfe34abfde32b00c5692b94fb1081ba8628fb55a32d9b822201"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-7745d8ffeac6-20261118-fanout-v3", roots["7745d8ffeac67f746b31f6cbdc249829bb77eb1708c12f798a9dd9bb45ba9ce1"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-17fa80ffb90c-20261118-fanout-v3", roots["17fa80ffb90c55459967e90d18555042a950e8b95b05f19ff2f922da458baa7e"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-2453c25e0fd9-20261118-fanout-v3", roots["2453c25e0fd99d77519b1f1ddc9805b827da1c1df87bc099d511a3626065633a"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-b6de900ea24b-20261118-fanout-v1", roots["b6de900ea24b3ed88a9db14daf000adb1a2f52334f946166f6ad096c77c68548"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-6fb852133855-20261118-fanout-v3", roots["6fb8521338557f53f633e24360bb5b102079730968e7b81d2f58fbfbb3b54cb0"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-f1afab0130fb-20261118-fanout-v3", roots["f1afab0130fb05642a485af36968c826238e912292f4616ac0658957fcfb28d1"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-f54f5e725ac7-20261118-fanout-v3", roots["f54f5e725ac75b7c1be4fcaee9d2c5ac24fb41b08fb7f8838dca9fb79580ee74"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-a3e91bffa7c0-20261118-fanout-v3", roots["a3e91bffa7c05edeb6836f0d37d7bd8d8ed18788e8dd4af9b806caa5d7b2de71"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-d2c321cc8c4e-20261118-fanout-v3", roots["d2c321cc8c4ec629077ecc8e006f372bc361c918ef0fcb540372fd347746cbcc"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-8df708d2db2f-20261118-fanout-v3", roots["8df708d2db2f5583431b8f9ce8a6e3ba9ae7840e0f78b38366948a69c0d24716"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-1e6691e3985a-20261118-fanout-v5", roots["1e6691e3985a8f008990dbebf047b578651e022a4f608e7f48b939f571d4e3ab"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-fda70bc2ca4c-20261118-fanout-v3", roots["fda70bc2ca4ce5311f719f17a2e26c8339c2e727ce3ad9309ecf27f8c039e02b"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-7f2f32dfb1d5-20261118-fanout-v3", roots["7f2f32dfb1d5d391d70b85d967e90ae248d7aa4dd109656f633a893c98febea6"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-75f92cb68a7f-20261118-fanout-v3", roots["75f92cb68a7f639c979be19a1d7ca4b7ce6ad63a2341562f3c966f82c7ab920d"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-f467f62240db-20261118-fanout-v3", roots["f467f62240db0faad0c3394d0f0be34ec1ed8a1310d287c04cd46b648525b571"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-b970df6089e6-20261118-fanout-v4", roots["b970df6089e634f8cd9c6b5a4fff54656cb34cbb9d0cf568611e963d313dccdb"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-bf9b46882519-20261118-fanout-v3", roots["bf9b46882519b68cecbc4399741d4c85064fcbfa3fd555333dda5525f12f0a93"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-007bd61b16ac-20261118-fanout-v1", roots["007bd61b16ac3792987956d92c01c689478dac00d39979d0e8acb010a07c50b3"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-0da7be31faa8-20261118-fanout-v3", roots["0da7be31faa899b2a6ed0dcfb7fcdec0b2811c102eebbfc106027bab2f54d46c"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-0a69bace753e-20261118-fanout-v3", roots["0a69bace753e236747f228b9b9cd1bf54e7115f8bee6015e666002b4c46c9e84"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-a3c4f7ccef86-20261118-fanout-v3", roots["a3c4f7ccef864eaee6395a776fdf14b7c0b755ed21f49e9c89cfda0447dba7a1"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-65707e97ab7c-20261118-fanout-v3", roots["65707e97ab7c15f24279c6b0c181bcebddc72e7f8580bad4cbfcf189dbea9d7e"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-09450e35c400-20261118-fanout-v1", roots["09450e35c4006cbfbd842e08cc4791a2cdbab96a88654777cc9a040297d80c9b"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-84ac2be58662-20261118-fanout-v1", roots["84ac2be58662525df11258ecf1a826044fdaeb1f82b0a4e3e85a806f409a3fa0"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-00a65ed900f9-20261118-fanout-v1", roots["00a65ed900f97cecf840ff2051c08e7a1c384fe07f648ce2099f425b9c579fdf"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-728154820784-20261118-fanout-v1", roots["7281548207848f0d0f7de142b10be3830a4be0450cb51603ef323d5fa90d621f"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-f8bad4552c9c-20261118-fanout-v1", roots["f8bad4552c9c01b57663e31de2536595c1a5f36d4f818810c99b7579a0442d34"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-529022bbcad4-20261118-fanout-v1", roots["529022bbcad4b4fda219945769eaa658cbc9be549857319d1893a7282245bc00"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-6adc774f56d4-20261118-fanout-v1", roots["6adc774f56d4677f42461e9b5a72107ec9b4acb4d7921aa979aebe634b456c48"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-6e0c83d2796a-20261118-fanout-v1", roots["6e0c83d2796a5a3e5401fcf4e6d7dbc85c637bd803ffa4d6e38b0200a23c48d1"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-2b251978865c-20261118-fanout-v1", roots["2b251978865c4ae3262c2e1c1bd2232fb3c1a6a32ae2aee649daebb5ae99fe45"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-334894d36534-20261118-fanout-v1", roots["334894d36534f55b36282d9dcd822d719c62db3e3d9af320a27ec4ffaa8bb5a7"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-53857068a2c7-20261118-fanout-v1", roots["53857068a2c766dfaeb9149bbef2f02eebcac0f091d4dd326a03b90cf73a3bd3"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-583d96ba1bba-20261118-fanout-v1", roots["583d96ba1bba3be225003af20b7cbad0899e6e8b98c12857d2dfbb2df7cdadf5"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-8a3aea50be11-20261118-fanout-v1", roots["8a3aea50be11ad5d460bc6e832b2ea5eb6e0774f75b9e3dda984158ce01b35c6"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-ce871c7c6e35-20261118-fanout-v1", roots["ce871c7c6e35d0f1629d9c44ffe8500872e58f6bca1e188223fe4e4835fdb062"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-9514e5e20dd8-20261118-fanout-v1", roots["9514e5e20dd865faef28bb1ea76698d30acfbe03b81e365a1b55a25114e707a4"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-a106a4ef7b92-20261118-fanout-v1", roots["a106a4ef7b922fd9efd0d754bd7b354e997a647e814d2eabed9f920aa520af23"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-55b998caa294-20261118-fanout-v1", roots["55b998caa2945907f1b1177130043fd61b382dc3de466e51aacb22f058164509"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-06c9061fd494-20261118-fanout-v1", roots["06c9061fd4948af545ba753349029fc8c6a5de2dc1d2f725ca4531058177f714"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-0f53f6fc13d9-20261118-fanout-v5", roots["0f53f6fc13d9f7022c8ee7a0aa661c0440ea6027ff4e934e7952601cfb526e2c"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-162c315eab1a-20261118-fanout-v1", roots["162c315eab1a4d9544efe0fc8a5b3ad847f1381a728a99913751921ce365c019"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-28197c71f63d-20261118-fanout-v1", roots["28197c71f63d1583b31d7cc881b76841d4474815168c3e63e53ab527bb25b785"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-70bb65c3bc87-20261118-fanout-v1", roots["70bb65c3bc87785d35081cbd0435a18962ef4eac525d0b54a0580907dc509737"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-8b22b9cdcee2-20261118-fanout-v1", roots["8b22b9cdcee26d056002e8efb2fcfad7a77c828f7b3a67661af9bbb6c14886e6"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-8f1c190dc60a-20261118-fanout-v1", roots["8f1c190dc60aa0c8acbfe877bd5dbeb274e68258b452908cc76eedd5adc4435c"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-940ef932a5d6-20261118-fanout-v1", roots["940ef932a5d68cf9104dfd219b2230285fb3b1f06721b9fd7fb2283a8500b03d"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-a21b65a9b88e-20261118-fanout-v1", roots["a21b65a9b88ec88fd0ad5015fe2907e2d89971bad4fe92d3808889b807198e1a"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-b384bc959cad-20261118-fanout-v1", roots["b384bc959cad3296e74490e5fc7b9ad7e93e34593658e3276949d0db16096ecb"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-b5701732e313-20261118-fanout-v1", roots["b5701732e3137a2d9e86e4bde847fcbb6329c98481658c574c145f1a3811fb03"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-d4c6ccac7396-20261118-fanout-v1", roots["d4c6ccac73962d84cd3d4cec2c70851d0ea02940ce0436882450532c2df4f9e6"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-f009da4d5b44-20261118-fanout-v1", roots["f009da4d5b4452af83e81335ad2d4a2db70d9d7f8d226b30f76e4c8b5bcf5da8"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-f76e659775a6-20261118-fanout-v1", roots["f76e659775a647b387ed8fc2c4f49bf63e6f5d1c4756e3391863764cf6fb01c9"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-139a197e4cda-20261118-fanout-v1", roots["139a197e4cdaab48661383f71730eb1d481d25daf8906c311e7ad0294dc61649"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-39722f592341-20261118-fanout-v1", roots["39722f59234103dc29a8caaf03d6c1622b6954fb01f6b083bcd9106b2d7249f4"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-7f2e4c9d47f3-20261118-fanout-v1", roots["7f2e4c9d47f315d654fc6fa6a53fa0d908a5cf4ac432d5bee73708508e28ad8f"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-317097d52dd7-20261118-fanout-v1", roots["317097d52dd71d08f60d71a3622af4d4a442ebd012cd4e99d98367746de5b17d"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-d5f91bf8d7b4-20261118-fanout-v1", roots["d5f91bf8d7b41fe125a39816d950a6d840bc9cc52b412eef07bee698f0b9e182"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-383d9fd329b2-20261118-fanout-v1", roots["383d9fd329b21ef289a8bc584e465325e26025d5b1dd35b74811bf1d831e4afe"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-d16f03fef762-20261118-fanout-v1", roots["d16f03fef76269e52ea1c39dc50847ccc561d1e89782366372bc13f936a0c525"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-e19812c884e7-20261118-fanout-v1", roots["e19812c884e754f7c64a4cc29e18fd0af0a0f06dd717d82edaa3800e4185b4a8"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-e7790b332148-20261118-fanout-v1", roots["e7790b332148a7573e8bad81ee2a5242f491435aec72cdc72eaee7b25c298565"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-8a571bb0887f-20261118-fanout-v1", roots["8a571bb0887f0667fc25cac41c2d25c7d2c27a636f639d51f7f102b62969b172"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-ad49f408bd75-20261118-fanout-v1", roots["ad49f408bd756cae1952d8f85f04d4210a75a1b20797e03412fd8b95a23f4938"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-1f92328f06d6-20261118-fanout-v1", roots["1f92328f06d661012c9b0196cac76bb8168b1370009fc5ffe934dd63659d0b61"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-3e3481ef24f7-20261118-fanout-v5", roots["3e3481ef24f791529e1ff1bf325d48d3ecccf38248bc9ef24ca623890e0d4b45"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-a149b3077b86-20261118-fanout-v1", roots["a149b3077b86222dfc773827c1c37a42c4755339a0c1520d344aa4615385cd38"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-5fa7cab34d20-20261118-fanout-v1", roots["5fa7cab34d20f2a77714dae2730dc0286ed77207a35099cf6d8128b08f375af1"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-b3bcdd688853-20261118-fanout-v5", roots["b3bcdd6888530414e97414c962742b3c4d8b56b02e6d80b152dafa9e5915a5d7"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-4fcff5fcb5a5-20261118-fanout-v5", roots["4fcff5fcb5a55ce3e56d6c89bd59739729a349559f0374be1ccb0b825e36747c"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-d3bfac7ecf84-20261118-fanout-v1", roots["d3bfac7ecf84f17408ba216adde4b3c6969adad03e14dacf0290d287d107fea5"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-95ceecf77830-20261118-fanout-v4", roots["95ceecf77830b8642cd1e669655f504c4d990eb528552e5b3a272c091b78428a"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-b03cdf1c7313-20261118-fanout-v1", roots["b03cdf1c7313b261dcf5e40862ed7ff148b7654aa5d8bb244d21e8d1beef365b"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-8837e708a0bd-20261118-fanout-v1", roots["8837e708a0bd1c91f7d7267df9de922b07b297a01abd041f07d5b6ce797858ae"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-e448dfbf05be-20261118-fanout-v4", roots["e448dfbf05be26541b5fa7bcea3d85dcac6e5356759d89dadf4f9bd3c8b182d1"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-4643e858df25-20261118-fanout-v1", roots["4643e858df25fd68d90a19ca3eb4f2a0a9c8c6cb604f2e5c92c284d5af480d62"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-2d473f4858d9-20261118-fanout-v4", roots["2d473f4858d9987775254ce4bff8939f812bfefeb04eb1e6f0a5ebde72969fd8"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-021b3d76a3a7-20261118-fanout-v1", roots["021b3d76a3a7b812b345102cb2856b74e57e6b5dae2ddd2eb53df30f8e550b0d"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-928644401359-20261118-fanout-v1", roots["9286444013590806985c200f2860b383a1aea12df40a1d155a8b2be7a75da37f"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-65858093b487-20261118-fanout-v1", roots["65858093b487eb21286e785487ff7bafdda9a0effdbd492296a3ec88adac99a7"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-ea3690afb084-20261118-fanout-v1", roots["ea3690afb084f65e0da1d03c2f8f831d8be946809c3c09422b81afd76d9427a0"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-587d6d7bc979-20261118-fanout-v1", roots["587d6d7bc97969380199e8f9038208fc8611da31a6e141415780e8c2604b34bf"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-519012b501e4-20261118-fanout-v1", roots["519012b501e410adc9cb826c027efedc7fd0ccd9b5e89d27f2c3712dabc38ed3"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-9145229fa3e1-20261118-fanout-v1", roots["9145229fa3e16e5795ff15a828cc8daf130219f4bca59de0f63f412cc5f6505b"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-d712be52b0fb-20261118-fanout-v1", roots["d712be52b0fbdb6fcef53d5596b729b89e70e0d40afbff5d01f4cbe64d7631f4"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-67c1c59d6b78-20261118-fanout-v1", roots["67c1c59d6b78f4b099a5ed5d164e4158f99c8b57b99c16463845bb6d79a69316"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-faac294cdafc-20261118-fanout-v1", roots["faac294cdafc8860e4e0adfce65714b0100ecbfce8c8af9e14329ca9fa415219"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-5187c6ad32a6-20261118-fanout-v1", roots["5187c6ad32a6209ebd2f8b36e2f78b95950a96e046c474db04ca64b8c981f885"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-1663fc05d7e2-20261118-fanout-v1", roots["1663fc05d7e2e2427b2063fdb264eda51e956e65e869efaa0a2f01c09971a12d"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-0af248dd5028-20261118-fanout-v1", roots["0af248dd5028935d07fa57bc8e3c5a88779c2119a479ea67fed440773020ca7b"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-7b7d4777c730-20261118-fanout-v1", roots["7b7d4777c730c08531bd3809dfec240e8d317e7f73ff324ea09713901a95b150"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-256067afb286-20261118-fanout-v1", roots["256067afb286cf77cbcd49c186eede037db3eb840d31dbe8d56b6fc6fc4faaa6"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-6ef922ee6733-20261118-fanout-v1", roots["6ef922ee6733c1be94bb847fe1e62b889d94aed3fa33e8fe2a685a4ff08b6976"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-91442d368a45-20261118-fanout-v1", roots["91442d368a45f15d2e40ce0b0a990d388e45a8fb46490d28877d52f5260af21c"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-f7e8484fc529-20261118-fanout-v1", roots["f7e8484fc5297478aa1f1746f02448ceff54d07dab8341ec4dde189ee4044b26"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-3c4cbfc796a3-20261118-fanout-v1", roots["3c4cbfc796a3df578d91831a756af2097d9c3d09e2f26695df1f483eff8c87d0"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-3e8353a25770-20261118-fanout-v1", roots["3e8353a2577021f282d5233fa3ad024dd98f6e12d14e038e9d50f3daca85eca1"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-c094ace10c53-20261118-fanout-v1", roots["c094ace10c53d5e89062832b81b99265a5b884024b90f47fae9bfd05611f7901"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-a54eb3d66496-20261118-fanout-v1", roots["a54eb3d66496370f607e64ffb3b1ff1ea939901a626300de624373ab8b18c9b9"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-e0c9d081b175-20261118-fanout-v1", roots["e0c9d081b175113666052f96fcaae1f38b803abc3fdcd7618fd05cc1f13e7210"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-debf605ff680-20261118-fanout-v1", roots["debf605ff68022a7b96ba4f05f55025383b8d76f7198338d672c30b406a10685"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-359f0bf28de5-20261118-fanout-v1", roots["359f0bf28de54aea4eb43e612f91fd001b794c1e292e0506fecfc1eed6ebcdfb"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-51535f062e5b-20261118-fanout-v1", roots["51535f062e5bebbc70e87b5e170ea1485c5fa5602e1c1728b3e2e42f4f84cdc9"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-5cc8184acf42-20261118-fanout-v5", roots["5cc8184acf429429e5835191771de09f571f5ec8249905ca60ae6ea2affdf673"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-cb4a1161be29-20261118-fanout-v1", roots["cb4a1161be29b103e59b070f3bae6136327428f746a3409d1dc249d623533a53"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-a52384ba0f34-20261118-fanout-v5", roots["a52384ba0f34c8c6df7d911bd8673ed75e44af0d41e4dffbec3358f03b015ae8"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-48847a216b03-20261118-fanout-v1", roots["48847a216b03b20746514b1858d1f3c6733d4803bfba939e550af7170765d214"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-0acf3614937b-20261118-fanout-v1", roots["0acf3614937b874374e73143530c23d6c2057a5840f255ea7bc0f1186bd39b46"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-d73f8ec0ba97-20261118-fanout-v1", roots["d73f8ec0ba9793004860382e2b2bb58eda4db08a36d728b2a8756776f3678815"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-2f69743a8941-20261118-fanout-v1", roots["2f69743a8941faee5918fcbdf46ea6057a376d1902856bb094f29c411aaca3b5"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-bf186106324a-20261118-fanout-v1", roots["bf186106324a43d5f5f1a50226587f721047064e54ec1730546bdf40701061de"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-0557c428ef4c-20261118-fanout-v1", roots["0557c428ef4cac55c6a9f6487054ccf620dbb6dec10c3627b9124a3b3b8e31ad"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-802b450f0236-20261118-fanout-v1", roots["802b450f0236afca7078a0ce916d993557ed8d48131e801f7262c02da500a807"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-892530d89fce-20261118-fanout-v1", roots["892530d89fce820e8ce6005046a8a93340487899500d6f795c82d0080d11d3ff"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-e9dd7b1d8994-20261118-fanout-v5", roots["e9dd7b1d8994732d3ddf71250574ced87a7287b9032f76c9b6f5cfdcccc093e4"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-3dd844297fbd-20261118-fanout-v1", roots["3dd844297fbda1ffe59720f953e2168064237e3436b5d3a81936d275496ffecf"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-28c5d00f4530-20261118-fanout-v1", roots["28c5d00f45307b23be175cab0095c64c8ca2dd806e6895fe1cee42456948213e"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-2877ad667da5-20261118-fanout-v5", roots["2877ad667da5260e6188746799121924cb3fd1e13043450dad6192ec7387803a"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-651b835c3d53-20261118-fanout-v5", roots["651b835c3d5335dc4604c3e1ba8c43cf83b245160bd10a3e7d69d13f8f530a4e"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-1d5ce6cbe006-20261118-fanout-v1", roots["1d5ce6cbe00625e175aa953b41ae4efdab565c8bbdc0b65b3f4345653eac1172"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-274a1c06a584-20261118-fanout-v1", roots["274a1c06a584325f3d85da4a602930b04d10455e39edbd665ccffad5e1ddf7d9"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-47d139c1b3d1-20261118-fanout-v1", roots["47d139c1b3d180e45dec10d7511dbca4904142992c0bdb566cf4c92ae7997bec"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-607606f87da1-20261118-fanout-v1", roots["607606f87da17ce8a8581cda77daff29c9c325a011b63fafc4a30fbf29efaff5"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-b78785cb8f1b-20261118-fanout-v1", roots["b78785cb8f1bde3c896fb6a5c713955523ebe9f1420f911bd4b37fa793a69d23"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-7a0116649b34-20261118-fanout-v1", roots["7a0116649b342e025886d35634fad63ab8f11e2d33d0cc9c92e759c5f600fcce"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-173328c08451-20261118-fanout-v1", roots["173328c08451bcf4bacd8e374fd2d9a8efda54dafe9134ba3de9b16d74a9e251"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-dfca246c1569-20261118-fanout-v1", roots["dfca246c15690ca8f7a99d9263027ee7ab4e784cad9829d8dfe493f7997f1d3a"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-ae014b919dea-20261118-fanout-v1", roots["ae014b919dea62407563de2aba68a2eb8b267fb41d6d1ccc293833c93823e4bf"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-1612d799cdf1-20261118-fanout-v5", roots["1612d799cdf188ad113cc346bb3174ee8f415271053c3b1086cc30359a082a7c"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-0f096f7c01aa-20261118-fanout-v1", roots["0f096f7c01aae3024742d20c10a4c26ef0cd5b2755899cc9141597b7cac034aa"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-e1d7823dd0ed-20261118-fanout-v1", roots["e1d7823dd0eda9921631776af4458732e0b07b3aa3717e15610016f9fcb2f79f"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-9a1fedaa6b14-20261118-fanout-v1", roots["9a1fedaa6b144abab91593ac4ef48c4d435a6fa8b1370a1cb3a54f900406f3bc"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-830507242368-20261118-fanout-v5", roots["8305072423682cbd2812b2013f4088c5f69f0f1b8d18729fb6228b5b94befea3"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-79598717dab2-20261118-fanout-v1", roots["79598717dab2dc077ccea31a7bfcf578ca4df8b6d9bca33ec65b78c2b4a8188a"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-27e4221ef4d7-20261118-fanout-v1", roots["27e4221ef4d71fb4d51fa184eb6baf685ae752a273d0a7db06ed88bebd13d30b"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-3ab9af3f4ebc-20261118-fanout-v5", roots["3ab9af3f4ebc470c2ccb8bc44cd70d4dc7268bb4d049a35b05a741ab3ca801da"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-51f876af2e17-20261118-fanout-v1", roots["51f876af2e17fdcc8a516c12deadfd0b2e4a3c34d17430d5a6746ad43c477475"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-723a0b90d78c-20261118-fanout-v1", roots["723a0b90d78c1daefca133d782c5a2fbaf607816b2b55e4a188847a3eff9dbfe"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-db8c046a7cfd-20261118-fanout-v1", roots["db8c046a7cfd1cddb83f24e7c079bcf59c6277652ccaacf7e7b725344a8a2965"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-fb8abebe41cd-20261118-fanout-v1", roots["fb8abebe41cd055ba362fea92c683113137236ab7a8851ff99f194ec282ea6c4"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-9f5f36d33ae6-20261118-fanout-v1", roots["9f5f36d33ae6fe5b0955445a9f4ceb731e5bf033cda7f5d7a724e4ddbacab6e2"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-0c5d99323e2f-20261118-fanout-v1", roots["0c5d99323e2f9b31532e2b87cede615977d14251daf1fa3fe9c0cc28ad07ccbe"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-a3f0ad366e6b-20261118-fanout-v1", roots["a3f0ad366e6b3c47f9d51d95d1c744418ecbbac84392e8e0ee6a1670765117c5"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-75309bb748ad-20261118-fanout-v1", roots["75309bb748add122f8eec531c7705d93cf05d6433154e71080ea631cd956cb66"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-81b068d2e7d5-20261118-fanout-v5", roots["81b068d2e7d5c5f0409a97eb2e324e6fc0ff1f8c89319907786aaf1905f9676d"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-fcea1968b7af-20261118-fanout-v1", roots["fcea1968b7af091e011302f4924bd95858b19dd48001e6cf02fd700e691f5bcb"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-8af885ef7203-20261118-fanout-v1", roots["8af885ef7203f0e9e32249b1580b75c12374417434c72ca643d534a8a64f7570"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-384aec417e3f-20261118-fanout-v1", roots["384aec417e3f0ccef95dedd851776c7d6359e9d2513920db335acea154e90bf0"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-4ca2882bbbbd-20261118-fanout-v1", roots["4ca2882bbbbd6d398efe095d7c7eb68dd99d65016af11b28003e168678e30984"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-5f19c41a54c9-20261118-fanout-v1", roots["5f19c41a54c914ba7052c37689b2280c7f3351bab3e96cf8023f8e52fe1c6ec1"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-b292507579a2-20261118-fanout-v1", roots["b292507579a2bd55aaf3ef483580bd89fb4a5996ad323ce485bb114af9916f95"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-d0cea6448a0f-20261118-fanout-v1", roots["d0cea6448a0fc8fcf57f93d8296e1d3f7ec0a24a7572a1ce0c0434caed7e0946"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-656214dd1c96-20261118-fanout-v1", roots["656214dd1c96ff4c1193d35707abbd667cc0053eda018134a3d531dff48291eb"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-0c019c02d744-20261118-fanout-v1", roots["0c019c02d744f8202ad5771e5c22e143caaeb432a0cc81c62d81a18ae78a93de"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-bb7050112168-20261118-fanout-v1", roots["bb7050112168b67a4ae2a62d5db9f0f27bfa83c0772458919caeedef5d0ea1d4"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-f286ca180626-20261118-fanout-v1", roots["f286ca18062692eb61ec7f8d61a8e9c13ba245acacd1e6fe4efa15243e85d22c"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-e5ede2045f7c-20261118-fanout-v1", roots["e5ede2045f7c06ec1da4432418d34d7f4f4a2a429d88531a60e0f7eb23a8cd64"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-e1e0aecd720f-20261118-fanout-v1", roots["e1e0aecd720f6906e6988b490933cd76424582032eac8cc52ce9b0ce107baef3"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-7fb6148ba662-20261118-fanout-v1", roots["7fb6148ba662ced14e3684c70cf15f21af97a37b3be54101364c32e5fad74c9f"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-a69be940f481-20261118-fanout-v1", roots["a69be940f481d9a799aeedeb7c79faf8217ffd94e7f729b937ccce8aa085f0af"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-6e8229a1bc25-20261118-fanout-v5", roots["6e8229a1bc254db678e1c7fe98994ee56e1e4015dde5bc78d546d841fa561521"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-6b4f68929944-20261118-fanout-v5", roots["6b4f6892994436ad949b990c5f6a01169035628e9fd646540c190250ab2c0214"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-82dfe9039f31-20261118-fanout-v1", roots["82dfe9039f3161eddf048bacc644251a1011f385a4999349ad787a46db03030f"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-4244600c8e59-20261118-fanout-v1", roots["4244600c8e59578d0fbefa2567cd7db4495ed6550d6f2e6844be5732616a87c0"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-413d90f184e9-20261118-fanout-v5", roots["413d90f184e958cc95e72638cf4d3920c765132a8bb473a2d7deed096685bc80"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-c926dfa85984-20261118-fanout-v5", roots["c926dfa85984a9d27733ce7d6a60004a24b3a6855a2a93d837b8e87a453d4a7d"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-31c3561573d9-20261118-fanout-v1", roots["31c3561573d9829e2f337f69a707c40c5f936176352c5c6a7ebfe191ff96c185"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-dc3a8dc3b69e-20261118-fanout-v1", roots["dc3a8dc3b69ed7ae16b967171f25c42d3add8f3f262d4520b7dfbaadcb8d321c"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-1af3d755b507-20261118-fanout-v1", roots["1af3d755b5076aaae9cc46334b7f319e4d67159c92b8b46472f3bf4f3c00643d"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-67c999d116ef-20261118-fanout-v1", roots["67c999d116efad9e0f4bb723dbfccc9361424d5fb291f1385850b7a73885caeb"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-a1cccb7ecdaa-20261118-fanout-v1", roots["a1cccb7ecdaaa393785ca8490635b8275f67b90166d3f2711c8dfba60d576d15"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-b81cd9164582-20261118-fanout-v1", roots["b81cd916458286253f8db74313138b16cbbda05880c8e9e28e338a4935013498"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-b8a48e7e45e3-20261118-fanout-v1", roots["b8a48e7e45e3770ea9538325f8882b10746fee7a512e69b68fadc72c62f2dd5b"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-d29d6d635b9c-20261118-fanout-v1", roots["d29d6d635b9c4007dc01785d3a40e37966692f11206b4988c45b8dd07ab50589"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-91839f8b6913-20261118-fanout-v2", roots["91839f8b6913340a41e7915e84d50818dcadff35b8a7a04e3b367a4cb098fb3b"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-ff3cb6404c89-20261118-fanout-v1", roots["ff3cb6404c897214795e05fbf454959486d59fce45dec6d3c8b4b22b06eb5dc7"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-cd2bce46a2f4-20261118-fanout-v1", roots["cd2bce46a2f4665cb28de5bd6c35dc1b8ca75ec39b9732721756afa14f4b028f"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-b386e14376df-20261118-fanout-v1", roots["b386e14376dfbcd911606ecce2d3e0622c75787e767ab35617d5248728063798"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-cf7c27e96980-20261118-fanout-v1", roots["cf7c27e96980a328455579f0a80a1754e6361899d2df7141d928cc3dd68a7314"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-3684fe44bf66-20261118-fanout-v1", roots["3684fe44bf665d6f717b4cf59249fa1bc45f83ddf66f7756e9b2bb3400912ff9"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-c632deae5492-20261118-fanout-v3", roots["c632deae5492a2489182110830673329e9801311921a6be16a310f35312e0c19"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-fcbf47285d98-20261118-fanout-v3", roots["fcbf47285d98335bf375bbd012f82e43d3126a0edc36e1f8bf992d9fc3eba2cc"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-a3e755e2ec68-20261118-fanout-v3", roots["a3e755e2ec68cb558e6c4e69b4808153b484aaeb91721f8637ce1b6ddf9397d3"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-c07d4e7c6b6e-20261118-fanout-v3", roots["c07d4e7c6b6e288cc7c23111d04eca5c50872c5b58fdb9b5f2ec67ae2093cdad"])
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
		if strings.Contains(src.Version, "-accel-withdraw-fix-") {
			require.Equal(t, flowClientTraceBuild, src.Build, src.Version)
		} else {
			require.Equal(t, flowLSMBuild, src.Build, src.Version)
		}
		require.Equal(t, flowLSMProfile, src.Profile, src.Version)
		require.Len(t, src.Root, 64, src.Version)
	}
	require.Equal(t, 2, lsm)
}

// flowServePayloadSchema creates the collector's payload tables and the two
// views the API is granted, as production has them.
func flowServePayloadSchema(t *testing.T, s *Server) {
	t.Helper()
	for _, sql := range []string{
		`CREATE TABLE tax_commission_flow_payloads (version text, chain text, validator text, field text, build text NOT NULL, codec text NOT NULL,
			raw_sha256 text NOT NULL, raw_bytes bigint NOT NULL, encoded_sha256 text NOT NULL, encoded_bytes bigint NOT NULL, chunk_count bigint NOT NULL,
			chunk_bytes bigint NOT NULL, PRIMARY KEY (version, chain, validator, field))`,
		`CREATE TABLE tax_commission_flow_payload_chunks (version text, chain text, validator text, field text, ordinal bigint, sha256 text NOT NULL,
			data bytea NOT NULL, PRIMARY KEY (version, chain, validator, field, ordinal))`,
		`CREATE VIEW tax_commission_flow_served_payloads WITH (security_barrier) AS SELECT version, chain, validator, field, build, codec, raw_sha256,
			raw_bytes, encoded_sha256, encoded_bytes, chunk_count, chunk_bytes FROM tax_commission_flow_payloads WHERE field IN ('run', 'rows')`,
		`CREATE VIEW tax_commission_flow_served_payload_chunks WITH (security_barrier) AS SELECT version, chain, validator, field, ordinal, sha256, data
			FROM tax_commission_flow_payload_chunks WHERE field IN ('run', 'rows')`,
	} {
		require.NoError(t, s.db.Exec(sql).Error, sql)
	}
}

// flowServeInsertPayload stores raw as the collector does past its inline
// limit: gzip bytes in 1 MiB chunks, an index, and returns the marker.
func flowServeInsertPayload(t *testing.T, s *Server, version, validator, field, raw string, corrupt bool) string {
	t.Helper()
	var enc bytes.Buffer
	zw := gzip.NewWriter(&enc)
	_, err := zw.Write([]byte(raw))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	rawSum, encSum, chunkSum := sha256.Sum256([]byte(raw)), sha256.Sum256(enc.Bytes()), sha256.Sum256(enc.Bytes())
	chunkSHA := hex.EncodeToString(chunkSum[:])
	if corrupt {
		chunkSHA = strings.Repeat("0", 64)
	}
	require.NoError(t, s.db.Exec(`INSERT INTO tax_commission_flow_payloads VALUES (?, ?, ?, ?, ?, 'gzip-json-bytes-v1', ?, ?, ?, ?, 1, 1048576)`,
		version, apiMainnet, validator, field, flowServeBuild, hex.EncodeToString(rawSum[:]), len(raw), hex.EncodeToString(encSum[:]), enc.Len()).Error)
	require.NoError(t, s.db.Exec(`INSERT INTO tax_commission_flow_payload_chunks VALUES (?, ?, ?, ?, 0, ?, ?)`,
		version, apiMainnet, validator, field, chunkSHA, enc.Bytes()).Error)
	marker, err := json.Marshal("cosmos-commission-payload/v1:" + field + ":" + hex.EncodeToString(rawSum[:]))
	require.NoError(t, err)
	return string(marker)
}

func TestCommissionFlowsServeChunkedPayloadsPostgres(t *testing.T) {
	s := flowPGServer(t)
	flowServeSchema(t, s)
	flowServePayloadSchema(t, s)
	s.flowSources = map[string][]commissionServingSource{apiMainnet: {flowServeSource("shard")}}
	good, corrupt, missing := flowPGValoper(6), flowPGValoper(7), flowPGValoper(8)

	store := func(validator string, corruptChunk bool, withIndex bool) {
		run, err := json.Marshal(flowPGRun(validator, flowStatusReconciled, `{"2025-02":"700000"}`))
		require.NoError(t, err)
		rows, err := json.Marshal(flowPGRows(validator, [3]string{flowPGFeb, flowExchange, "700000"}))
		require.NoError(t, err)
		marker := `"cosmos-commission-payload/v1:rows:abc"`
		if withIndex {
			marker = flowServeInsertPayload(t, s, "shard", validator, "rows", string(rows), corruptChunk)
		}
		flowServeInsertRaw(t, s, "shard", flowServeBuild, flowServeProfile, validator, string(run), marker)
	}
	store(good, false, true)
	store(corrupt, true, true)
	store(missing, false, false)
	// A private evidence payload the API views must never return.
	flowServeInsertPayload(t, s, "shard", good, "evidence", `{"private":"evidence"}`, false)

	check := func() {
		s.flowVer = nil
		served, err := s.servedCommissionFlows(apiMainnet, "")
		require.NoError(t, err)
		require.Len(t, served, 1, "only the verified chunked rows are served")
		require.True(t, served[good].Versioned)
		require.Len(t, served[good].Rows, len(flowRuleNames))
		require.Equal(t, "700000", served[good].Rows[0].AmountBase)
		var got ValidatorCommissionFlows
		flowPGGet(t, s.handleValidatorCommissionFlows, "/validator-commission-flows?validator="+good, http.StatusOK, &got)
		require.Equal(t, "0.7", got.Total.Withdrawn.String())
		flowPGGet(t, s.handleValidatorCommissionFlows, "/validator-commission-flows?validator="+corrupt, http.StatusNotFound, nil)
	}
	check()

	// As a role holding only the API's grants: the views, never the payload tables.
	role := fmt.Sprintf("flow_chunk_api_%d", time.Now().UnixNano())
	var schema string
	require.NoError(t, s.db.Raw("SELECT current_schema()").Scan(&schema).Error)
	for _, sql := range []string{
		"CREATE ROLE " + role + " NOLOGIN",
		"GRANT USAGE ON SCHEMA " + schema + " TO " + role,
		"GRANT SELECT ON ALL TABLES IN SCHEMA " + schema + " TO " + role,
		"REVOKE SELECT ON tax_commission_flow_versions, tax_commission_flow_payloads, tax_commission_flow_payload_chunks FROM " + role,
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
	require.Error(t, s.db.Exec("SELECT data FROM tax_commission_flow_payload_chunks").Error)
	var evidence int64
	require.NoError(t, s.db.Raw("SELECT count(*) FROM tax_commission_flow_served_payload_chunks WHERE field = 'evidence'").Scan(&evidence).Error)
	require.Zero(t, evidence)
	check()

	// Without the view grants the chunked row is refused and nothing fails.
	require.NoError(t, s.db.Exec("RESET ROLE").Error)
	require.NoError(t, s.db.Exec("REVOKE SELECT ON tax_commission_flow_served_payloads, tax_commission_flow_served_payload_chunks FROM "+role).Error)
	require.NoError(t, s.db.Exec("SET ROLE "+role).Error)
	s.flowVer = nil
	served, err := s.servedCommissionFlows(apiMainnet, "")
	require.NoError(t, err)
	require.Empty(t, served)
}
