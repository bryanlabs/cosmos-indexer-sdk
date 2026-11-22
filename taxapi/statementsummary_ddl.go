package taxapi

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

const statementDDLFunction = "statement_guard_ddl"

// Each physical source owns a database-global event name and a schema-local
// function. There is no shared function to replace when installing another
// source. Keep guards even when a source or its summary is retired.
func statementDDLIdentity(tx *gorm.DB) (string, uint32, error) {
	schema, err := summarySchema(tx)
	if err != nil {
		return "", 0, err
	}
	var oid uint32
	err = tx.Raw("SELECT to_regclass('taxable_events')::oid").Row().Scan(&oid)
	return schema, oid, err
}

func statementDDLName(oid uint32) string {
	return fmt.Sprintf("statement_ddl_%d", oid)
}

func statementDDLDefinition(schema string, oid uint32) string {
	literal := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	return fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s.statement_guard_ddl() RETURNS event_trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
SET lock_timeout = '2s'
AS $fn$
BEGIN
	-- statement DDL guard v1: inheritance can change without catalog xmin changes.
	IF to_regclass(%s) IS DISTINCT FROM %d::oid
		OR to_regclass(%s) IS NULL THEN
		RETURN;
	END IF;
	-- Also latch restoration of a temporarily renamed state table. While
	-- its name is absent this guard cannot withdraw, and parent DML can
	-- bypass the source's statement logger.
	IF EXISTS (SELECT 1 FROM pg_event_trigger_ddl_commands() d
		WHERE d.classid = 'pg_catalog.pg_class'::regclass
		AND (d.objid = %d::oid OR d.objid = to_regclass(%s)))
		OR EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhparent = %d::oid OR i.inhrelid = %d::oid) THEN
		UPDATE %s.statement_state SET built_before = NULL, metadata_fingerprint = NULL, updated_at = now();
	END IF;
END
$fn$`, schema, literal(schema+".taxable_events"), oid, literal(schema+".statement_state"), oid, literal(schema+".statement_state"), oid, oid, schema)
}

func installStatementDDLGuard(tx *gorm.DB, schema string) error {
	var super bool
	if err := tx.Raw("SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Row().Scan(&super); err != nil {
		return err
	}
	if !super {
		return fmt.Errorf("statement DDL guard installation requires superuser")
	}
	var oid uint32
	if err := tx.Raw("SELECT to_regclass('taxable_events')::oid").Row().Scan(&oid); err != nil {
		return err
	}
	name := statementDDLName(oid)
	var exists, matches bool
	if err := tx.Raw(`SELECT EXISTS (SELECT 1 FROM pg_event_trigger WHERE evtname = ?),
		COALESCE((SELECT evtevent = 'ddl_command_end' AND evtfoid = to_regprocedure(?)
		AND evttags = ARRAY['ALTER TABLE']::text[] FROM pg_event_trigger WHERE evtname = ?), false)`,
		name, schema+"."+statementDDLFunction+"()", name).Row().Scan(&exists, &matches); err != nil {
		return err
	}
	if exists && !matches {
		return fmt.Errorf("statement DDL guard %s collides with an unexpected function or event definition", name)
	}
	if err := tx.Exec(statementDDLDefinition(schema, oid)).Error; err != nil {
		return err
	}
	if !exists {
		if err := tx.Exec("CREATE EVENT TRIGGER " + name + " ON ddl_command_end WHEN TAG IN ('ALTER TABLE') EXECUTE FUNCTION " + schema + "." + statementDDLFunction + "()").Error; err != nil {
			return err
		}
	}
	return tx.Exec("ALTER EVENT TRIGGER " + name + " ENABLE ALWAYS").Error
}

// Presence is checked independently of continuity in the API's snapshot.
// Fill additionally authenticates the exact body before recording a baseline.
const summaryDDLGuardPresenceSQL = `EXISTS (SELECT 1 FROM pg_event_trigger e JOIN pg_proc p ON p.oid = e.evtfoid
 WHERE e.evtname = 'statement_ddl_' || c.oid::text AND e.evtevent = 'ddl_command_end'
 AND e.evtenabled = 'A' AND e.evttags = ARRAY['ALTER TABLE']::text[]
 AND p.pronamespace = c.relnamespace AND p.proname = 'statement_guard_ddl' AND p.pronargs = 0
 AND p.prorettype = 'pg_catalog.event_trigger'::regtype AND p.prokind = 'f' AND NOT p.proretset
 AND p.prosecdef AND p.prolang = (SELECT oid FROM pg_language WHERE lanname = 'plpgsql')
 AND p.proconfig @> ARRAY['search_path=pg_catalog, pg_temp','lock_timeout=2s']::text[] AND cardinality(p.proconfig) = 2)`

// Include function, namespace and event versions, not just restored text.
// A schema rename can temporarily make the qualified guard body inert without
// an ALTER TABLE event. Namespace continuity rejects even a rename round trip.
// VACUUM FULL and CLUSTER do not change these objects and are not ALTER TABLE.
const summaryDDLGuardFingerprintSQL = `(SELECT p.oid::text || ':' || p.xmin::text || ':' || n.oid::text || ':' || n.xmin::text || ':' || pg_get_functiondef(p.oid)
 FROM pg_proc p JOIN pg_class c ON c.relnamespace = p.pronamespace JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.oid = to_regclass('taxable_events') AND p.proname = 'statement_guard_ddl' AND p.pronargs = 0),
 (SELECT e.oid::text || ':' || e.xmin::text || ':' || e.evtevent || ':' || e.evtfoid::text || ':' || e.evtenabled::text || ':' || e.evttags::text
 FROM pg_event_trigger e WHERE e.evtname = 'statement_ddl_' || to_regclass('taxable_events')::oid::text)`

func statementDDLGuardMatches(tx *gorm.DB) (bool, error) {
	schema, oid, err := statementDDLIdentity(tx)
	if err != nil {
		return false, err
	}
	body := strings.Split(statementDDLDefinition(schema, oid), "$fn$")[1]
	var matches bool
	err = tx.Raw(`SELECT `+summaryDDLGuardPresenceSQL+` AND COALESCE((SELECT p.prosrc = ?
		FROM pg_proc p WHERE p.oid = to_regprocedure(?)), false)
		FROM pg_class c WHERE c.oid = to_regclass('taxable_events')`, body, schema+"."+statementDDLFunction+"()").Row().Scan(&matches)
	return matches, err
}
