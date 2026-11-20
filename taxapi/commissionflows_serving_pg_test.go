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
	require.Len(t, seen, 140)
	require.Empty(t, defaultCommissionServingSources[apiTestnet])
}

// Each retry and giant entry serves only its one pinned root, and no root is
// pinned by two of them: later-s02 has two deadline retries (v2 and v3),
// each under its own version.
func TestCommissionServingRetriesPinOneRootEach(t *testing.T) {
	roots := map[string]string{}
	for _, src := range defaultCommissionServingSources[apiMainnet] {
		if !strings.Contains(src.Version, "-retry-") && src.Build != flowGiantBuild && src.Build != flowWithdrawFixBuild && src.Build != flowSlashBuild && src.Build != flowClientTraceBuild {
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
	require.Len(t, roots, 117)
	require.Equal(t, flowCorrectedPrefix+"accel-later-s02-20261115-deadline-retry-v2", roots["2d473f4858d9987775254ce4bff8939f812bfefeb04eb1e6f0a5ebde72969fd8"])
	require.Equal(t, flowCorrectedPrefix+"accel-later-s02-20261115-deadline-retry-v3", roots["50c56a74de37966c61278d7381538fe0d8fa3746d87eadae5d633a471f017de1"])
	require.Equal(t, flowCorrectedPrefix+"accel-four-s05-20261115-deadline-retry-v3", roots["5187c6ad32a6209ebd2f8b36e2f78b95950a96e046c474db04ca64b8c981f885"])
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
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-1e6691e3985a-20261118-fanout-v3", roots["1e6691e3985a8f008990dbebf047b578651e022a4f608e7f48b939f571d4e3ab"])
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
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-0f53f6fc13d9-20261118-fanout-v1", roots["0f53f6fc13d9f7022c8ee7a0aa661c0440ea6027ff4e934e7952601cfb526e2c"])
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
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-3e3481ef24f7-20261118-fanout-v1", roots["3e3481ef24f791529e1ff1bf325d48d3ecccf38248bc9ef24ca623890e0d4b45"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-a149b3077b86-20261118-fanout-v1", roots["a149b3077b86222dfc773827c1c37a42c4755339a0c1520d344aa4615385cd38"])
	require.Equal(t, flowCorrectedPrefix+"accel-withdraw-fix-5fa7cab34d20-20261118-fanout-v1", roots["5fa7cab34d20f2a77714dae2730dc0286ed77207a35099cf6d8128b08f375af1"])
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
