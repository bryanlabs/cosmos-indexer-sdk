// Retained synthetic fixtures, including event guards. Destructive source
// proofs are confined to rolled-back transactions.
//
//nolint:goconst
package taxapi

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func summaryDDLFill(t *testing.T, s *Server, rebuild bool) {
	t.Helper()
	_, err := FillStatementSummary(context.Background(), s.db, rebuild, summaryOpts(summaryDay(2026, 11, 13)))
	require.NoError(t, err)
	require.True(t, summaryPlan(t, s, statementPGBig).Summary)
}

func summaryDDLWithdrawn(t *testing.T, db *gorm.DB) {
	t.Helper()
	var withdrawn bool
	require.NoError(t, db.Raw("SELECT built_before IS NULL AND metadata_fingerprint IS NULL FROM statement_state").Row().Scan(&withdrawn))
	require.True(t, withdrawn, "DDL withdrawal must atomically clear both trust fields")
}

func TestStatementSummaryDDLEmptyParentLatchPostgres(t *testing.T) {
	s := summaryPGServer(t)
	peer := summaryPGPeer(t, s)
	summaryDDLFill(t, s, false)
	before := summaryCount(t, s.db, "SELECT count(*) FROM ONLY taxable_events")
	fingerprint, err := currentSummaryFingerprint(s.db)
	require.NoError(t, err)
	summaryExec(t, peer, "CREATE TABLE retained_empty_parent ()")
	summaryExec(t, peer, "ALTER TABLE taxable_events INHERIT retained_empty_parent")
	summaryDDLWithdrawn(t, peer)

	// Parent DELETE bypasses the child's statement logger. The guard has
	// already withdrawn the summary, even inside the deleting transaction.
	tx := peer.Begin()
	require.NoError(t, tx.Error)
	defer func() { _ = tx.Rollback().Error }()
	summaryDDLWithdrawn(t, tx)
	require.Equal(t, before, summaryExec(t, tx, "DELETE FROM retained_empty_parent"))
	require.Zero(t, summaryCount(t, tx, "SELECT count(*) FROM ONLY taxable_events"))
	summaryDDLWithdrawn(t, tx)
	require.NoError(t, tx.Rollback().Error)
	require.Equal(t, before, summaryCount(t, peer, "SELECT count(*) FROM ONLY taxable_events"))

	summaryExec(t, peer, "ALTER TABLE taxable_events NO INHERIT retained_empty_parent")
	summaryDDLWithdrawn(t, peer)
	afterFingerprint, err := currentSummaryFingerprint(s.db)
	require.NoError(t, err)
	require.Equal(t, fingerprint, afterFingerprint, "empty inheritance round trip defeats catalog-only continuity")
	require.False(t, summaryPlan(t, s, statementPGBig).Summary, "restoring plain catalogs cannot restore old trust")
	require.Zero(t, requireSummaryEqualsBase(t, s, "empty parent detached"))
	summaryDDLFill(t, s, true)
	require.Positive(t, requireSummaryEqualsBase(t, s, "explicit rebuild after empty parent"))
}

func TestStatementSummaryDDLPopulatedParentUpdatePostgres(t *testing.T) {
	s := summaryPGServer(t)
	peer := summaryPGPeer(t, s)
	summaryDDLFill(t, s, false)
	summaryExec(t, peer, "CREATE TABLE retained_populated_parent (LIKE taxable_events)")
	summaryExec(t, peer, "INSERT INTO retained_populated_parent SELECT * FROM ONLY taxable_events WHERE id = (SELECT min(id) FROM ONLY taxable_events)")
	summaryExec(t, peer, "ALTER TABLE taxable_events INHERIT retained_populated_parent")
	summaryDDLWithdrawn(t, peer)
	require.Positive(t, summaryExec(t, peer, "UPDATE retained_populated_parent SET amount = (amount::numeric + 1)::text WHERE category IN ('reward','commission')"))
	summaryExec(t, peer, "ALTER TABLE taxable_events NO INHERIT retained_populated_parent")
	summaryDDLWithdrawn(t, peer)
	require.Zero(t, requireSummaryEqualsBase(t, s, "parent update retained after detach"))
	summaryDDLFill(t, s, true)
	require.Positive(t, requireSummaryEqualsBase(t, s, "rebuild includes parent update"))
}

func TestStatementSummaryDDLGuardContinuityPostgres(t *testing.T) {
	s := summaryPGServer(t)
	peer := summaryPGPeer(t, s)
	summaryDDLFill(t, s, false)
	schema, oid, err := statementDDLIdentity(peer)
	require.NoError(t, err)
	name := statementDDLName(oid)

	summaryExec(t, peer, "ALTER EVENT TRIGGER "+name+" DISABLE")
	trust, err := readSummaryTrust(s.db)
	require.NoError(t, err)
	require.False(t, trust.DDLGuard)
	_, err = FillStatementSummary(context.Background(), s.db, true, summaryOpts(summaryDay(2026, 11, 13)))
	require.ErrorContains(t, err, "DDL guard definition differs")
	summaryExec(t, peer, "ALTER EVENT TRIGGER "+name+" ENABLE ALWAYS")
	require.Contains(t, summaryPlan(t, s, statementPGBig).Reason, "metadata changed")
	summaryDDLFill(t, s, true)

	correct := statementDDLDefinition(schema, oid)
	summaryExec(t, peer, fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s.statement_guard_ddl() RETURNS event_trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '2s'
AS $fn$ BEGIN RETURN; END $fn$`, schema))
	_, err = FillStatementSummary(context.Background(), s.db, true, summaryOpts(summaryDay(2026, 11, 13)))
	require.ErrorContains(t, err, "DDL guard definition differs")
	summaryExec(t, peer, correct)
	require.Contains(t, summaryPlan(t, s, statementPGBig).Reason, "metadata changed", "restoring a no-op guard must not restore old trust")
	summaryDDLFill(t, s, true)
	require.Positive(t, requireSummaryEqualsBase(t, s, "guard explicitly rebuilt"))
}

func TestStatementSummaryDDLUnrelatedMaintenanceAndIsolationPostgres(t *testing.T) {
	a := summaryPGServer(t)
	peer := summaryPGPeer(t, a)
	summaryDDLFill(t, a, false)
	var stateVersion string
	require.NoError(t, a.db.Raw("SELECT xmin::text FROM statement_state").Row().Scan(&stateVersion))
	fingerprint, err := currentSummaryFingerprint(a.db)
	require.NoError(t, err)

	// A different source's installation must not replace a global function
	// or mutate the first source's state, even as its logger DDL executes.
	b := summaryPGServer(t)
	summaryDDLFill(t, b, false)
	summaryExec(t, peer, "CREATE TABLE retained_unrelated (id bigint)")
	summaryExec(t, peer, "ALTER TABLE retained_unrelated ADD COLUMN note text")
	summaryExec(t, peer, "VACUUM FULL taxable_events")
	summaryExec(t, peer, "CLUSTER taxable_events USING taxable_events_pkey")
	summaryExec(t, peer, "VACUUM FULL statement_state")
	summaryExec(t, peer, "CLUSTER statement_state USING statement_state_pkey")
	var afterVersion string
	require.NoError(t, a.db.Raw("SELECT xmin::text FROM statement_state").Row().Scan(&afterVersion))
	require.Equal(t, stateVersion, afterVersion, "unrelated DDL and maintenance must not even write source state")
	afterFingerprint, err := currentSummaryFingerprint(a.db)
	require.NoError(t, err)
	require.Equal(t, fingerprint, afterFingerprint)
	require.True(t, summaryPlan(t, a, statementPGBig).Summary)
	require.True(t, summaryPlan(t, b, statementPGBig).Summary)

	summaryExec(t, b.db, "ALTER TABLE taxable_events ADD COLUMN retained_extra text")
	summaryDDLWithdrawn(t, b.db)
	require.True(t, summaryPlan(t, a, statementPGBig).Summary)
	require.NoError(t, a.db.Raw("SELECT xmin::text FROM statement_state").Row().Scan(&afterVersion))
	require.Equal(t, stateVersion, afterVersion)
	require.Positive(t, requireSummaryEqualsBase(t, a, "independent source and physical maintenance"))
}

func TestStatementSummaryDDLReinstallAndCollisionPostgres(t *testing.T) {
	s := summaryPGServer(t)
	summaryDDLFill(t, s, false)
	schema, oid, err := statementDDLIdentity(s.db)
	require.NoError(t, err)
	name := statementDDLName(oid)
	summaryExec(t, s.db, "ALTER EVENT TRIGGER "+name+" DISABLE")
	require.NoError(t, InstallStatementSummary(context.Background(), s.db, 0, 1, nil))
	summaryDDLWithdrawn(t, s.db)
	matches, err := statementDDLGuardMatches(s.db)
	require.NoError(t, err)
	require.True(t, matches)
	summaryDDLFill(t, s, true)

	// No event drops: rename and retain the genuine guard, then retain a
	// deliberately conflicting event. Installation must reject the latter.
	summaryExec(t, s.db, "ALTER EVENT TRIGGER "+name+" RENAME TO "+name+"_retained")
	trust, err := readSummaryTrust(s.db)
	require.NoError(t, err)
	require.False(t, trust.DDLGuard, "missing expected global event name must fail closed")
	_, err = FillStatementSummary(context.Background(), s.db, true, summaryOpts(summaryDay(2026, 11, 13)))
	require.ErrorContains(t, err, "DDL guard definition differs")
	summaryExec(t, s.db, "ALTER EVENT TRIGGER "+name+"_retained RENAME TO "+name)
	require.Contains(t, summaryPlan(t, s, statementPGBig).Reason, "metadata changed")
	summaryDDLFill(t, s, true)
	summaryExec(t, s.db, "ALTER EVENT TRIGGER "+name+" RENAME TO "+name+"_retained")
	summaryExec(t, s.db, "CREATE EVENT TRIGGER "+name+" ON ddl_command_end WHEN TAG IN ('CREATE TABLE') EXECUTE FUNCTION "+schema+".statement_guard_ddl()")
	err = InstallStatementSummary(context.Background(), s.db, 0, 1, nil)
	require.ErrorContains(t, err, "collides with an unexpected function or event definition")
	require.False(t, summaryPlan(t, s, statementPGBig).Summary)
}

func TestStatementSummaryDDLTemporaryInheritanceDuringFillPostgres(t *testing.T) {
	s := summaryPGServer(t)
	peer := summaryPGPeer(t, s)
	summaryExec(t, peer, "CREATE TABLE retained_fill_parent ()")
	summaryPublishHook = func() {
		summaryExec(t, peer, "ALTER TABLE taxable_events INHERIT retained_fill_parent")
		summaryDDLWithdrawn(t, peer)
		summaryExec(t, peer, "ALTER TABLE taxable_events NO INHERIT retained_fill_parent")
	}
	t.Cleanup(func() { summaryPublishHook = nil })
	_, err := FillStatementSummary(context.Background(), s.db, false, summaryOpts(summaryDay(2026, 11, 13)))
	summaryPublishHook = nil
	require.ErrorContains(t, err, "invalidated while filling")
	summaryDDLWithdrawn(t, peer)
	require.Zero(t, requireSummaryEqualsBase(t, s, "temporary inheritance prevents publication"))
	summaryDDLFill(t, s, true)
	require.Positive(t, requireSummaryEqualsBase(t, s, "clean rebuild after temporary inheritance"))
}

func TestStatementSummaryDDLChildAttachmentPostgres(t *testing.T) {
	s := summaryPGServer(t)
	peer := summaryPGPeer(t, s)
	summaryDDLFill(t, s, false)
	summaryExec(t, peer, "CREATE TABLE retained_child (LIKE taxable_events)")
	// The ALTER command targets the child, not our source. pg_inherits
	// must nevertheless latch the source's temporary participation as parent.
	summaryExec(t, peer, "ALTER TABLE retained_child INHERIT taxable_events")
	summaryDDLWithdrawn(t, peer)
	summaryExec(t, peer, "ALTER TABLE retained_child NO INHERIT taxable_events")
	summaryDDLWithdrawn(t, peer)
	require.Zero(t, requireSummaryEqualsBase(t, s, "child attachment and detachment"))
	summaryDDLFill(t, s, true)
	require.Positive(t, requireSummaryEqualsBase(t, s, "clean rebuild after child detached"))
}

func TestStatementSummaryDDLStateRenameLatchPostgres(t *testing.T) {
	s := summaryPGServer(t)
	peer := summaryPGPeer(t, s)
	summaryDDLFill(t, s, false)
	summaryExec(t, peer, "CREATE TABLE retained_state_parent (LIKE taxable_events)")
	summaryExec(t, peer, "ALTER TABLE statement_state RENAME TO retained_moved_state")
	summaryExec(t, peer, "ALTER TABLE taxable_events INHERIT retained_state_parent")
	require.Positive(t, summaryExec(t, peer, "UPDATE retained_state_parent SET amount = (amount::numeric + 1)::text WHERE category IN ('reward','commission')"))
	summaryExec(t, peer, "ALTER TABLE taxable_events NO INHERIT retained_state_parent")
	summaryExec(t, peer, "ALTER TABLE retained_moved_state RENAME TO statement_state")
	summaryDDLWithdrawn(t, peer)
	require.Zero(t, requireSummaryEqualsBase(t, s, "state rename round trip with parent mutation"))
	summaryDDLFill(t, s, true)
	require.Positive(t, requireSummaryEqualsBase(t, s, "rebuild after state rename"))
}

func TestStatementSummaryDDLSchemaRenameContinuityPostgres(t *testing.T) {
	s := summaryPGServer(t)
	peer := summaryPGPeer(t, s)
	summaryDDLFill(t, s, false)
	schema, oid, err := statementDDLIdentity(peer)
	require.NoError(t, err)
	renamed := fmt.Sprintf("retained_guard_schema_%d", oid)
	summaryExec(t, peer, "CREATE TABLE retained_schema_parent (LIKE taxable_events)")
	summaryExec(t, peer, "ALTER SCHEMA "+schema+" RENAME TO "+renamed)
	summaryExec(t, peer, "SET search_path = "+renamed)
	summaryExec(t, peer, "ALTER TABLE taxable_events INHERIT retained_schema_parent")
	require.Positive(t, summaryExec(t, peer, "UPDATE retained_schema_parent SET amount = (amount::numeric + 1)::text WHERE category IN ('reward','commission')"))
	summaryExec(t, peer, "ALTER TABLE taxable_events NO INHERIT retained_schema_parent")
	summaryExec(t, peer, "ALTER SCHEMA "+renamed+" RENAME TO "+schema)
	summaryExec(t, peer, "SET search_path = "+schema)
	require.Contains(t, summaryPlan(t, s, statementPGBig).Reason, "metadata changed")
	require.Zero(t, requireSummaryEqualsBase(t, s, "schema rename round trip with parent mutation"))
	summaryDDLFill(t, s, true)
	require.Positive(t, requireSummaryEqualsBase(t, s, "rebuild after schema rename"))
}

func TestStatementSummaryDDLWrongFunctionCollisionPostgres(t *testing.T) {
	s := summaryPGServer(t)
	summaryDDLFill(t, s, false)
	schema, oid, err := statementDDLIdentity(s.db)
	require.NoError(t, err)
	name := statementDDLName(oid)
	summaryExec(t, s.db, "ALTER EVENT TRIGGER "+name+" RENAME TO "+name+"_retained")
	summaryExec(t, s.db, "CREATE FUNCTION "+schema+".retained_wrong_guard() RETURNS event_trigger LANGUAGE plpgsql AS $fn$ BEGIN RETURN; END $fn$")
	summaryExec(t, s.db, "CREATE EVENT TRIGGER "+name+" ON ddl_command_end WHEN TAG IN ('ALTER TABLE') EXECUTE FUNCTION "+schema+".retained_wrong_guard()")
	summaryExec(t, s.db, "ALTER EVENT TRIGGER "+name+" ENABLE ALWAYS")
	_, err = FillStatementSummary(context.Background(), s.db, true, summaryOpts(summaryDay(2026, 11, 13)))
	require.ErrorContains(t, err, "DDL guard definition differs")
	err = InstallStatementSummary(context.Background(), s.db, 0, 1, nil)
	require.ErrorContains(t, err, "collides with an unexpected function or event definition")
	require.False(t, summaryPlan(t, s, statementPGBig).Summary)
}
