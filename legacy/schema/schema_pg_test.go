package schema_test

import (
	"strings"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/legacy/schema"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema/pgtest"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestSQLRefusesARoleNameThatIsNotAnIdentifier(t *testing.T) {
	for _, name := range []string{"", "Legacy", "a; DROP TABLE txes", "legacy-importer", `"quoted"`} {
		_, err := schema.SQL(name)
		require.Error(t, err, name)
	}
	text, err := schema.SQL(schema.DefaultRole)
	require.NoError(t, err)
	require.NotContains(t, text, "{{role}}")
	require.Contains(t, text, "CREATE ROLE legacy_importer LOGIN")
}

// The migration must not rewrite, lock out or add to anything that existed.
func TestMigrationLeavesTheServingSchemaUntouched(t *testing.T) {
	db := pgtest.Open(t)
	before := pgtest.Snapshot(t, db.Owner, "public")
	var columns, triggers int64
	count := func(target *int64, query string) {
		require.NoError(t, db.Owner.Raw(query).Scan(target).Error)
	}
	const publicColumns = `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public'`
	const publicTriggers = `SELECT count(*) FROM pg_trigger g JOIN pg_class c ON c.oid = g.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND NOT g.tgisinternal`
	count(&columns, publicColumns)
	count(&triggers, publicTriggers)

	// Running it again changes nothing either.
	legacyBefore := pgtest.Snapshot(t, db.Owner, "legacy")
	require.NoError(t, schema.Migrate(db.Owner, db.Role))
	pgtest.RequireUntouched(t, legacyBefore, pgtest.Snapshot(t, db.Owner, "legacy"))

	pgtest.RequireUntouched(t, before, pgtest.Snapshot(t, db.Owner, "public"))
	var columnsAfter, triggersAfter int64
	count(&columnsAfter, publicColumns)
	count(&triggersAfter, publicTriggers)
	require.Equal(t, columns, columnsAfter)
	require.Equal(t, triggers, triggersAfter)

	var eras []string
	require.NoError(t, db.Owner.Raw(`SELECT chain_id || ' ' || era || ' ' || coalesce(export_height::text, 'none') || ' ' || export_paid_rewards || ' ' || published FROM legacy.eras ORDER BY era`).Scan(&eras).Error)
	require.Equal(t, []string{"cosmoshub-1 1 500000 true false", "cosmoshub-2 2 2902000 true false",
		"cosmoshub-3 3 5200790 false false", "cosmoshub-4 4 none false false"}, eras)
}

func TestImporterRoleCanAddLegacyRowsAndReachNothingElse(t *testing.T) {
	db := pgtest.Open(t)
	findings, err := schema.Confined(db.Importer)
	require.NoError(t, err)
	require.Empty(t, findings)

	// The owner of the serving tables is not confined, and the check says so.
	findings, err = schema.Confined(db.Owner)
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	before := pgtest.Snapshot(t, db.Owner, "public")
	denied := func(statement string) {
		t.Helper()
		err := db.Importer.Exec(statement).Error
		require.Error(t, err, statement)
		// 42501 is insufficient privilege: permission denied, or not the owner.
		require.Contains(t, err.Error(), "SQLSTATE 42501", statement)
	}
	for _, statement := range []string{
		`SELECT 1 FROM public.txes`,
		`SELECT 1 FROM public.blocks`,
		`INSERT INTO public.chains (chain_id, name) VALUES ('cosmoshub-3', 'cosmoshub-3')`,
		`UPDATE public.txes SET code = 5`,
		`UPDATE public.blocks SET chain_id = 1`,
		`DELETE FROM public.txes`,
		`TRUNCATE public.txes`,
		`SELECT nextval('public.txes_id_seq')`,
		`CREATE TABLE public.left_behind (id integer)`,
		`CREATE TABLE legacy.left_behind (id integer)`,
		`CREATE SCHEMA another`,
		`ALTER TABLE legacy.blocks ADD COLUMN extra text`,
		`DROP TABLE legacy.blocks`,
		`INSERT INTO legacy.eras (chain_id, era, first_height, export_paid_rewards) VALUES ('cosmoshub-5', 4, 1, false)`,
		`UPDATE legacy.eras SET published = true`,
		`UPDATE legacy.blocks SET proposer = ''`,
		`DELETE FROM legacy.tx_inclusions`,
		`DELETE FROM legacy.taxable_events`,
		`UPDATE legacy.genesis_accounts SET liquid = 0`,
		`TRUNCATE legacy.genesis_accounts`,
		`ALTER TABLE legacy.blocks DISABLE TRIGGER written_once_rows`,
	} {
		denied(statement)
	}

	// What it is for: adding rows.
	hash := strings.Repeat("CD", 32)
	require.NoError(t, db.Importer.Exec(`INSERT INTO legacy.blocks (chain_id, height, time, block_hash, proposer, tx_count)
		VALUES ('cosmoshub-3', 5200791, '2021-02-18T06:00:04Z', ?, 'synthetic', 1)`, hash).Error)
	require.NoError(t, db.Importer.Exec(`INSERT INTO legacy.tx_inclusions (chain_id, height, tx_index, hash, code, code_evidence,
		gas_wanted, gas_used, fee, fee_payer, fee_charged, fee_evidence, memo, signers, raw)
		VALUES ('cosmoshub-3', 5200791, 0, ?, 0, 'synthetic', 1, 1, '', 'synthetic', true, 'synthetic', '', '{}', '\x00')`, hash).Error)
	// The same hash again in a later block is a second inclusion, not a conflict.
	require.NoError(t, db.Importer.Exec(`INSERT INTO legacy.blocks (chain_id, height, time, block_hash, proposer, tx_count)
		VALUES ('cosmoshub-2', 5200791, '2019-12-01T00:00:00Z', ?, 'synthetic', 1)`, hash).Error)
	require.NoError(t, db.Importer.Exec(`INSERT INTO legacy.tx_inclusions (chain_id, height, tx_index, hash, code, code_evidence,
		gas_wanted, gas_used, fee, fee_payer, fee_charged, fee_evidence, memo, signers, raw)
		VALUES ('cosmoshub-2', 5200791, 0, ?, 5, 'synthetic', 1, 1, '', 'synthetic', false, 'synthetic', '', '{}', '\x00')`, hash).Error)
	require.NoError(t, db.Importer.Exec(`INSERT INTO legacy.taxable_events (chain_id, height, phase, tx_index, msg_index, sub_index,
		category, amount, denom, "timestamp", amount_source)
		VALUES ('cosmoshub-3', 5200791, 2, 0, 0, 0, 'transfer', '5', 'uatom', '2021-02-18T06:00:04Z', 'event')`).Error)

	// Rows the schema must refuse whoever writes them.
	for _, statement := range []string{
		`INSERT INTO legacy.blocks (chain_id, height, time, block_hash, proposer, tx_count) VALUES ('cosmoshub-4', 5200791, now(), '` + hash + `', '', 0)`,
		`INSERT INTO legacy.taxable_events (chain_id, height, phase, tx_index, msg_index, sub_index, category, amount, denom, "timestamp", amount_source) VALUES ('cosmoshub-3', 7, 2, 0, 0, 0, 'transfer', '5', 'uatom', now(), 'guess')`,
		`INSERT INTO legacy.taxable_events (chain_id, height, phase, tx_index, msg_index, sub_index, category, amount, denom, "timestamp", amount_source) VALUES ('cosmoshub-3', 7, 2, 0, 0, 0, 'transfer', '-5', 'uatom', now(), 'event')`,
		`INSERT INTO legacy.taxable_events (chain_id, height, phase, tx_index, msg_index, sub_index, category, amount, denom, "timestamp", amount_source) VALUES ('cosmoshub-3', 7, 0, 0, 0, 0, 'transfer', '5', 'uatom', now(), 'genesis')`,
		`INSERT INTO legacy.taxable_events (chain_id, height, phase, tx_index, msg_index, sub_index, category, amount, denom, "timestamp", amount_source) VALUES ('cosmoshub-3', 7, 3, 1, 0, 0, 'transfer', '5', 'uatom', now(), 'event')`,
		`INSERT INTO legacy.taxable_events (chain_id, height, phase, tx_index, msg_index, sub_index, category, amount, denom, "timestamp", amount_source) VALUES ('cosmoshub-3', 5200791, 2, 0, 0, 0, 'transfer', '5', 'uatom', now(), 'event')`,
	} {
		require.Error(t, db.Owner.Exec(statement).Error, statement)
	}

	// Stored chain facts cannot be rewritten by the owner either.
	for _, statement := range []string{
		`UPDATE legacy.blocks SET proposer = 'other'`,
		`DELETE FROM legacy.tx_inclusions`,
		`TRUNCATE legacy.tx_inclusions`,
	} {
		err := db.Owner.Exec(statement).Error
		require.Error(t, err, statement)
		require.Contains(t, err.Error(), "written once", statement)
	}
	pgtest.RequireUntouched(t, before, pgtest.Snapshot(t, db.Owner, "public"))
}

// Each of these is a way out that a check of whole table grants to the role
// alone would miss. The check must see it, and stop seeing it once undone.
func TestConfinedSeesGrantsThatDoNotNameTheRoleOrTheWholeTable(t *testing.T) {
	db := pgtest.Open(t)
	clean := func() {
		t.Helper()
		findings, err := schema.Confined(db.Importer)
		require.NoError(t, err)
		require.Empty(t, findings)
	}
	clean()
	for _, way := range []struct {
		open, finding, close string
		everyRole            bool
	}{
		{`GRANT SELECT (memo) ON public.txes TO ` + db.Role,
			"tables and views outside legacy", `REVOKE SELECT (memo) ON public.txes FROM ` + db.Role, false},
		{`GRANT SELECT ON public.txes TO PUBLIC`,
			"tables and views outside legacy", `REVOKE SELECT ON public.txes FROM PUBLIC`, true},
		{`GRANT UPDATE (amount) ON legacy.taxable_events TO ` + db.Role,
			"written once tables", `REVOKE UPDATE (amount) ON legacy.taxable_events FROM ` + db.Role, false},
		{`CREATE FUNCTION public.rewrite_codes() RETURNS void LANGUAGE sql SECURITY DEFINER AS 'UPDATE public.txes SET code = 5'`,
			"functions that run with their owner's rights", `DROP FUNCTION public.rewrite_codes()`, true},
		{`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO PUBLIC`,
			"default privileges", `ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE SELECT ON TABLES FROM PUBLIC`, true},
		{`ALTER DEFAULT PRIVILEGES GRANT SELECT ON TABLES TO ` + db.Role,
			"default privileges", `ALTER DEFAULT PRIVILEGES REVOKE SELECT ON TABLES FROM ` + db.Role, false},
	} {
		require.NoError(t, db.Owner.Exec(way.open).Error, way.open)
		findings, err := schema.Confined(db.Importer)
		require.NoError(t, err, way.open)
		require.Len(t, findings, 1, way.open)
		require.Contains(t, findings[0], way.finding, way.open)

		// The migration applies the same test before it commits.
		err = schema.Migrate(db.Owner, db.Role)
		require.Error(t, err, way.open)
		require.Contains(t, err.Error(), "is not confined", way.open)
		if way.everyRole {
			// A role it would leave unconfined is not created at all.
			other := db.Role + "_b"
			err = schema.Migrate(db.Owner, other)
			require.Error(t, err, way.open)
			require.Contains(t, err.Error(), "is not confined", way.open)
			var left int64
			require.NoError(t, db.Owner.Raw(`SELECT count(*) FROM pg_roles WHERE rolname = ?`, other).Scan(&left).Error)
			require.Zero(t, left, way.open)
		}

		require.NoError(t, db.Owner.Exec(way.close).Error, way.close)
		clean()
	}

	// A login as the owner that only switched to the importer role can
	// switch back, so it is not confined.
	require.NoError(t, db.Owner.Connection(func(tx *gorm.DB) error {
		require.NoError(t, tx.Exec("SET ROLE "+db.Role).Error)
		findings, err := schema.Confined(tx)
		require.NoError(t, err)
		require.Len(t, findings, 1)
		require.Contains(t, findings[0], "switched")
		return tx.Exec("RESET ROLE").Error
	}))
}
