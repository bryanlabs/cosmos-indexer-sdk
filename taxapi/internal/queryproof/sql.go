package queryproof

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

type Value struct {
	Kind string
	Text string
}

func value(v any) (Value, error) {
	switch x := v.(type) {
	case nil:
		return Value{Kind: "null"}, nil
	case int64:
		return Value{Kind: "int", Text: fmt.Sprint(x)}, nil
	case float64:
		return Value{Kind: "float", Text: fmt.Sprint(x)}, nil
	case bool:
		return Value{Kind: "bool", Text: fmt.Sprint(x)}, nil
	case string:
		return Value{Kind: "string", Text: x}, nil
	case []byte:
		return Value{Kind: "bytes", Text: base64.StdEncoding.EncodeToString(x)}, nil
	case time.Time:
		return Value{Kind: "time", Text: x.UTC().Format(time.RFC3339Nano)}, nil
	default:
		return Value{}, errors.New("unsupported SQL proof value type")
	}
}
func (v Value) decode() (driver.Value, error) {
	switch v.Kind {
	case "null":
		return nil, nil
	case "string":
		return v.Text, nil
	case "bytes":
		return base64.StdEncoding.DecodeString(v.Text)
	case "time":
		return time.Parse(time.RFC3339Nano, v.Text)
	case "int":
		return strconv.ParseInt(v.Text, 10, 64)
	case "float":
		return strconv.ParseFloat(v.Text, 64)
	case "bool":
		return strconv.ParseBool(v.Text)
	default:
		return nil, errors.New("invalid SQL proof value")
	}
}

type SQLRequest struct {
	Query string
	Args  []Value
}
type SQLResponse struct {
	Columns       []string
	DatabaseTypes []string
	Rows          [][]Value
	Error         string
	RowsAffected  int64
}

func request(q string, args []driver.NamedValue) (SQLRequest, error) {
	r := SQLRequest{Query: q}
	for _, a := range args {
		v, e := value(a.Value)
		if e != nil {
			return r, e
		}
		r.Args = append(r.Args, v)
	}
	return r, nil
}

var driverSerial atomic.Uint64

// OpenSQL never saves the DSN. A replay deliberately receives no real driver
// and cannot dial a database, even for an unknown query or reconnect.
func OpenSQL(t *Tape, dsn string) (*sql.DB, error) {
	name := fmt.Sprintf("query-proof-%d", driverSerial.Add(1))
	sql.Register(name, &proofDriver{t: t, dsn: dsn})
	db, err := sql.Open(name, "")
	if err == nil {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	}
	return db, err
}

type proofDriver struct {
	t   *Tape
	dsn string
}
type proofConn struct {
	t         *Tape
	real      driver.Conn
	virtualTx bool
}

func (d *proofDriver) Open(_ string) (driver.Conn, error) {
	c := &proofConn{t: d.t}
	if d.t.replay {
		return c, nil
	}
	raw, err := stdlib.GetDefaultDriver().Open(d.dsn)
	if err != nil {
		return nil, d.t.Refuse(errors.New("source connection failed"))
	}
	c.real = raw
	fail := func() (driver.Conn, error) {
		_ = raw.Close()
		return nil, d.t.Refuse(errors.New("source snapshot import or guard failed"))
	}
	exec, ok := raw.(driver.ExecerContext)
	if !ok {
		return fail()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err = exec.ExecContext(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY", nil); err != nil {
		return fail()
	}
	token := d.t.Source().ExportedSnapshot
	if !regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{8}-[0-9]+$`).MatchString(token) {
		return fail()
	}
	if _, err = exec.ExecContext(ctx, "SET TRANSACTION SNAPSHOT '"+token+"'", nil); err != nil {
		return fail()
	}
	query, ok := raw.(driver.QueryerContext)
	if !ok {
		return fail()
	}
	rows, err := query.QueryContext(ctx, "SELECT current_setting('transaction_read_only'), current_setting('transaction_isolation'), pg_current_snapshot()::text, ("+identitySQL+")", nil)
	if err != nil {
		return fail()
	}
	vals := make([]driver.Value, 4)
	err = rows.Next(vals)
	closeErr := rows.Close()
	if err != nil || closeErr != nil || fmt.Sprint(vals[0]) != "on" || fmt.Sprint(vals[1]) != "repeatable read" || fmt.Sprint(vals[2]) != d.t.Source().PGSnapshot || Digest([]byte(fmt.Sprint(vals[3]))) != d.t.Source().DatabaseIdentitySHA256 {
		return fail()
	}
	return c, nil
}
func (c *proofConn) Prepare(q string) (driver.Stmt, error) { return &proofStmt{c: c, q: q}, nil }
func (c *proofConn) Close() error {
	if c.real != nil {
		_, _ = c.real.(driver.ExecerContext).ExecContext(context.Background(), "ROLLBACK", nil)
		return c.real.Close()
	}
	return nil
}
func (c *proofConn) Begin() (driver.Tx, error) {
	return nil, c.t.Refuse(errors.New("proof requires explicit read-only repeatable-read transaction"))
}
func (c *proofConn) BeginTx(_ context.Context, o driver.TxOptions) (driver.Tx, error) {
	if !o.ReadOnly || o.Isolation != driver.IsolationLevel(sql.LevelRepeatableRead) || c.virtualTx {
		return nil, c.t.Refuse(errors.New("invalid proof transaction semantics"))
	}
	c.virtualTx = true
	return &proofTx{c: c}, nil
}

type proofTx struct{ c *proofConn }

func (tx *proofTx) Commit() error   { tx.c.virtualTx = false; return nil }
func (tx *proofTx) Rollback() error { tx.c.virtualTx = false; return nil }
func (c *proofConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	statement := strings.TrimSuffix(strings.TrimSpace(q), ";")
	fields := strings.Fields(strings.ToUpper(statement))
	if strings.Contains(statement, ";") || len(fields) == 0 || (fields[0] != "SELECT" && fields[0] != "WITH") {
		return nil, c.t.Refuse(errors.New("proof refused non-read query"))
	}
	req, err := request(q, args)
	if err != nil {
		return nil, c.t.Refuse(err)
	}
	var response SQLResponse
	if c.t.replay {
		b, e := c.t.Take("sql-query", encoded(req))
		if e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &response); e != nil {
			return nil, c.t.Refuse(e)
		}
	} else {
		rows, e := c.real.(driver.QueryerContext).QueryContext(ctx, q, args)
		if e != nil {
			response.Error = e.Error()
		} else {
			response.Columns = rows.Columns()
			for i := range response.Columns {
				typ := ""
				if col, ok := rows.(driver.RowsColumnTypeDatabaseTypeName); ok {
					typ = col.ColumnTypeDatabaseTypeName(i)
				}
				response.DatabaseTypes = append(response.DatabaseTypes, typ)
			}
			total := 0
			for {
				values := make([]driver.Value, len(response.Columns))
				e = rows.Next(values)
				if e == io.EOF {
					break
				}
				if e != nil {
					response.Error = e.Error()
					break
				}
				row := make([]Value, len(values))
				for i, v := range values {
					row[i], e = value(v)
					if e != nil {
						_ = rows.Close()
						return nil, c.t.Refuse(e)
					}
					total += len(row[i].Text)
				}
				if total > MaxResponseBytes {
					_ = rows.Close()
					return nil, c.t.Refuse(errors.New("SQL source response bound exceeded"))
				}
				response.Rows = append(response.Rows, row)
			}
			if e = rows.Close(); e != nil && response.Error == "" {
				response.Error = e.Error()
			}
		}
		if err = c.t.Record("sql-query", encoded(req), encoded(response)); err != nil {
			return nil, err
		}
	}
	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	if len(response.Columns) != len(response.DatabaseTypes) {
		return nil, c.t.Refuse(errors.New("proof SQL column metadata mismatch"))
	}
	for _, row := range response.Rows {
		if len(row) != len(response.Columns) {
			return nil, c.t.Refuse(errors.New("proof SQL row arity mismatch"))
		}
	}
	return &proofRows{response: response, tape: c.t}, nil
}
func (c *proofConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	// Only this read-planning setting is part of the production snapshot facade.
	// Source writes, ad-hoc DDL and arbitrary SET commands are never accepted.
	if q != "SET LOCAL plan_cache_mode = force_custom_plan" || len(args) != 0 {
		return nil, c.t.Refuse(errors.New("proof refused non-read source command"))
	}
	req, _ := request(q, args)
	var response SQLResponse
	if c.t.replay {
		b, e := c.t.Take("sql-exec", encoded(req))
		if e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &response); e != nil {
			return nil, c.t.Refuse(e)
		}
	} else {
		result, e := c.real.(driver.ExecerContext).ExecContext(ctx, q, args)
		if e != nil {
			response.Error = e.Error()
		} else {
			response.RowsAffected, _ = result.RowsAffected()
		}
		if e = c.t.Record("sql-exec", encoded(req), encoded(response)); e != nil {
			return nil, e
		}
	}
	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	return driver.RowsAffected(response.RowsAffected), nil
}

type proofRows struct {
	tape     *Tape
	response SQLResponse
	n        int
}

func (r *proofRows) Columns() []string                       { return r.response.Columns }
func (r *proofRows) Close() error                            { return nil }
func (r *proofRows) ColumnTypeDatabaseTypeName(i int) string { return r.response.DatabaseTypes[i] }
func (r *proofRows) Next(dest []driver.Value) error {
	if r.n == len(r.response.Rows) {
		return io.EOF
	}
	for i, v := range r.response.Rows[r.n] {
		x, e := v.decode()
		if e != nil {
			return r.tape.Refuse(e)
		}
		dest[i] = x
	}
	r.n++
	return nil
}

type proofStmt struct {
	c *proofConn
	q string
}

func (s *proofStmt) Close() error  { return nil }
func (s *proofStmt) NumInput() int { return -1 }
func (s *proofStmt) Query(args []driver.Value) (driver.Rows, error) {
	a := make([]driver.NamedValue, len(args))
	for i, v := range args {
		a[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return s.c.QueryContext(context.Background(), s.q, a)
}
func (s *proofStmt) Exec(args []driver.Value) (driver.Result, error) {
	a := make([]driver.NamedValue, len(args))
	for i, v := range args {
		a[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return s.c.ExecContext(context.Background(), s.q, a)
}
