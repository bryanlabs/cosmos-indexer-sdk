package main

import (
	"context"
	"database/sql"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Admission, the session lock, and all writes share this exact connection.
// A dead session is terminal: neither the guard nor the writer uses the pool.
type rehearsalDestination struct {
	pool   *sql.DB
	conn   *sql.Conn
	failed bool
}

func openRehearsalDestination(ctx context.Context, dsn, target string) (_ *rehearsalDestination, err error) {
	if guardDSN(dsn, target) != nil {
		return nil, refused
	}
	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, refused
	}
	d := &rehearsalDestination{pool: pool}
	defer func() {
		if err != nil {
			d.close()
		}
	}()
	d.conn, err = pool.Conn(ctx)
	if err != nil {
		return nil, refused
	}
	// Conn is the dedicated *sql.Conn, never pool. GORM's own admission
	// queries and automatic ping therefore cannot switch physical sessions.
	guard, err := gorm.Open(postgres.New(postgres.Config{Conn: d.conn}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, refused
	}
	var name string
	if d.conn.QueryRowContext(ctx, "SELECT current_database()").Scan(&name) != nil || name != target {
		return nil, refused
	}
	findings, err := schema.Confined(guard.WithContext(ctx))
	if err != nil || len(findings) != 0 {
		return nil, refused
	}
	var locked bool
	if d.conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(340310)").Scan(&locked) != nil || !locked {
		return nil, refused
	}
	// A rehearsal never resumes a populated destination. These checks happen
	// under the session lock that remains held throughout every ledger write.
	for _, table := range []string{"blocks", "tx_inclusions", "taxable_events", "block_events", "import_ranges", "genesis_files", "genesis_accounts", "genesis_delegations", "genesis_unbondings", "genesis_validators", "genesis_withdraw_addresses", "reconciliation", "balance_days"} {
		var count int64
		if d.conn.QueryRowContext(ctx, "SELECT count(*) FROM legacy."+table).Scan(&count) != nil || count != 0 {
			return nil, refused
		}
	}
	return d, nil
}

func (d *rehearsalDestination) write(ctx context.Context, b ledger.Block) error {
	if d.failed {
		return refused
	}
	if _, err := ledger.Write(ctx, d.conn, []ledger.Block{b}); err != nil {
		d.failed = true
		return refused
	}
	return nil
}

func (d *rehearsalDestination) close() {
	if d.conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = d.conn.ExecContext(ctx, "SELECT pg_advisory_unlock(340310)")
		cancel()
		_ = d.conn.Close()
	}
	if d.pool != nil {
		_ = d.pool.Close()
	}
}
