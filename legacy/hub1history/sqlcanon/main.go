// Command sqlcanon uses only read-only SQL on the existing private fixture.
// It never creates a database, table, role, or other object, or writes a row.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var refused = errors.New("private read-only SQL canonicalization refused; details withheld")

type Pin struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func config(path string) (*pgx.ConnConfig, error) {
	raw, e := os.ReadFile(path)
	if e != nil {
		return nil, refused
	}
	var value string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		if strings.HasPrefix(line, "LEGACY_STORE_TEST_ADMIN_DSN=") {
			if value != "" {
				return nil, refused
			}
			value = strings.TrimSpace(strings.TrimPrefix(line, "LEGACY_STORE_TEST_ADMIN_DSN="))
			if len(value) > 1 && (value[0] == '\'' || value[0] == '"') {
				if value[len(value)-1] != value[0] {
					return nil, refused
				}
				value = value[1 : len(value)-1]
			}
		}
	}
	c, e := pgx.ParseConfig(value)
	if e != nil || value == "" {
		return nil, refused
	}
	ip := net.ParseIP(c.Host)
	if ip == nil || !ip.IsLoopback() || c.Port != 25671 || c.Database != "test" || len(c.Fallbacks) != 0 {
		return nil, refused
	}
	c.ConnectTimeout = 5 * time.Second
	return c, nil
}

func connect(ctx context.Context, path string) (*pgx.Conn, string, error) {
	c, e := config(path)
	if e != nil {
		return nil, "", e
	}
	admin, e := pgx.ConnectConfig(ctx, c)
	if e != nil {
		return nil, "", refused
	}
	// This existing test server is only used to discover a retained fixture.
	tx, e := admin.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if e != nil {
		admin.Close(ctx)
		return nil, "", refused
	}
	var name string
	e = tx.QueryRow(ctx, `SELECT datname FROM pg_catalog.pg_database WHERE datname LIKE 'legacy_replay_rehearsal_%' AND datallowconn ORDER BY datname LIMIT 1`).Scan(&name)
	_ = tx.Rollback(ctx)
	_ = admin.Close(ctx)
	if e != nil || !strings.HasPrefix(name, "legacy_replay_rehearsal_") {
		return nil, "", refused
	}
	c.Database = name
	db, e := pgx.ConnectConfig(ctx, c)
	if e != nil {
		return nil, "", refused
	}
	return db, name, nil
}

func run() error {
	env := flag.String("fixture-env", "", "existing private fixture credential file, never printed")
	source := flag.String("blocks", "", "original complete raw block NDJSON")
	events := flag.String("event-samples", "", "local PG-formatted event samples to verify read-only")
	out := flag.String("output", "", "exclusive new canonical block NDJSON")
	receipt := flag.String("receipt", "", "exclusive private compatibility receipt")
	flag.Parse()
	if *env == "" || *source == "" || *events == "" || *out == "" || *receipt == "" {
		return refused
	}
	a, _ := filepath.Abs(*out)
	b, _ := filepath.Abs(*source)
	if a == b {
		return refused
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	db, name, e := connect(ctx, *env)
	if e != nil {
		return e
	}
	defer db.Close(ctx)
	tx, e := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if e != nil {
		return refused
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SET LOCAL search_path=pg_catalog; SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO, YMD'; SET LOCAL statement_timeout='30s'; SET LOCAL lock_timeout='250ms'`); e != nil {
		return refused
	}
	var readOnly string
	if e = tx.QueryRow(ctx, `SHOW transaction_read_only`).Scan(&readOnly); e != nil || readOnly != "on" {
		return refused
	}
	var blockType, eventType string
	if e = tx.QueryRow(ctx, `SELECT 'legacy.blocks'::pg_catalog.regtype::text,'legacy.block_events'::pg_catalog.regtype::text`).Scan(&blockType, &eventType); e != nil {
		return refused
	}
	sample, e := os.Open(*events)
	if e != nil {
		return refused
	}
	defer sample.Close()
	scan := bufio.NewScanner(sample)
	scan.Buffer(make([]byte, 65536), 128<<20)
	samples := 0
	sampleSum := sha256.New()
	for scan.Scan() {
		_, _ = sampleSum.Write(scan.Bytes())
		_, _ = sampleSum.Write([]byte{'\n'})
		var canonical string
		if e = tx.QueryRow(ctx, `SELECT pg_catalog.to_jsonb(r)::text FROM pg_catalog.jsonb_populate_record(NULL::legacy.block_events,$1::jsonb) r`, scan.Text()).Scan(&canonical); e != nil || canonical != scan.Text() {
			return refused
		}
		samples++
	}
	if scan.Err() != nil || samples < 1 {
		return refused
	}
	input, e := os.Open(*source)
	if e != nil {
		return refused
	}
	defer input.Close()
	output, e := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return refused
	}
	defer output.Close()
	sum := sha256.New()
	writer := bufio.NewWriterSize(io.MultiWriter(output, sum), 1<<20)
	s := bufio.NewScanner(input)
	s.Buffer(make([]byte, 65536), 1<<20)
	var count int64
	inputSum := sha256.New()
	batch := make([]json.RawMessage, 0, 2000)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		data, e := json.Marshal(batch)
		if e != nil {
			return refused
		}
		rows, e := tx.Query(ctx, `SELECT pg_catalog.to_jsonb(r)::text FROM pg_catalog.jsonb_array_elements($1::jsonb) WITH ORDINALITY x(value,n) CROSS JOIN LATERAL pg_catalog.jsonb_populate_record(NULL::legacy.blocks,x.value) r ORDER BY x.n`, string(data))
		if e != nil {
			return refused
		}
		n := 0
		for rows.Next() {
			var raw string
			if e = rows.Scan(&raw); e != nil {
				rows.Close()
				return refused
			}
			if _, e = writer.WriteString(raw + "\n"); e != nil {
				rows.Close()
				return refused
			}
			n++
			count++
		}
		e = rows.Err()
		rows.Close()
		if e != nil || n != len(batch) {
			return refused
		}
		batch = batch[:0]
		return nil
	}
	for s.Scan() {
		_, _ = inputSum.Write(s.Bytes())
		_, _ = inputSum.Write([]byte{'\n'})
		if !json.Valid(s.Bytes()) {
			return refused
		}
		batch = append(batch, bytes.Clone(s.Bytes()))
		if len(batch) == cap(batch) {
			if e = flush(); e != nil {
				return e
			}
		}
	}
	if s.Err() != nil {
		return refused
	}
	if e = flush(); e != nil {
		return e
	}
	if count != 500015 {
		return refused
	}
	if e = writer.Flush(); e != nil {
		return e
	}
	if e = output.Sync(); e != nil {
		return e
	}
	if e = tx.Rollback(ctx); e != nil {
		return refused
	}
	result := map[string]any{"database": name, "port": 25671, "transaction_read_only": true, "rows_canonicalized_by_actual_postgresql": count, "event_samples_exact_postgresql_equal": samples, "ddl": 0, "dml": 0, "production_connections": 0, "block_file": Pin{*out, hex.EncodeToString(sum.Sum(nil))}, "input_blocks_sha256": hex.EncodeToString(inputSum.Sum(nil)), "event_samples_sha256": hex.EncodeToString(sampleSum.Sum(nil))}
	f, e := os.OpenFile(*receipt, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return refused
	}
	defer f.Close()
	if e = json.NewEncoder(f).Encode(result); e != nil {
		return e
	}
	fmt.Printf("private read-only PostgreSQL blocks=%d event_samples=%d DDL=0 DML=0\n", count, samples)
	return nil
}
func main() {
	if run() != nil {
		fmt.Fprintln(os.Stderr, refused)
		os.Exit(2)
	}
}
