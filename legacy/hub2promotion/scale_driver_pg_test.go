package hub2promotion

// This executable test driver is deliberately private-only. Compile with go test
// -c, then select ONLY TestPrivateScaleDriver. It has no production adapter.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

const scaleDumpSHA = "6f3295e7a188502d73b2e99020e00a29b6c83510139dfe6f6f424f5f73975519"
const scaleReceiptSHA = "7c61a99e4c3a559d6057c5e07e52d784148dfefc01211dd2eac942a466cd7613"
const scaleRetainedNodePath = "/var/lib/bryanlabs-private/hub2-atomic-scale-20261114-c1"
const scaleRetainedNodeUID = "fa6a9a34-36a8-45f4-acb9-0706a0138cc5"

// The private Identity storage slot is visibly NOT a PVC UID for this profile.
// Node UID/path provenance is pinned by the externally reviewed Job packet.
func scaleStorageIdentity(kind, pvc, nodeName, nodeUID string) (string, error) {
	if kind == "pvc" && pvc != "" {
		return pvc, nil
	}
	if kind == "retained-nodepath" && nodeName == "controller-1" && nodeUID == scaleRetainedNodeUID {
		return "PRIVATE-RETAINED-NODEPATH:" + nodeUID + ":" + digest([]byte(scaleRetainedNodePath)), nil
	}
	return "", fmt.Errorf("explicit private retained storage identity required")
}

// seedScaleHistory copies only SQL projections, through a read-only source
// snapshot. Synthetic Hub3 history is visibly offset and never migration proof.
// The existing synthetic genesis and all14 protected fixtures remain in place.
func seedScaleHistory(ctx context.Context, source, target *sql.DB) (Snapshot, error) {
	tx, e := beginAuditBudget(ctx, source, true)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	// The source SELECT stays open during bounded target batches. Its budget
	// includes client backpressure, not just PostgreSQL scan execution.
	if _, e = tx.ExecContext(ctx, `SET LOCAL datestyle='ISO, YMD'; SET LOCAL statement_timeout='30min'`); e != nil {
		return nil, e
	}
	seed, e := target.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer seed.Rollback()
	if _, e = seed.ExecContext(ctx, `SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO, YMD'; SET LOCAL statement_timeout='10min'`); e != nil {
		return nil, e
	}
	out := Snapshot{}
	for _, tb := range inserts {
		if strings.HasPrefix(tb.name, "genesis_") {
			continue
		}
		query := `SELECT pg_catalog.jsonb_set(pg_catalog.jsonb_set(pg_catalog.to_jsonb(r),'{chain_id}','"cosmoshub-3"'::jsonb),'{height}',pg_catalog.to_jsonb(height+100))::text FROM legacy.` + tb.name + ` r WHERE chain_id='cosmoshub-2' ORDER BY ` + quotedColumns(tb.key)
		rows, e := tx.QueryContext(ctx, query)
		if e != nil {
			return nil, e
		}
		batch := make([]json.RawMessage, 0, 64)
		hash := sha256.New()
		var count int64
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			b, e := json.Marshal(batch)
			if e != nil {
				return e
			}
			result, e := seed.ExecContext(ctx, "INSERT INTO legacy."+tb.name+" ("+quotedColumns(tb.columns)+") SELECT "+quotedColumns(tb.columns)+" FROM pg_catalog.jsonb_populate_recordset(NULL::legacy."+tb.name+",$1::jsonb)", string(b))
			if e != nil {
				return fmt.Errorf("synthetic history seed refused: %s", tb.name)
			}
			n, e := result.RowsAffected()
			if e != nil || n != int64(len(batch)) {
				return fmt.Errorf("synthetic seed count: %s", tb.name)
			}
			batch = batch[:0]
			return nil
		}
		for rows.Next() {
			var raw string
			if e = rows.Scan(&raw); e != nil {
				rows.Close()
				return nil, e
			}
			io.WriteString(hash, raw+"\n")
			count++
			batch = append(batch, json.RawMessage(raw))
			if len(batch) == cap(batch) {
				if e = flush(); e != nil {
					rows.Close()
					return nil, e
				}
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		if e = flush(); e != nil {
			return nil, e
		}
		expected := Fingerprint{count, hex.EncodeToString(hash.Sum(nil))}
		actual, e := fingerprint(ctx, seed, tb, "chain_id='cosmoshub-3' AND height>100", false)
		if e != nil {
			return nil, e
		}
		if actual != expected {
			return nil, fmt.Errorf("synthetic source/target mismatch: %s", tb.name)
		}
		out[tb.name] = actual
	}
	if e = seed.Commit(); e != nil {
		return nil, e
	}
	return out, nil
}

func TestPGScaleCloneSeedExactBounded(t *testing.T) {
	source := newFixture(t)
	target := newFixture(t)
	_, e := MergeFixture(context.Background(), source.loader, source.owner, source.m, source.runtime)
	require.NoError(t, e)
	before := source.full(t)
	seeded, e := seedScaleHistory(context.Background(), source.owner, target.owner)
	require.NoError(t, e)
	require.Len(t, seeded, 4)
	require.EqualValues(t, 5, seeded["blocks"].Rows)
	require.Equal(t, before, source.full(t))
	require.NoError(t, emptyHub2(context.Background(), target.owner))
}

// localScaleTool never accepts an endpoint/DSN or an executable from input.
// It runs only PG16 tools at the private server's fixed loopback address.
func localScaleTool(ctx context.Context, tool, db string, input io.Reader, output io.Writer, args ...string) error {
	if !strings.HasPrefix(db, "legacy_replay_rehearsal_") || (tool != "pg_dump" && tool != "pg_restore") {
		return fmt.Errorf("private tool/database refused")
	}
	a := []string{"--host=127.0.0.1", "--port=25673", "--username=postgres", "--dbname=" + db}
	a = append(a, args...)
	cmd := exec.CommandContext(ctx, tool, a...)
	cmd.Stdin = input
	cmd.Stdout = output
	// Keep potential SQL diagnostics private. The caller records phase failure,
	// not raw rows, SQL values, credentials or PostgreSQL error detail.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if e := cmd.Run(); e != nil {
		return fmt.Errorf("private %s failed; diagnostics suppressed", tool)
	}
	return nil
}
func localSchema(ctx context.Context, db string) ([]byte, error) {
	var out bytes.Buffer
	e := localScaleTool(ctx, "pg_dump", db, nil, &out, "--schema-only", "--no-comments")
	if e != nil {
		return nil, e
	}
	var lines []string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, `\restrict `) || strings.HasPrefix(l, `\unrestrict `) {
			continue
		}
		lines = append(lines, l)
	}
	return []byte(strings.Join(lines, "\n")), nil
}

func TestPrivateScaleDriver(t *testing.T) {
	if os.Getenv("HUB2_PRIVATE_SCALE_RUN") != "retained-prefix1400000-only" {
		t.Skip("private scale driver is opt-in, not ordinary PG acceptance")
	}
	cfg, e := pgx.ParseConfig(os.Getenv("LEGACY_STORE_TEST_ADMIN_DSN"))
	require.NoError(t, e)
	require.Equal(t, "127.0.0.1", cfg.Host)
	require.EqualValues(t, 25673, cfg.Port)
	require.Empty(t, cfg.Fallbacks)
	require.Equal(t, "postgres", cfg.User)
	require.Equal(t, "postgres", cfg.Database)
	// The Job provides actual downward-API identity and explicitly typed storage.
	// A retained-nodepath fixture is not represented as an actual PVC.
	storageID, e := scaleStorageIdentity(os.Getenv("HUB2_SCALE_STORAGE_KIND"), os.Getenv("HUB2_SCALE_PVC_UID"), os.Getenv("HUB2_SCALE_NODE_NAME"), os.Getenv("HUB2_SCALE_NODE_UID"))
	require.NoError(t, e)
	runtime := Identity{DeploymentUID: os.Getenv("HUB2_SCALE_JOB_UID"), PodUID: os.Getenv("HUB2_SCALE_POD_UID"), PVCUID: storageID, Endpoint: "127.0.0.1:25673"}
	require.NotEmpty(t, runtime.DeploymentUID)
	require.NotEmpty(t, runtime.PodUID)
	require.NotEmpty(t, runtime.PVCUID)
	root := stdlib.OpenDB(*cfg)
	defer root.Close()
	var cleanServer bool
	require.NoError(t, root.QueryRow(`SELECT count(*)=3 AND bool_and(datname IN ('postgres','template0','template1')) FROM pg_catalog.pg_database`).Scan(&cleanServer))
	require.True(t, cleanServer, "scale driver requires its own NEW cluster, never another fixture server")
	var actualPort, version int
	require.NoError(t, root.QueryRow(`SELECT inet_server_port(),current_setting('server_version_num')::int`).Scan(&actualPort, &version))
	require.Equal(t, 25673, actualPort)
	require.True(t, version >= 160000 && version < 170000)
	var autovacuum, listen, fsync, fullPageWrites string
	require.NoError(t, root.QueryRow(`SELECT current_setting('autovacuum'),current_setting('listen_addresses'),current_setting('fsync'),current_setting('full_page_writes')`).Scan(&autovacuum, &listen, &fsync, &fullPageWrites))
	require.Equal(t, "off", autovacuum, "only this isolated bounded fixture disables background lock competitors")
	require.Equal(t, "127.0.0.1", listen)
	require.Equal(t, "on", fsync)
	require.Equal(t, "on", fullPageWrites)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()
	dir := artifactDir(t)
	started := time.Now()
	lastPhase := started
	phase := func(name string, data any) {
		b, e := json.Marshal(map[string]any{"utc": time.Now().UTC(), "phase": name, "elapsed_ns": time.Since(started).Nanoseconds(), "since_previous_phase_ns": time.Since(lastPhase).Nanoseconds(), "data": data})
		require.NoError(t, e)
		_, e = os.Stat(filepath.Join(dir, name+".json"))
		require.True(t, os.IsNotExist(e))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name+".json"), b, 0600))
		lastPhase = time.Now()
		t.Logf("private-scale phase=%s utc=%s", name, lastPhase.UTC().Format(time.RFC3339))
	}
	phase("started", map[string]any{"runtime": runtime, "fixture_only": true, "production_execution": false, "storage_kind": os.Getenv("HUB2_SCALE_STORAGE_KIND"), "node_name": os.Getenv("HUB2_SCALE_NODE_NAME"), "retained_nodepath": scaleRetainedNodePath})
	dump := Pin{os.Getenv("HUB2_SCALE_SOURCE_DUMP"), scaleDumpSHA}
	require.NoError(t, hashFile(dump))
	receipt := Pin{os.Getenv("HUB2_SCALE_SOURCE_RECEIPT"), scaleReceiptSHA}
	require.NoError(t, hashFile(receipt))
	genesis := Pin{os.Getenv("HUB2_SCALE_GENESIS"), os.Getenv("HUB2_SCALE_GENESIS_SHA")}
	require.NoError(t, hashFile(genesis))
	sourceName := "legacy_replay_rehearsal_scale_source_" + randomID()
	_, e = root.ExecContext(ctx, "CREATE DATABASE "+sourceName)
	require.NoError(t, e)
	sc := cfg.Copy()
	sc.Database = sourceName
	source := stdlib.OpenDB(*sc)
	defer source.Close()
	// Restore into a NEW database, never an existing rehearsal datadir/database.
	input, e := openPinned(dump)
	require.NoError(t, e)
	require.NoError(t, localScaleTool(ctx, "pg_restore", sourceName, input, nil, "--single-transaction", "--exit-on-error", "--no-owner", "--no-privileges"))
	require.NoError(t, input.Close())
	require.NoError(t, hashFile(dump))
	var sourceBytes int64
	require.NoError(t, source.QueryRowContext(ctx, `SELECT pg_database_size(current_database())`).Scan(&sourceBytes))
	phase("source-restored", map[string]any{"database": sourceName, "dump": dump, "database_bytes": sourceBytes})
	sourceBefore, e := snapshot(ctx, source, false, true)
	require.NoError(t, e)
	tx, e := beginAuditBudget(ctx, source, true)
	require.NoError(t, e)
	// No promotion scope exists in the source. Bind DB/OID/system directly.
	var sourceID Identity
	var system string
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT current_database(),oid FROM pg_catalog.pg_database WHERE datname=current_database()`).Scan(&sourceID.Database, &sourceID.OID))
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT system_identifier::text FROM pg_catalog.pg_control_system()`).Scan(&system))
	sourceID.SystemHash = digest([]byte(system))
	require.NoError(t, tx.Rollback())
	tables, e := ExportPrivateScaleProjection(ctx, source, sourceID, filepath.Join(dir, "projection"))
	require.NoError(t, e)
	expected := map[string]int64{"blocks": 1400000, "tx_inclusions": 383166, "block_events": 2800000, "taxable_events": 382592, "genesis_files": 1, "genesis_accounts": 1027, "genesis_delegations": 1112, "genesis_unbondings": 60, "genesis_validators": 105, "genesis_withdraw_addresses": 0}
	var projectionBytes int64
	for _, p := range tables {
		require.Equal(t, expected[p.Table], p.Rows)
		st, e := os.Stat(p.File.Path)
		require.NoError(t, e)
		projectionBytes += st.Size()
	}
	phase("projection-exported", map[string]any{"tables": tables, "bytes": projectionBytes})
	f := newFixture(t)
	seeded, e := seedScaleHistory(ctx, source, f.owner)
	require.NoError(t, e)
	phase("populated-target-seeded", seeded)
	m := f.m
	m.Profile = PrivateScaleProfile
	m.Last = 1400000
	m.Canonical = 0
	m.MigrationLast = 0
	m.TailFirst = 0
	m.TailLast = 0
	m.CanonicalRows = 0
	m.CanonicalProjectionSHA = valueEmptySHA()
	m.Tables = tables
	m.Source = receipt
	m.Financial = receipt
	m.Genesis = genesis
	// These are explicitly fixture-only component labels bound to accepted SQL,
	// never native roots or a whole-chain financial receipt.
	m.Components = []Component{{1, 1400000, receipt, digest([]byte("PRIVATE SQL prefix opening, NOT native attestation")), digest([]byte("PRIVATE SQL prefix ending, NOT native attestation")), true}}
	m.BinarySHA = ""
	exe, e := os.Executable()
	require.NoError(t, e)
	binaryFile, e := os.Open(exe)
	require.NoError(t, e)
	h := sha256.New()
	_, e = io.Copy(h, binaryFile)
	require.NoError(t, e)
	require.NoError(t, binaryFile.Close())
	m.BinarySHA = hex.EncodeToString(h.Sum(nil))
	m.BuildIdentity = savePin(t, dir, "driver-build.json", []byte(fmt.Sprintf(`{"private_test_driver":true,"binary_sha256":%q}`, m.BinarySHA)))
	m.BuildSHA = m.BuildIdentity.SHA256
	f.runtime = runtime
	f.m = m
	refreshIdentity(t, &f)
	m = f.m
	m.Protected, e = snapshot(ctx, f.owner, true, true)
	require.NoError(t, e)
	require.NoError(t, m.ValidatePrivateScale())
	b, e := json.MarshalIndent(m, "", "  ")
	require.NoError(t, e)
	savePin(t, dir, "scale-manifest.json", b)
	var walStart string
	require.NoError(t, f.owner.QueryRowContext(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&walStart))
	phase("merge-started", map[string]any{"destination": m.Destination, "protected": m.Protected, "wal_start": walStart})
	mergeStart := time.Now()
	result, e := MergePrivateScale(ctx, f.loader, f.owner, m, runtime)
	phase("merge-returned", map[string]any{"result": result, "success": e == nil, "commit_unknown": e == ErrCommitUnknown})
	require.NoError(t, e)
	var mergeWAL, targetBytes, tempBytes int64
	require.NoError(t, f.owner.QueryRowContext(ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_insert_lsn(),$1)::bigint,pg_database_size(current_database()),temp_bytes FROM pg_stat_database WHERE datname=current_database()`, walStart).Scan(&mergeWAL, &targetBytes, &tempBytes))
	phase("merge-measurements", map[string]any{"isolated_server_wal_bytes": mergeWAL, "target_database_bytes": targetBytes, "database_temp_bytes": tempBytes, "merge_call_wall_ns": time.Since(mergeStart).Nanoseconds(), "lock_hold_upper_bound_ns": result.Duration.Nanoseconds() - result.LockWait.Nanoseconds()})
	outcome, e := AuditOutcome(ctx, f.owner, m, runtime)
	require.NoError(t, e)
	require.Equal(t, OutcomeExact, outcome)
	phase("fresh-audit-exact", outcome)
	// Same populated target must refuse, without adopting or rewriting any rows.
	fullBefore, e := snapshot(ctx, f.owner, false, true)
	require.NoError(t, e)
	_, e = MergePrivateScale(ctx, f.loader, f.owner, m, runtime)
	require.ErrorContains(t, e, "preexisting unowned Hub2 cohort")
	fullAfter, e := snapshot(ctx, f.owner, false, true)
	require.NoError(t, e)
	require.Equal(t, fullBefore, fullAfter)
	phase("populated-retry-refused", true)
	logical, e := snapshot(ctx, f.owner, false, false)
	require.NoError(t, e)
	backupPath := filepath.Join(dir, "scale-target.dump")
	backup, e := os.OpenFile(backupPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	require.NoError(t, e)
	require.NoError(t, localScaleTool(ctx, "pg_dump", m.Destination.Database, nil, backup, "--format=custom"))
	require.NoError(t, backup.Sync())
	require.NoError(t, backup.Close())
	backupInfo, e := os.Stat(backupPath)
	require.NoError(t, e)
	backupPin := Pin{backupPath, ""}
	backup, e = os.Open(backupPath)
	require.NoError(t, e)
	backupHash := sha256.New()
	_, e = io.Copy(backupHash, backup)
	require.NoError(t, e)
	require.NoError(t, backup.Close())
	backupPin.SHA256 = hex.EncodeToString(backupHash.Sum(nil))
	phase("backup-complete", map[string]any{"pin": backupPin, "bytes": backupInfo.Size()})
	restoredName := "legacy_replay_rehearsal_scale_restore_" + randomID()
	_, e = root.ExecContext(ctx, "CREATE DATABASE "+restoredName)
	require.NoError(t, e)
	backup, e = os.Open(backupPath)
	require.NoError(t, e)
	require.NoError(t, localScaleTool(ctx, "pg_restore", restoredName, backup, nil, "--single-transaction", "--exit-on-error"))
	require.NoError(t, backup.Close())
	rc := cfg.Copy()
	rc.Database = restoredName
	restored := stdlib.OpenDB(*rc)
	defer restored.Close()
	restoredLogical, e := snapshot(ctx, restored, false, false)
	require.NoError(t, e)
	require.Equal(t, logical, restoredLogical)
	ddl, e := localSchema(ctx, m.Destination.Database)
	require.NoError(t, e)
	restoredDDL, e := localSchema(ctx, restoredName)
	require.NoError(t, e)
	require.Equal(t, string(ddl), string(restoredDDL), "full schema/owners/ACL equality")
	require.NoError(t, CheckScope(ctx, restored, m.Scope))
	tx, e = beginAuditBudget(ctx, restored, true)
	require.NoError(t, e)
	require.NoError(t, verifyProjection(ctx, tx, m))
	require.NoError(t, tx.Rollback())
	require.NoError(t, hashFile(backupPin))
	phase("restore-verified", map[string]any{"database": restoredName, "logical": restoredLogical, "schema_acl_sha": digest(restoredDDL)})
	sourceAfter, e := snapshot(ctx, source, false, true)
	require.NoError(t, e)
	require.Equal(t, sourceBefore, sourceAfter)
	require.NoError(t, hashFile(dump))
	require.NoError(t, hashFile(receipt))
	require.NoError(t, hashFile(genesis))
	phase("accepted", map[string]any{"accepted_core_evidence": "3138d1b:184cases:all9PGgroups:all13cohort-conflicts:retained-not-replayed", "all14_source_physical": sourceAfter, "all14_target_logical": logical, "result": result, "projection_bytes": projectionBytes, "source_physical_unchanged": true, "all14_protected_physical_unchanged": true, "restore_database": restoredName, "all14_restored_logical_equal": true, "schema_acl_sha": digest(ddl), "production_execution": false, "complete_financial_acceptance": false})
}
