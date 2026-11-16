package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/legacy/genesis"
)

// A part entry is one part's share of the staging receipt, written when the
// part is stored: the part is read again from its sealed files, required to
// equal the stored rows, and those rows are hashed. The final receipt is then
// put together from the entries plus one pass over the database, and refuses
// if any part's rows no longer hash to its entry. An entry holds no clock
// time or database name: it says that rows with these hashes are what this
// sealed source gives, which is true of any database holding those rows.

const entrySchema = "hub2-staging-part-entry-v1"

type partEntry struct {
	Schema       string            `json:"schema"`
	Chain        string            `json:"chain"`
	SourceCommit string            `json:"decoder_source_commit"`
	RowFormat    string            `json:"row_format"`
	Genesis      *receiptGenesis   `json:"genesis,omitempty"`
	Component    *receiptComponent `json:"component,omitempty"`
	Payout       *receiptPayout    `json:"restart_payout,omitempty"`
	Tail         *receiptTail      `json:"historical_tail,omitempty"`
}

func entryPath(dir, part string) string { return filepath.Join(dir, part+".entry.json") }

func componentPart(last int64) string { return fmt.Sprintf("component-%d", last) }

func readEntry(dir, part string, m Manifest) (partEntry, error) {
	var x partEntry
	raw, e := os.ReadFile(entryPath(dir, part))
	if e != nil || json.Unmarshal(raw, &x) != nil || x.Schema != entrySchema || x.Chain != chainID || x.SourceCommit != m.SourceCommit || x.RowFormat != rowFormat {
		return x, why("part entry %s is missing, unreadable or from another decoder", part)
	}
	return x, nil
}

func writeEntry(dir, part string, m Manifest, x partEntry) error {
	x.Schema, x.Chain, x.SourceCommit, x.RowFormat = entrySchema, chainID, m.SourceCommit, rowFormat
	raw, e := json.MarshalIndent(x, "", " ")
	if e != nil {
		return refused
	}
	f, e := os.OpenFile(entryPath(dir, part), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return why("part entry %s exists; an entry is never overwritten", part)
	}
	if _, e = f.Write(append(raw, '\n')); e != nil || f.Sync() != nil || f.Close() != nil {
		return refused
	}
	fmt.Printf("part entry written part=%s sha256=%s\n", part, fileSHA(entryPath(dir, part)))
	return nil
}

// hashRows hashes the rows one query returns, in the receipt's row format.
func hashRows(ctx context.Context, tx *sql.Tx, table, where, order string, args ...any) (tableHash, error) {
	rows, e := tx.QueryContext(ctx, "SELECT (pg_catalog.to_jsonb(r)-'loaded_at')::pg_catalog.text FROM legacy."+table+" r "+where+" ORDER BY "+order, args...)
	if e != nil {
		return tableHash{}, refused
	}
	defer rows.Close()
	b := newBucket()
	for rows.Next() {
		var line string
		if rows.Scan(&line) != nil {
			return tableHash{}, refused
		}
		b.add(line)
	}
	if rows.Err() != nil {
		return tableHash{}, refused
	}
	return b.sum(), nil
}

func keyOf(table string) string {
	for _, k := range fingerprintKeys {
		if k[0] == table {
			return k[1]
		}
	}
	return ""
}

// hashHeights hashes the stored rows of one height range, table by table,
// exactly as the whole-database pass files them under a part.
func hashHeights(ctx context.Context, tx *sql.Tx, first, last int64, tables []string) (map[string]tableHash, error) {
	out := map[string]tableHash{}
	for _, t := range tables {
		where := "WHERE chain_id=$1 AND height BETWEEN $2 AND $3"
		if t == "taxable_events" {
			where += " AND phase<>4"
		}
		h, e := hashRows(ctx, tx, t, where, keyOf(t), chainID, first, last)
		if e != nil {
			return nil, e
		}
		out[t] = h
	}
	return out, nil
}

func readSnapshot(ctx context.Context, conn *sql.Conn) (*sql.Tx, error) {
	tx, e := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if e != nil {
		return nil, refused
	}
	if _, e = tx.ExecContext(ctx, `SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO, YMD'`); e != nil {
		tx.Rollback()
		return nil, refused
	}
	return tx, nil
}

// writeEntries writes the entry of every listed part that has none yet. Each
// new entry costs one read of that part's sealed files and one indexed read
// of its stored rows. The session is read-only.
func writeEntries(m Manifest, dir string) error {
	if !filepath.IsAbs(dir) {
		return refused
	}
	if st, e := os.Stat(dir); e != nil || !st.IsDir() {
		return why("entries directory missing")
	}
	header, export, e := receiptHeader(m, true)
	if e != nil {
		return e
	}
	ctx := context.Background()
	d, opening, e := openReadOnly(ctx, m)
	if e != nil {
		return e
	}
	defer d.close()
	exists := func(part string) bool { _, e := os.Lstat(entryPath(dir, part)); return e == nil }
	if !exists("genesis") {
		if e = genesisEntry(ctx, d.conn, m, dir, opening); e != nil {
			return e
		}
	}
	started := time.Now()
	for i, c := range m.Components {
		if exists(componentPart(c.Last)) {
			continue
		}
		x := partEntry{}
		rc := header.Components[i]
		if c.Last == canonicalHeight {
			if _, e = importComponent(ctx, d.conn, m, c, opening.SHA256, export, false, true, started); e != nil {
				return why("component %d: stored rows are not the rows its sealed source gives", c.Last)
			}
		} else if _, e = importComponent(ctx, d.conn, m, c, opening.SHA256, nil, false, true, started); e != nil {
			return why("component %d: stored rows are not the rows its sealed source gives", c.Last)
		}
		tx, e := readSnapshot(ctx, d.conn)
		if e != nil {
			return e
		}
		rc.Tables, e = hashHeights(ctx, tx, c.First, c.Last, []string{"blocks", "tx_inclusions", "block_events", "taxable_events"})
		if e == nil && c.Last == canonicalHeight && header.Payout != nil {
			p := *header.Payout
			var notZero, elsewhere int64
			p.Rows, e = hashRows(ctx, tx, "taxable_events", "WHERE chain_id=$1 AND phase=4", keyOf("taxable_events"), chainID)
			if e == nil && tx.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE tx_index<>0 OR msg_index<>0),count(*) FILTER (WHERE height<>$2) FROM legacy.taxable_events WHERE chain_id=$1 AND phase=4`, chainID, canonicalHeight).Scan(&notZero, &elsewhere) != nil {
				e = refused
			}
			if e == nil && (notZero != 0 || elsewhere != 0 || p.Rows.Rows != m.CanonicalExport.Rows) {
				e = why("restart payout rows are not all tx 0 message 0 at the migration height, or their count differs from the export")
			}
			p.IndexZero = true
			x.Payout = &p
		}
		tx.Rollback()
		if e != nil {
			return e
		}
		x.Component = &rc
		if pins(m) != nil {
			return why("a pinned file changed while it was read")
		}
		if e = writeEntry(dir, componentPart(c.Last), m, x); e != nil {
			return e
		}
	}
	if m.Tail != nil && !exists("tail") {
		if _, e = importTail(ctx, d.conn, m, true); e != nil {
			return e
		}
		tx, e := readSnapshot(ctx, d.conn)
		if e != nil {
			return e
		}
		t := *header.Tail
		t.Tables, e = hashHeights(ctx, tx, m.Tail.First, m.Tail.Last, []string{"blocks", "tx_inclusions", "block_events"})
		var n int64
		if e == nil && (tx.QueryRowContext(ctx, `SELECT count(*) FROM legacy.taxable_events WHERE chain_id=$1 AND height>$2`, chainID, canonicalHeight).Scan(&n) != nil || n != 0) {
			e = why("tax row in the tail")
		}
		tx.Rollback()
		if e != nil {
			return e
		}
		if pins(m) != nil {
			return why("a pinned file changed while it was read")
		}
		if e = writeEntry(dir, "tail", m, partEntry{Tail: &t}); e != nil {
			return e
		}
	}
	return nil
}

func genesisEntry(ctx context.Context, conn *sql.Conn, m Manifest, dir string, opening *genesis.File) error {
	gormDB, e := guarded(conn)
	if e != nil {
		return refused
	}
	gr, e := genesis.Load(gormDB, opening)
	if e != nil || !gr.AlreadyLoaded || !reflect.DeepEqual(gr.File, gr.Stored) {
		return why("stored genesis rows differ from the genesis file")
	}
	tx, e := readSnapshot(ctx, conn)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	g := receiptGenesis{SHA256: opening.SHA256, StoredEqual: true, Tables: map[string]tableHash{}}
	for _, k := range fingerprintKeys {
		if strings.HasPrefix(k[0], "genesis_") {
			if g.Tables[k[0]], e = hashRows(ctx, tx, k[0], "", k[1]); e != nil {
				return e
			}
		}
	}
	return writeEntry(dir, "genesis", m, partEntry{Genesis: &g})
}

// assembleReceipt builds the receipt from one pass over the database and the
// stored part entries. Every part's rows must hash to its entry, and every
// entry must name the seal and source digests the manifest pins today.
func assembleReceipt(m Manifest, dir string) (StagingReceipt, error) {
	r, e := buildReceipt(m, false)
	if e != nil {
		return r, e
	}
	g, e := readEntry(dir, "genesis", m)
	if e != nil {
		return r, e
	}
	r.Genesis.StoredEqual = true
	if g.Genesis == nil || !reflect.DeepEqual(*g.Genesis, r.Genesis) {
		return r, why("genesis rows differ from their entry")
	}
	for i := range r.Components {
		x, e := readEntry(dir, componentPart(r.Components[i].Last), m)
		if e != nil {
			return r, e
		}
		r.Components[i].Rederived = true
		if x.Component == nil || !reflect.DeepEqual(*x.Component, r.Components[i]) {
			return r, why("rows or pins of component %d differ from its entry", r.Components[i].Last)
		}
		if r.Components[i].Last == canonicalHeight && r.Payout != nil {
			r.Payout.Rederived = true
			if x.Payout == nil || *x.Payout != *r.Payout {
				return r, why("restart payout rows differ from their entry")
			}
		}
	}
	if r.Payout != nil && !r.Payout.Rederived {
		return r, why("restart payout has no entry")
	}
	if r.Tail != nil {
		x, e := readEntry(dir, "tail", m)
		if e != nil {
			return r, e
		}
		r.Tail.Rederived = true
		if x.Tail == nil || !reflect.DeepEqual(*x.Tail, *r.Tail) {
			return r, why("tail rows differ from their entry")
		}
	}
	return r, nil
}
