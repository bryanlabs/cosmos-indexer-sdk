package hub2promotion

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

var ErrCommitUnknown = errors.New("COMMIT outcome unknown; fresh read-only audit required; never auto-retry")
var ErrProductionDisabled = errors.New("production execution disabled; this command is approval-plan/private-fixture only")

type Result struct {
	BackendPID        int           `json:"backend_pid"`
	SynchronousCommit string        `json:"synchronous_commit"`
	Isolation         string        `json:"isolation"`
	Chunks            int           `json:"chunks"`
	Rows              int64         `json:"rows"`
	LockWait          time.Duration `json:"lock_wait"`
	LegacyLockHeld    time.Duration `json:"legacy_lock_held"`
	Duration          time.Duration `json:"duration"`
}
type hooks struct {
	beforeLocks  func() error
	afterChunk   func(int) error
	beforeCommit func() error
	commit       func(*sql.Tx) error
}

func DestinationLock(i Identity) int64 {
	h := sha256.Sum256([]byte(fmt.Sprintf("hub2-promotion-v1:%s:%d:%s:%s", i.Database, i.OID, i.SystemHash, i.PVCUID)))
	return int64(binary.BigEndian.Uint64(h[:8]))
}

// MergeFixture executes only bounded synthetic fixtures. There is deliberately
// no production-enable switch or environment DSN fallback. Private prefix scale
// has a separate named entry point; complete-chain plan admission is separate.
// A later reviewed approval adapter must reuse this core.
func MergeFixture(ctx context.Context, loader, auditor *sql.DB, m Manifest, runtime Identity) (Result, error) {
	if !m.Fixture || m.Profile != FixtureProfile {
		return Result{}, ErrProductionDisabled
	}
	return merge(ctx, loader, auditor, m, runtime, hooks{})
}

// MergePrivateScale uses the identical core only for the retained accepted
// 1.4M SQL prefix in a NEW private destination. It cannot represent migration.
func MergePrivateScale(ctx context.Context, loader, auditor *sql.DB, m Manifest, runtime Identity) (Result, error) {
	if !m.Fixture || m.Profile != PrivateScaleProfile {
		return Result{}, ErrProductionDisabled
	}
	return merge(ctx, loader, auditor, m, runtime, hooks{})
}

func merge(ctx context.Context, loader, auditor *sql.DB, m Manifest, runtime Identity, h hooks) (r Result, err error) {
	started := time.Now()
	defer func() { r.Duration = time.Since(started) }()
	if err = m.Recheck(); err != nil {
		return r, err
	}
	if runtime.DeploymentUID != m.Destination.DeploymentUID || runtime.PodUID != m.Destination.PodUID || runtime.PVCUID != m.Destination.PVCUID || runtime.Endpoint != m.Destination.Endpoint {
		return r, errors.New("runtime UID/endpoint mismatch")
	}
	conn, err := loader.Conn(ctx)
	if err != nil {
		return r, err
	}
	defer conn.Close()
	var current, session string
	if err = conn.QueryRowContext(ctx, `SELECT current_user::pg_catalog.text,session_user::pg_catalog.text,pg_catalog.pg_backend_pid()`).Scan(&current, &session, &r.BackendPID); err != nil {
		return r, err
	}
	if current != session || session != m.Scope.Loader {
		return r, errors.New("loader is not its authenticated role")
	}
	if _, err = conn.ExecContext(ctx, `SET search_path=pg_catalog,pg_temp; SET timezone='UTC'; SET datestyle='ISO, YMD'; SET row_security=on`); err != nil {
		return r, err
	}
	for _, key := range []int64{340310, DestinationLock(m.Destination)} {
		var got bool
		if err = conn.QueryRowContext(ctx, `SELECT pg_catalog.pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
			return r, err
		}
		if !got {
			return r, errors.New("promotion advisory lock busy")
		}
		defer func(k int64) {
			c, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = conn.ExecContext(c, `SELECT pg_catalog.pg_advisory_unlock($1)`, k)
		}(key)
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='250ms'; SET LOCAL statement_timeout='30s'; SET LOCAL synchronous_commit=on`); err != nil {
		return r, err
	}
	if m.Profile == PrivateScaleProfile {
		if _, err = tx.ExecContext(ctx, `SET LOCAL statement_timeout='10min'`); err != nil {
			return r, err
		}
	}
	if err = tx.QueryRowContext(ctx, `SELECT pg_catalog.current_setting('synchronous_commit'),pg_catalog.current_setting('transaction_isolation')`).Scan(&r.SynchronousCommit, &r.Isolation); err != nil {
		return r, err
	}
	if r.SynchronousCommit != "on" || r.Isolation != "repeatable read" {
		return r, errors.New("durability/isolation setting refused")
	}
	// Verify the helper BEFORE invoking a SECURITY DEFINER, then again under locks.
	if err = verifyLocalIdentity(ctx, tx, m); err != nil {
		return r, err
	}
	if h.beforeLocks != nil {
		if err = h.beforeLocks(); err != nil {
			return r, err
		}
	}
	lockStart := time.Now()
	if _, err = tx.ExecContext(ctx, "SELECT "+m.Scope.Schema+".lock_legacy()"); err != nil {
		return r, errors.New("bounded legacy lock acquisition refused")
	}
	r.LockWait = time.Since(lockStart)
	lockedAt := time.Now()
	defer func() { r.LegacyLockHeld = time.Since(lockedAt) }()
	// Calling a SECURITY DEFINER function in RR may establish a snapshot before
	// its LOCK runs. Never assume otherwise. The authoritative baseline is read
	// by a fresh READ ONLY auditor AFTER every lock, then compared to RR. A commit
	// that slipped between those points causes refusal, never stale admission.
	baseline, err := beginAuditBudget(ctx, auditor, m.Profile == PrivateScaleProfile)
	if err != nil {
		return r, err
	}
	defer baseline.Rollback()
	id, err := ProbeIdentity(ctx, baseline, m.Scope, runtime)
	if err != nil {
		return r, err
	}
	if id != m.Destination {
		return r, errors.New("fresh auditor destination identity mismatch")
	}
	if err = CheckScope(ctx, baseline, m.Scope); err != nil {
		return r, err
	}
	if err = verifyLocalIdentity(ctx, tx, m); err != nil {
		return r, err
	}
	if err = erasAndGenesis(ctx, baseline, m); err != nil {
		return r, err
	}
	if err = erasAndGenesis(ctx, tx, m); err != nil {
		return r, err
	}
	if err = emptyHub2(ctx, baseline); err != nil {
		return r, err
	}
	if err = emptyHub2(ctx, tx); err != nil {
		return r, err
	}
	before, err := snapshot(ctx, baseline, true, true)
	if err != nil {
		return r, err
	}
	localBefore, err := snapshot(ctx, tx, true, true)
	if err != nil {
		return r, err
	}
	if !equalSnapshot(before, localBefore) {
		return r, errors.New("RR snapshot predates lock-protected baseline")
	}
	if !equalSnapshot(before, m.Protected) {
		return r, errors.New("retained protected baseline drift")
	}
	if err = baseline.Rollback(); err != nil {
		return r, err
	}
	for n, t := range inserts {
		p := m.Tables[n]
		f, e := openPinned(p.File)
		if e != nil {
			return r, e
		}
		streamHash := sha256.New()
		sc := bufio.NewScanner(io.TeeReader(f, streamHash))
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		var count int64
		batch := make([]json.RawMessage, 0, 64)
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			data, e := json.Marshal(batch)
			if e != nil {
				return e
			}
			res, e := tx.ExecContext(ctx, qualifiedInsert(m.Scope, t), string(data))
			if e != nil {
				return fmt.Errorf("projection insert refused: %s", t.name)
			}
			affected, e := res.RowsAffected()
			if e != nil || affected != int64(len(batch)) {
				return errors.New("insert row count mismatch")
			}
			r.Chunks++
			r.Rows += affected
			batch = batch[:0]
			if h.afterChunk != nil {
				return h.afterChunk(r.Chunks)
			}
			return nil
		}
		for sc.Scan() {
			raw := bytes.Clone(sc.Bytes())
			if e = validateRow(raw, t, m); e != nil {
				f.Close()
				return r, e
			}
			batch = append(batch, raw)
			count++
			if len(batch) == cap(batch) {
				if e = flush(); e != nil {
					f.Close()
					return r, e
				}
			}
		}
		scanErr := sc.Err()
		f.Close()
		if scanErr != nil {
			return r, scanErr
		}
		if e = flush(); e != nil {
			return r, e
		}
		if count != p.Rows || hex.EncodeToString(streamHash.Sum(nil)) != p.File.SHA256 {
			return r, errors.New("projection count/tail/SHA mismatch")
		}
	}
	if err = verifyProjection(ctx, tx, m); err != nil {
		return r, err
	}
	after, err := snapshot(ctx, tx, true, true)
	if err != nil {
		return r, err
	}
	if !equalSnapshot(before, after) {
		return r, errors.New("preexisting full-row/physical preservation failed")
	}
	if err = erasAndGenesis(ctx, tx, m); err != nil {
		return r, err
	}
	if err = verifyLocalIdentity(ctx, tx, m); err != nil {
		return r, err
	}
	if err = m.Recheck(); err != nil {
		return r, err
	}
	if err = freshScopeAudit(ctx, auditor, m, runtime); err != nil {
		return r, err
	}
	if h.beforeCommit != nil {
		if err = h.beforeCommit(); err != nil {
			return r, err
		}
	}
	if h.commit != nil {
		err = h.commit(tx)
	} else {
		err = tx.Commit()
	}
	if err != nil {
		return r, ErrCommitUnknown
	}
	return r, nil
}
func validateRow(raw []byte, t table, m Manifest) error {
	var row map[string]json.RawMessage
	d := json.NewDecoder(bytes.NewReader(raw))
	if e := d.Decode(&row); e != nil {
		return errors.New("invalid projection JSON")
	}
	cols := projectionColumns(t)
	if len(row) != len(cols) {
		return errors.New("missing/extra projection column")
	}
	for _, c := range cols {
		v, ok := row[c]
		if !ok || bytes.Equal(v, []byte("null")) {
			return errors.New("null/missing projection field")
		}
	}
	var chain string
	if json.Unmarshal(row["chain_id"], &chain) != nil || chain != Chain {
		return errors.New("projection is not Hub2")
	}
	if v, ok := row["height"]; ok {
		var height int64
		if json.Unmarshal(v, &height) != nil || height < 0 || height > m.Last {
			return errors.New("projection height outside history")
		}
	}
	if t.name == "taxable_events" {
		var phase int
		var height int64
		var source string
		if json.Unmarshal(row["phase"], &phase) != nil || json.Unmarshal(row["height"], &height) != nil || json.Unmarshal(row["amount_source"], &source) != nil {
			return errors.New("invalid tax policy fields")
		}
		if phase == 4 || source == "export_payout" {
			if phase != 4 || source != "export_payout" || height != m.Canonical {
				return errors.New("intermediate export is not historical income")
			}
		}
	}
	return nil
}
