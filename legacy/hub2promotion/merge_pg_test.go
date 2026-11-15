package hub2promotion

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPG1PopulatedDestinationAtomicExactAndRetry(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := deadline(t)
	defer cancel()
	before, e := snapshot(ctx, f.owner, true, true)
	require.NoError(t, e)
	r, e := MergeFixture(ctx, f.loader, f.owner, f.m, f.runtime)
	require.NoError(t, e)
	require.Equal(t, "on", r.SynchronousCommit)
	require.Equal(t, "repeatable read", r.Isolation)
	require.Greater(t, r.Chunks, 2)
	require.Greater(t, r.Rows, int64(10))
	after, e := snapshot(ctx, f.owner, true, true)
	require.NoError(t, e)
	require.Equal(t, before, after)
	require.NoError(t, verifyProjection(ctx, f.owner, f.m))
	a, e := AuditOutcome(ctx, f.owner, f.m, f.runtime)
	require.NoError(t, e)
	require.Equal(t, OutcomeExact, a)
	all := f.full(t)
	_, e = MergeFixture(ctx, f.loader, f.owner, f.m, f.runtime)
	require.Error(t, e)
	require.Equal(t, all, f.full(t))
	t.Logf("atomic whole-load rows=%d chunks=%d lock=%s elapsed=%s pid=%d", r.Rows, r.Chunks, r.LockWait, r.Duration, r.BackendPID)
}
func TestPG2AllThirteenPreexistingCohortsRefuse(t *testing.T) {
	for _, tb := range allTables {
		if tb.name == "eras" {
			continue
		}
		t.Run(tb.name, func(t *testing.T) {
			f := newFixture(t)
			fixtureInsert(t, f, tb.name)
			before := f.full(t)
			_, e := MergeFixture(context.Background(), f.loader, f.owner, f.m, f.runtime)
			require.Error(t, e)
			require.Equal(t, before, f.full(t))
		})
	}
}

func TestPG5LocksCompetitionAndPublicConcurrency(t *testing.T) {
	for _, kind := range []string{"old-advisory", "destination-advisory", "legacy-writer", "table-ddl"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			before := f.full(t)
			c, e := f.owner.Conn(context.Background())
			require.NoError(t, e)
			defer c.Close()
			tx, e := c.BeginTx(context.Background(), nil)
			require.NoError(t, e)
			defer tx.Rollback()
			switch kind {
			case "old-advisory":
				_, e = tx.Exec("SELECT pg_advisory_xact_lock(340310)")
			case "destination-advisory":
				_, e = tx.Exec("SELECT pg_advisory_xact_lock($1)", DestinationLock(f.m.Destination))
			case "legacy-writer":
				_, e = tx.Exec("LOCK legacy.blocks IN ROW EXCLUSIVE MODE")
			case "table-ddl":
				_, e = tx.Exec("LOCK legacy.taxable_events IN ACCESS EXCLUSIVE MODE")
			}
			require.NoError(t, e)
			start := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, e = MergeFixture(ctx, f.loader, f.owner, f.m, f.runtime)
			require.Error(t, e)
			require.Less(t, time.Since(start), 1500*time.Millisecond)
			require.NoError(t, tx.Rollback())
			requireEmptyUnchanged(t, f, before)
			t.Logf("bounded contention %s refused in %s", kind, time.Since(start))
		})
	}
	t.Run("two-promoters-and-public-writes", func(t *testing.T) {
		f := newFixture(t)
		mustExec(t, f.owner, "ALTER ROLE "+f.publicWriter+" PASSWORD '${PRESERVATION_REDACTED}'")
		public := fixtureDB(t, f, f.publicWriter)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		done := make(chan error, 1)
		go func() {
			_, e := merge(context.Background(), f.loader, f.owner, f.m, f.runtime, hooks{afterChunk: func(n int) error { once.Do(func() { close(entered); <-release }); return nil }})
			done <- e
		}()
		<-entered
		var held int
		require.NoError(t, f.owner.QueryRow(`SELECT count(DISTINCT l.relation) FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid JOIN pg_class c ON c.oid=l.relation JOIN pg_namespace ns ON ns.oid=c.relnamespace WHERE a.datname=$1 AND a.usename=$2 AND l.mode='ShareRowExclusiveLock' AND l.granted AND ns.nspname='legacy'`, f.m.Destination.Database, f.m.Scope.Loader).Scan(&held))
		require.Equal(t, 14, held)
		var viewLocks int
		require.NoError(t, f.owner.QueryRow(`SELECT count(DISTINCT l.relation) FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid JOIN pg_class c ON c.oid=l.relation JOIN pg_namespace ns ON ns.oid=c.relnamespace WHERE a.datname=$1 AND a.usename=$2 AND l.mode='RowExclusiveLock' AND l.granted AND ns.nspname=$3`, f.m.Destination.Database, f.m.Scope.Loader, f.m.Scope.Schema).Scan(&viewLocks))
		require.Equal(t, 10, viewLocks)
		ddl, e := f.owner.Begin()
		require.NoError(t, e)
		_, e = ddl.Exec("SET LOCAL lock_timeout='100ms'; ALTER VIEW " + f.m.Scope.Schema + ".taxable_events RESET(check_option)")
		require.Error(t, e)
		require.NoError(t, ddl.Rollback())
		writerTx, e := f.owner.Begin()
		require.NoError(t, e)
		_, e = writerTx.Exec(`SET LOCAL lock_timeout='100ms'; INSERT INTO legacy.blocks SELECT chain_id,2,time,block_hash,proposer,0 FROM legacy.blocks WHERE chain_id='cosmoshub-3' AND height=1`)
		require.Error(t, e)
		require.NoError(t, writerTx.Rollback())
		start := time.Now()
		var n int
		require.NoError(t, public.QueryRow("SELECT count(*) FROM public.protected_fixture").Scan(&n))
		require.Equal(t, 1, n)
		mustExec(t, public, "INSERT INTO public.protected_fixture VALUES(2,'legitimate-concurrent-append')")
		require.Less(t, time.Since(start), time.Second)
		_, e = MergeFixture(context.Background(), f.loader, f.owner, f.m, f.runtime)
		require.Error(t, e)
		close(release)
		require.NoError(t, <-done)
		require.NoError(t, public.QueryRow("SELECT count(*) FROM public.protected_fixture WHERE id=1 AND payload='synthetic-protected'").Scan(&n))
		require.Equal(t, 1, n)
		require.NoError(t, public.QueryRow("SELECT count(*) FROM public.protected_fixture").Scan(&n))
		require.Equal(t, 2, n)
		t.Logf("ordinary unrelated public SELECT+append latency=%s", time.Since(start))
	})
}
func TestPG5FreshPostLockAuditorDetectsRRAdmissionRace(t *testing.T) {
	f := newFixture(t)
	var expected Snapshot
	_, e := merge(context.Background(), f.loader, f.owner, f.m, f.runtime, hooks{beforeLocks: func() error {
		mustExec(t, f.owner, `INSERT INTO legacy.blocks SELECT chain_id,2,time,block_hash,proposer,0 FROM legacy.blocks WHERE chain_id='cosmoshub-3' AND height=1`)
		expected = f.full(t)
		return nil
	}})
	require.ErrorContains(t, e, "RR snapshot predates lock-protected baseline")
	require.NoError(t, emptyHub2(context.Background(), f.owner))
	require.Equal(t, expected, f.full(t))
}

func TestPG6WholeLoadRollbackLateFailuresCancelAndSessionDeath(t *testing.T) {
	for _, kind := range []string{"late-chunk", "last-table-SQL", "cancel", "pinned-session-death"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			before := f.full(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := hooks{}
			switch kind {
			case "late-chunk":
				h.afterChunk = func(n int) error {
					if n == 7 {
						return errors.New("retained injected late failure")
					}
					return nil
				}
			case "last-table-SQL":
				mustExec(t, f.owner, `ALTER TABLE legacy.taxable_events ADD CONSTRAINT private_last_table_failure CHECK (chain_id <> 'cosmoshub-2')`)
				refreshIdentity(t, &f)
				before = f.full(t)
			case "cancel":
				h.afterChunk = func(n int) error {
					if n == 7 {
						cancel()
					}
					return nil
				}
			case "pinned-session-death":
				h.afterChunk = func(n int) error {
					if n == 7 {
						var pid int
						require.NoError(t, f.owner.QueryRow(`SELECT pid FROM pg_stat_activity WHERE datname=$1 AND usename=$2 AND state='idle in transaction'`, f.m.Destination.Database, f.m.Scope.Loader).Scan(&pid))
						var killed bool
						require.NoError(t, f.owner.QueryRow("SELECT pg_terminate_backend($1)", pid).Scan(&killed))
						require.True(t, killed)
					}
					return nil
				}
			}
			_, e := merge(ctx, f.loader, f.owner, f.m, f.runtime, h)
			require.Error(t, e)
			require.Eventually(t, func() bool { return emptyHub2(context.Background(), f.owner) == nil }, 10*time.Second, 100*time.Millisecond)
			requireEmptyUnchanged(t, f, before)
		})
	}
}
func TestPG7UnknownCommitReadOnlyExactEmptyConflict(t *testing.T) {
	for _, kind := range []string{"committed", "rolled-back", "mixed"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			before := f.full(t)
			if kind == "mixed" {
				fixtureInsert(t, f, "genesis_files")
			} else {
				_, e := merge(context.Background(), f.loader, f.owner, f.m, f.runtime, hooks{commit: func(tx *sql.Tx) error {
					if kind == "committed" {
						require.NoError(t, tx.Commit())
					} else {
						require.NoError(t, tx.Rollback())
					}
					return errors.New("simulated lost COMMIT acknowledgement")
				}})
				require.ErrorIs(t, e, ErrCommitUnknown)
			}
			immediatelyBefore := f.full(t)
			out, e := AuditOutcome(context.Background(), f.owner, f.m, f.runtime)
			require.NoError(t, e)
			expected := map[string]Outcome{"committed": OutcomeExact, "rolled-back": OutcomeEmpty, "mixed": OutcomeConflict}[kind]
			require.Equal(t, expected, out)
			require.Equal(t, immediatelyBefore, f.full(t))
			if kind == "rolled-back" {
				require.Equal(t, before, f.full(t))
			}
		})
	}
}
func TestPG8MutatingAndMalformedSourceRefusals(t *testing.T) {
	f := newFixture(t)
	before := f.full(t)
	cases := []string{"missing-tail", "corrupt-tail", "dropped-column", "gap-header", "duplicate-header", "wrong-component-join", "intermediate-export", "duplicate-canonical", "missing-canonical", "tail-in-migration", "missing-native-pin", "source-mutation"}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			m := cloneManifest(f.m)
			h := hooks{}
			switch kind {
			case "missing-tail":
				rewriteTable(t, f, &m, "blocks", func(s []string) []string { return s[:len(s)-1] })
			case "corrupt-tail":
				rewriteTable(t, f, &m, "blocks", func(s []string) []string { s[len(s)-1] = "{bad"; return s })
			case "dropped-column":
				m.Tables[1].Columns = m.Tables[1].Columns[:len(m.Tables[1].Columns)-1]
			case "gap-header":
				rewriteTable(t, f, &m, "blocks", func(s []string) []string { return append(s[:2], s[3:]...) })
			case "duplicate-header":
				rewriteTable(t, f, &m, "blocks", func(s []string) []string { s[2] = s[1]; return s })
			case "wrong-component-join":
				c := m.Components[0]
				c.Last = 3
				m.Components[0] = c
				c.First = 4
				c.Last = 5
				c.Opening = digest([]byte("wrong"))
				m.Components = append(m.Components, c)
			case "intermediate-export":
				rewriteTable(t, f, &m, "taxable_events", func(s []string) []string { s[1] = strings.Replace(s[1], `"height": 3`, `"height": 2`, 1); return s })
			case "duplicate-canonical":
				rewriteTable(t, f, &m, "taxable_events", func(s []string) []string { return append(s, s[1]) })
			case "missing-canonical":
				rewriteTable(t, f, &m, "taxable_events", func(s []string) []string { return s[:1] })
			case "tail-in-migration":
				m.MigrationLast = 5
			case "missing-native-pin":
				m.Financial = Pin{}
			case "source-mutation":
				b, e := os.ReadFile(m.Source.Path)
				require.NoError(t, e)
				m.Source = savePin(t, f.dir, "mutating-source-"+randomID()+".json", b)
				h.afterChunk = func(n int) error {
					if n == 7 {
						return os.WriteFile(m.Source.Path, []byte(`{"mutated":true}`), 0600)
					}
					return nil
				}
			}
			_, e := merge(context.Background(), f.loader, f.owner, m, f.runtime, h)
			require.Error(t, e)
			requireEmptyUnchanged(t, f, before)
		})
	}
}
