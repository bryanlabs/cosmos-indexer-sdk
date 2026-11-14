package hub2promotion

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ExportProjection consumes an already accepted private SQL staging database.
// It never opens the original native source or calls a decoder/financial tool.
// Its source transaction is READ ONLY and its directory must be NEW/retained.
func ExportProjection(ctx context.Context, source *sql.DB, expected Identity, dir string) ([]Projection, error) {
	if !strings.HasPrefix(expected.Database, "legacy_replay_rehearsal_") || !filepath.IsAbs(dir) {
		return nil, errors.New("private staging identity and absolute NEW directory required")
	}
	tx, e := beginAudit(ctx, source)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO, YMD'`); e != nil {
		return nil, e
	}
	var name, system string
	var oid uint32
	if e = tx.QueryRowContext(ctx, `SELECT current_database(),oid FROM pg_catalog.pg_database WHERE datname=current_database()`).Scan(&name, &oid); e != nil {
		return nil, e
	}
	if e = tx.QueryRowContext(ctx, `SELECT system_identifier::pg_catalog.text FROM pg_catalog.pg_control_system()`).Scan(&system); e != nil {
		return nil, e
	}
	sum := sha256.Sum256([]byte(system))
	if name != expected.Database || oid != expected.OID || hex.EncodeToString(sum[:]) != expected.SystemHash {
		return nil, errors.New("source staging identity mismatch")
	}
	if e = os.Mkdir(dir, 0700); e != nil {
		return nil, e
	}
	var out []Projection
	for _, t := range inserts {
		rows, e := tx.QueryContext(ctx, "SELECT pg_catalog.to_jsonb(r)::pg_catalog.text FROM legacy."+t.name+" r WHERE chain_id='cosmoshub-2' ORDER BY "+quotedColumns(t.key))
		if e != nil {
			return nil, e
		}
		path := filepath.Join(dir, t.name+".ndjson")
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			rows.Close()
			return nil, e
		}
		h := sha256.New()
		w := bufio.NewWriterSize(io.MultiWriter(f, h), 64<<10)
		var n int64
		for rows.Next() {
			var raw string
			if e = rows.Scan(&raw); e != nil {
				f.Close()
				rows.Close()
				return nil, e
			}
			if _, e = io.WriteString(w, raw+"\n"); e != nil {
				f.Close()
				rows.Close()
				return nil, e
			}
			n++
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			f.Close()
			return nil, e
		}
		if e = w.Flush(); e != nil {
			f.Close()
			return nil, e
		}
		if e = f.Sync(); e != nil {
			f.Close()
			return nil, e
		}
		if e = f.Close(); e != nil {
			return nil, e
		}
		out = append(out, Projection{t.name, projectionColumns(t), n, Pin{path, hex.EncodeToString(h.Sum(nil))}})
	}
	return out, nil
}
