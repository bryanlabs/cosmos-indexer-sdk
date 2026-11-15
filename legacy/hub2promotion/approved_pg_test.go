package hub2promotion

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

func TestPGApprovedStaticChainsAtomicPreservationAndRollback(t *testing.T) {
	for _, chain := range []string{"cosmoshub-1", "cosmoshub-2"} {
		t.Run(chain, func(t *testing.T) {
			f := newFixtureForChain(t, chain)
			ctx := context.Background()
			sibling := "cosmoshub-1"
			if chain == sibling {
				sibling = "cosmoshub-2"
			}
			// Materially populate the other approved cohort too. A Hub1 load must
			// preserve Hub2 just as a Hub2 load preserves existing Hub1, Hub3 and Hub4.
			for _, tb := range inserts {
				raw := canonicalRow(t, f.owner, tb, fixtureRow(tb.name, sibling, digest([]byte(sibling))))
				_, err := f.loader.Exec("INSERT INTO "+f.m.Scope.Schema+"."+tb.name+" SELECT * FROM pg_catalog.jsonb_populate_record(NULL::legacy."+tb.name+",$1::jsonb)", raw)
				var pgerr *pgconn.PgError
				require.ErrorAs(t, err, &pgerr)
				require.Equal(t, "44000", pgerr.Code, "fresh sibling key must fail its CHECK OPTION, not a duplicate or FK")
				mustExec(t, f.owner, "INSERT INTO legacy."+tb.name+" SELECT * FROM pg_catalog.jsonb_populate_record(NULL::legacy."+tb.name+",$1::jsonb)", raw)
			}
			var e error
			f.m.Protected, e = snapshotChain(ctx, f.owner, chain, true, true)
			require.NoError(t, e)
			before := f.full(t)
			_, e = merge(ctx, f.loader, f.owner, f.m, f.runtime, hooks{afterChunk: func(n int) error {
				if n >= 4 {
					return errors.New("synthetic late failure")
				}
				return nil
			}})
			require.Error(t, e)
			require.NoError(t, emptyChain(ctx, f.owner, chain))
			require.Equal(t, before, f.full(t))
			result, e := MergeFixture(ctx, f.loader, f.owner, f.m, f.runtime)
			require.NoError(t, e)
			require.Positive(t, result.Rows)
			after, e := snapshotChain(ctx, f.owner, chain, true, true)
			require.NoError(t, e)
			require.Equal(t, f.m.Protected, after)
			outcome, e := AuditOutcome(ctx, f.owner, f.m, f.runtime)
			require.NoError(t, e)
			require.Equal(t, OutcomeExact, outcome)
			require.NoError(t, verifyCommitted(ctx, f.owner, f.m, f.runtime))
			wrong := cloneManifest(f.m)
			fp := wrong.Protected["blocks"]
			fp.Rows++
			wrong.Protected["blocks"] = fp
			require.ErrorIs(t, verifyCommitted(ctx, f.owner, wrong, f.runtime), ErrCommittedUnverified)
			committed := f.full(t)
			_, e = MergeFixture(ctx, f.loader, f.owner, f.m, f.runtime)
			require.Error(t, e)
			require.Equal(t, committed, f.full(t))
		})
	}
}

func TestPGApprovedStaticChainsAll13CohortsRefuse(t *testing.T) {
	for _, chain := range []string{"cosmoshub-1", "cosmoshub-2"} {
		t.Run(chain, func(t *testing.T) {
			for _, tb := range allTables {
				if tb.name == "eras" {
					continue
				}
				t.Run(tb.name, func(t *testing.T) {
					f := newFixtureForChain(t, chain)
					fixtureInsert(t, f, tb.name)
					before := f.full(t)
					_, e := MergeFixture(context.Background(), f.loader, f.owner, f.m, f.runtime)
					require.Error(t, e)
					require.Equal(t, before, f.full(t))
				})
			}
		})
	}
}
