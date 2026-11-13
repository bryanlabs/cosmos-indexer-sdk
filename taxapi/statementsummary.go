package taxapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Validator statement summary.
//
// A statement for a large validator over all time reads tens of millions of
// reward rows, which cannot be done on request. The summary keeps the two
// things a statement needs per finished month: the per-day sums
// (statement_days) and the distinct reward recipients (statement_delegators).
//
// The tables are filled and kept current by the taxstatement Job, never by
// the API. The API only reads them, in the same snapshot as the live rows it
// still reads for the months the summary does not cover.
//
// What keeps the summary equal to taxable_events:
//
//   - statement_state.built_before: whole months before it are in the summary.
//   - A statement-level trigger on taxable_events marks (validator, month) in
//     statement_dirty when an insert, update or delete touches a reward or
//     commission row before statement_state.log_before. The API reads a
//     marked month live. A write to a month that already carries a mark adds
//     nothing, so a backfill that rewrites the same months costs one small
//     row per month, not one per statement.
//   - The Job recomputes a marked month from taxable_events. Because a
//     writer may have relied on an existing mark instead of adding its own,
//     the Job first claims the marks, waits until every transaction that
//     could have seen them unclaimed has ended, recomputes from a snapshot
//     taken after that, and only then removes the claimed marks. A write
//     that comes later finds no unclaimed mark and adds a new one.
//   - log_before is raised first, then every transaction that could have
//     read the old value is waited out the same way, and only then is a
//     month built and built_before raised. So no change to a built month can
//     go unmarked.
//   - The trigger never fails the write. If the mark cannot be written, the
//     sequence statement_log_failures is bumped instead, and the summary is
//     not used until a rebuild acknowledges that value.
//   - The API checks in its snapshot that the four triggers are present and
//     enabled on the same table the summary was built from; if not it reads
//     live rows only.
const (
	statementSummaryLockKey = 2026111301

	statementDaysTable       = "statement_days"
	statementDelegatorsTable = "statement_delegators"
	statementDirtyTable      = "statement_dirty"
	statementStateTable      = "statement_state"
	statementFailuresSeq     = "statement_log_failures"
	statementLogFunction     = "statement_log_change"

	// statementAllValidators marks the whole summary as changed (TRUNCATE).
	statementAllValidators = "*"

	statementTriggerCount = 4

	summaryDayLayout = "2006-01-02"
)

var statementTriggerNames = []string{"statement_log_insert", "statement_log_update", "statement_log_delete", "statement_log_truncate"}

// statementSummaryObjects creates the tables, the failure counter and the
// trigger function in schema. None of it touches taxable_events.
func statementSummaryObjects(schema string) []string {
	q := func(sql string) string { return strings.ReplaceAll(sql, "{s}", schema) }
	return []string{
		q(`CREATE TABLE IF NOT EXISTS {s}.statement_days (
	validator_address text NOT NULL,
	day date NOT NULL,
	category text,
	denom text,
	amount numeric,
	events bigint NOT NULL
)`),
		q(`CREATE UNIQUE INDEX IF NOT EXISTS statement_days_key ON {s}.statement_days (validator_address, day, category, denom) NULLS NOT DISTINCT`),
		q(`CREATE TABLE IF NOT EXISTS {s}.statement_delegators (
	validator_address text NOT NULL,
	month date NOT NULL,
	to_addr text NOT NULL,
	PRIMARY KEY (validator_address, month, to_addr)
)`),
		q(`CREATE TABLE IF NOT EXISTS {s}.statement_dirty (
	validator_address text NOT NULL,
	month date,
	claimed boolean NOT NULL DEFAULT false,
	logged_at timestamptz NOT NULL DEFAULT clock_timestamp()
)`),
		q(`CREATE INDEX IF NOT EXISTS statement_dirty_cell ON {s}.statement_dirty (validator_address, month)`),
		// No truncation by vacuum: it is the one routine operation that would
		// take a lock a writer's mark has to wait for.
		q(`ALTER TABLE {s}.statement_dirty SET (vacuum_truncate = false, autovacuum_vacuum_scale_factor = 0, autovacuum_vacuum_threshold = 5000)`),
		q(`CREATE TABLE IF NOT EXISTS {s}.statement_state (
	id boolean PRIMARY KEY DEFAULT true CHECK (id),
	log_before timestamptz,
	log_horizon text,
	built_before timestamptz,
	claim_horizon text,
	base_table oid,
	failures_acked bigint NOT NULL DEFAULT 0,
	updated_at timestamptz NOT NULL DEFAULT now()
)`),
		q(`ALTER TABLE {s}.statement_state SET (vacuum_truncate = false)`),
		q(`INSERT INTO {s}.statement_state (id) VALUES (true) ON CONFLICT DO NOTHING`),
		q(`CREATE SEQUENCE IF NOT EXISTS {s}.statement_log_failures`),
		// One function for the four triggers.
		//
		// A statement that touches no reward or commission row before
		// log_before (every write at the chain tip) returns after one look at
		// its own rows: it reads statement_state and nothing else, takes no
		// lock on statement_dirty and writes nothing.
		//
		// Otherwise it marks each (validator, month) it changed that has no
		// unclaimed mark yet. An update that rewrites the same values (a
		// re-indexed message) changes nothing a statement reads and marks
		// nothing.
		//
		// Everything runs inside the inner block, so any error (a missing
		// table, a lock not granted within two seconds, a full disk) is
		// caught: the write to taxable_events goes through, and the failure
		// counter is bumped so the summary is not trusted until it is rebuilt.
		// A sequence is used for that because it takes no row lock and keeps
		// its value even if the writing transaction later rolls back.
		q(`CREATE OR REPLACE FUNCTION {s}.statement_log_change() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
SET lock_timeout = '2s'
AS $fn$
BEGIN
	BEGIN
		IF TG_OP = 'TRUNCATE' THEN
			INSERT INTO {s}.statement_dirty (validator_address, month) VALUES ('*', NULL);
		ELSIF TG_OP = 'INSERT' THEN
			IF NOT EXISTS (SELECT 1 FROM new_rows r, {s}.statement_state st WHERE r.validator_address <> '' AND r.category IN ('reward','commission') AND r."timestamp" < st.log_before) THEN
				RETURN NULL;
			END IF;
			INSERT INTO {s}.statement_dirty (validator_address, month)
			SELECT c.validator_address, c.month
			FROM (SELECT DISTINCT r.validator_address, date_trunc('month', r."timestamp" AT TIME ZONE 'UTC')::date AS month
				FROM new_rows r, {s}.statement_state st
				WHERE r.validator_address <> '' AND r.category IN ('reward','commission') AND r."timestamp" < st.log_before) c
			WHERE NOT EXISTS (SELECT 1 FROM {s}.statement_dirty d
				WHERE d.validator_address = c.validator_address AND d.month = c.month AND NOT d.claimed);
		ELSIF TG_OP = 'DELETE' THEN
			IF NOT EXISTS (SELECT 1 FROM old_rows r, {s}.statement_state st WHERE r.validator_address <> '' AND r.category IN ('reward','commission') AND r."timestamp" < st.log_before) THEN
				RETURN NULL;
			END IF;
			INSERT INTO {s}.statement_dirty (validator_address, month)
			SELECT c.validator_address, c.month
			FROM (SELECT DISTINCT r.validator_address, date_trunc('month', r."timestamp" AT TIME ZONE 'UTC')::date AS month
				FROM old_rows r, {s}.statement_state st
				WHERE r.validator_address <> '' AND r.category IN ('reward','commission') AND r."timestamp" < st.log_before) c
			WHERE NOT EXISTS (SELECT 1 FROM {s}.statement_dirty d
				WHERE d.validator_address = c.validator_address AND d.month = c.month AND NOT d.claimed);
		ELSE
			IF NOT EXISTS (SELECT 1 FROM old_rows r, {s}.statement_state st WHERE r.validator_address <> '' AND r.category IN ('reward','commission') AND r."timestamp" < st.log_before)
				AND NOT EXISTS (SELECT 1 FROM new_rows r, {s}.statement_state st WHERE r.validator_address <> '' AND r.category IN ('reward','commission') AND r."timestamp" < st.log_before) THEN
				RETURN NULL;
			END IF;
			INSERT INTO {s}.statement_dirty (validator_address, month)
			SELECT c.validator_address, c.month
			FROM (SELECT DISTINCT r.validator_address, date_trunc('month', r."timestamp" AT TIME ZONE 'UTC')::date AS month
				FROM (
				(SELECT id, validator_address, category, amount, denom, to_addr, "timestamp" FROM old_rows EXCEPT ALL SELECT id, validator_address, category, amount, denom, to_addr, "timestamp" FROM new_rows)
				UNION ALL
				(SELECT id, validator_address, category, amount, denom, to_addr, "timestamp" FROM new_rows EXCEPT ALL SELECT id, validator_address, category, amount, denom, to_addr, "timestamp" FROM old_rows)
			) r, {s}.statement_state st
				WHERE r.validator_address <> '' AND r.category IN ('reward','commission') AND r."timestamp" < st.log_before) c
			WHERE NOT EXISTS (SELECT 1 FROM {s}.statement_dirty d
				WHERE d.validator_address = c.validator_address AND d.month = c.month AND NOT d.claimed);
		END IF;
	EXCEPTION WHEN OTHERS THEN
		BEGIN
			PERFORM nextval('{s}.statement_log_failures'::regclass);
		EXCEPTION WHEN OTHERS THEN
			NULL;
		END;
		RAISE WARNING 'validator statement change log failed (%): %', SQLSTATE, SQLERRM;
	END;
	RETURN NULL;
END
$fn$`),
	}
}

// statementSummaryTriggers attaches the function to taxable_events. Each
// statement takes a SHARE ROW EXCLUSIVE lock on taxable_events for an
// instant: writers wait, readers do not. ENABLE ALWAYS makes the triggers
// fire under session_replication_role = replica as well.
func statementSummaryTriggers(schema string) []string {
	q := func(sql string) string { return strings.ReplaceAll(sql, "{s}", schema) }
	out := []string{
		q(`CREATE OR REPLACE TRIGGER statement_log_insert AFTER INSERT ON {s}.taxable_events
	REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION {s}.statement_log_change()`),
		q(`CREATE OR REPLACE TRIGGER statement_log_update AFTER UPDATE ON {s}.taxable_events
	REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION {s}.statement_log_change()`),
		q(`CREATE OR REPLACE TRIGGER statement_log_delete AFTER DELETE ON {s}.taxable_events
	REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION {s}.statement_log_change()`),
		q(`CREATE OR REPLACE TRIGGER statement_log_truncate AFTER TRUNCATE ON {s}.taxable_events
	FOR EACH STATEMENT EXECUTE FUNCTION {s}.statement_log_change()`),
	}
	for _, name := range statementTriggerNames {
		out = append(out, q(`ALTER TABLE {s}.taxable_events ENABLE ALWAYS TRIGGER `+name))
	}
	return out
}

func summarySchema(tx *gorm.DB) (string, error) {
	var schema string
	if err := tx.Raw("SELECT quote_ident(n.nspname) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = to_regclass('taxable_events')").Row().Scan(&schema); err != nil {
		return "", fmt.Errorf("taxable_events not found on the search path: %w", err)
	}
	return schema, nil
}

// InstallStatementSummary creates the summary objects next to taxable_events
// and attaches the triggers, in one transaction. The only lock on
// taxable_events is the trigger one; if it is not granted within lockTimeout
// nothing is changed and the attempt is repeated after a pause, so a busy
// writer is held up for at most lockTimeout per attempt.
func InstallStatementSummary(ctx context.Context, db *gorm.DB, lockTimeout time.Duration, attempts int, logf func(string, ...any)) error {
	if lockTimeout <= 0 {
		lockTimeout = 3 * time.Second
	}
	if attempts <= 0 {
		attempts = 1
	}
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			schema, serr := summarySchema(tx)
			if serr != nil {
				return serr
			}
			if e := tx.Exec(fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", lockTimeout.Milliseconds())).Error; e != nil {
				return e
			}
			for _, ddl := range statementSummaryObjects(schema) {
				if e := tx.Exec(ddl).Error; e != nil {
					return e
				}
			}
			// A summary built from another table (taxable_events was replaced
			// since), or one whose triggers were missing or switched off until
			// this install, may have missed changes: it has to be built again.
			if e := tx.Exec(`UPDATE statement_state SET built_before = NULL, updated_at = now()
				WHERE built_before IS NOT NULL AND (base_table IS DISTINCT FROM 'taxable_events'::regclass::oid
					OR (SELECT count(*) FROM pg_trigger t WHERE t.tgrelid = 'taxable_events'::regclass AND NOT t.tgisinternal AND t.tgenabled = 'A'
						AND t.tgname IN ('statement_log_insert','statement_log_update','statement_log_delete','statement_log_truncate')) <> ?)`, statementTriggerCount).Error; e != nil {
				return e
			}
			if e := tx.Exec(`UPDATE statement_state SET base_table = 'taxable_events'::regclass::oid`).Error; e != nil {
				return e
			}
			for _, ddl := range statementSummaryTriggers(schema) {
				if e := tx.Exec(ddl).Error; e != nil {
					return e
				}
			}
			return nil
		})
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "55P03") && !strings.Contains(err.Error(), "lock timeout") {
			return err
		}
		if logf != nil {
			logf("statement-summary install: lock on taxable_events not granted within %s (attempt %d of %d)", lockTimeout, attempt, attempts)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return err
}

// DropStatementSummary removes the triggers first, so no writer refers to
// the tables any more, then the tables. The API reads live rows only once
// they are gone.
func DropStatementSummary(ctx context.Context, db *gorm.DB, lockTimeout time.Duration) error {
	if lockTimeout <= 0 {
		lockTimeout = 3 * time.Second
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		schema, err := summarySchema(tx)
		if err != nil {
			return err
		}
		if e := tx.Exec(fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", lockTimeout.Milliseconds())).Error; e != nil {
			return e
		}
		for _, name := range statementTriggerNames {
			if e := tx.Exec("DROP TRIGGER IF EXISTS " + name + " ON " + schema + ".taxable_events").Error; e != nil {
				return e
			}
		}
		for _, ddl := range []string{
			"DROP FUNCTION IF EXISTS " + schema + "." + statementLogFunction + "()",
			"DROP TABLE IF EXISTS " + schema + "." + statementDaysTable,
			"DROP TABLE IF EXISTS " + schema + "." + statementDelegatorsTable,
			"DROP TABLE IF EXISTS " + schema + "." + statementDirtyTable,
			"DROP TABLE IF EXISTS " + schema + "." + statementStateTable,
			"DROP SEQUENCE IF EXISTS " + schema + "." + statementFailuresSeq,
		} {
			if e := tx.Exec(ddl).Error; e != nil {
				return e
			}
		}
		return nil
	})
}

// summaryTrust is what decides, inside one snapshot, whether the summary may
// stand in for taxable_events.
type summaryTrust struct {
	Installed   bool
	LogBefore   *time.Time
	BuiltBefore *time.Time
	SameTable   bool
	PlainTable  bool
	Triggers    int
	Failures    int64
	Acked       int64
	AllDirty    bool
}

// Reason is empty when the summary may be used, else why not.
func (t summaryTrust) Reason() string {
	switch {
	case !t.Installed:
		return "not installed"
	case t.BuiltBefore == nil:
		return "not built"
	case !t.SameTable:
		return "taxable_events was replaced since the summary was built"
	case !t.PlainTable:
		return "taxable_events is partitioned or has child tables"
	case t.Triggers != statementTriggerCount:
		return fmt.Sprintf("%d of %d change triggers are present and enabled", t.Triggers, statementTriggerCount)
	case t.Failures != t.Acked:
		return fmt.Sprintf("the change log failed %d time(s) since the last build", t.Failures-t.Acked)
	case t.AllDirty:
		return "taxable_events was truncated since the last build"
	}
	return ""
}

const summaryTrustSQL = `SELECT st.log_before, st.built_before,
	COALESCE(st.base_table = c.oid, false) AS same_table,
	(c.relkind = 'r' AND NOT c.relispartition AND NOT EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhparent = c.oid)) AS plain_table,
	(SELECT count(*) FROM pg_trigger t WHERE t.tgrelid = c.oid AND NOT t.tgisinternal AND t.tgenabled = 'A'
		AND t.tgname IN ('statement_log_insert','statement_log_update','statement_log_delete','statement_log_truncate')) AS triggers,
	COALESCE(pg_sequence_last_value('statement_log_failures'::regclass), 0) AS failures,
	st.failures_acked AS acked,
	EXISTS (SELECT 1 FROM statement_dirty d WHERE d.validator_address = '*') AS all_dirty
FROM statement_state st, pg_class c
WHERE c.oid = 'taxable_events'::regclass`

func readSummaryTrust(tx *gorm.DB) (summaryTrust, error) {
	var t summaryTrust
	if err := tx.Raw(`SELECT to_regclass('statement_state') IS NOT NULL AND to_regclass('statement_dirty') IS NOT NULL
		AND to_regclass('statement_days') IS NOT NULL AND to_regclass('statement_delegators') IS NOT NULL
		AND to_regclass('statement_log_failures') IS NOT NULL`).Row().Scan(&t.Installed); err != nil {
		return t, err
	}
	if !t.Installed {
		return t, nil
	}
	var row struct {
		LogBefore   *time.Time
		BuiltBefore *time.Time
		SameTable   bool
		PlainTable  bool
		Triggers    int
		Failures    int64
		Acked       int64
		AllDirty    bool
	}
	res := tx.Raw(summaryTrustSQL).Scan(&row)
	if res.Error != nil {
		return t, res.Error
	}
	if res.RowsAffected == 0 {
		t.Installed = false
		return t, nil
	}
	t.LogBefore, t.BuiltBefore, t.SameTable, t.PlainTable = row.LogBefore, row.BuiltBefore, row.SameTable, row.PlainTable
	t.Triggers, t.Failures, t.Acked, t.AllDirty = row.Triggers, row.Failures, row.Acked, row.AllDirty
	return t, nil
}

func monthFloor(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func monthCeil(t time.Time) time.Time {
	floor := monthFloor(t)
	if floor.Equal(t.UTC()) {
		return floor
	}
	return floor.AddDate(0, 1, 0)
}

type statementRange struct{ From, To time.Time }

// statementReadPlan says which part of a statement comes from the summary
// and which is read from taxable_events. Whole months in [SumLo, SumHi)
// come from the summary, except the Dirty ones; Live is everything else in
// the period. Without Summary the period is one live range.
type statementReadPlan struct {
	Summary bool
	SumLo   time.Time
	SumHi   time.Time
	Dirty   []string
	Live    []statementRange
	Reason  string
}

// planStatementRead decides, in tx's snapshot, how to read the statement for
// val over [start, end).
func planStatementRead(tx *gorm.DB, val string, start, end time.Time, useSummary bool) (statementReadPlan, error) {
	live := statementReadPlan{Live: []statementRange{{start, end}}}
	if !useSummary {
		live.Reason = "summary turned off"
		return live, nil
	}
	trust, err := readSummaryTrust(tx)
	if err != nil {
		return live, err
	}
	if reason := trust.Reason(); reason != "" {
		live.Reason = reason
		return live, nil
	}
	lo, hi := monthCeil(start), monthFloor(end)
	if built := monthFloor(*trust.BuiltBefore); built.Before(hi) {
		hi = built
	}
	if !lo.Before(hi) {
		live.Reason = "no whole built month in the period"
		return live, nil
	}
	rows, err := tx.Raw("SELECT DISTINCT month FROM statement_dirty WHERE validator_address = ? AND month >= ?::text::date AND month < ?::text::date ORDER BY month",
		val, lo.Format(summaryDayLayout), hi.Format(summaryDayLayout)).Rows()
	if err != nil {
		return live, err
	}
	var dirty []time.Time
	for rows.Next() {
		var m time.Time
		if err := rows.Scan(&m); err != nil {
			_ = rows.Close()
			return live, err
		}
		dirty = append(dirty, m)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return live, err
	}
	if err := rows.Close(); err != nil {
		return live, err
	}
	plan := statementReadPlan{Summary: true, SumLo: lo, SumHi: hi}
	add := func(from, to time.Time) {
		if !from.Before(to) {
			return
		}
		if n := len(plan.Live); n > 0 && plan.Live[n-1].To.Equal(from) {
			plan.Live[n-1].To = to
			return
		}
		plan.Live = append(plan.Live, statementRange{from, to})
	}
	add(start, lo)
	for _, m := range dirty {
		m = monthFloor(m)
		plan.Dirty = append(plan.Dirty, m.Format(summaryDayLayout))
		add(m, m.AddDate(0, 1, 0))
	}
	add(hi, end)
	return plan, nil
}

const statementLiveScope = "validator_address = ? AND category IN ('reward','commission')"

func (p statementReadPlan) liveCondition(val string) (string, []any) {
	args := []any{val}
	if len(p.Live) == 0 {
		return statementLiveScope + " AND FALSE", args
	}
	parts := make([]string, 0, len(p.Live))
	for _, r := range p.Live {
		parts = append(parts, "(timestamp >= ? AND timestamp < ?)")
		args = append(args, r.From, r.To)
	}
	return statementLiveScope + " AND (" + strings.Join(parts, " OR ") + ")", args
}

// summaryMonthFilter restricts column (a date) to the whole months the plan
// takes from the summary. The bounds and the excluded months travel as text,
// so the driver needs no date encoding.
func (p statementReadPlan) summaryMonthFilter(column, monthOf string) (string, []any) {
	cond := column + " >= ?::text::date AND " + column + " < ?::text::date"
	args := []any{p.SumLo.Format(summaryDayLayout), p.SumHi.Format(summaryDayLayout)}
	if len(p.Dirty) > 0 {
		cond += " AND to_char(" + monthOf + ", 'YYYY-MM-DD') NOT IN ?"
		args = append(args, p.Dirty)
	}
	return cond, args
}

// statementSummarySQL is statementAggregateSQL with the summary added: the
// day rows of the summary months join the live day rows (a month is one or
// the other, never both), and the recipient counts are taken over the union
// of the live (month, recipient) pairs and the stored ones. A summary month
// with reward rows but no recipient still yields a month row with zero, as
// the live read does.
func (p statementReadPlan) statementSummarySQL(val string) (string, []any) {
	liveCond, liveArgs := p.liveCondition(val)
	dayCond, dayArgs := p.summaryMonthFilter("day", "date_trunc('month', day)")
	monthCond, monthArgs := p.summaryMonthFilter("month", "month")
	var args []any
	sqlText := `WITH ev AS MATERIALIZED (
	SELECT date_trunc('day', timestamp) AS day, category, denom, amount, to_addr
	FROM taxable_events
	WHERE ` + liveCond + `
), pairs AS MATERIALIZED (
	SELECT date_trunc('month', day) AS month, to_addr
	FROM ev WHERE category = 'reward' GROUP BY 1, 2
	UNION ALL
	SELECT month::timestamptz, to_addr
	FROM statement_delegators WHERE validator_address = ? AND ` + monthCond + `
	UNION ALL
	SELECT DISTINCT date_trunc('month', day)::timestamptz, NULL
	FROM statement_days WHERE validator_address = ? AND category = 'reward' AND ` + dayCond + `
)
SELECT 0 AS part, day, '' AS month, category, denom, SUM(amount::numeric)::text AS amount, COUNT(*) AS events, 0::bigint AS delegators
FROM ev GROUP BY day, category, denom
UNION ALL
SELECT 0, day::timestamptz, '', category, denom, amount::text, events, 0
FROM statement_days WHERE validator_address = ? AND ` + dayCond + `
UNION ALL
SELECT 1, NULL, to_char(month, 'YYYY-MM'), '', '', '', 0, COUNT(to_addr)
FROM pairs GROUP BY month
UNION ALL
SELECT 2, NULL, '', '', '', '', 0, COUNT(*)
FROM (SELECT to_addr FROM pairs WHERE to_addr IS NOT NULL GROUP BY to_addr) d
ORDER BY 1, 2`
	args = append(args, liveArgs...)
	args = append(args, val)
	args = append(args, monthArgs...)
	args = append(args, val)
	args = append(args, dayArgs...)
	args = append(args, val)
	args = append(args, dayArgs...)
	return sqlText, args
}

// liveRowEstimate asks the planner how many rows the live part of the plan
// reads.
func (p statementReadPlan) liveRowEstimate(tx *gorm.DB, val string) (int64, error) {
	if len(p.Live) == 0 {
		return 0, nil
	}
	cond, args := p.liveCondition(val)
	var plan string
	if err := tx.Raw("EXPLAIN (FORMAT JSON) SELECT 1 FROM taxable_events WHERE "+cond, args...).Row().Scan(&plan); err != nil {
		return 0, err
	}
	return planRows(plan)
}

func statementReadOnly() *sql.TxOptions {
	return &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
}

// summaryGone reports an error from reading summary objects that were
// dropped between the check and the read (a rollback of the summary).
func summaryGone(err error) bool {
	var gone bool
	for e := err; e != nil && !gone; e = errors.Unwrap(e) {
		gone = strings.Contains(e.Error(), "42P01")
	}
	return gone
}

// describe says in a few words how the statement was read, for the log.
func (p statementReadPlan) describe() string {
	if !p.Summary {
		return "live rows only (" + p.Reason + ")"
	}
	return fmt.Sprintf("summary %s to %s, %d changed month(s) and %d range(s) read live",
		p.SumLo.Format(apiMonthLayout), p.SumHi.AddDate(0, -1, 0).Format(apiMonthLayout), len(p.Dirty), len(p.Live))
}
