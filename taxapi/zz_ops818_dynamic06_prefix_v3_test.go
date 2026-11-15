//go:build ops818dynamic06prefixv3 && ops818harness && ops818cohorts0607

package taxapi

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type p6Scope struct {
	Source, Address, Key, UniquenessSHA string
	Since, End, PrefixEnd               time.Time
	Queries                             []string
}
type p6Log struct {
	logger.Interface
	Queries []map[string]any
}

func (l *p6Log) Trace(_ context.Context, b time.Time, fc func() (string, int64), err error) {
	q, n := fc()
	e := ""
	if err != nil {
		e = err.Error()
	}
	l.Queries = append(l.Queries, map[string]any{"sql": q, "rows": n, "elapsed_ms": time.Since(b).Milliseconds(), "error": e})
}
func p6Validate(s p6Scope) error {
	if s.Source != xcSource || s.Address == "" || (!s.Since.Before(s.PrefixEnd) || !s.PrefixEnd.Before(s.End)) || s.Key != xcID(s.Address, s.Since, s.End) || len(s.Queries) != 2 {
		return errors.New("scope rejected")
	}
	return nil
}
func p6Typed(v any, typ string) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano), nil
	case []byte:
		switch typ {
		case "INT2", "INT4", "INT8", "NUMERIC", "FLOAT4", "FLOAT8":
			if !json.Valid(x) {
				return nil, errors.New("numeric encoding")
			}
			return json.Number(string(x)), nil
		case "TEXT", "VARCHAR", "BPCHAR", "NAME":
			return string(x), nil
		}
	case string, int64, float64, bool:
		return v, nil
	}
	return nil, errors.New("unhandled SQL type")
}
func p6Stream(w io.Writer, r *sql.Rows) (int, []map[string]string, error) {
	defer r.Close()
	names, e := r.Columns()
	if e != nil {
		return 0, nil, e
	}
	types, e := r.ColumnTypes()
	if e != nil {
		return 0, nil, e
	}
	schema := []map[string]string{}
	seen := map[string]bool{}
	for i, n := range names {
		if seen[n] {
			return 0, nil, errors.New("duplicate column")
		}
		seen[n] = true
		schema = append(schema, map[string]string{"name": n, "database_type": types[i].DatabaseTypeName()})
	}
	enc := json.NewEncoder(w)
	n := 0
	for r.Next() {
		v := make([]any, len(names))
		p := make([]any, len(names))
		for i := range v {
			p[i] = &v[i]
		}
		if e = r.Scan(p...); e != nil {
			return n, schema, e
		}
		row := map[string]any{}
		for i, k := range names {
			row[k], e = p6Typed(v[i], types[i].DatabaseTypeName())
			if e != nil {
				return n, schema, e
			}
		}
		if n > 0 {
			if _, e = io.WriteString(w, ","); e != nil {
				return n, schema, e
			}
		}
		if e = enc.Encode(row); e != nil {
			return n, schema, e
		}
		n++
	}
	return n, schema, r.Err()
}
func TestOPS818Dynamic06PrefixV3SourceChecks(t *testing.T) {
	s := p6Scope{Source: xcSource, Address: "synthetic", Since: time.Unix(1, 0), PrefixEnd: time.Unix(2, 0), End: time.Unix(3, 0), Queries: []string{"a", "b"}}
	s.Key = xcID(s.Address, s.Since, s.End)
	if p6Validate(s) != nil {
		t.Fatal("valid scope")
	}
	s.Key = "wrong"
	if p6Validate(s) == nil {
		t.Fatal("wrong key accepted")
	}
	if x, e := p6Typed([]byte("001literal"), "TEXT"); e != nil || x != "001literal" {
		t.Fatal("literal changed")
	}
	if _, e := p6Typed([]byte("bad"), "NUMERIC"); e == nil {
		t.Fatal("bad numeric accepted")
	}
	if _, e := p6Typed([]byte("x"), "UNKNOWN"); e == nil {
		t.Fatal("unknown type accepted")
	}
}
func TestOPS818Dynamic06PrefixV3Source(t *testing.T) {
	root := os.Getenv("OPS818_DYNAMIC06_PREFIX_V3_ROOT")
	if root == "" {
		t.Skip("private source only")
	}
	var s p6Scope
	if xcRead(filepath.Join(root, "scope.private.json"), &s) != nil || p6Validate(s) != nil {
		t.Fatal("scope rejected")
	}
	h, e := xcHash(filepath.Join(root, "uniqueness.private.json"))
	if e != nil || h != s.UniquenessSHA {
		t.Fatal("uniqueness binding")
	}
	var u struct { Source string `json:"source"`; Request struct { Source string `json:"source"`; Request struct { Address, Since string; End string `json:"asof_exclusive"`; Key string `json:"canonical_window"` } `json:"request"` } `json:"request"` }
 if xcRead(filepath.Join(root,"uniqueness.private.json"), &u)!=nil || u.Source!=xcSource || u.Request.Source!=xcSource || u.Request.Request.Address!=s.Address || u.Request.Request.Key!=s.Key { t.Fatal("actual request binding") }
 rs,es:=time.Parse(time.RFC3339Nano,u.Request.Request.Since); re,ee:=time.Parse(time.RFC3339Nano,u.Request.Request.End)
 if es!=nil || ee!=nil || !rs.Equal(s.Since) || !re.Equal(s.End) { t.Fatal("request time binding") }
	head, e := exec.Command("git", "rev-parse", "HEAD").Output()
	if e != nil || strings.TrimSpace(string(head)) != xcSource || exec.Command("git", "diff", "--quiet", xcSource, "--").Run() != nil {
		t.Fatal("tracked tree differs from source")
	}
	rec := &p6Log{Interface: logger.Default.LogMode(logger.Silent)}
	db, e := gorm.Open(postgres.Open("host=127.0.0.1 port=15439 user=postgres dbname=cosmostax sslmode=disable application_name=ops818-dynamic06_prefix_v3-source-v1 default_transaction_read_only=on"), &gorm.Config{Logger: rec})
	if e != nil {
		t.Fatal("source connection failed")
	}
	pool, e := db.DB()
	if e != nil {
		t.Fatal("source pool")
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	server := &Server{db: db, native: DefaultNativeAsset, readOnlyLedger: true, privateReadSnapshotTimeout: 180 * time.Second}
	var input reportInput
	controls := []map[string]any{}
	counts := []int{}
	schemas := [][]map[string]string{}
	began := time.Now()
	phase := "begin"
	rawPath := filepath.Join(root, "native.raw.json.gz")
	f, e := os.OpenFile(rawPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		t.Fatal("raw reserve")
	}
	ch := sha256.New()
	rh := sha256.New()
	z := gzip.NewWriter(io.MultiWriter(f, ch))
	out := io.MultiWriter(z, rh)
	fmt.Println("phase dynamic06_prefix_v3_source_started count 1")
	e = server.withReadSnapshot(func(reader *Server) error {
		if e := reader.db.Exec("SET LOCAL statement_timeout='180s'").Error; e != nil {
			return e
		}
		if e := reader.db.Exec("SET LOCAL lock_timeout='2s'").Error; e != nil {
			return e
		}
		if e := reader.db.Exec("SET LOCAL jit=off").Error; e != nil {
			return e
		}
		control := func() error {
			var c struct {
				Readonly           string
				AssignedXID        *int64
				Snapshot, Database string
			}
			if e := reader.db.Raw("SELECT current_setting('transaction_read_only') AS readonly,txid_current_if_assigned() AS assigned_x_id,txid_current_snapshot()::text AS snapshot,current_database() AS database").Scan(&c).Error; e != nil {
				return e
			}
			if c.Readonly != "on" || c.AssignedXID != nil || c.Database != "cosmostax" {
				return errors.New("source controls")
			}
			controls = append(controls, map[string]any{"readonly": c.Readonly, "assigned_xid": c.AssignedXID, "snapshot": c.Snapshot, "database": c.Database})
			return nil
		}
		if e := control(); e != nil {
			return e
		}
		phase = "full_readiness"
		refunds, e := reader.reportReadiness(apiMainnet, []string{s.Address}, s.Since, s.End)
		if e != nil {
			return e
		}
		phase = "full_typed_input"
		rows, e := reader.loadWalletRows(apiMainnet, s.Address, s.Since, s.PrefixEnd, refunds)
		if e != nil {
			return e
		}
		input = reportInput{Wallets: []walletRows{rows}, Fingerprint: ""}
		if e = xcSave(filepath.Join(root, "full.input.private.json"), input); e != nil {
			return e
		}
		fmt.Printf("phase dynamic06_prefix_v3_input_captured count %d\n", len(rows.Events))
		for i, q := range s.Queries {
			phase = fmt.Sprintf("bank_namespace_%d", i)
			key := []string{"message_attributes", "block_attributes"}[i]
			prefix := ","
			if i == 0 {
				prefix = "{"
			}
			if _, e = fmt.Fprintf(out, "%s%q:[", prefix, key); e != nil {
				return e
			}
			r, e := reader.db.Raw(q, s.Address, s.Address, s.Since, s.PrefixEnd).Rows()
			if e != nil {
				return e
			}
			n, schema, e := p6Stream(out, r)
			counts = append(counts, n)
			schemas = append(schemas, schema)
			if e != nil {
				return e
			}
			if _, e = io.WriteString(out, "]"); e != nil {
				return e
			}
			fmt.Printf("phase dynamic06_prefix_v3_bank_namespace_complete count %d\n", n)
		}
		if e = control(); e != nil {
			return e
		}
		_, e = io.WriteString(out, `,"readonly":"on","assigned_xid":null}`)
		return e
	})
	ze := z.Close()
	fe := f.Sync()
	ce := f.Close()
	if e == nil {
		e = ze
	}
	if e == nil {
		e = fe
	}
	if e == nil {
		e = ce
	}
	proof := map[string]any{"source": xcSource, "window": s.Key, "address": s.Address, "since": s.Since, "asof_exclusive": s.PrefixEnd, "report_readiness_asof_exclusive": s.End, "native_complete": e == nil, "full_input_and_bank_same_snapshot": true, "snapshot_controls": controls, "queries": rec.Queries, "bank_queries": s.Queries, "bank_namespace_counts": counts, "bank_typed_schemas": schemas, "raw_snapshot_sha256": fmt.Sprintf("%x", rh.Sum(nil)), "compressed_sha256": fmt.Sprintf("%x", ch.Sum(nil)), "raw_bank_source_kind": "fresh_indexed_ABCI_bank_attributes_separate_from_tax_semantics", "native_snapshot_seconds": 180, "current_full_input_source": xcSource, "current_input": filepath.Join(root, "full.input.private.json"), "phase": phase, "elapsed_ms": time.Since(began).Milliseconds(), "financially_accepted": false, "closing_reconciled": false, "fingerprint_contract": false, "pagination": "single streamed cursor per namespace; EOF and rows.Err required", "boundaries": "exact UTC [Since, End) SQL predicates; no height/time truncation"}
	ih, ie := xcHash(filepath.Join(root, "full.input.private.json"))
	if ie == nil {
		proof["current_input_sha256"] = ih
	}
	if len(controls) > 0 {
		proof["readonly"] = controls[0]["readonly"]
		proof["assigned_xid"] = controls[0]["assigned_xid"]
	}
	if e != nil {
		proof["error"] = e.Error()
	}
	if xcSave(filepath.Join(root, "source.receipt.private.json"), proof) != nil {
		t.Fatal("receipt save")
	}
	if e != nil {
		t.Fatal("source failed explicitly")
	}
	fmt.Println("phase dynamic06_prefix_v3_source_complete count 1")
}
func TestOPS818Dynamic06PrefixV3Convert(t *testing.T) {
	root := os.Getenv("OPS818_DYNAMIC06_PREFIX_V3_ROOT")
	if root == "" {
		t.Skip("private conversion")
	}
	var s p6Scope
	var in reportInput
	var c struct {
		Verified bool `json:"verified_against_actual_bank_attributes"`
		Credits  struct {
			Records []struct {
				Message uint   `json:"message"`
				Coin    string `json:"coin"`
			} `json:"credits"`
		} `json:"credits"`
	}
	if xcRead(filepath.Join(root, "scope.private.json"), &s) != nil || p6Validate(s) != nil || xcRead(filepath.Join(root, "full.input.private.json"), &in) != nil || xcRead(filepath.Join(root, "credits.verified.private.json"), &c) != nil || !c.Verified {
		t.Fatal("conversion binding")
	}
	credits := map[flowCredit]bool{}
	for _, r := range c.Credits.Records {
		credits[flowCredit{r.Message, r.Coin}] = true
	}
	events, e := xcConvert(in, s.Address, s.Since, s.End, credits)
	if e != nil {
		t.Fatal("actual 6754 converter")
	}
	if xcSave(filepath.Join(root, "native.events.private.json"), events) != nil {
		t.Fatal("events save")
	}
	fmt.Printf("phase dynamic06_prefix_v3_events_converted count %d\n", len(events))
}
