package ledger

import (
	"context"
	"database/sql"
	"fmt"
)

// Range is one importer's own run of heights on one chain. Every importer
// names its range explicitly and no two ranges of a chain may overlap.
type Range struct {
	Chain    string
	From, To int64
}

// Progress is what legacy.import_ranges records for a range. The counts
// describe the heights from From up to, not including, Next: every one of
// those is stored. Held counts heights of the last run that were not stored.
type Progress struct {
	Next                             int64
	Stored, Held, Transactions, Rows int64
	Status                           string
}

// Claim holds a range for one importer until Release. The lock is tied to
// its own database connection, so it ends if the importer dies.
type Claim struct {
	conn *sql.Conn
	r    Range
}

// Begin claims the range and returns where to resume. It refuses a range
// that overlaps another recorded range of the chain, a range recorded with
// a different end, and a range another importer is working on right now.
func Begin(ctx context.Context, db *sql.DB, r Range, build string) (*Claim, Progress, error) {
	if r.From <= 0 || r.To < r.From {
		return nil, Progress{}, fmt.Errorf("a legacy range needs a first height and a last height at or after it")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, Progress{}, err
	}
	fail := func(err error) (*Claim, Progress, error) {
		_ = conn.Close()
		return nil, Progress{}, err
	}
	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`,
		fmt.Sprintf("legacy import %s %d", r.Chain, r.From)).Scan(&locked); err != nil {
		return fail(err)
	}
	if !locked {
		return fail(fmt.Errorf("another importer is working on this range"))
	}
	var overlapping int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM legacy.import_ranges
WHERE chain_id = $1 AND from_height <> $2 AND from_height <= $3 AND to_height >= $2`, r.Chain, r.From, r.To).Scan(&overlapping); err != nil {
		return fail(err)
	}
	if overlapping != 0 {
		return fail(fmt.Errorf("the range overlaps another recorded range of this chain"))
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO legacy.import_ranges (chain_id, from_height, to_height, status, next_height, build)
VALUES ($1, $2, $3, 'running', $2, $4) ON CONFLICT (chain_id, from_height) DO NOTHING`, r.Chain, r.From, r.To, build); err != nil {
		return fail(err)
	}
	var to int64
	var p Progress
	if err := conn.QueryRowContext(ctx, `SELECT to_height, next_height, heights_stored, transactions, tax_rows FROM legacy.import_ranges
WHERE chain_id = $1 AND from_height = $2`, r.Chain, r.From).Scan(&to, &p.Next, &p.Stored, &p.Transactions, &p.Rows); err != nil {
		return fail(err)
	}
	if to != r.To {
		return fail(fmt.Errorf("a range that starts at this height is recorded with a different last height"))
	}
	if p.Next < r.From || p.Next > r.To+1 {
		return fail(fmt.Errorf("the recorded resume height is outside the range"))
	}
	p.Status = "running"
	if _, err := conn.ExecContext(ctx, `UPDATE legacy.import_ranges SET status = 'running', heights_held = 0, build = $3, updated_at = now()
WHERE chain_id = $1 AND from_height = $2`, r.Chain, r.From, build); err != nil {
		return fail(err)
	}
	return &Claim{conn: conn, r: r}, p, nil
}

// Record stores the progress of the range.
func (c *Claim) Record(ctx context.Context, p Progress) error {
	if p.Status != "running" && p.Status != "done" && p.Status != "failed" {
		return fmt.Errorf("legacy range status not recognized")
	}
	if p.Status == "done" && (p.Next != c.r.To+1 || p.Held != 0) {
		return fmt.Errorf("a legacy range is done only when every height in it is stored")
	}
	_, err := c.conn.ExecContext(ctx, `UPDATE legacy.import_ranges SET status = $3, next_height = $4, heights_stored = $5, heights_held = $6,
transactions = $7, tax_rows = $8, updated_at = now() WHERE chain_id = $1 AND from_height = $2`,
		c.r.Chain, c.r.From, p.Status, p.Next, p.Stored, p.Held, p.Transactions, p.Rows)
	return err
}

// Release gives the range up.
func (c *Claim) Release() {
	_ = c.conn.Close()
}
