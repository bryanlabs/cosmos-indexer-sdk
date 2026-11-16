package hub2promotion

import (
	"bytes"
	"context"
	"database/sql"
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

// helperPod names the private pod that runs the test database, from
// LEGACY_STORE_TEST_HELPER_POD. The backup and memory tests run pg_dump,
// pg_restore and a /proc read inside it, so it must be the server behind
// LEGACY_STORE_TEST_ADMIN_DSN. An unset or malformed name fails the test.
func helperPod(t *testing.T) string {
	t.Helper()
	name := os.Getenv("LEGACY_STORE_TEST_HELPER_POD")
	require.Regexp(t, `^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`, name, "LEGACY_STORE_TEST_HELPER_POD must name the private test database pod")
	return "pod/" + name
}

func pgHelper(t *testing.T, ctx context.Context, program, dbname, scope string, input *os.File, output *os.File) []byte {
	t.Helper()
	require.True(t, strings.HasPrefix(dbname, "legacy_replay_rehearsal_"))
	// The existing private helper only. No shell text comes from a DSN or user
	// SQL. Database/scope names are fixture-generated validated identifiers.
	script := `export PGPASSWORD="$POSTGRES_PASSWORD"; program="$1"; shift; exec "$program" -h127.0.0.1 -U"$POSTGRES_USER" "$@"`
	args := []string{"--context", "bryanlabs", "-n", "agents", "exec"}
	if input != nil {
		args = append(args, "-i")
	}
	args = append(args, helperPod(t), "--", "sh", "-c", script, "sh", program)
	switch program {
	case "pg_dump":
		args = append(args, "--format=custom", "--schema=legacy", "--schema="+scope, "--dbname="+dbname)
	case "pg_restore":
		args = append(args, "--single-transaction", "--exit-on-error", "--dbname="+dbname)
	case "schema":
		t.Fatal("bad helper program")
	default:
		t.Fatal("unapproved helper program")
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdin = input
	var out, err bytes.Buffer
	if output != nil {
		cmd.Stdout = output
	} else {
		cmd.Stdout = &out
	}
	cmd.Stderr = &err
	require.NoError(t, cmd.Run(), "private fixture backup helper failed: %s", err.String())
	return out.Bytes()
}
func schemaDump(t *testing.T, ctx context.Context, dbname, scope string) []byte {
	args := []string{"--context", "bryanlabs", "-n", "agents", "exec", helperPod(t), "--", "sh", "-c", `export PGPASSWORD="$POSTGRES_PASSWORD"; exec pg_dump -h127.0.0.1 -U"$POSTGRES_USER" --schema-only --no-comments --schema=legacy --schema="$2" --dbname="$1"`, "sh", dbname, scope}
	b, e := exec.CommandContext(ctx, "kubectl", args...).Output()
	require.NoError(t, e)
	var keep []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, `\restrict `) || strings.HasPrefix(line, `\unrestrict `) {
			continue
		}
		keep = append(keep, line)
	}
	return []byte(strings.Join(keep, "\n"))
}
func TestPG9TinyBackupRestoresToNewRetainedDatabaseExactSchemaACLRows(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, e := MergeFixture(ctx, f.loader, f.owner, f.m, f.runtime)
	require.NoError(t, e)
	sourcePhysical := f.full(t)
	sourceLogical, e := snapshot(ctx, f.owner, false, false)
	require.NoError(t, e)
	dumpPath := filepath.Join(f.dir, "tiny-legacy-and-scope.dump")
	dump, e := os.OpenFile(dumpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	require.NoError(t, e)
	pgHelper(t, ctx, "pg_dump", f.m.Destination.Database, f.m.Scope.Schema, nil, dump)
	require.NoError(t, dump.Close())
	cfg, e := pgx.ParseConfig(os.Getenv("LEGACY_STORE_TEST_ADMIN_DSN"))
	require.NoError(t, e)
	root := stdlib.OpenDB(*cfg)
	defer root.Close()
	name := "legacy_replay_rehearsal_restore_" + randomID()
	_, e = root.ExecContext(ctx, "CREATE DATABASE "+name)
	require.NoError(t, e)
	cfg.Database = name
	restored := stdlib.OpenDB(*cfg)
	defer restored.Close()
	var empty int
	require.NoError(t, restored.QueryRowContext(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname IN ('legacy',$1)`, f.m.Scope.Schema).Scan(&empty))
	require.Zero(t, empty, "restore destination must be NEW, not reused")
	input, e := os.Open(dumpPath)
	require.NoError(t, e)
	pgHelper(t, ctx, "pg_restore", name, f.m.Scope.Schema, input, nil)
	require.NoError(t, input.Close())
	targetLogical, e := snapshot(ctx, restored, false, false)
	require.NoError(t, e)
	require.Equal(t, sourceLogical, targetLogical)
	originalDDL := schemaDump(t, ctx, f.m.Destination.Database, f.m.Scope.Schema)
	restoredDDL := schemaDump(t, ctx, name, f.m.Scope.Schema)
	require.Equal(t, string(originalDDL), string(restoredDDL), "all fourteen table definitions, owners and ACLs must restore exactly")
	savePin(t, f.dir, "source-schema-acl.sql", originalDDL)
	savePin(t, f.dir, "restored-schema-acl.sql", restoredDDL)
	require.Equal(t, sourcePhysical, f.full(t))
	require.NoError(t, CheckScope(ctx, restored, f.m.Scope))
	// Explicitly READ ONLY verification connection for the newly restored data.
	tx, e := restored.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, e)
	require.NoError(t, verifyProjection(ctx, tx, f.m))
	require.NoError(t, tx.Rollback())
	t.Logf("retained new restore=%s exact14schema/ACL/content=true schemaSHA=%s", name, digest(originalDDL))
}
