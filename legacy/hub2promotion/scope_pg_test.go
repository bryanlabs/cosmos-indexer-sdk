package hub2promotion

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPG3DatabaseEnforcedViewAndPrivilegeBoundary(t *testing.T) {
	f := newFixture(t)
	before := f.full(t)
	for _, tb := range inserts {
		t.Run("other-chain-"+tb.name, func(t *testing.T) {
			raw := canonicalRow(t, f.owner, tb, fixtureRow(tb.name, "cosmoshub-1", f.m.Genesis.SHA256))
			_, e := f.loader.Exec(qualifiedInsert(f.m.Scope, tb), "["+raw+"]")
			require.Error(t, e)
			require.Contains(t, e.Error(), "check option")
		})
	}
	for _, tb := range allTables {
		t.Run("base-DML-"+tb.name, func(t *testing.T) {
			for _, stmt := range []string{"INSERT INTO legacy." + tb.name + " SELECT * FROM legacy." + tb.name + " WHERE false", "UPDATE legacy." + tb.name + " SET chain_id=chain_id WHERE false", "DELETE FROM legacy." + tb.name + " WHERE false", "TRUNCATE legacy." + tb.name} {
				_, e := f.loader.Exec(stmt)
				require.Error(t, e)
				require.Contains(t, e.Error(), "permission denied")
			}
		})
	}
	for _, stmt := range []string{"SELECT * FROM public.protected_fixture", "INSERT INTO public.protected_fixture VALUES(3,'forbidden')", "UPDATE public.protected_fixture SET payload='forbidden' WHERE false", "DELETE FROM public.protected_fixture WHERE false", "TRUNCATE public.protected_fixture", "SET ROLE " + f.m.Scope.ViewOwner, "SET ROLE " + f.m.Scope.LockOwner, "SET ROLE pg_read_all_data", "CREATE TABLE public.escape(id integer)", "ALTER FUNCTION " + f.m.Scope.Schema + ".lock_legacy() OWNER TO " + f.m.Scope.Loader, "SELECT legacy.refuse_change()"} {
		t.Run(stmt, func(t *testing.T) { _, e := f.loader.Exec(stmt); require.Error(t, e) })
	}
	// An alternate definer owned by a privileged account is not executable by
	// the loader. If somebody later grants PUBLIC access, admission rejects it.
	mustExec(t, f.owner, `CREATE FUNCTION public.private_alternate_definer() RETURNS integer LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'; REVOKE ALL ON FUNCTION public.private_alternate_definer() FROM PUBLIC`)
	_, e := f.loader.Exec("SELECT public.private_alternate_definer()")
	require.Error(t, e)
	require.Contains(t, e.Error(), "permission denied")
	require.Equal(t, before, f.full(t))
}
func TestPG3HelperAndScopeDriftRefuseEvenRepinned(t *testing.T) {
	for _, kind := range []string{"helper-body", "helper-owner", "helper-acl", "helper-search-path", "view-chain", "view-check-option", "view-trigger", "alternate-secdef", "alternate-secdef-trigger", "loader-membership", "loader-base-update", "loader-public-select", "loader-public-column-select", "loader-public-column-reference", "owner-login", "owner-membership"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			switch kind {
			case "helper-body":
				mustExec(t, f.owner, "CREATE OR REPLACE FUNCTION "+f.m.Scope.Schema+".lock_legacy() RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS 'BEGIN NULL; END'")
			case "helper-owner":
				mustExec(t, f.owner, "ALTER FUNCTION "+f.m.Scope.Schema+".lock_legacy() OWNER TO "+f.m.Scope.ViewOwner)
			case "helper-acl":
				mustExec(t, f.owner, "GRANT EXECUTE ON FUNCTION "+f.m.Scope.Schema+".lock_legacy() TO PUBLIC")
			case "helper-search-path":
				mustExec(t, f.owner, "ALTER FUNCTION "+f.m.Scope.Schema+".lock_legacy() SET search_path=public")
			case "view-chain":
				tb := inserts[0]
				mustExec(t, f.owner, "CREATE OR REPLACE VIEW "+f.m.Scope.Schema+"."+tb.name+" AS SELECT "+quotedColumns(tb.columns)+" FROM legacy."+tb.name+" WHERE chain_id='cosmoshub-3' WITH CASCADED CHECK OPTION")
			case "view-check-option":
				mustExec(t, f.owner, "ALTER VIEW "+f.m.Scope.Schema+".genesis_files RESET (check_option)")
			case "view-trigger":
				mustExec(t, f.owner, "CREATE FUNCTION "+f.m.Scope.Schema+".test_trigger() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'; REVOKE ALL ON FUNCTION "+f.m.Scope.Schema+".test_trigger() FROM PUBLIC; CREATE TRIGGER bypass INSTEAD OF INSERT ON "+f.m.Scope.Schema+".genesis_files FOR EACH ROW EXECUTE FUNCTION "+f.m.Scope.Schema+".test_trigger()")
			case "alternate-secdef":
				mustExec(t, f.owner, "CREATE FUNCTION public.unsafe_extra() RETURNS integer LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'")
			case "alternate-secdef-trigger":
				mustExec(t, f.owner, "CREATE FUNCTION public.unsafe_trigger() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS 'BEGIN RETURN NEW; END'")
			case "loader-membership":
				mustExec(t, f.owner, "GRANT "+f.m.Scope.LockOwner+" TO "+f.m.Scope.Loader)
			case "loader-base-update":
				mustExec(t, f.owner, "GRANT UPDATE ON legacy.taxable_events TO "+f.m.Scope.Loader)
			case "loader-public-select":
				mustExec(t, f.owner, "GRANT SELECT ON public.protected_fixture TO "+f.m.Scope.Loader)
			case "loader-public-column-select":
				mustExec(t, f.owner, "GRANT SELECT(payload) ON public.protected_fixture TO "+f.m.Scope.Loader)
			case "loader-public-column-reference":
				mustExec(t, f.owner, "GRANT REFERENCES(id) ON public.protected_fixture TO "+f.m.Scope.Loader)
			case "owner-login":
				mustExec(t, f.owner, "ALTER ROLE "+f.m.Scope.LockOwner+" LOGIN")
			case "owner-membership":
				mustExec(t, f.owner, "GRANT pg_write_all_data TO "+f.m.Scope.ViewOwner)
			}
			refreshIdentity(t, &f)
			before := f.full(t)
			_, e := MergeFixture(context.Background(), f.loader, f.owner, f.m, f.runtime)
			require.Error(t, e)
			requireEmptyUnchanged(t, f, before)
		})
	}
}
func TestPG3InheritedPublicTempAndHostileShadowObjects(t *testing.T) {
	f := newFixture(t)
	f.loader.SetMaxOpenConns(1)
	var temp bool
	require.NoError(t, f.loader.QueryRow("SELECT has_database_privilege(current_database(),'TEMP')").Scan(&temp))
	require.True(t, temp, "PUBLIC TEMP is real, not falsely denied")
	for _, tb := range allTables {
		mustExec(t, f.loader, "CREATE TEMP TABLE "+tb.name+"(chain_id text, poison text)")
	}
	mustExec(t, f.loader, "CREATE DOMAIN pg_temp.text AS pg_catalog.text; CREATE DOMAIN pg_temp.jsonb AS pg_catalog.jsonb")
	mustExec(t, f.loader, "SET search_path=pg_temp,public")
	_, e := MergeFixture(context.Background(), f.loader, f.owner, f.m, f.runtime)
	require.NoError(t, e)
	require.NoError(t, verifyProjection(context.Background(), f.owner, f.m))
	var n int
	require.NoError(t, f.loader.QueryRow("SELECT count(*) FROM pg_temp.blocks").Scan(&n))
	require.Zero(t, n)
}
func TestPG4DestinationBuildAndSourceIdentityNegatives(t *testing.T) {
	f := newFixture(t)
	before := f.full(t)
	for _, kind := range []string{"database", "oid", "system", "deployment-uid", "pod-uid", "pvc-uid", "catalog", "scope", "endpoint", "source-sha", "binary", "build", "profile", "column", "layout", "fixture-as-complete"} {
		t.Run(kind, func(t *testing.T) {
			m := cloneManifest(f.m)
			switch kind {
			case "database":
				m.Destination.Database = "legacy_replay_rehearsal_wrong"
			case "oid":
				m.Destination.OID++
			case "system":
				m.Destination.SystemHash = digest([]byte("wrong-system"))
			case "deployment-uid":
				m.Destination.DeploymentUID = "wrong"
			case "pod-uid":
				m.Destination.PodUID = "wrong"
			case "pvc-uid":
				m.Destination.PVCUID = "wrong"
			case "catalog":
				m.Destination.CatalogSHA = digest([]byte("wrong-catalog"))
			case "scope":
				m.Destination.ScopeSHA = digest([]byte("wrong-scope"))
			case "endpoint":
				m.Destination.Endpoint = "wrong-endpoint"
			case "source-sha":
				m.Source.SHA256 = digest([]byte("wrong-source"))
			case "binary":
				m.BinarySHA = ""
			case "build":
				m.BuildIdentity.SHA256 = digest([]byte("counterfeit-clean-build"))
			case "profile":
				m.Profile = CompleteProfile
			case "column":
				m.Tables[0].Columns[0] = "untrusted"
			case "layout":
				m.Tables[0], m.Tables[1] = m.Tables[1], m.Tables[0]
			case "fixture-as-complete":
				m.Fixture = false
				m.Profile = CompleteProfile
			}
			_, e := MergeFixture(context.Background(), f.loader, f.owner, m, f.runtime)
			require.Error(t, e)
			requireEmptyUnchanged(t, f, before)
		})
	}
}

// Production holds the report summary's change logger and its schema-change
// guard in public with default PUBLIC execute. Exactly those two, as
// production defines them, may sit beside the loader. A third function, or
// either name with any other shape, still refuses.
func TestScopeAdmitsOnlyTheTwoReportTrustFunctions(t *testing.T) {
	logger := "CREATE FUNCTION public.statement_log_change() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS 'BEGIN RETURN NULL; END'"
	guard := "CREATE FUNCTION public.statement_guard_ddl() RETURNS event_trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS 'BEGIN RETURN; END'"
	owned := "; ALTER FUNCTION public.statement_log_change() OWNER TO postgres; ALTER FUNCTION public.statement_guard_ddl() OWNER TO postgres"
	cases := []struct{ name, sql, refusal string }{
		{"exactly the two production functions", logger + "; " + guard + owned, ""},
		{"unlisted third function", logger + "; " + guard + owned + "; CREATE FUNCTION public.statement_log_other() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NULL; END'; ALTER FUNCTION public.statement_log_other() OWNER TO postgres", "alternate callable function"},
		{"unlisted third definer function", logger + "; " + guard + owned + "; CREATE FUNCTION public.statement_guard_other() RETURNS event_trigger LANGUAGE plpgsql SECURITY DEFINER AS 'BEGIN RETURN; END'; ALTER FUNCTION public.statement_guard_other() OWNER TO postgres", "alternate callable function"},
		{"listed name with another owner", logger + "; " + guard, "alternate callable function"},
		{"listed name that can be called", "CREATE FUNCTION public.statement_log_change() RETURNS integer LANGUAGE plpgsql SECURITY DEFINER AS 'BEGIN RETURN 1; END'; ALTER FUNCTION public.statement_log_change() OWNER TO postgres", "alternate callable function"},
		{"listed name with an argument", "CREATE FUNCTION public.statement_guard_ddl(integer) RETURNS integer LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'; ALTER FUNCTION public.statement_guard_ddl(integer) OWNER TO postgres", "alternate callable function"},
		{"listed name in another language", "CREATE FUNCTION public.statement_log_change() RETURNS integer LANGUAGE sql AS 'SELECT 1'; ALTER FUNCTION public.statement_log_change() OWNER TO postgres", "alternate callable function"},
		{"listed name in another schema", "CREATE SCHEMA other_reports; CREATE FUNCTION other_reports.statement_log_change() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS 'BEGIN RETURN NULL; END'; ALTER FUNCTION other_reports.statement_log_change() OWNER TO postgres; GRANT USAGE ON SCHEMA other_reports TO PUBLIC", "alternate callable function"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			// The production owner. The role is cluster wide, so a peer test may hold it already.
			mustExec(t, f.owner, "DO $$BEGIN IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='postgres') THEN BEGIN CREATE ROLE postgres NOLOGIN; EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END; END IF; END$$")
			require.NoError(t, CheckScope(context.Background(), f.owner, f.m.Scope))
			mustExec(t, f.owner, tc.sql)
			e := CheckScope(context.Background(), f.owner, f.m.Scope)
			if tc.refusal == "" {
				require.NoError(t, e)
				// A fresh install beside them passes too, and the loader still cannot call either.
				again := Scope{Schema: f.m.Scope.Schema + "b", Loader: f.m.Scope.Loader + "b", ViewOwner: f.m.Scope.ViewOwner + "b", LockOwner: f.m.Scope.LockOwner + "b", ChainID: f.m.Scope.ChainID}
				require.NoError(t, InstallScope(context.Background(), f.owner, again))
				_, e = f.loader.Exec("SELECT public.statement_log_change()")
				require.ErrorContains(t, e, "trigger functions can only be called as triggers")
				_, e = f.loader.Exec("SELECT public.statement_guard_ddl()")
				require.Error(t, e)
				return
			}
			require.ErrorContains(t, e, tc.refusal)
		})
	}
}
