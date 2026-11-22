package taxapi

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Evidence change journal (OPS-817).
//
// A ready report is served only while the wallet's review fingerprint is
// unchanged, and that fingerprint reads the wallet's whole evidence history
// (55 s for a busy wallet on the live database). The journal lets a repeat
// view check only what was written since the report was last checked in full.
//
// Statement-level triggers on every table the fingerprint and the report gates
// read record, in the writer's own transaction:
//
//   - report_evidence_log: the block of every row a statement inserted,
//     deleted or actually changed (an upsert that rewrites a row unchanged
//     records nothing). A change that cannot be tied to a block (a truncate,
//     a changed address, type or key row, a row whose parent is gone) records
//     a NULL block, meaning anything may have changed.
//   - report_evidence_link: every address-shaped value an inserted or changed
//     row could attribute a transaction to a wallet by (signer, fee payer,
//     taxable from, to and validator, event attribute value). A row moved to
//     another parent (a transaction to another block, a block to another
//     chain, a message to another transaction, an event to another message)
//     records NULL.
//
// A report keeps the MVCC snapshot of its last full check and the blocks of
// the wallet's transactions in it. A later view reads only the journal rows
// that snapshot could not see: none may name one of those blocks or one of the
// wallet's addresses, or be NULL. Otherwise the full check runs as before.
//
// The journal is trusted only while its triggers, functions, the tables'
// columns and foreign keys are exactly as when the report was checked. A
// trigger never swallows an error: if the journal cannot be written, the
// write it describes fails with it. A write made with
// session_replication_role other than origin (foreign keys off) withdraws
// trust until the journal is reinstalled.
const (
	evidenceKeep = 26 * time.Hour
	// evidenceAddressPattern, with a length of 8 to 128 and at least one
	// "1", is the shape of a value the link journal keeps. A report whose
	// addresses do not all have it always runs the full check. The length
	// is checked apart from the pattern: a bounded repeat costs Postgres
	// about 80 microseconds per value, the plain class almost nothing.
	evidenceAddressPattern = `^[a-z][a-z0-9]*$`
	// evidenceBulkSetting, set to on in a session, marks every journaled
	// write of that session global instead of tracing it row by row.
	evidenceBulkSetting   = "report_evidence.bulk"
	evidenceAddressMinLen = 8
	evidenceAddressMaxLen = 128
)

var evidenceAddressRe = regexp.MustCompile(evidenceAddressPattern)

// evidenceAddressRecorded reports whether the link journal would record addr.
func evidenceAddressRecorded(addr string) bool {
	return len(addr) >= evidenceAddressMinLen && len(addr) <= evidenceAddressMaxLen &&
		strings.Contains(addr, "1") && evidenceAddressRe.MatchString(addr)
}

type evidenceJournalTable struct {
	table string
	// blocks lists the block of each changed row (relation c).
	blocks string
	// links lists the address-shaped values of each changed row, or "".
	links string
	// reparent names the columns whose change on UPDATE moves a row to
	// another parent, or "".
	reparent string
}

const (
	evidenceViaTx = `SELECT t.block_id FROM (SELECT DISTINCT c.tx_id FROM c WHERE c.tx_id IS NOT NULL) k
 LEFT JOIN {s}.txes t ON t.id = k.tx_id`
	evidenceViaMessage = `SELECT t.block_id FROM (SELECT DISTINCT c.message_id FROM c WHERE c.message_id IS NOT NULL) k
 LEFT JOIN {s}.messages m ON m.id = k.message_id
 LEFT JOIN {s}.txes t ON t.id = m.tx_id`
	evidenceViaEvent = `SELECT t.block_id FROM (SELECT DISTINCT c.message_event_id FROM c WHERE c.message_event_id IS NOT NULL) k
 LEFT JOIN {s}.message_events me ON me.id = k.message_event_id
 LEFT JOIN {s}.messages m ON m.id = me.message_id
 LEFT JOIN {s}.txes t ON t.id = m.tx_id`
)

// Names the journal's specs and triggers repeat.
const (
	evBlocks    = "blocks"
	evTxes      = "txes"
	evMessages  = "messages"
	evChains    = "chains"
	evLinkTable = "report_evidence_link"
	evLogTable  = "report_evidence_log"
	evOpIns     = "ins"
	evOpUpd     = "upd"
	evOpDel     = "del"
	evOpTrunc   = "trunc"
)

var (
	evidenceChangeOps     = []string{evOpIns, evOpUpd, evOpDel, evOpTrunc}
	evidenceDimensionOps  = []string{evOpUpd, evOpDel, evOpTrunc}
	evidenceGuardedTables = []string{evLogTable, evLinkTable}
)

var evidenceJournalTables = []evidenceJournalTable{
	{table: evBlocks, blocks: "SELECT c.id FROM c", reparent: "id, chain_id"},
	{table: evTxes, blocks: "SELECT c.block_id FROM c WHERE c.block_id IS NOT NULL", reparent: "id, block_id"},
	{table: "failed_txes", blocks: "SELECT c.block_id FROM c WHERE c.block_id IS NOT NULL"},
	{table: evMessages, blocks: evidenceViaTx, reparent: "id, tx_id"},
	{table: "fees", blocks: evidenceViaTx, links: `SELECT a.address FROM (SELECT DISTINCT c.payer_address_id FROM c WHERE c.payer_address_id IS NOT NULL) k
 LEFT JOIN {s}.addresses a ON a.id = k.payer_address_id`},
	{table: "tx_signer_addresses", blocks: evidenceViaTx, links: `SELECT a.address FROM (SELECT DISTINCT c.address_id FROM c WHERE c.address_id IS NOT NULL) k
 LEFT JOIN {s}.addresses a ON a.id = k.address_id`},
	{table: "failed_messages", blocks: evidenceViaTx},
	{table: "message_events", blocks: evidenceViaMessage, reparent: "id, message_id"},
	{table: "message_parser_errors", blocks: evidenceViaMessage},
	{table: "taxable_events", blocks: evidenceViaMessage, links: `SELECT DISTINCT v.a FROM c, LATERAL (VALUES (c.from_addr), (c.to_addr), (c.validator_address)) v(a) WHERE v.a IS NOT NULL`},
	{table: "message_event_attributes", blocks: evidenceViaEvent, links: `SELECT DISTINCT c.value FROM c WHERE c.value IS NOT NULL`},
}

// Lookup tables the evidence joins through. A new row changes nothing already
// read: with the foreign keys in place, only evidence rows written after it
// can refer to it, and those are journaled. An update that changes a value, a
// delete or a truncate is global.
var evidenceDimensionTables = []string{"addresses", evChains, "denoms", "message_types", "message_event_types", "message_event_attribute_keys"}

// The foreign keys the journal relies on: a child row can only name a parent
// that already exists, so a new parent row never adopts old children.
var evidenceForeignKeys = []string{
	"blocks.chain_id>chains",
	"failed_messages.tx_id>txes",
	"failed_txes.block_id>blocks",
	"fees.denomination_id>denoms",
	"fees.payer_address_id>addresses",
	"fees.tx_id>txes",
	"message_event_attributes.message_event_attribute_key_id>message_event_attribute_keys",
	"message_event_attributes.message_event_id>message_events",
	"message_events.message_event_type_id>message_event_types",
	"message_events.message_id>messages",
	"message_parser_errors.message_id>messages",
	"messages.message_type_id>message_types",
	"messages.tx_id>txes",
	"taxable_events.message_id>messages",
	"tx_signer_addresses.address_id>addresses",
	"tx_signer_addresses.tx_id>txes",
	"txes.block_id>blocks",
}

var evidenceJournalOwnTables = []string{evLinkTable, evLogTable, "report_evidence_pruned", "report_evidence_state"}

func evidenceTableNames() []string {
	out := make([]string, 0, len(evidenceJournalTables)+len(evidenceDimensionTables)+len(evidenceJournalOwnTables))
	for _, t := range evidenceJournalTables {
		out = append(out, t.table)
	}
	out = append(out, evidenceDimensionTables...)
	out = append(out, evidenceJournalOwnTables...)
	sort.Strings(out)
	return out
}

// evidenceTriggerNames lists table/trigger/function triples that must all be
// present and enabled ALWAYS.
func evidenceTriggerNames() []string {
	var out []string
	for _, t := range evidenceJournalTables {
		for _, op := range evidenceChangeOps {
			out = append(out, t.table+"/report_evidence_"+op+"/report_evidence_change")
		}
	}
	for _, t := range evidenceDimensionTables {
		for _, op := range evidenceDimensionOps {
			out = append(out, t+"/report_evidence_"+op+"/report_evidence_dimension")
		}
	}
	for _, t := range evidenceGuardedTables {
		out = append(out, t+"/report_evidence_guard/report_evidence_log_guard")
	}
	sort.Strings(out)
	return out
}

func evidenceJournalObjects(schema string) []string {
	q := func(sql string) string { return strings.ReplaceAll(sql, "{s}", schema) }
	var cases strings.Builder
	for i, t := range evidenceJournalTables {
		kw := "ELSIF"
		if i == 0 {
			kw = "IF"
		}
		fmt.Fprintf(&cases, "\t%s TG_TABLE_NAME = %s THEN\n\t\tblocks := %s;\n\t\tlinks := %s;\n\t\treparent := %s;\n",
			kw, quoteLiteral(t.table), quoteLiteral(q(t.blocks)), sqlTextOrNull(q(t.links)), sqlTextOrNull(t.reparent))
	}
	cases.WriteString("\tEND IF;\n")
	return []string{
		q(`CREATE TABLE IF NOT EXISTS {s}.report_evidence_log (
	xid xid8 NOT NULL,
	block_id bigint,
	logged_at timestamptz NOT NULL DEFAULT clock_timestamp()
)`),
		q(`CREATE UNIQUE INDEX IF NOT EXISTS report_evidence_log_key ON {s}.report_evidence_log (xid, block_id) NULLS NOT DISTINCT`),
		q(`CREATE INDEX IF NOT EXISTS report_evidence_log_age ON {s}.report_evidence_log (logged_at)`),
		q(`CREATE TABLE IF NOT EXISTS {s}.report_evidence_link (
	xid xid8 NOT NULL,
	address text,
	logged_at timestamptz NOT NULL DEFAULT clock_timestamp()
)`),
		q(`CREATE UNIQUE INDEX IF NOT EXISTS report_evidence_link_key ON {s}.report_evidence_link (address, xid) NULLS NOT DISTINCT`),
		q(`CREATE INDEX IF NOT EXISTS report_evidence_link_age ON {s}.report_evidence_link (logged_at)`),
		q(`CREATE TABLE IF NOT EXISTS {s}.report_evidence_state (
	id boolean PRIMARY KEY DEFAULT true CHECK (id),
	install_xid xid8 NOT NULL,
	trusted boolean NOT NULL DEFAULT true,
	installed_at timestamptz NOT NULL DEFAULT now()
)`),
		q(`CREATE TABLE IF NOT EXISTS {s}.report_evidence_pruned (
	id boolean PRIMARY KEY DEFAULT true CHECK (id),
	below xid8 NOT NULL DEFAULT '0'
)`),
		q(`INSERT INTO {s}.report_evidence_pruned (id) VALUES (true) ON CONFLICT DO NOTHING`),
		// One function for the evidence tables. Nested loops keep each parent
		// lookup on its primary key whatever the size of the statement.
		q(`CREATE OR REPLACE FUNCTION {s}.report_evidence_change() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
SET enable_hashjoin = off
SET enable_mergejoin = off
AS $fn$
DECLARE
	blocks text;
	links text;
	reparent text;
	changed text;
	moved boolean;
BEGIN
	IF current_setting('session_replication_role') <> 'origin' THEN
		UPDATE {s}.report_evidence_state SET trusted = false WHERE trusted;
		INSERT INTO {s}.report_evidence_log (xid, block_id) VALUES (pg_current_xact_id(), NULL) ON CONFLICT DO NOTHING;
		RETURN NULL;
	END IF;
	-- A bulk writer (replay or correction Job) sets report_evidence.bulk
	-- and pays one row per transaction instead of per-row work: the
	-- global mark sends every report checked before it to the full check.
	IF TG_OP = 'TRUNCATE' OR current_setting('` + evidenceBulkSetting + `', true) = 'on' THEN
		INSERT INTO {s}.report_evidence_log (xid, block_id) VALUES (pg_current_xact_id(), NULL) ON CONFLICT DO NOTHING;
		RETURN NULL;
	END IF;
` + cases.String() + `	IF blocks IS NULL THEN
		INSERT INTO {s}.report_evidence_log (xid, block_id) VALUES (pg_current_xact_id(), NULL) ON CONFLICT DO NOTHING;
		RETURN NULL;
	END IF;
	IF TG_OP = 'INSERT' THEN
		changed := 'SELECT * FROM new_rows';
	ELSIF TG_OP = 'DELETE' THEN
		changed := 'SELECT * FROM old_rows';
	ELSE
		changed := '(SELECT * FROM old_rows EXCEPT ALL SELECT * FROM new_rows) UNION ALL (SELECT * FROM new_rows EXCEPT ALL SELECT * FROM old_rows)';
	END IF;
	EXECUTE format('WITH c AS (%s) INSERT INTO {s}.report_evidence_log (xid, block_id) SELECT DISTINCT pg_current_xact_id(), k.block_id FROM (%s) k(block_id) ON CONFLICT DO NOTHING', changed, blocks);
	IF links IS NOT NULL AND TG_OP <> 'DELETE' THEN
		EXECUTE format('WITH c AS (%s) INSERT INTO {s}.report_evidence_link (xid, address) SELECT DISTINCT pg_current_xact_id(), k.a FROM (%s) k(a) WHERE k.a IS NULL OR (length(k.a) BETWEEN ` + strconv.Itoa(evidenceAddressMinLen) + ` AND ` + strconv.Itoa(evidenceAddressMaxLen) + ` AND strpos(k.a, %L) > 0 AND k.a ~ %L) ON CONFLICT DO NOTHING', changed, links, '1', '` + evidenceAddressPattern + `');
	END IF;
	IF reparent IS NOT NULL AND TG_OP = 'UPDATE' THEN
		EXECUTE format('SELECT EXISTS (SELECT %1$s FROM new_rows EXCEPT SELECT %1$s FROM old_rows)', reparent) INTO moved;
		IF moved THEN
			INSERT INTO {s}.report_evidence_link (xid, address) VALUES (pg_current_xact_id(), NULL) ON CONFLICT DO NOTHING;
		END IF;
	END IF;
	RETURN NULL;
END
$fn$`),
		q(`CREATE OR REPLACE FUNCTION {s}.report_evidence_dimension() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
	hit boolean;
BEGIN
	IF current_setting('session_replication_role') <> 'origin' THEN
		UPDATE {s}.report_evidence_state SET trusted = false WHERE trusted;
		hit := true;
	ELSIF TG_OP = 'TRUNCATE' THEN
		hit := true;
	ELSIF TG_OP = 'DELETE' THEN
		EXECUTE 'SELECT EXISTS (SELECT 1 FROM old_rows)' INTO hit;
	ELSE
		EXECUTE 'SELECT EXISTS ((SELECT * FROM old_rows EXCEPT ALL SELECT * FROM new_rows) UNION ALL (SELECT * FROM new_rows EXCEPT ALL SELECT * FROM old_rows))' INTO hit;
	END IF;
	IF hit THEN
		INSERT INTO {s}.report_evidence_log (xid, block_id) VALUES (pg_current_xact_id(), NULL) ON CONFLICT DO NOTHING;
	END IF;
	RETURN NULL;
END
$fn$`),
		// The journal is trimmed only by age, by the prune function, which
		// records how far it went. Truncating it would hide changes.
		q(`CREATE OR REPLACE FUNCTION {s}.report_evidence_log_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
	RAISE EXCEPTION 'the report evidence journal may only be trimmed by report_evidence_prune()';
END
$fn$`),
		// Deletes journal rows older than keep (at least 25 hours, past the
		// 24 hour report age limit) and records the newest transaction it
		// removed. A report whose checked snapshot could not see that
		// transaction runs the full check instead.
		q(`CREATE OR REPLACE FUNCTION {s}.report_evidence_prune(keep interval DEFAULT interval '26 hours') RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
SET lock_timeout = '2s'
AS $fn$
DECLARE
	n1 bigint;
	n2 bigint;
	h1 xid8;
	h2 xid8;
	hi xid8;
BEGIN
	IF keep IS NULL OR keep < interval '25 hours' THEN
		keep := interval '25 hours';
	END IF;
	WITH d AS (DELETE FROM {s}.report_evidence_log WHERE logged_at < clock_timestamp() - keep RETURNING xid)
	SELECT count(*), max(xid) INTO n1, h1 FROM d;
	WITH d AS (DELETE FROM {s}.report_evidence_link WHERE logged_at < clock_timestamp() - keep RETURNING xid)
	SELECT count(*), max(xid) INTO n2, h2 FROM d;
	hi := greatest(h1, h2);
	IF hi IS NOT NULL THEN
		UPDATE {s}.report_evidence_pruned SET below = greatest(below, (hi::text::bigint + 1)::text::xid8);
	END IF;
	RETURN n1 + n2;
END
$fn$`),
		q(`REVOKE ALL ON {s}.report_evidence_log, {s}.report_evidence_link, {s}.report_evidence_state, {s}.report_evidence_pruned FROM PUBLIC`),
		q(`REVOKE ALL ON FUNCTION {s}.report_evidence_change(), {s}.report_evidence_dimension(), {s}.report_evidence_log_guard(), {s}.report_evidence_prune(interval) FROM PUBLIC`),
	}
}

func sqlTextOrNull(s string) string {
	if s == "" {
		return "NULL"
	}
	return quoteLiteral(s)
}

// evidenceJournalTriggers attaches the functions. Each CREATE TRIGGER takes a
// SHARE ROW EXCLUSIVE lock for an instant: it waits for writers in flight, so
// every later write is journaled and every earlier one is committed before the
// installation is.
func evidenceJournalTriggers(schema string) []string {
	q := func(sql string) string { return strings.ReplaceAll(sql, "{s}", schema) }
	var out []string
	attach := func(table, fn string, ops []string) {
		for _, op := range ops {
			var ddl string
			switch op {
			case evOpIns:
				ddl = `CREATE OR REPLACE TRIGGER report_evidence_ins AFTER INSERT ON {s}.` + table + ` REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION {s}.` + fn + `()`
			case evOpUpd:
				ddl = `CREATE OR REPLACE TRIGGER report_evidence_upd AFTER UPDATE ON {s}.` + table + ` REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION {s}.` + fn + `()`
			case evOpDel:
				ddl = `CREATE OR REPLACE TRIGGER report_evidence_del AFTER DELETE ON {s}.` + table + ` REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION {s}.` + fn + `()`
			case evOpTrunc:
				ddl = `CREATE OR REPLACE TRIGGER report_evidence_trunc AFTER TRUNCATE ON {s}.` + table + ` FOR EACH STATEMENT EXECUTE FUNCTION {s}.` + fn + `()`
			}
			out = append(out, q(ddl), q(`ALTER TABLE {s}.`+table+` ENABLE ALWAYS TRIGGER report_evidence_`+op))
		}
	}
	for _, t := range evidenceJournalTables {
		attach(t.table, "report_evidence_change", evidenceChangeOps)
	}
	for _, t := range evidenceDimensionTables {
		attach(t, "report_evidence_dimension", evidenceDimensionOps)
	}
	for _, t := range evidenceGuardedTables {
		out = append(out,
			q(`CREATE OR REPLACE TRIGGER report_evidence_guard BEFORE TRUNCATE ON {s}.`+t+` FOR EACH STATEMENT EXECUTE FUNCTION {s}.report_evidence_log_guard()`),
			q(`ALTER TABLE {s}.`+t+` ENABLE ALWAYS TRIGGER report_evidence_guard`),
		)
	}
	return out
}

// evidenceJournalState starts a new trust epoch: earlier checks stop matching
// (the state row is part of the metadata digest).
func evidenceJournalState(schema string) string {
	return strings.ReplaceAll(`INSERT INTO {s}.report_evidence_state (id, install_xid, trusted) VALUES (true, pg_current_xact_id(), true)
ON CONFLICT (id) DO UPDATE SET install_xid = excluded.install_xid, trusted = true, installed_at = now()`, "{s}", schema)
}

// EvidenceJournalSQL is the whole installation, in order, for one
// transaction. It is the DDL InstallEvidenceJournal runs.
func EvidenceJournalSQL(schema string) []string {
	out := append([]string{}, evidenceJournalObjects(schema)...)
	out = append(out, evidenceJournalTriggers(schema)...)
	return append(out, evidenceJournalState(schema))
}

// InstallEvidenceJournal creates or repairs the journal in one transaction
// next to txes. If a table lock is not granted within lockTimeout nothing is
// changed and the attempt is repeated after a pause.
func InstallEvidenceJournal(ctx context.Context, db *gorm.DB, lockTimeout time.Duration, attempts int, logf func(string, ...any)) error {
	if lockTimeout <= 0 {
		lockTimeout = 3 * time.Second
	}
	if attempts <= 0 {
		attempts = 1
	}
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			schema, serr := evidenceSchema(tx)
			if serr != nil {
				return serr
			}
			if e := tx.Exec(fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", lockTimeout.Milliseconds())).Error; e != nil {
				return e
			}
			for _, ddl := range EvidenceJournalSQL(schema) {
				if e := tx.Exec(ddl).Error; e != nil {
					return e
				}
			}
			return nil
		})
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "55P03") && !strings.Contains(err.Error(), "lock timeout") && !strings.Contains(err.Error(), "40P01") {
			return err
		}
		if logf != nil {
			logf("evidence journal install: table lock not granted within %s (attempt %d of %d)", lockTimeout, attempt, attempts)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return err
}

// DropEvidenceJournal removes the triggers, then the journal. Reports fall
// back to the full check once it is gone.
func DropEvidenceJournal(ctx context.Context, db *gorm.DB, lockTimeout time.Duration) error {
	if lockTimeout <= 0 {
		lockTimeout = 3 * time.Second
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		schema, err := evidenceSchema(tx)
		if err != nil {
			return err
		}
		if e := tx.Exec(fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", lockTimeout.Milliseconds())).Error; e != nil {
			return e
		}
		for _, triple := range evidenceTriggerNames() {
			parts := strings.Split(triple, "/")
			if strings.HasPrefix(parts[0], "report_evidence_") {
				continue
			}
			if e := tx.Exec("DROP TRIGGER IF EXISTS " + parts[1] + " ON " + schema + "." + parts[0]).Error; e != nil {
				return e
			}
		}
		for _, ddl := range []string{
			"DROP TABLE IF EXISTS " + schema + ".report_evidence_log",
			"DROP TABLE IF EXISTS " + schema + ".report_evidence_link",
			"DROP TABLE IF EXISTS " + schema + ".report_evidence_state",
			"DROP TABLE IF EXISTS " + schema + ".report_evidence_pruned",
			"DROP FUNCTION IF EXISTS " + schema + ".report_evidence_change()",
			"DROP FUNCTION IF EXISTS " + schema + ".report_evidence_dimension()",
			"DROP FUNCTION IF EXISTS " + schema + ".report_evidence_log_guard()",
			"DROP FUNCTION IF EXISTS " + schema + ".report_evidence_prune(interval)",
		} {
			if e := tx.Exec(ddl).Error; e != nil {
				return e
			}
		}
		return nil
	})
}

func evidenceSchema(tx *gorm.DB) (string, error) {
	var schema string
	if err := tx.Raw("SELECT quote_ident(n.nspname) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = to_regclass('txes')").Row().Scan(&schema); err != nil {
		return "", fmt.Errorf("txes not found on the search path: %w", err)
	}
	return schema, nil
}

func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// evidenceTrust is read inside a report's snapshot. Meta digests every column,
// trigger and constraint of the journaled tables, the journal functions and
// the trust epoch, so any DDL, disabled trigger or reinstall since a report
// was checked changes it.
type evidenceTrust struct {
	Installed   bool
	Trusted     bool
	Meta        string
	Triggers    int
	ForeignKeys int
	PrunedBelow string
	Snapshot    string
	Xmin        string
}

func (t evidenceTrust) ok() bool {
	return t.Installed && t.Trusted && t.Meta != "" &&
		t.Triggers == len(evidenceTriggerNames()) && t.ForeignKeys == len(evidenceForeignKeys)
}

func readEvidenceTrust(tx *gorm.DB) (evidenceTrust, error) {
	var t evidenceTrust
	var schema string
	err := tx.Raw(`SELECT coalesce((SELECT quote_ident(n.nspname) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.oid = to_regclass('report_evidence_state') AND c.relnamespace = (SELECT relnamespace FROM pg_class WHERE oid = to_regclass('txes'))), '')`).Row().Scan(&schema)
	if err != nil || schema == "" {
		return t, err
	}
	t.Installed = true
	q := strings.ReplaceAll(`WITH ns AS (
 SELECT relnamespace AS oid FROM pg_class WHERE oid = '{s}.report_evidence_state'::regclass
), rels AS (
 SELECT n AS name, to_regclass('{s}.' || quote_ident(n)) AS rel FROM unnest(?::text[]) n
), fns AS (
 SELECT p.oid, p.proname, p.xmin FROM pg_proc p, ns
 WHERE p.pronamespace = ns.oid
   AND p.proname IN ('report_evidence_change', 'report_evidence_dimension', 'report_evidence_log_guard', 'report_evidence_prune')
)
SELECT
 (SELECT md5(string_agg(x, ';' ORDER BY x)) FROM (
   SELECT 'rel:' || r.name || ':' || coalesce(r.rel::oid::text, '-') AS x FROM rels r
   UNION ALL SELECT 'att:' || a.attrelid || ':' || a.attnum || ':' || a.attname || ':' || a.atttypid || ':' || a.attisdropped::text || ':' || a.xmin
     FROM pg_attribute a WHERE a.attrelid IN (SELECT rel FROM rels WHERE rel IS NOT NULL) AND a.attnum > 0
   UNION ALL SELECT 'trg:' || t.tgrelid || ':' || t.tgname || ':' || t.tgenabled::text || ':' || t.tgtype || ':' || t.tgfoid || ':' || t.xmin
     FROM pg_trigger t WHERE t.tgrelid IN (SELECT rel FROM rels WHERE rel IS NOT NULL)
   UNION ALL SELECT 'con:' || c.conrelid || ':' || c.oid || ':' || c.conname || ':' || c.convalidated::text || ':' || c.xmin
     FROM pg_constraint c WHERE c.conrelid IN (SELECT rel FROM rels WHERE rel IS NOT NULL)
   UNION ALL SELECT 'fn:' || f.proname || ':' || f.oid || ':' || f.xmin FROM fns f
   UNION ALL SELECT 'state:' || s.install_xid || ':' || s.trusted::text FROM {s}.report_evidence_state s
 ) m),
 (SELECT count(*) FROM pg_trigger t JOIN rels r ON r.rel = t.tgrelid JOIN fns f ON f.oid = t.tgfoid
   WHERE t.tgenabled = 'A' AND (r.name || '/' || t.tgname || '/' || f.proname) = ANY(?::text[])),
 (SELECT count(*) FROM pg_constraint c
   JOIN rels r ON r.rel = c.conrelid
   JOIN pg_class p ON p.oid = c.confrelid
   JOIN ns ON ns.oid = p.relnamespace
   JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
   WHERE c.contype = 'f' AND c.convalidated AND NOT c.condeferrable AND array_length(c.conkey, 1) = 1
     AND (r.name || '.' || a.attname || '>' || p.relname) = ANY(?::text[])),
 coalesce((SELECT trusted FROM {s}.report_evidence_state), false),
 coalesce((SELECT below::text FROM {s}.report_evidence_pruned), ''),
 pg_current_snapshot()::text,
 pg_snapshot_xmin(pg_current_snapshot())::text`, "{s}", schema)
	tables := "{" + strings.Join(evidenceTableNames(), ",") + "}"
	triggers := "{" + strings.Join(evidenceTriggerNames(), ",") + "}"
	fks := "{" + strings.Join(evidenceForeignKeys, ",") + "}"
	var meta *string
	err = tx.Raw(q, tables, triggers, fks).Row().Scan(&meta, &t.Triggers, &t.ForeignKeys, &t.Trusted, &t.PrunedBelow, &t.Snapshot, &t.Xmin)
	if meta != nil {
		t.Meta = *meta
	}
	return t, err
}

// PruneEvidenceJournal trims journal rows past the report age limit.
func PruneEvidenceJournal(db *gorm.DB) (int64, error) {
	var n int64
	err := db.Raw("SELECT report_evidence_prune(?::interval)", fmt.Sprintf("%d seconds", int64(evidenceKeep.Seconds()))).Row().Scan(&n)
	return n, err
}
