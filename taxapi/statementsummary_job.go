package taxapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"gorm.io/gorm"
)

// The functions in this file fill and maintain the statement summary. They
// are run by the taxstatement Job, one at a time (an advisory lock), and
// never by the API.

// StatementSummaryOptions tunes a fill, refresh or check. The zero value is
// usable.
type StatementSummaryOptions struct {
	// DrainTimeout bounds one run's wait for open transactions that began
	// before a change the Job made (summaryDrain). Default 20 minutes.
	DrainTimeout time.Duration
	// SplitRows is the planner estimate above which one validator's range is
	// built in smaller pieces (years, then months). Default 2 million.
	SplitRows int64
	// Now is the clock; the current UTC month is never built.
	Now func() time.Time
	// Logf receives progress lines.
	Logf func(string, ...any)
	// Validators restricts a fill to these operators (tests).
	Validators []string
}

func (o StatementSummaryOptions) drainTimeout() time.Duration {
	if o.DrainTimeout > 0 {
		return o.DrainTimeout
	}
	return 20 * time.Minute
}

func (o StatementSummaryOptions) splitRows() int64 {
	if o.SplitRows > 0 {
		return o.SplitRows
	}
	return 2_000_000
}

func (o StatementSummaryOptions) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

func (o StatementSummaryOptions) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// ErrStatementSummaryBusy is returned when another fill or refresh holds the
// lock.
var ErrStatementSummaryBusy = errors.New("another statement summary job is running")

// withSummaryLock runs fn on one connection that holds the summary lock.
func withSummaryLock(ctx context.Context, db *gorm.DB, fn func(conn *gorm.DB) error) error {
	return db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
		var got bool
		if err := conn.Raw("SELECT pg_try_advisory_lock(?)", statementSummaryLockKey).Row().Scan(&got); err != nil {
			return err
		}
		if !got {
			return ErrStatementSummaryBusy
		}
		defer func() {
			// The request context may be done; release on a fresh one so the
			// pooled connection does not keep the lock.
			_ = conn.WithContext(context.Background()).Exec("SELECT pg_advisory_unlock(?)", statementSummaryLockKey).Error
		}()
		return fn(conn)
	})
}

// summaryBuildSettings are the session settings of a build statement: read
// the validator's rows with a bitmap scan and read-ahead, never by scanning
// the whole table, with days and months in UTC.
func summaryBuildSettings(tx *gorm.DB) error {
	for _, setting := range []string{
		"LOCK TABLE taxable_events IN ACCESS SHARE MODE",
		"SET LOCAL plan_cache_mode = force_custom_plan",
		"SET LOCAL work_mem = '64MB'",
		"SET LOCAL effective_io_concurrency = 128",
		"SET LOCAL jit = off",
		"SET LOCAL TimeZone = 'UTC'",
	} {
		if err := tx.Exec(setting).Error; err != nil {
			return err
		}
	}
	return nil
}

// Derive only the physical table that the change triggers cover. A child
// attached temporarily during a build must never become phantom income
// after detachment. Current children still make API summaries untrusted.
const summaryBuildSQL = `WITH ev AS MATERIALIZED (
	SELECT (timestamp AT TIME ZONE 'UTC')::date AS day, category, denom, amount, to_addr
	FROM ONLY taxable_events
	WHERE validator_address = @validator AND category IN ('reward','commission') AND timestamp >= @from AND timestamp < @to
), d AS (
	INSERT INTO statement_days (validator_address, day, category, denom, amount, events)
	SELECT @validator, day, category, denom, SUM(amount::numeric), COUNT(*) FROM ev GROUP BY day, category, denom
	RETURNING 1
), p AS (
	INSERT INTO statement_delegators (validator_address, month, to_addr)
	SELECT DISTINCT @validator, date_trunc('month', day::timestamp)::date, to_addr FROM ev WHERE category = 'reward' AND to_addr IS NOT NULL
	RETURNING 1
)
SELECT (SELECT count(*) FROM d) AS days, (SELECT count(*) FROM p) AS pairs`

// summaryRebuildRange replaces the summary of val for the whole months in
// [from, to) with what taxable_events holds in tx's snapshot.
func summaryRebuildRange(tx *gorm.DB, val string, from, to time.Time) (days, pairs int64, err error) {
	lo, hi := from.Format(summaryDayLayout), to.Format(summaryDayLayout)
	if err = tx.Exec("DELETE FROM statement_days WHERE validator_address = ? AND day >= ?::text::date AND day < ?::text::date", val, lo, hi).Error; err != nil {
		return 0, 0, err
	}
	if err = tx.Exec("DELETE FROM statement_delegators WHERE validator_address = ? AND month >= ?::text::date AND month < ?::text::date", val, lo, hi).Error; err != nil {
		return 0, 0, err
	}
	err = tx.Raw(summaryBuildSQL, sql.Named("validator", val), sql.Named("from", from), sql.Named("to", to)).Row().Scan(&days, &pairs)
	return days, pairs, err
}

// summaryValidators lists every operator address with rows, by walking the
// statement index one address at a time instead of reading all of it.
func summaryValidators(conn *gorm.DB) ([]string, error) {
	var out []string
	rows, err := conn.Raw(`WITH RECURSIVE v AS (
		(SELECT validator_address FROM ONLY taxable_events WHERE validator_address <> '' ORDER BY validator_address LIMIT 1)
		UNION ALL
		SELECT (SELECT t.validator_address FROM ONLY taxable_events t WHERE t.validator_address > v.validator_address AND t.validator_address <> '' ORDER BY t.validator_address LIMIT 1)
		FROM v WHERE v.validator_address IS NOT NULL
	) SELECT validator_address FROM v WHERE validator_address IS NOT NULL`).Rows()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func summaryFirstRow(conn *gorm.DB, val string) (*time.Time, error) {
	var first *time.Time
	err := conn.Raw(`SELECT LEAST(
		(SELECT min(timestamp) FROM ONLY taxable_events WHERE validator_address = @validator AND category = 'reward'),
		(SELECT min(timestamp) FROM ONLY taxable_events WHERE validator_address = @validator AND category = 'commission'))`,
		sql.Named("validator", val)).Row().Scan(&first)
	return first, err
}

// summaryBuildValidator builds val's summary for [from, to), in pieces small
// enough to hold in memory and temp space: the whole range when the planner
// expects few rows, else year by year, else month by month. Each piece is
// its own transaction.
func summaryBuildValidator(ctx context.Context, conn *gorm.DB, val string, from, to time.Time, opts StatementSummaryOptions) (rows, pairs int64, err error) {
	if !from.Before(to) {
		return 0, 0, nil
	}
	oneMonth := !from.AddDate(0, 1, 0).Before(to)
	if !oneMonth {
		plan := statementReadPlan{Live: []statementRange{{from, to}}}
		estimate, eerr := plan.liveRowEstimate(conn, val)
		if eerr != nil {
			return 0, 0, eerr
		}
		if estimate > opts.splitRows() {
			step := func(t time.Time) time.Time { return time.Date(t.Year()+1, 1, 1, 0, 0, 0, 0, time.UTC) }
			if !step(from).Before(to) {
				step = func(t time.Time) time.Time { return t.AddDate(0, 1, 0) }
			}
			for lo := from; lo.Before(to); {
				hi := step(lo)
				if hi.After(to) {
					hi = to
				}
				r, p, perr := summaryBuildValidator(ctx, conn, val, lo, hi, opts)
				if perr != nil {
					return rows, pairs, perr
				}
				rows, pairs, lo = rows+r, pairs+p, hi
			}
			return rows, pairs, nil
		}
	}
	err = conn.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := summaryBuildSettings(tx); err != nil {
			return err
		}
		var err error
		rows, pairs, err = summaryRebuildRange(tx, val, from, to)
		return err
	})
	return rows, pairs, err
}

// summaryHorizon returns a transaction id above every one handed out so far.
// A transaction or snapshot that existed before a change this session has
// just committed is older than it.
func summaryHorizon(ctx context.Context, conn *gorm.DB) (string, error) {
	var horizon string
	err := conn.WithContext(ctx).Raw("SELECT pg_snapshot_xmax(pg_current_snapshot())::xid::text").Row().Scan(&horizon)
	return horizon, err
}

// summaryDrain waits until no session is left that began, or took its
// snapshot, before horizon: no transaction that could still act on what it
// read before then. An unrelated long transaction (a report, a dump) makes
// this wait too. It returns nil when drained, and a description of who is
// still open when the wait ran out.
func summaryDrain(ctx context.Context, conn *gorm.DB, horizon string, opts StatementSummaryOptions) (waiting string, err error) {
	var visible bool
	if err := conn.WithContext(ctx).Raw(`SELECT rolsuper OR pg_has_role(current_user, 'pg_read_all_stats', 'USAGE') FROM pg_roles WHERE rolname = current_user`).Row().Scan(&visible); err != nil {
		return "", err
	}
	if !visible {
		return "", errors.New("cannot drain safely: job role needs pg_read_all_stats or superuser visibility of other writers")
	}
	deadline := time.Now().Add(opts.drainTimeout())
	for {
		var sessions, prepared int64
		var oldest float64
		if err := conn.WithContext(ctx).Raw(`SELECT count(*), COALESCE(max(EXTRACT(EPOCH FROM clock_timestamp() - COALESCE(xact_start, query_start))), 0)
			FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND backend_type <> 'autovacuum worker'
				AND (age(backend_xid) > age(?::text::xid) OR age(backend_xmin) > age(?::text::xid))`, horizon, horizon).Row().Scan(&sessions, &oldest); err != nil {
			return "", err
		}
		if err := conn.WithContext(ctx).Raw(`SELECT count(*) FROM pg_prepared_xacts WHERE database = current_database() AND age(transaction) > age(?::text::xid)`, horizon).Row().Scan(&prepared); err != nil {
			return "", err
		}
		if sessions == 0 && prepared == 0 {
			return "", nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Sprintf("%d session(s) and %d prepared transaction(s) older than the change are still open after %s (oldest %.0f s)",
				sessions, prepared, opts.drainTimeout(), oldest), nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(summaryDrainPoll):
		}
	}
}

// summaryDrainPoll is how often summaryDrain looks again.
var summaryDrainPoll = 2 * time.Second

// summaryCoverFrom raises statement_state.log_before to target and waits
// (summaryDrain) for every transaction that could have written to
// taxable_events without seeing it. Only after that may a month before
// target be built. With fresh set the wait starts from now even if
// log_before was raised earlier; a fill uses that, because it has just
// removed marks a writer may have relied on.
func summaryCoverFrom(ctx context.Context, conn *gorm.DB, target time.Time, fresh bool, opts StatementSummaryOptions) (waiting string, err error) {
	res := conn.WithContext(ctx).Exec(`UPDATE statement_state SET log_before = ?, log_horizon = NULL, updated_at = now() WHERE log_before IS NULL OR log_before < ?`, target, target)
	if res.Error != nil {
		return "", res.Error
	}
	if fresh {
		if err := conn.WithContext(ctx).Exec("UPDATE statement_state SET log_horizon = NULL").Error; err != nil {
			return "", err
		}
	}
	var horizon *string
	if err := conn.WithContext(ctx).Raw("SELECT log_horizon FROM statement_state").Row().Scan(&horizon); err != nil {
		return "", err
	}
	if horizon == nil {
		h, err := summaryHorizon(ctx, conn)
		if err != nil {
			return "", err
		}
		if err := conn.WithContext(ctx).Exec("UPDATE statement_state SET log_horizon = ?", h).Error; err != nil {
			return "", err
		}
		horizon = &h
	}
	return summaryDrain(ctx, conn, *horizon, opts)
}

// StatementSummaryFillResult counts what a fill wrote.
type StatementSummaryFillResult struct {
	Validators  int
	DayRows     int64
	Pairs       int64
	BuiltBefore time.Time
}

// FillStatementSummary builds the summary for every whole month before the
// current one. It refuses a summary that is already built unless rebuild is
// set; a rebuild first withdraws the summary from the API (which then reads
// live rows), empties it and acknowledges change-log failures and a
// truncation seen so far.
func FillStatementSummary(ctx context.Context, db *gorm.DB, rebuild bool, opts StatementSummaryOptions) (StatementSummaryFillResult, error) {
	var res StatementSummaryFillResult
	err := withSummaryLock(ctx, db, func(conn *gorm.DB) error {
		trust, err := readSummaryTrust(conn)
		if err != nil {
			return err
		}
		if !trust.Installed {
			return errors.New("statement summary is not installed")
		}
		if trust.BuiltBefore != nil && !rebuild {
			return fmt.Errorf("statement summary is already built through %s; use a refresh, or a rebuild to start over", trust.BuiltBefore.UTC().Format(summaryDayLayout))
		}
		if !trust.PlainTable {
			return errors.New("statement summary cannot be built: taxable_events is partitioned or has child tables, whose writes the change triggers do not see")
		}
		if trust.Triggers != statementTriggerCount {
			return fmt.Errorf("statement summary cannot be built: %d of %d change triggers are present and enabled; run the install first", trust.Triggers, statementTriggerCount)
		}
		fingerprint, err := currentSummaryFingerprint(conn.WithContext(ctx))
		if err != nil {
			return err
		}
		loggerOK, err := statementLoggerMatches(conn.WithContext(ctx))
		if err != nil {
			return err
		}
		if !loggerOK {
			return errors.New("statement logger definition differs from this implementation; reinstall before rebuilding")
		}
		guardOK, err := statementDDLGuardMatches(conn.WithContext(ctx))
		if err != nil {
			return err
		}
		if !guardOK {
			return errors.New("statement DDL guard definition differs from this implementation; reinstall before rebuilding")
		}
		// Carry this validated fingerprint unchanged through the whole build.
		// A logger or DDL guard swap during validation must not become a new baseline.
		confirmed, err := currentSummaryFingerprint(conn.WithContext(ctx))
		if err != nil {
			return err
		}
		if confirmed != fingerprint {
			return errors.New("statement logger or DDL guard metadata changed while validating; reinstall or retry")
		}
		// Failures and a truncation seen up to here are covered by the build
		// that follows; any later one keeps the summary untrusted.
		acked := trust.Failures
		if err := conn.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("UPDATE statement_state SET built_before = NULL, claim_horizon = NULL, base_table = 'taxable_events'::regclass::oid, metadata_fingerprint = ?, updated_at = now()", fingerprint).Error; err != nil {
				return err
			}
			for _, stmt := range []string{
				"DELETE FROM statement_dirty",
				"DELETE FROM statement_days",
				"DELETE FROM statement_delegators",
			} {
				if err := tx.Exec(stmt).Error; err != nil {
					return err
				}
			}
			return nil
		}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead}); err != nil {
			return err
		}
		target := monthFloor(opts.now())
		waiting, err := summaryCoverFrom(ctx, conn, target, true, opts)
		if err != nil {
			return err
		}
		if waiting != "" {
			return fmt.Errorf("nothing was built: %s", waiting)
		}
		validators := opts.Validators
		if len(validators) == 0 {
			if validators, err = summaryValidators(conn.WithContext(ctx)); err != nil {
				return err
			}
		}
		for i, val := range validators {
			first, err := summaryFirstRow(conn.WithContext(ctx), val)
			if err != nil {
				return err
			}
			if first == nil {
				continue
			}
			began := time.Now()
			days, pairs, err := summaryBuildValidator(ctx, conn, val, monthFloor(*first), target, opts)
			if err != nil {
				return fmt.Errorf("%s: %w", val, err)
			}
			res.Validators++
			res.DayRows += days
			res.Pairs += pairs
			opts.logf("statement-summary fill %d/%d %s: %d day rows, %d delegator months, %s", i+1, len(validators), val, days, pairs, time.Since(began).Round(time.Millisecond))
		}
		if summaryPublishHook != nil {
			summaryPublishHook()
		}
		published := conn.WithContext(ctx).Exec("UPDATE statement_state SET built_before = ?, failures_acked = ?, updated_at = now() WHERE metadata_fingerprint = ?", target, acked, fingerprint)
		if published.Error != nil {
			return published.Error
		}
		if published.RowsAffected != 1 {
			return errors.New("statement summary was invalidated while filling; nothing was published")
		}
		res.BuiltBefore = target
		// Fresh statistics, and a visibility map so recipient counts can be
		// read from the index alone.
		for _, table := range []string{statementDaysTable, statementDelegatorsTable, statementDirtyTable} {
			if err := conn.WithContext(ctx).Exec("VACUUM (ANALYZE) " + table).Error; err != nil {
				opts.logf("statement-summary fill: vacuum %s: %v", table, err)
			}
		}
		return nil
	})
	return res, err
}

// StatementSummaryRefreshResult counts what a refresh did.
type StatementSummaryRefreshResult struct {
	Cells       int
	Validators  int
	Advanced    bool
	BuiltBefore time.Time
	// Waiting is set when the run stopped to wait for open transactions.
	// Nothing is wrong: the marked months are still read live, and the next
	// run goes on from here.
	Waiting string
}

// RefreshStatementSummary recomputes every marked (validator, month) from
// taxable_events and then, when a month has ended since the last run,
// builds it and raises built_before.
//
// A writer adds no mark to a month that already has an unclaimed one. So the
// marks are first claimed (from then on a writer adds a new mark), then
// every transaction that may have relied on them is waited out, and only
// then are the months recomputed, from a snapshot that contains all of those
// writers, and the claimed marks removed in the same transaction.
func RefreshStatementSummary(ctx context.Context, db *gorm.DB, opts StatementSummaryOptions) (StatementSummaryRefreshResult, error) {
	var res StatementSummaryRefreshResult
	err := withSummaryLock(ctx, db, func(conn *gorm.DB) error {
		trust, err := readSummaryTrust(conn)
		if err != nil {
			return err
		}
		if reason := trust.Reason(); reason != "" {
			return fmt.Errorf("statement summary is not in use (%s); it needs a rebuild", reason)
		}
		built := monthFloor(*trust.BuiltBefore)
		res.BuiltBefore = built
		builtDay := built.Format(summaryDayLayout)

		// Claim. A claim left by an earlier run that had to wait is kept, with
		// its horizon, so the wait is not started over.
		var horizon *string
		if err := conn.WithContext(ctx).Raw("SELECT claim_horizon FROM statement_state").Row().Scan(&horizon); err != nil {
			return err
		}
		if horizon == nil {
			if err := conn.WithContext(ctx).Exec("UPDATE statement_dirty SET claimed = true WHERE NOT claimed AND month < ?::text::date", builtDay).Error; err != nil {
				return err
			}
			var claimed bool
			if err := conn.WithContext(ctx).Raw("SELECT EXISTS (SELECT 1 FROM statement_dirty WHERE claimed)").Row().Scan(&claimed); err != nil {
				return err
			}
			if claimed {
				h, err := summaryHorizon(ctx, conn)
				if err != nil {
					return err
				}
				if err := conn.WithContext(ctx).Exec("UPDATE statement_state SET claim_horizon = ?, updated_at = now()", h).Error; err != nil {
					return err
				}
				horizon = &h
			}
		}
		if horizon != nil {
			waiting, err := summaryDrain(ctx, conn, *horizon, opts)
			if err != nil {
				return err
			}
			if waiting != "" {
				res.Waiting = "changed months not recomputed yet: " + waiting
				opts.logf("statement-summary refresh: %s", res.Waiting)
				return nil
			}
			last := ""
			for {
				var val string
				err := conn.WithContext(ctx).Raw("SELECT validator_address FROM statement_dirty WHERE claimed AND validator_address > ? ORDER BY validator_address LIMIT 1", last).Row().Scan(&val)
				if errors.Is(err, sql.ErrNoRows) {
					break
				}
				if err != nil {
					return err
				}
				last = val
				cells, err := summaryRefreshValidator(ctx, conn, val)
				if err != nil {
					return fmt.Errorf("%s: %w", val, err)
				}
				res.Cells += cells
				res.Validators++
				opts.logf("statement-summary refresh %s: %d changed month(s) recomputed", val, cells)
			}
			if err := conn.WithContext(ctx).Exec("UPDATE statement_state SET claim_horizon = NULL, updated_at = now()").Error; err != nil {
				return err
			}
		}

		target := monthFloor(opts.now())
		if !built.Before(target) {
			return nil
		}
		waiting, err := summaryCoverFrom(ctx, conn, target, false, opts)
		if err != nil {
			return err
		}
		if waiting != "" {
			res.Waiting = fmt.Sprintf("months from %s not built yet: %s", built.Format(apiMonthLayout), waiting)
			opts.logf("statement-summary refresh: %s", res.Waiting)
			return nil
		}
		validators, err := summaryValidators(conn.WithContext(ctx))
		if err != nil {
			return err
		}
		for _, val := range validators {
			if _, _, err := summaryBuildValidator(ctx, conn, val, built, target, opts); err != nil {
				return fmt.Errorf("%s: %w", val, err)
			}
		}
		if summaryPublishHook != nil {
			summaryPublishHook()
		}
		published := conn.WithContext(ctx).Exec("UPDATE statement_state SET built_before = ?, updated_at = now() WHERE metadata_fingerprint IS NOT NULL", target)
		if published.Error != nil {
			return published.Error
		}
		if published.RowsAffected != 1 {
			return errors.New("statement summary was invalidated while advancing; nothing was published")
		}
		res.Advanced, res.BuiltBefore = true, target
		opts.logf("statement-summary refresh: built %s to %s for %d validators", built.Format(apiMonthLayout), target.AddDate(0, -1, 0).Format(apiMonthLayout), len(validators))
		return nil
	})
	return res, err
}

// summaryRefreshHook, when set by a test, runs after a validator's changed
// months are recomputed and before their marks are removed.
var summaryRefreshHook func()

// summaryPublishHook lets tests race invalidation with actual publication.
var summaryPublishHook func()

// summaryRefreshValidator recomputes the claimed months of val and removes
// their claimed marks, in one transaction and one snapshot.
func summaryRefreshValidator(ctx context.Context, conn *gorm.DB, val string) (int, error) {
	cells := 0
	err := conn.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := summaryBuildSettings(tx); err != nil {
			return err
		}
		rows, err := tx.Raw("SELECT DISTINCT month FROM statement_dirty WHERE validator_address = ? AND claimed ORDER BY month", val).Rows()
		if err != nil {
			return err
		}
		var months []time.Time
		for rows.Next() {
			var m time.Time
			if err := rows.Scan(&m); err != nil {
				_ = rows.Close()
				return err
			}
			months = append(months, monthFloor(m))
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, m := range months {
			if _, _, err := summaryRebuildRange(tx, val, m, m.AddDate(0, 1, 0)); err != nil {
				return err
			}
		}
		if summaryRefreshHook != nil {
			summaryRefreshHook()
		}
		if err := tx.Exec("DELETE FROM statement_dirty WHERE validator_address = ? AND claimed", val).Error; err != nil {
			return err
		}
		cells = len(months)
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	return cells, err
}

// StatementSummaryStatus is the state of the summary for the Job's status
// line and the alerts built on it.
type StatementSummaryStatus struct {
	Installed    bool
	Trusted      bool
	Reason       string
	LogBefore    *time.Time
	BuiltBefore  *time.Time
	DirtyRows    int64
	DirtyCells   int64
	OldestDirty  time.Duration
	Failures     int64
	DayRows      int64
	Pairs        int64
	SummaryBytes int64
}

// Line renders the status as one key=value log line.
func (s StatementSummaryStatus) Line() string {
	day := func(t *time.Time) string {
		if t == nil {
			return "none"
		}
		return t.UTC().Format(summaryDayLayout)
	}
	reason := s.Reason
	if reason == "" {
		reason = "ok"
	}
	return fmt.Sprintf("statement-summary status installed=%t trusted=%t built_before=%s log_before=%s dirty_rows=%d dirty_cells=%d oldest_dirty_seconds=%d log_failures=%d day_rows=%d delegator_months=%d bytes=%d reason=%q",
		s.Installed, s.Trusted, day(s.BuiltBefore), day(s.LogBefore), s.DirtyRows, s.DirtyCells, int64(s.OldestDirty.Seconds()), s.Failures, s.DayRows, s.Pairs, s.SummaryBytes, reason)
}

// ReadStatementSummaryStatus reads the status. The row counts of the two
// summary tables are the planner's, not a scan.
func ReadStatementSummaryStatus(ctx context.Context, db *gorm.DB) (StatementSummaryStatus, error) {
	var st StatementSummaryStatus
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		trust, err := readSummaryTrust(tx)
		if err != nil {
			return err
		}
		st.Installed = trust.Installed
		st.Reason = trust.Reason()
		st.Trusted = st.Reason == ""
		if !trust.Installed {
			return nil
		}
		st.LogBefore, st.BuiltBefore, st.Failures = trust.LogBefore, trust.BuiltBefore, trust.Failures-trust.Acked
		var oldest float64
		if err := tx.Raw(`SELECT count(*), count(DISTINCT (validator_address, month)), COALESCE(EXTRACT(EPOCH FROM clock_timestamp() - min(logged_at)), 0)
			FROM statement_dirty WHERE month < (SELECT built_before FROM statement_state) OR validator_address = '*'`).Row().Scan(&st.DirtyRows, &st.DirtyCells, &oldest); err != nil {
			return err
		}
		st.OldestDirty = time.Duration(oldest * float64(time.Second))
		return tx.Raw(`SELECT GREATEST((SELECT reltuples FROM pg_class WHERE oid = 'statement_days'::regclass), 0)::bigint,
			GREATEST((SELECT reltuples FROM pg_class WHERE oid = 'statement_delegators'::regclass), 0)::bigint,
			pg_total_relation_size('statement_days') + pg_total_relation_size('statement_delegators') + pg_total_relation_size('statement_dirty')`).Row().Scan(&st.DayRows, &st.Pairs, &st.SummaryBytes)
	}, statementReadOnly())
	return st, err
}

// StatementSummaryMismatch is one (validator, month) whose summary differs
// from taxable_events.
type StatementSummaryMismatch struct {
	Validator      string
	Month          string
	DayRows        int64
	DelegatorMonth int64
}

// StatementSummaryCheckResult is the outcome of a spot check.
type StatementSummaryCheckResult struct {
	Checked    int
	Mismatches []StatementSummaryMismatch
}

const summaryCompareSQL = `WITH ev AS MATERIALIZED (
	SELECT (timestamp AT TIME ZONE 'UTC')::date AS day, category, denom, amount, to_addr
	FROM ONLY taxable_events
	WHERE validator_address = @validator AND category IN ('reward','commission') AND timestamp >= @from AND timestamp < @to
), b_days AS (
	SELECT day, category, denom, SUM(amount::numeric) AS amount, COUNT(*) AS events FROM ev GROUP BY day, category, denom
), s_days AS (
	SELECT day, category, denom, amount, events FROM statement_days
	WHERE validator_address = @validator AND day >= CAST(CAST(@lo AS text) AS date) AND day < CAST(CAST(@hi AS text) AS date)
), b_del AS (
	SELECT DISTINCT date_trunc('month', day::timestamp)::date AS month, to_addr FROM ev WHERE category = 'reward' AND to_addr IS NOT NULL
), s_del AS (
	SELECT month, to_addr FROM statement_delegators
	WHERE validator_address = @validator AND month >= CAST(CAST(@lo AS text) AS date) AND month < CAST(CAST(@hi AS text) AS date)
)
SELECT (SELECT count(*) FROM ((TABLE b_days EXCEPT TABLE s_days) UNION ALL (TABLE s_days EXCEPT TABLE b_days)) x),
	(SELECT count(*) FROM ((TABLE b_del EXCEPT TABLE s_del) UNION ALL (TABLE s_del EXCEPT TABLE b_del)) x)`

// summaryCompare counts the day rows and the delegator months that differ
// between taxable_events and the summary for val over the whole months in
// [from, to), in tx's snapshot.
func summaryCompare(tx *gorm.DB, val string, from, to time.Time) (days, pairs int64, err error) {
	err = tx.Raw(summaryCompareSQL, sql.Named("validator", val), sql.Named("from", from), sql.Named("to", to),
		sql.Named("lo", from.Format(summaryDayLayout)), sql.Named("hi", to.Format(summaryDayLayout))).Row().Scan(&days, &pairs)
	return days, pairs, err
}

// withdrawStatementSummary uses a fresh pooled connection and context. The
// nontransactional counter survives caller cancellation, a blocked state row,
// or a builder that already passed its initial trust check. Clearing the
// fingerprint is an independent durable fallback and cannot be republished.
func withdrawStatementSummary(db *gorm.DB, schema string) error {
	counterCtx, cancelCounter := context.WithTimeout(context.Background(), 5*time.Second)
	counterErr := db.WithContext(counterCtx).Exec("SELECT nextval(?::regclass)", schema+"."+statementFailuresSeq).Error
	cancelCounter()
	stateCtx, cancelState := context.WithTimeout(context.Background(), 5*time.Second)
	stateErr := db.WithContext(stateCtx).Exec("UPDATE " + schema + ".statement_state SET built_before = NULL, metadata_fingerprint = NULL, updated_at = now()").Error
	cancelState()
	if counterErr == nil || stateErr == nil {
		return nil
	}
	return fmt.Errorf("both integrity counter and state withdrawal failed: %v; %w", counterErr, stateErr)
}

var summaryCompareHook func()

// CheckStatementSummary compares the summary with taxable_events for
// (validator, month) cells, each in one read-only snapshot that also proves
// the cell was not marked changed. With cells <= 0 it compares every month
// of every validator. Otherwise half the sample is drawn from the cells the
// summary holds and half from validators and months at random, so a month
// the summary is missing altogether is found too.
func CheckStatementSummary(ctx context.Context, db *gorm.DB, cells int, seed int64, opts StatementSummaryOptions) (StatementSummaryCheckResult, error) {
	var res StatementSummaryCheckResult
	err := withSummaryLock(ctx, db, func(conn *gorm.DB) error {
		var err error
		res, err = checkStatementSummaryLocked(ctx, conn, db, cells, seed, opts)
		return err
	})
	return res, err
}

func checkStatementSummaryLocked(ctx context.Context, db, withdrawalDB *gorm.DB, cells int, seed int64, opts StatementSummaryOptions) (StatementSummaryCheckResult, error) {
	var res StatementSummaryCheckResult
	trust, err := readSummaryTrust(db.WithContext(ctx))
	if err != nil {
		return res, err
	}
	if reason := trust.Reason(); reason != "" {
		return res, fmt.Errorf("statement summary is not in use (%s)", reason)
	}
	schema, err := summarySchema(db.WithContext(ctx))
	if err != nil {
		return res, err
	}
	built := monthFloor(*trust.BuiltBefore)
	validators, err := summaryValidators(db.WithContext(ctx))
	if err != nil {
		return res, err
	}
	// Stored-only validators and months matter too: a missing base validator
	// must not hide orphaned summary amounts or recipients from a full check.
	var stored []string
	if err := db.WithContext(ctx).Raw(`SELECT validator_address FROM statement_days UNION SELECT validator_address FROM statement_delegators`).Scan(&stored).Error; err != nil {
		return res, err
	}
	seen := make(map[string]bool, len(validators)+len(stored))
	for _, val := range validators {
		seen[val] = true
	}
	for _, val := range stored {
		if !seen[val] {
			validators = append(validators, val)
			seen[val] = true
		}
	}
	sort.Strings(validators)
	type cell struct {
		val      string
		from, to time.Time
	}
	var todo []cell
	if cells <= 0 {
		for _, val := range validators {
			first, err := summaryFirstRow(db.WithContext(ctx), val)
			if err != nil {
				return res, err
			}
			from := built
			if first != nil && monthFloor(*first).Before(built) {
				from = monthFloor(*first)
			}
			// Summary rows from before the validator's first row would be wrong too.
			var earliest *time.Time
			if err := db.WithContext(ctx).Raw("SELECT LEAST((SELECT min(day)::timestamp FROM statement_days WHERE validator_address = ?), (SELECT min(month)::timestamp FROM statement_delegators WHERE validator_address = ?))", val, val).Row().Scan(&earliest); err != nil {
				return res, err
			}
			if earliest != nil && monthFloor(*earliest).Before(from) {
				from = monthFloor(*earliest)
			}
			for lo := from; lo.Before(built); lo = lo.AddDate(0, 1, 0) {
				todo = append(todo, cell{val, lo, lo.AddDate(0, 1, 0)})
			}
		}
	} else {
		rng := rand.New(rand.NewSource(seed)) //nolint:gosec // sampling, not security
		rows, err := db.WithContext(ctx).Raw(`SELECT validator_address, month FROM (
			SELECT validator_address, date_trunc('month', day::timestamp)::timestamp AS month FROM statement_days
			UNION SELECT validator_address, month::timestamp FROM statement_delegators
		) cells ORDER BY md5(validator_address || month::text || ?::text) LIMIT ?`, seed, (cells+1)/2).Rows()
		if err != nil {
			return res, err
		}
		for rows.Next() {
			var val string
			var m time.Time
			if err := rows.Scan(&val, &m); err != nil {
				_ = rows.Close()
				return res, err
			}
			m = monthFloor(m)
			todo = append(todo, cell{val, m, m.AddDate(0, 1, 0)})
		}
		if err := rows.Close(); err != nil {
			return res, err
		}
		for i := 0; i < cells/2 && len(validators) > 0; i++ {
			val := validators[rng.Intn(len(validators))]
			first, err := summaryFirstRow(db.WithContext(ctx), val)
			if err != nil {
				return res, err
			}
			if first == nil || !monthFloor(*first).Before(built) {
				continue
			}
			from := monthFloor(*first)
			months := (built.Year()-from.Year())*12 + int(built.Month()) - int(from.Month())
			m := from.AddDate(0, rng.Intn(months), 0)
			todo = append(todo, cell{val, m, m.AddDate(0, 1, 0)})
		}
	}
	sort.Slice(todo, func(i, j int) bool {
		if todo[i].val != todo[j].val {
			return todo[i].val < todo[j].val
		}
		return todo[i].from.Before(todo[j].from)
	})
	for _, c := range todo {
		var days, pairs int64
		skipped := false
		err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := summaryBuildSettings(tx); err != nil {
				return err
			}
			// A cell marked changed in this snapshot, or no longer covered,
			// is the refresh Job's to recompute, not a mismatch.
			now, err := readSummaryTrust(tx)
			if err != nil {
				return err
			}
			if now.Reason() != "" || monthFloor(*now.BuiltBefore).Before(c.to) {
				skipped = true
				return nil
			}
			var dirty bool
			if err := tx.Raw("SELECT EXISTS (SELECT 1 FROM statement_dirty WHERE validator_address = ? AND month = ?::text::date)", c.val, c.from.Format(summaryDayLayout)).Row().Scan(&dirty); err != nil {
				return err
			}
			if dirty {
				skipped = true
				return nil
			}
			days, pairs, err = summaryCompare(tx, c.val, c.from, c.to)
			if err == nil && summaryCompareHook != nil {
				summaryCompareHook()
			}
			return err
		}, statementReadOnly())
		if !skipped && (days != 0 || pairs != 0) {
			res.Checked++
			res.Mismatches = append(res.Mismatches, StatementSummaryMismatch{Validator: c.val, Month: c.from.Format(apiMonthLayout), DayRows: days, DelegatorMonth: pairs})
			// Withdraw even if read-only transaction completion was cancelled
			// after returning a mismatch. A durable counter/fingerprint marker
			// cannot be undone by a builder if cancellation retired this session.
			if withdrawErr := withdrawStatementSummary(withdrawalDB, schema); withdrawErr != nil {
				return res, fmt.Errorf("summary mismatched and could not be withdrawn: %w", withdrawErr)
			}
			opts.logf("statement-summary check MISMATCH %s %s: %d day row(s), %d delegator entr(ies) differ", c.val, c.from.Format(apiMonthLayout), days, pairs)
			if err != nil {
				return res, err
			}
			return res, ctx.Err()
		}
		if err != nil {
			return res, fmt.Errorf("%s %s: %w", c.val, c.from.Format(apiMonthLayout), err)
		}
		if !skipped {
			res.Checked++
		}
	}
	return res, nil
}
