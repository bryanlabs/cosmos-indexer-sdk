package hub2promotion

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPG3FreshAuditorDetectsPostBaselinePrivilegeAndFunctionDrift(t *testing.T) {
	for _, kind := range []string{"outside-column-grant", "new-public-secdef", "helper-acl"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			before := f.full(t)
			_, e := merge(context.Background(), f.loader, f.owner, f.m, f.runtime, hooks{afterChunk: func(n int) error {
				if n != 7 {
					return nil
				}
				switch kind {
				case "outside-column-grant":
					mustExec(t, f.owner, "GRANT SELECT(payload) ON public.protected_fixture TO "+f.m.Scope.Loader)
				case "new-public-secdef":
					mustExec(t, f.owner, "CREATE FUNCTION public.late_unsafe_definer() RETURNS integer LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'")
				case "helper-acl":
					mustExec(t, f.owner, "GRANT EXECUTE ON FUNCTION "+f.m.Scope.Schema+".lock_legacy() TO PUBLIC")
				}
				return nil
			}})
			require.Error(t, e)
			requireEmptyUnchanged(t, f, before)
		})
	}
}
func TestPG7SessionDefaultAsyncCommitCannotWeakenDurability(t *testing.T) {
	f := newFixture(t)
	mustExec(t, f.owner, "ALTER ROLE "+f.m.Scope.Loader+" SET synchronous_commit=off")
	refreshIdentity(t, &f)
	r, e := MergeFixture(context.Background(), f.loader, f.owner, f.m, f.runtime)
	require.NoError(t, e)
	require.Equal(t, "on", r.SynchronousCommit)
	require.Equal(t, "repeatable read", r.Isolation)
}
