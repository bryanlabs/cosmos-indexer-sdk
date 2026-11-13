package taxapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestCommissionFlowVersionOptions(t *testing.T) {
	require.Error(t, (CommissionFlowBuildOptions{}).validate())
	o := CommissionFlowBuildOptions{Version: "proof", Build: "build", AsOf: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)}
	require.NoError(t, o.validate())
	o.AsOf = o.AsOf.Add(time.Hour)
	require.Error(t, o.validate())
}

// This proof intentionally preserves its uniquely named schema and all rows.
// Run only against a dedicated test database, never production.
func TestCommissionFlowVersionsAppendOnlyPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_VERSIONS_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_VERSIONS_TEST_DSN to a dedicated test database")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	schema := fmt.Sprintf("commission_versions_proof_%d", time.Now().UnixNano())
	require.NoError(t, db.Exec("CREATE SCHEMA "+schema).Error)
	bootstrap, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, bootstrap.Close())
	// Bind the schema in the DSN, including replacement pool connections.
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		require.NoError(t, err)
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	conn, err := db.DB()
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, db.Exec("SET statement_timeout='10s'").Error)
	require.NoError(t, db.Exec("SET TIME ZONE 'UTC'").Error)
	t.Logf("preserved proof schema: %s", schema)
	require.NoError(t, CommissionFlowsMigrate(db))
	require.NoError(t, CommissionFlowVersionsMigrate(db))
	require.NoError(t, CommissionFlowVersionsMigrate(db))
	s := &Server{db: db}
	from := time.Date(2024, 3, 21, 0, 0, 0, 0, time.UTC)
	asOf := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	run := CommissionFlowRun{Chain: apiMainnet, Validator: flowPGValidator, FromDay: from, AsOf: asOf.AddDate(0, 0, -1), ComputedAt: asOf, Status: flowStatusReconciled, Commission: "{}", Untracked: "{}", Overdraw: "{}", DriftBase: "0"}
	rows := []CommissionFlow{{Chain: apiMainnet, Validator: flowPGValidator, Rule: "fifo", Month: "2026-10", Class: flowHeld, AmountBase: "1000000"}}
	require.NoError(t, db.Create(&run).Error)
	require.NoError(t, db.Create(&rows).Error)
	opts := CommissionFlowBuildOptions{Version: "proof-v1", Build: "sha256:one", AsOf: asOf}
	for _, q := range []string{
		"CREATE TABLE taxable_events (message_id bigint, from_addr text, to_addr text, category text)",
		"CREATE TABLE messages (id bigint, tx_id bigint)",
		"CREATE TABLE txes (id bigint, memo text)",
		"INSERT INTO txes SELECT g, 'acct'||g FROM generate_series(1,120) g",
		"INSERT INTO messages SELECT g,g FROM generate_series(1,120) g",
		"INSERT INTO taxable_events SELECT g, 'sender'||g, 'cosmos1depositlike', 'transfer' FROM generate_series(1,120) g",
	} {
		require.NoError(t, db.Exec(q).Error)
	}
	nodeHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"identified_client_state":{"client_state":{"chain_id":"proof-chain"}}}`))
	}))
	t.Cleanup(nodeHTTP.Close)
	t.Setenv("NODE_REST_API", nodeHTTP.URL)
	node, err := NewLedgerNode()
	require.NoError(t, err)
	loader := &dbFlowLoader{s: s, chain: apiMainnet, node: node, private: true, mu: &sync.Mutex{}, labels: map[string]AddressLabel{}, chans: map[string]string{}}
	stop, err := loader.stop(flowPGDeposit)
	require.NoError(t, err)
	require.Equal(t, flowExchangeLike, stop.Class)
	require.Equal(t, "proof-chain", loader.channelChain("transfer", "channel-1"))
	var count int64
	require.NoError(t, db.Model(&AddressLabel{}).Count(&count).Error)
	require.Zero(t, count, "private discovery must not change shared labels")
	require.NoError(t, db.Model(&IBCChannel{}).Count(&count).Error)
	require.Zero(t, count, "private channel lookup must not change shared mappings")
	evidence := loader.evidence()
	require.Len(t, evidence.Labels, 1)
	require.Equal(t, "proof-chain", evidence.Channels["transfer/channel-1"])
	raw := "{\n  \"z\": 1, \"a\": [2, 3]\n}"
	hash := sha256.Sum256([]byte(raw))
	evidence.Archive = []FlowArchiveReceipt{{Address: flowPGDeposit, Height: 123, Response: raw, SHA256: hex.EncodeToString(hash[:])}}
	require.NoError(t, s.saveCommissionFlowVersion(opts, run, rows, evidence))
	exists, err := s.reuseCommissionFlowVersion(opts, apiMainnet, run.Validator, from)
	require.NoError(t, err)
	require.True(t, exists)
	exists, err = s.reuseCommissionFlowVersion(opts, apiMainnet, flowPGOther, from)
	require.NoError(t, err)
	require.False(t, exists)
	mismatched := opts
	mismatched.Build = "sha256:wrong"
	_, err = s.reuseCommissionFlowVersion(mismatched, apiMainnet, run.Validator, from)
	require.Error(t, err)
	// Duplicate attempt cannot alter even one byte of prior result.
	changed := run
	changed.Status = flowStatusDrift
	require.Error(t, s.saveCommissionFlowVersion(opts, changed, nil))
	// Different build, start date or end date cannot mix into the generation.
	other := run
	other.Validator = flowPGOther
	bad := opts
	bad.Build = "sha256:two"
	require.Error(t, s.saveCommissionFlowVersion(bad, other, nil))
	other.FromDay = from.AddDate(0, 0, 1)
	require.Error(t, s.saveCommissionFlowVersion(opts, other, nil))
	other.FromDay = from
	other.AsOf = other.AsOf.AddDate(0, 0, 1)
	bad = opts
	bad.AsOf = bad.AsOf.AddDate(0, 0, 1)
	require.Error(t, s.saveCommissionFlowVersion(bad, other, nil))
	other.AsOf = run.AsOf
	// Failed run evidence is immutable too.
	other.Status = flowStatusError
	other.Reason = "proof refusal"
	require.NoError(t, s.saveCommissionFlowVersion(opts, other, nil))
	_, err = s.reuseCommissionFlowVersion(opts, apiMainnet, other.Validator, from)
	require.ErrorContains(t, err, "requires a new output version")
	// A new generation appends, without replacing old serving data or evidence.
	opts.Version = "proof-v2"
	opts.Build = "sha256:two"
	require.NoError(t, s.saveCommissionFlowVersion(opts, changed, nil))
	var snapshots []CommissionFlowVersion
	require.NoError(t, db.Order("version, validator").Find(&snapshots).Error)
	require.Len(t, snapshots, 3)
	var old CommissionFlowRun
	require.NoError(t, db.First(&old).Error)
	old.ComputedAt = old.ComputedAt.UTC()
	require.Equal(t, run, old)
	var oldRows []CommissionFlow
	require.NoError(t, db.Find(&oldRows).Error)
	require.Equal(t, rows, oldRows)
	var first CommissionFlowVersion
	require.NoError(t, db.Where("version = ? AND validator = ?", "proof-v1", run.Validator).First(&first).Error)
	var decodedRun CommissionFlowRun
	var decodedRows []CommissionFlow
	require.NoError(t, json.Unmarshal([]byte(first.RunJSON), &decodedRun))
	require.Equal(t, run, decodedRun)
	require.NoError(t, json.Unmarshal([]byte(first.RowsJSON), &decodedRows))
	require.Equal(t, rows, decodedRows)
	var decodedEvidence CommissionFlowEvidence
	require.NoError(t, json.Unmarshal([]byte(first.EvidenceJSON), &decodedEvidence))
	require.Equal(t, evidence, decodedEvidence)
	hash = sha256.Sum256([]byte(decodedEvidence.Archive[0].Response))
	require.Equal(t, decodedEvidence.Archive[0].SHA256, hex.EncodeToString(hash[:]))
}
