package taxapi

// Coverage rollup (OPS-671). GET /coverage counts every message, and how many
// of them the tax layer classified, by message type over a period. Read live
// over all history that was 4 to 7 minutes and up to 10 GB of Postgres temp
// files per request. The rollup keeps those counts per UTC day, message type
// and fixed range of message ids (a chunk), plus transaction counts per day,
// so a period is a sum over a few thousand rows.
//
// The taxcoverage Job is the only writer. The API only reads: it sums the
// whole days the period covers, counts ids above the built frontier live, and
// recounts a partial first or last day from the chunks that have rows on it.
// When the tables are missing, unreadable or too far behind, /coverage runs
// the original live query instead, with the same result.
//
// A chunk is rebuilt when it can have changed: the last (still filling) chunk
// every run, a chunk whose message ids gained a taxable event (taxable_events
// ids at or above the last run's frontier, less a margin for late commits), a
// recent chunk whose row count moved, a chunk whose distinct classified
// message count no longer matches (a fingerprint pass after taxable_events
// rows were deleted or updated, and every few hours regardless), and the
// oldest chunks in turn, so every chunk is recomputed about once a day.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

const (
	coverageRollupVersion = 1

	rollupSourceMessages = 1
	rollupSourceTxes     = 2

	// DefaultCoverageMessageChunk and DefaultCoverageTxChunk are the id range
	// one chunk covers. A message chunk took about 7 s to count live.
	DefaultCoverageMessageChunk int64 = 500_000
	DefaultCoverageTxChunk      int64 = 1_000_000

	// coverageRollupLockKey serializes taxcoverage runs (session advisory lock).
	coverageRollupLockKey int64 = 671_000_001

	// coverageRollupMaxTail is the most message or tx ids above the built
	// frontier the API counts live before it treats the rollup as behind.
	coverageRollupMaxTail int64 = 1_500_000

	// coverageRollupTimeout bounds one /coverage computation from the rollup.
	coverageRollupTimeout = 100 * time.Second
	// statementScanTimeout bounds the scan counts a coverage statement adds to
	// a report; the statement says "not available" past it.
	statementScanTimeout = 10 * time.Second
)

// ErrCoverageRollupBusy is returned when another taxcoverage run holds the lock.
var ErrCoverageRollupBusy = errors.New("coverage rollup: another run holds the lock")

// errCoverageRollupUnavailable means the API cannot answer from the rollup.
var errCoverageRollupUnavailable = errors.New("coverage rollup unavailable")

// CoverageRollupDDL is what the database owner runs once to create the
// rollup (bare-metal cluster/apps/cosmos-tax-auth/coverage-rollup.sql). It is
// additive: no existing table is touched.
const CoverageRollupDDL = `
CREATE TABLE IF NOT EXISTS tax_coverage_rollup_state (
  id smallint PRIMARY KEY CHECK (id = 1),
  version integer NOT NULL,
  message_chunk bigint NOT NULL CHECK (message_chunk > 0),
  tx_chunk bigint NOT NULL CHECK (tx_chunk > 0),
  messages_through bigint NOT NULL DEFAULT 0,
  txes_through bigint NOT NULL DEFAULT 0,
  te_through bigint NOT NULL DEFAULT 0,
  te_changes bigint NOT NULL DEFAULT -1,
  fingerprint_from bigint NOT NULL DEFAULT -1,
  fingerprint_at timestamptz,
  built_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS tax_coverage_rollup_chunks (
  source smallint NOT NULL,
  chunk_lo bigint NOT NULL,
  chunk_hi bigint NOT NULL,
  id_rows bigint NOT NULL,
  te_messages bigint NOT NULL,
  dirty boolean NOT NULL DEFAULT false,
  built_at timestamptz NOT NULL,
  PRIMARY KEY (source, chunk_lo)
);
CREATE TABLE IF NOT EXISTS tax_coverage_rollup (
  source smallint NOT NULL,
  chunk_lo bigint NOT NULL,
  chain_id bigint NOT NULL,
  day date NOT NULL,
  message_type_id bigint NOT NULL,
  total bigint NOT NULL,
  classified bigint NOT NULL,
  PRIMARY KEY (source, chunk_lo, chain_id, day, message_type_id)
);
CREATE INDEX IF NOT EXISTS tax_coverage_rollup_day ON tax_coverage_rollup (source, day);
INSERT INTO tax_coverage_rollup_state (id, version, message_chunk, tx_chunk)
VALUES (1, 1, 500000, 1000000) ON CONFLICT (id) DO NOTHING;
`

// Chunk queries. The block is read per transaction through a LATERAL probe
// (OFFSET 0 keeps it a probe): joining blocks directly hashed all 28.7M blocks
// and spilled about 1.2 GB of temp per million messages.
const (
	rollupMessageCounts = `SELECT b.chain_id AS chain_id, (b.time_stamp AT TIME ZONE 'UTC')::date AS day, m.message_type_id AS message_type_id,
 count(*) AS total,
 count(*) FILTER (WHERE EXISTS (SELECT 1 FROM taxable_events te WHERE te.message_id = m.id)) AS classified
FROM messages m
JOIN message_types mt ON mt.id = m.message_type_id
JOIN txes t ON t.id = m.tx_id
CROSS JOIN LATERAL (SELECT b.chain_id, b.time_stamp FROM blocks b WHERE b.id = t.block_id OFFSET 0) b
WHERE m.id >= @lo AND m.id < @hi AND b.time_stamp IS NOT NULL AND b.chain_id IS NOT NULL
GROUP BY 1, 2, 3`
	rollupTxCounts = `SELECT b.chain_id AS chain_id, (b.time_stamp AT TIME ZONE 'UTC')::date AS day, 0::bigint AS message_type_id,
 count(*) AS total, 0::bigint AS classified
FROM txes t
CROSS JOIN LATERAL (SELECT b.chain_id, b.time_stamp FROM blocks b WHERE b.id = t.block_id OFFSET 0) b
WHERE t.id >= @lo AND t.id < @hi AND b.time_stamp IS NOT NULL AND b.chain_id IS NOT NULL
GROUP BY 1, 2`
	// Live counts by type for an id range and an exact time window, for the
	// unbuilt tail and partial days.
	liveMessageCounts = `SELECT mt.message_type AS message_type, count(*) AS total,
 count(*) FILTER (WHERE EXISTS (SELECT 1 FROM taxable_events te WHERE te.message_id = m.id)) AS classified
FROM messages m
JOIN message_types mt ON mt.id = m.message_type_id
JOIN txes t ON t.id = m.tx_id
CROSS JOIN LATERAL (SELECT b.chain_id, b.time_stamp FROM blocks b WHERE b.id = t.block_id OFFSET 0) b
WHERE m.id >= @lo AND m.id < @hi AND b.time_stamp >= @from AND b.time_stamp < @to
 AND b.chain_id IN (SELECT c.id FROM chains c WHERE c.chain_id IN @chains)
GROUP BY 1`
	liveTxCounts = `SELECT count(*) FROM txes t
CROSS JOIN LATERAL (SELECT b.chain_id, b.time_stamp FROM blocks b WHERE b.id = t.block_id OFFSET 0) b
WHERE t.id >= @lo AND t.id < @hi AND b.time_stamp >= @from AND b.time_stamp < @to
 AND b.chain_id IN (SELECT c.id FROM chains c WHERE c.chain_id IN @chains)`
)

type rollupState struct {
	Version         int
	MessageChunk    int64
	TxChunk         int64
	MessagesThrough int64
	TxesThrough     int64
	TeThrough       int64
	TeChanges       int64
	FingerprintFrom int64
	FingerprintAt   *time.Time
	BuiltAt         time.Time
}

func (st rollupState) chunk(source int) int64 {
	if source == rollupSourceTxes {
		return st.TxChunk
	}
	return st.MessageChunk
}

func (st rollupState) through(source int) int64 {
	if source == rollupSourceTxes {
		return st.TxesThrough
	}
	return st.MessagesThrough
}

func readRollupState(db *gorm.DB) (rollupState, bool, error) {
	var rows []rollupState
	err := db.Raw(`SELECT version, message_chunk, tx_chunk, messages_through, txes_through, te_through, te_changes,
 fingerprint_from, fingerprint_at, built_at FROM tax_coverage_rollup_state WHERE id = 1`).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return rollupState{}, false, err
	}
	return rows[0], true, nil
}

func rollupTop(db *gorm.DB, table string) (int64, error) {
	var top int64
	err := db.Raw("SELECT coalesce(max(id), 0) + 1 FROM " + table).Scan(&top).Error
	return top, err
}

// teChangeCounter is the deletes plus updates Postgres has counted on
// taxable_events. It is statistics, not data: any move starts a fingerprint
// pass, and a reset (a smaller number) does too.
func teChangeCounter(db *gorm.DB) (int64, error) {
	var n sql.NullInt64
	err := db.Raw(`SELECT n_tup_del + n_tup_upd FROM pg_stat_all_tables WHERE relid = to_regclass('taxable_events')`).Scan(&n).Error
	return n.Int64, err
}

// CoverageRollupOptions tune a taxcoverage run.
type CoverageRollupOptions struct {
	// Budget stops a run after this long, with its progress committed; zero
	// runs until the rollup is current.
	Budget time.Duration
	// BatchTime ends a build transaction after this long (default 60s).
	BatchTime time.Duration
	// Rolling is how many of the oldest chunks per source a current rollup
	// rebuilds each run (default 1).
	Rolling int
	// TeMargin re-reads this many taxable_events ids below the last frontier
	// for changes committed late (default 20000).
	TeMargin int64
	// RecentChecks recounts the rows of this many full chunks below the last
	// one each run (default 4).
	RecentChecks int
	// FingerprintEvery starts a fingerprint pass at least this often (default 6h);
	// FingerprintAfterChange starts one this long after the last when
	// taxable_events rows were deleted or updated (default 1h).
	FingerprintEvery       time.Duration
	FingerprintAfterChange time.Duration
	// FingerprintPerRun caps the chunks one run fingerprints (default 60).
	FingerprintPerRun int
	// ForceFingerprint starts a pass now.
	ForceFingerprint bool
	// WorkMem is SET LOCAL work_mem for build transactions (default 32MB).
	WorkMem string
	Logf    func(string, ...any)
	Now     func() time.Time
}

func (o *CoverageRollupOptions) defaults() {
	if o.BatchTime <= 0 {
		o.BatchTime = time.Minute
	}
	if o.Rolling < 0 {
		o.Rolling = 0
	} else if o.Rolling == 0 {
		o.Rolling = 1
	}
	if o.TeMargin <= 0 {
		o.TeMargin = 20_000
	}
	if o.RecentChecks <= 0 {
		o.RecentChecks = 4
	}
	if o.FingerprintEvery <= 0 {
		o.FingerprintEvery = 6 * time.Hour
	}
	if o.FingerprintAfterChange <= 0 {
		o.FingerprintAfterChange = time.Hour
	}
	if o.FingerprintPerRun <= 0 {
		o.FingerprintPerRun = 60
	}
	if o.WorkMem == "" {
		o.WorkMem = "32MB"
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
}

// CoverageRollupResult is what one run did.
type CoverageRollupResult struct {
	Built, Dirtied, Rebuilt, Rolled, Fingerprinted int
	MessagesThrough, MessagesTop                   int64
	TxesThrough, TxesTop                           int64
	Current                                        bool
}

type rollupRun struct {
	db       *gorm.DB
	opts     CoverageRollupOptions
	deadline time.Time
	res      CoverageRollupResult
}

func (r *rollupRun) outOfTime() bool {
	return !r.deadline.IsZero() && time.Now().After(r.deadline)
}

// RefreshCoverageRollup brings the rollup up to the indexed frontier. The
// caller gives it a pool of one connection, so the session lock holds for
// the whole run.
func RefreshCoverageRollup(ctx context.Context, db *gorm.DB, opts CoverageRollupOptions) (CoverageRollupResult, error) {
	opts.defaults()
	db = db.WithContext(ctx)
	var locked bool
	if err := db.Raw("SELECT pg_try_advisory_lock(?)", coverageRollupLockKey).Scan(&locked).Error; err != nil {
		return CoverageRollupResult{}, err
	}
	if !locked {
		return CoverageRollupResult{}, ErrCoverageRollupBusy
	}
	defer db.Session(&gorm.Session{Context: context.Background()}).Exec("SELECT pg_advisory_unlock(?)", coverageRollupLockKey)
	r := &rollupRun{db: db, opts: opts}
	if opts.Budget > 0 {
		r.deadline = time.Now().Add(opts.Budget)
	}
	err := r.run()
	return r.res, err
}

func (r *rollupRun) run() error {
	st, ok, err := readRollupState(r.db)
	if err != nil {
		return err
	}
	if !ok {
		if err := r.db.Exec(`INSERT INTO tax_coverage_rollup_state (id, version, message_chunk, tx_chunk)
 VALUES (1, ?, ?, ?) ON CONFLICT (id) DO NOTHING`, coverageRollupVersion, DefaultCoverageMessageChunk, DefaultCoverageTxChunk).Error; err != nil {
			return err
		}
		if st, _, err = readRollupState(r.db); err != nil {
			return err
		}
	}
	if st.Version != coverageRollupVersion {
		return fmt.Errorf("coverage rollup: tables are version %d, this build writes version %d", st.Version, coverageRollupVersion)
	}
	msgTop, err := rollupTop(r.db, "messages")
	if err != nil {
		return err
	}
	txTop, err := rollupTop(r.db, "txes")
	if err != nil {
		return err
	}
	teTop, err := rollupTop(r.db, "taxable_events")
	if err != nil {
		return err
	}
	r.res.MessagesTop, r.res.TxesTop = msgTop, txTop

	// 1. Chunks whose messages gained or changed a taxable event since the
	// last run, and recent chunks whose row count moved.
	if err := r.markDirty(st, teTop); err != nil {
		return err
	}
	// 2. Rebuild dirty chunks.
	if err := r.rebuildWhere("dirty", "ORDER BY source, chunk_lo"); err != nil {
		return err
	}
	// 3. Extend to the frontier: the last chunk again, then new chunks.
	for _, src := range []int{rollupSourceMessages, rollupSourceTxes} {
		top := msgTop
		if src == rollupSourceTxes {
			top = txTop
		}
		if err := r.extend(src, top); err != nil {
			return err
		}
	}
	st, _, err = readRollupState(r.db)
	if err != nil {
		return err
	}
	r.res.MessagesThrough, r.res.TxesThrough = st.MessagesThrough, st.TxesThrough
	r.res.Current = st.MessagesThrough >= msgTop && st.TxesThrough >= txTop
	if !r.res.Current || r.outOfTime() {
		return nil
	}
	// 4. Fingerprint pass over message chunks, a slice per run.
	if err := r.fingerprint(st); err != nil {
		return err
	}
	if err := r.rebuildWhere("dirty", "ORDER BY source, chunk_lo"); err != nil {
		return err
	}
	// 5. The oldest chunks in turn.
	for _, src := range []int{rollupSourceMessages, rollupSourceTxes} {
		n0 := r.res.Rebuilt
		if err := r.rebuildWhere(fmt.Sprintf("source = %d", src), fmt.Sprintf("ORDER BY built_at, chunk_lo LIMIT %d", r.opts.Rolling)); err != nil {
			return err
		}
		r.res.Rolled += r.res.Rebuilt - n0
	}
	return nil
}

// markDirty flags message chunks named by new taxable_events ids and recent
// chunks whose id count moved, then advances te_through, in one transaction.
func (r *rollupRun) markDirty(st rollupState, teTop int64) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if st.MessagesThrough > 0 && st.TeThrough < teTop {
			// Ids past the last frontier are new: their chunks are rebuilt.
			res := tx.Exec(`UPDATE tax_coverage_rollup_chunks c SET dirty = true
WHERE c.source = 1 AND NOT c.dirty AND c.chunk_lo IN (
 SELECT DISTINCT (te.message_id / @size) * @size FROM taxable_events te
 WHERE te.id >= @from AND te.id < @top AND te.message_id IS NOT NULL AND te.message_id < @through)`,
				map[string]any{"size": st.MessageChunk, "from": st.TeThrough, "top": teTop, "through": st.MessagesThrough})
			if res.Error != nil {
				return res.Error
			}
			r.res.Dirtied += int(res.RowsAffected)
		}
		if st.MessagesThrough > 0 && st.TeThrough > 0 {
			// Ids just below it were read last run unless they committed late,
			// so their chunks are rebuilt only when the classified count moved.
			// Rebuilding them every run kept a run from reaching the frontier.
			res := tx.Exec(`UPDATE tax_coverage_rollup_chunks c SET dirty = true
WHERE c.source = 1 AND NOT c.dirty AND c.chunk_lo IN (
 SELECT DISTINCT (te.message_id / @size) * @size FROM taxable_events te
 WHERE te.id >= @from AND te.id < @to AND te.message_id IS NOT NULL AND te.message_id < @through)
 AND c.te_messages <> (SELECT count(DISTINCT te.message_id) FROM taxable_events te WHERE te.message_id >= c.chunk_lo AND te.message_id < c.chunk_hi)`,
				map[string]any{"size": st.MessageChunk, "from": max(st.TeThrough-r.opts.TeMargin, 0), "to": st.TeThrough, "through": st.MessagesThrough})
			if res.Error != nil {
				return res.Error
			}
			r.res.Dirtied += int(res.RowsAffected)
		}
		for _, src := range []int{rollupSourceMessages, rollupSourceTxes} {
			size, through := st.chunk(src), st.through(src)
			last := (through / size) * size
			if through%size == 0 {
				last = through
			}
			table := "messages"
			if src == rollupSourceTxes {
				table = "txes"
			}
			res := tx.Exec(`UPDATE tax_coverage_rollup_chunks c SET dirty = true
WHERE c.source = @src AND NOT c.dirty AND c.chunk_lo >= @from AND c.chunk_lo < @last
 AND c.id_rows <> (SELECT count(*) FROM `+table+` x WHERE x.id >= c.chunk_lo AND x.id < c.chunk_hi)`,
				map[string]any{"src": src, "from": last - int64(r.opts.RecentChecks)*size, "last": last})
			if res.Error != nil {
				return res.Error
			}
			r.res.Dirtied += int(res.RowsAffected)
		}
		return tx.Exec("UPDATE tax_coverage_rollup_state SET te_through = GREATEST(te_through, ?) WHERE id = 1", teTop).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
}

type rollupChunkRef struct {
	Source  int
	ChunkLo int64
	ChunkHi int64
}

// rebuildWhere rebuilds the chunks a filter names, in batches.
func (r *rollupRun) rebuildWhere(where, order string) error {
	var refs []rollupChunkRef
	if err := r.db.Raw("SELECT source, chunk_lo, chunk_hi FROM tax_coverage_rollup_chunks WHERE " + where + " " + order).Scan(&refs).Error; err != nil {
		return err
	}
	for len(refs) > 0 && !r.outOfTime() {
		n := 0
		err := r.batch(func(tx *gorm.DB) error {
			began := time.Now()
			for n < len(refs) && (n == 0 || time.Since(began) < r.opts.BatchTime) {
				c := refs[n]
				if err := buildRollupChunk(tx, c.Source, c.ChunkLo, c.ChunkHi, r.opts.Now()); err != nil {
					return err
				}
				n++
			}
			return tx.Exec("UPDATE tax_coverage_rollup_state SET built_at = ? WHERE id = 1", r.opts.Now()).Error
		})
		if err != nil {
			return err
		}
		r.res.Rebuilt += n
		refs = refs[n:]
	}
	return nil
}

// extend rebuilds the last chunk of a source with the new frontier and builds
// the chunks after it, advancing the frontier with each batch.
func (r *rollupRun) extend(src int, top int64) error {
	for !r.outOfTime() {
		st, _, err := readRollupState(r.db)
		if err != nil {
			return err
		}
		size, through := st.chunk(src), st.through(src)
		if through >= top {
			return nil
		}
		lo := (through / size) * size
		if through%size == 0 {
			lo = through
		}
		col := "messages_through"
		if src == rollupSourceTxes {
			col = "txes_through"
		}
		built := 0
		err = r.batch(func(tx *gorm.DB) error {
			began := time.Now()
			hi := lo
			for lo < top && (built == 0 || time.Since(began) < r.opts.BatchTime) {
				hi = min(lo+size, top)
				if err := buildRollupChunk(tx, src, lo, hi, r.opts.Now()); err != nil {
					return err
				}
				built++
				lo = hi
			}
			return tx.Exec("UPDATE tax_coverage_rollup_state SET "+col+" = ?, built_at = ? WHERE id = 1", hi, r.opts.Now()).Error
		})
		if err != nil {
			return err
		}
		r.res.Built += built
		r.opts.Logf("coverage rollup: source %d built %d chunk(s) through id %d of %d", src, built, lo, top)
	}
	return nil
}

func (r *rollupRun) batch(fn func(*gorm.DB) error) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL work_mem = '" + strings.ReplaceAll(r.opts.WorkMem, "'", "") + "'").Error; err != nil {
			return err
		}
		return fn(tx)
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
}

// buildRollupChunk replaces one chunk's rows with a fresh count of ids [lo, hi).
func buildRollupChunk(tx *gorm.DB, src int, lo, hi int64, now time.Time) error {
	q, table := rollupMessageCounts, "messages"
	if src == rollupSourceTxes {
		q, table = rollupTxCounts, "txes"
	}
	args := map[string]any{"lo": lo, "hi": hi}
	if err := tx.Exec("DELETE FROM tax_coverage_rollup WHERE source = ? AND chunk_lo = ?", src, lo).Error; err != nil {
		return err
	}
	if err := tx.Exec(`INSERT INTO tax_coverage_rollup (source, chunk_lo, chain_id, day, message_type_id, total, classified)
SELECT `+fmt.Sprint(src)+`, `+fmt.Sprint(lo)+`, chain_id, day, message_type_id, total, classified FROM (`+q+`) x`, args).Error; err != nil {
		return err
	}
	var idRows, teMessages int64
	if err := tx.Raw("SELECT count(*) FROM "+table+" WHERE id >= ? AND id < ?", lo, hi).Scan(&idRows).Error; err != nil {
		return err
	}
	if src == rollupSourceMessages {
		if err := tx.Raw("SELECT count(DISTINCT message_id) FROM taxable_events WHERE message_id >= ? AND message_id < ?", lo, hi).Scan(&teMessages).Error; err != nil {
			return err
		}
	}
	return tx.Exec(`INSERT INTO tax_coverage_rollup_chunks (source, chunk_lo, chunk_hi, id_rows, te_messages, dirty, built_at)
VALUES (?, ?, ?, ?, ?, false, ?)
ON CONFLICT (source, chunk_lo) DO UPDATE SET chunk_hi = EXCLUDED.chunk_hi, id_rows = EXCLUDED.id_rows,
 te_messages = EXCLUDED.te_messages, dirty = false, built_at = EXCLUDED.built_at`, src, lo, hi, idRows, teMessages, now).Error
}

// fingerprint compares each message chunk's distinct classified message count
// with the one stored when it was built, a slice of chunks per run, and marks
// the ones that differ dirty.
func (r *rollupRun) fingerprint(st rollupState) error {
	now := r.opts.Now()
	changes, err := teChangeCounter(r.db)
	if err != nil {
		return err
	}
	if st.FingerprintFrom < 0 {
		due := r.opts.ForceFingerprint || st.FingerprintAt == nil || now.Sub(*st.FingerprintAt) >= r.opts.FingerprintEvery ||
			(changes != st.TeChanges && now.Sub(*st.FingerprintAt) >= r.opts.FingerprintAfterChange)
		if !due {
			return nil
		}
		if err := r.db.Exec("UPDATE tax_coverage_rollup_state SET fingerprint_from = 0, te_changes = ? WHERE id = 1", changes).Error; err != nil {
			return err
		}
		st.FingerprintFrom = 0
		r.opts.Logf("coverage rollup: fingerprint pass started")
	}
	var refs []rollupChunkRef
	if err := r.db.Raw(`SELECT source, chunk_lo, chunk_hi FROM tax_coverage_rollup_chunks
WHERE source = 1 AND chunk_lo >= ? ORDER BY chunk_lo LIMIT ?`, st.FingerprintFrom, r.opts.FingerprintPerRun).Scan(&refs).Error; err != nil {
		return err
	}
	next := int64(-1)
	for i, c := range refs {
		if r.outOfTime() {
			next = c.ChunkLo
			break
		}
		res := r.db.Exec(`UPDATE tax_coverage_rollup_chunks c SET dirty = true
WHERE c.source = 1 AND c.chunk_lo = @lo AND NOT c.dirty AND c.te_messages <>
 (SELECT count(DISTINCT te.message_id) FROM taxable_events te WHERE te.message_id >= c.chunk_lo AND te.message_id < c.chunk_hi)`,
			map[string]any{"lo": c.ChunkLo})
		if res.Error != nil {
			return res.Error
		}
		r.res.Dirtied += int(res.RowsAffected)
		r.res.Fingerprinted++
		if i == len(refs)-1 && len(refs) == r.opts.FingerprintPerRun {
			next = c.ChunkLo + 1
		}
	}
	if next >= 0 {
		return r.db.Exec("UPDATE tax_coverage_rollup_state SET fingerprint_from = ? WHERE id = 1", next).Error
	}
	r.opts.Logf("coverage rollup: fingerprint pass finished")
	return r.db.Exec("UPDATE tax_coverage_rollup_state SET fingerprint_from = -1, fingerprint_at = ? WHERE id = 1", now).Error
}

// CheckCoverageRollup recounts n sampled chunks (0 = all) live, each against
// its stored rows in the same snapshot, and returns the chunks that differ.
// A chunk can differ for a few minutes after a change, until the next run.
func CheckCoverageRollup(ctx context.Context, db *gorm.DB, n int, seed int64, logf func(string, ...any)) ([]string, error) {
	db = db.WithContext(ctx)
	var refs []rollupChunkRef
	if err := db.Raw("SELECT source, chunk_lo, chunk_hi FROM tax_coverage_rollup_chunks ORDER BY source, chunk_lo").Scan(&refs).Error; err != nil {
		return nil, err
	}
	if n > 0 && n < len(refs) {
		rng := rand.New(rand.NewSource(seed)) //nolint:gosec // sampling, not security
		rng.Shuffle(len(refs), func(i, j int) { refs[i], refs[j] = refs[j], refs[i] })
		refs = refs[:n]
		sort.Slice(refs, func(i, j int) bool {
			return refs[i].Source < refs[j].Source || (refs[i].Source == refs[j].Source && refs[i].ChunkLo < refs[j].ChunkLo)
		})
	}
	type cell struct {
		ChainID, MessageTypeID, Total, Classified int64
		Day                                       time.Time
	}
	key := func(c cell) string {
		return fmt.Sprintf("%d|%s|%d|%d|%d", c.ChainID, c.Day.Format(apiDateLayout), c.MessageTypeID, c.Total, c.Classified)
	}
	var diffs []string
	for _, c := range refs {
		err := db.Transaction(func(tx *gorm.DB) error {
			q := rollupMessageCounts
			if c.Source == rollupSourceTxes {
				q = rollupTxCounts
			}
			var live, stored []cell
			if err := tx.Raw(q, map[string]any{"lo": c.ChunkLo, "hi": c.ChunkHi}).Scan(&live).Error; err != nil {
				return err
			}
			if err := tx.Raw("SELECT chain_id, day, message_type_id, total, classified FROM tax_coverage_rollup WHERE source = ? AND chunk_lo = ?", c.Source, c.ChunkLo).Scan(&stored).Error; err != nil {
				return err
			}
			a, b := make([]string, 0, len(live)), make([]string, 0, len(stored))
			for _, x := range live {
				a = append(a, key(x))
			}
			for _, x := range stored {
				b = append(b, key(x))
			}
			sort.Strings(a)
			sort.Strings(b)
			if strings.Join(a, ",") != strings.Join(b, ",") {
				diffs = append(diffs, fmt.Sprintf("source %d chunk %d", c.Source, c.ChunkLo))
			}
			return nil
		}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		if err != nil {
			return diffs, err
		}
	}
	if logf != nil {
		logf("coverage rollup check: %d chunk(s) recounted, %d differ", len(refs), len(diffs))
	}
	return diffs, nil
}

// CoverageRollupStatus is a one-line summary of the rollup.
func CoverageRollupStatus(ctx context.Context, db *gorm.DB) (string, error) {
	db = db.WithContext(ctx)
	st, ok, err := readRollupState(db)
	if err != nil {
		return "", err
	}
	if !ok {
		return "coverage rollup: no state row", nil
	}
	msgTop, err := rollupTop(db, "messages")
	if err != nil {
		return "", err
	}
	txTop, err := rollupTop(db, "txes")
	if err != nil {
		return "", err
	}
	var c struct{ Chunks, Dirty, Rows int64 }
	if err := db.Raw(`SELECT (SELECT count(*) FROM tax_coverage_rollup_chunks) AS chunks,
 (SELECT count(*) FROM tax_coverage_rollup_chunks WHERE dirty) AS dirty,
 (SELECT count(*) FROM tax_coverage_rollup) AS rows`).Scan(&c).Error; err != nil {
		return "", err
	}
	fp := "never"
	if st.FingerprintAt != nil {
		fp = st.FingerprintAt.UTC().Format(time.RFC3339)
	}
	if st.FingerprintFrom >= 0 {
		fp += fmt.Sprintf(" (pass at chunk %d)", st.FingerprintFrom)
	}
	return fmt.Sprintf("coverage rollup: messages through %d of %d, txes through %d of %d, %d chunks (%d dirty), %d rows, built %s, fingerprint %s",
		st.MessagesThrough, msgTop, st.TxesThrough, txTop, c.Chunks, c.Dirty, c.Rows, st.BuiltAt.UTC().Format(time.RFC3339), fp), nil
}

// ---- API read side ----

// coverageRollup answers period counts from the rollup for the API. It holds
// the root database handle, so a report's read snapshot never runs it.
type coverageRollup struct {
	db     *gorm.DB
	flight flightGroup

	mu    sync.Mutex
	cache map[string]rollupCacheEntry
}

type rollupCacheEntry struct {
	frontier string
	counts   *periodScanCounts
}

// periodScanCounts are a period's message counts by type and its transaction count.
type periodScanCounts struct {
	Types        []typeCount
	Messages     int64
	Transactions int64
	// Rollup is how the counts were made.
	Rollup CoverageRollupInfo
}

// CoverageRollupInfo says how /coverage was counted.
type CoverageRollupInfo struct {
	BuiltAt         time.Time `json:"built_at"`
	MessagesThrough int64     `json:"messages_through"`
	LiveMessages    int64     `json:"live_messages"`
	TxesThrough     int64     `json:"txes_through"`
	LiveTxes        int64     `json:"live_txes"`
}

func newCoverageRollup(db *gorm.DB) *coverageRollup {
	if db == nil {
		return nil
	}
	return &coverageRollup{db: db, cache: map[string]rollupCacheEntry{}}
}

func rollupReadOnly() *sql.TxOptions {
	return &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
}

// frontier reads what the cached counts depend on: the rollup state and the
// newest message, tx and taxable event ids.
func (c *coverageRollup) frontier(db *gorm.DB) (string, rollupState, error) {
	var readable bool
	if err := db.Raw(`SELECT coalesce(has_table_privilege(to_regclass('tax_coverage_rollup_state'), 'SELECT')
 AND has_table_privilege(to_regclass('tax_coverage_rollup'), 'SELECT')
 AND has_table_privilege(to_regclass('tax_coverage_rollup_chunks'), 'SELECT'), false)`).Scan(&readable).Error; err != nil {
		return "", rollupState{}, err
	}
	if !readable {
		return "", rollupState{}, errCoverageRollupUnavailable
	}
	st, ok, err := readRollupState(db)
	if err != nil {
		return "", rollupState{}, err
	}
	if !ok || st.Version != coverageRollupVersion || st.MessagesThrough == 0 {
		return "", rollupState{}, errCoverageRollupUnavailable
	}
	var tops struct{ Msg, Tx, Te int64 }
	if err := db.Raw(`SELECT (SELECT coalesce(max(id), 0) + 1 FROM messages) AS msg,
 (SELECT coalesce(max(id), 0) + 1 FROM txes) AS tx,
 (SELECT coalesce(max(id), 0) + 1 FROM taxable_events) AS te`).Scan(&tops).Error; err != nil {
		return "", rollupState{}, err
	}
	if tops.Msg-st.MessagesThrough > coverageRollupMaxTail || tops.Tx-st.TxesThrough > coverageRollupMaxTail {
		return "", rollupState{}, fmt.Errorf("%w: %d message and %d tx ids not built yet", errCoverageRollupUnavailable,
			tops.Msg-st.MessagesThrough, tops.Tx-st.TxesThrough)
	}
	return fmt.Sprintf("%d|%d|%d|%d|%d|%d", st.BuiltAt.UnixNano(), st.MessagesThrough, st.TxesThrough, tops.Msg, tops.Tx, tops.Te), st, nil
}

// periodCounts returns the counts for [start, end), from the cache when
// nothing they depend on moved. Identical requests in flight share one read.
func (c *coverageRollup) periodCounts(ctx context.Context, chain string, start, end time.Time) (*periodScanCounts, error) {
	if c == nil {
		return nil, errCoverageRollupUnavailable
	}
	period := chain + "|" + start.UTC().Format(time.RFC3339Nano) + "|" + end.UTC().Format(time.RFC3339Nano)
	frontier, _, err := c.frontier(c.db.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if e, ok := c.cache[period]; ok && e.frontier == frontier {
		c.mu.Unlock()
		return e.counts, nil
	}
	c.mu.Unlock()
	v, err := c.flight.do(ctx, period+"|"+frontier, func() (any, error) {
		cctx, cancel := context.WithTimeout(context.Background(), coverageRollupTimeout)
		defer cancel()
		var out *periodScanCounts
		var fr string
		err := c.db.WithContext(cctx).Transaction(func(tx *gorm.DB) error {
			var st rollupState
			var err error
			if fr, st, err = c.frontier(tx); err != nil {
				return err
			}
			out, err = rollupPeriodCounts(tx, st, chain, start, end)
			return err
		}, rollupReadOnly())
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		if len(c.cache) >= 256 {
			c.cache = map[string]rollupCacheEntry{}
		}
		c.cache[period] = rollupCacheEntry{frontier: fr, counts: out}
		c.mu.Unlock()
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*periodScanCounts), nil
}

func utcFloorDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// rollupWindows splits [start, end) into whole UTC days [fullFrom, fullTo)
// and at most two partial-day windows that are recounted live.
func rollupWindows(start, end time.Time) (fullFrom, fullTo time.Time, partial [][2]time.Time) {
	start, end = start.UTC(), end.UTC()
	if !end.After(start) {
		return start, start, nil
	}
	fullFrom = utcFloorDay(start)
	if !fullFrom.Equal(start) {
		fullFrom = fullFrom.AddDate(0, 0, 1)
		partial = append(partial, [2]time.Time{start, minTime(fullFrom, end)})
	}
	fullTo = utcFloorDay(end)
	if !fullTo.Equal(end) && !fullTo.Before(fullFrom) {
		partial = append(partial, [2]time.Time{fullTo, end})
	}
	if fullTo.Before(fullFrom) {
		fullTo = fullFrom
	}
	return fullFrom, fullTo, partial
}

// rollupPeriodCounts sums the rollup for [start, end) inside one read-only
// snapshot: whole days from stored rows, partial days and ids above the
// built frontier counted live.
func rollupPeriodCounts(tx *gorm.DB, st rollupState, chain string, start, end time.Time) (*periodScanCounts, error) {
	chains := chainIDs(chain)
	byType := map[string]*typeCount{}
	add := func(rows []typeCount) {
		for _, r := range rows {
			t := byType[r.MessageType]
			if t == nil {
				t = &typeCount{MessageType: r.MessageType}
				byType[r.MessageType] = t
			}
			t.Total += r.Total
			t.Classified += r.Classified
		}
	}
	var txCount int64
	fullFrom, fullTo, partial := rollupWindows(start, end)
	if fullTo.After(fullFrom) {
		var rows []typeCount
		args := map[string]any{"from": fullFrom.Format(apiDateLayout), "to": fullTo.Format(apiDateLayout), "chains": chains,
			"mthrough": st.MessagesThrough, "tthrough": st.TxesThrough}
		if err := tx.Raw(`SELECT mt.message_type AS message_type, sum(r.total)::bigint AS total, sum(r.classified)::bigint AS classified
FROM tax_coverage_rollup r JOIN message_types mt ON mt.id = r.message_type_id
WHERE r.source = 1 AND r.chunk_lo < @mthrough AND r.day >= CAST(@from AS date) AND r.day < CAST(@to AS date)
 AND r.chain_id IN (SELECT c.id FROM chains c WHERE c.chain_id IN @chains)
GROUP BY 1`, args).Scan(&rows).Error; err != nil {
			return nil, err
		}
		add(rows)
		var n sql.NullInt64
		if err := tx.Raw(`SELECT sum(r.total)::bigint FROM tax_coverage_rollup r
WHERE r.source = 2 AND r.chunk_lo < @tthrough AND r.day >= CAST(@from AS date) AND r.day < CAST(@to AS date)
 AND r.chain_id IN (SELECT c.id FROM chains c WHERE c.chain_id IN @chains)`, args).Scan(&n).Error; err != nil {
			return nil, err
		}
		txCount += n.Int64
	}
	// Partial days: only the chunks with rows on that day can hold activity in it.
	for _, w := range partial {
		day := utcFloorDay(w[0]).Format(apiDateLayout)
		for _, src := range []int{rollupSourceMessages, rollupSourceTxes} {
			var los []int64
			if err := tx.Raw(`SELECT DISTINCT chunk_lo FROM tax_coverage_rollup WHERE source = ? AND day = ?::date AND chunk_lo < ? ORDER BY 1`,
				src, day, st.through(src)).Scan(&los).Error; err != nil {
				return nil, err
			}
			for _, lo := range los {
				hi := min(lo+st.chunk(src), st.through(src))
				rows, n, err := liveCounts(tx, src, lo, hi, w[0], w[1], chains)
				if err != nil {
					return nil, err
				}
				add(rows)
				txCount += n
			}
		}
	}
	// Ids above the built frontier, counted live over the exact period.
	rows, _, err := liveCounts(tx, rollupSourceMessages, st.MessagesThrough, 1<<62, start, end, chains)
	if err != nil {
		return nil, err
	}
	add(rows)
	_, n, err := liveCounts(tx, rollupSourceTxes, st.TxesThrough, 1<<62, start, end, chains)
	if err != nil {
		return nil, err
	}
	txCount += n
	var tops struct{ Msg, Tx int64 }
	if err := tx.Raw(`SELECT (SELECT coalesce(max(id), 0) + 1 FROM messages) AS msg, (SELECT coalesce(max(id), 0) + 1 FROM txes) AS tx`).Scan(&tops).Error; err != nil {
		return nil, err
	}
	out := &periodScanCounts{Transactions: txCount, Rollup: CoverageRollupInfo{
		BuiltAt: st.BuiltAt.UTC(), MessagesThrough: st.MessagesThrough, TxesThrough: st.TxesThrough,
		LiveMessages: max(tops.Msg-st.MessagesThrough, 0), LiveTxes: max(tops.Tx-st.TxesThrough, 0),
	}}
	for _, t := range byType {
		if t.Total == 0 && t.Classified == 0 {
			continue
		}
		out.Types = append(out.Types, *t)
		out.Messages += t.Total
	}
	sortTypeCounts(out.Types)
	return out, nil
}

// sortTypeCounts orders as the live query does (total, descending), with the
// type name breaking ties so the order is stable.
func sortTypeCounts(rows []typeCount) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Total != rows[j].Total {
			return rows[i].Total > rows[j].Total
		}
		return rows[i].MessageType < rows[j].MessageType
	})
}

func liveCounts(tx *gorm.DB, src int, lo, hi int64, from, to time.Time, chains []string) ([]typeCount, int64, error) {
	args := map[string]any{"lo": lo, "hi": hi, "from": from.UTC(), "to": to.UTC(), "chains": chains}
	if src == rollupSourceTxes {
		var n int64
		err := tx.Raw(liveTxCounts, args).Scan(&n).Error
		return nil, n, err
	}
	var rows []typeCount
	err := tx.Raw(liveMessageCounts, args).Scan(&rows).Error
	return rows, 0, err
}

// flightGroup runs one call per key at a time; callers with the same key
// wait for it. The call runs on its own context, so one caller leaving does
// not cancel it for the others.
type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

type flightCall struct {
	done chan struct{}
	val  any
	err  error
}

func (g *flightGroup) do(ctx context.Context, key string, fn func() (any, error)) (any, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = map[string]*flightCall{}
	}
	c, ok := g.calls[key]
	if !ok {
		c = &flightCall{done: make(chan struct{})}
		g.calls[key] = c
		go func() {
			defer func() {
				g.mu.Lock()
				delete(g.calls, key)
				g.mu.Unlock()
				close(c.done)
			}()
			c.val, c.err = fn()
		}()
	}
	g.mu.Unlock()
	select {
	case <-c.done:
		return c.val, c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
