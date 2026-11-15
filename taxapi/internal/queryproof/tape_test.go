package queryproof

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) *Tape {
	t.Helper()
	x, e := NewCapture(Source{Kind: "scratch", ExportedSnapshot: "00000001-00000001-1", PGSnapshot: "10:20:", DatabaseIdentitySHA256: Digest([]byte("fixture database")), ReadOnly: true, Isolation: "repeatable read"}, Identity{SourceCommit: "fixture implementation", RootSHA256: Digest([]byte("fixture root")), HarnessExecutableSHA256: Digest([]byte("fixture executable")), HarnessSourceSHA256: Digest([]byte("fixture harness"))})
	if e != nil {
		t.Fatal("fixture rejected")
	}
	return x
}
func frozen(t *testing.T, x *Tape) *Tape {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "capture")
	sha, e := x.Save(dir)
	if e != nil {
		t.Fatal("capture failed")
	}
	r, e := Load(filepath.Join(dir, "bundle.private.json"), sha)
	if e != nil {
		t.Fatal("load failed")
	}
	return r
}
func TestCompleteMissingExhaustedExtraAndTampering(t *testing.T) {
	t.Run("complete", func(t *testing.T) {
		x := fixture(t)
		if e := x.Record("sql-query", []byte(`{"q":1}`), []byte(`{"result":2}`)); e != nil {
			t.Fatal(e)
		}
		r := frozen(t, x)
		if _, e := r.Take("sql-query", []byte(`{"q":1}`)); e != nil {
			t.Fatal(e)
		}
		if e := r.Finish(false); e != nil {
			t.Fatal(e)
		}
	})
	for _, which := range []string{"missing", "exhausted", "extra"} {
		t.Run(which, func(t *testing.T) {
			x := fixture(t)
			_ = x.Record("sql-query", []byte(`{"q":1}`), []byte(`{"result":2}`))
			r := frozen(t, x)
			switch which {
			case "missing":
				_, _ = r.Take("sql-query", []byte(`{"q":9}`))
			case "exhausted":
				_, _ = r.Take("sql-query", []byte(`{"q":1}`))
				_, _ = r.Take("sql-query", []byte(`{"q":1}`))
			}
			if r.Finish(false) == nil {
				t.Fatal("unaccounted inputs accepted")
			}
		})
	}
	t.Run("tampered", func(t *testing.T) {
		x := fixture(t)
		_ = x.Record("http", []byte(`{}`), []byte(`{"body":1}`))
		dir := filepath.Join(t.TempDir(), "capture")
		sha, e := x.Save(dir)
		if e != nil {
			t.Fatal(e)
		}
		path := filepath.Join(dir, "bundle.private.json")
		b, _ := os.ReadFile(path)
		b = []byte(strings.Replace(string(b), `"body":1`, `"body":2`, 1))
		if e = os.WriteFile(path, b, 0600); e != nil {
			t.Fatal(e)
		}
		if _, e = Load(path, sha); e == nil {
			t.Fatal("bundle tampering accepted")
		}
		if _, e = Load(path, Digest(b)); e == nil {
			t.Fatal("entry tampering accepted")
		}
	})
}
func TestCoverageDuplicateIsOnlyExplicitWaiver(t *testing.T) {
	req := SQLRequest{Query: `SELECT COUNT(*) FROM "blocks" WHERE chain_id = $1::int AND height >= $2::bigint AND height <= $3::bigint AND tx_indexed = true`, Args: []Value{{Kind: "int", Text: "1"}, {Kind: "int", Text: "10"}, {Kind: "int", Text: "20"}}}
	// The exception recognizes the exact production COUNT SQL, not an arbitrary
	// query whose words merely resemble the coverage predicate.
	for _, allow := range []bool{false, true} {
		x := fixture(t)
		_ = x.Record("sql-query", encoded(req), encoded(SQLResponse{Columns: []string{"count"}, Rows: [][]Value{{{Kind: "int", Text: "11"}}}}))
		r := frozen(t, x)
		if r.Finish(allow) == nil {
			t.Fatal("single unused coverage read waived")
		}
	}
}
func TestHTTPHeightReplayWithoutNetworkFallback(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("x-cosmos-block-height", r.Header.Get("x-cosmos-block-height"))
		_, _ = w.Write([]byte(`{"result":"fixture"}`))
	}))
	defer srv.Close()
	x := fixture(t)
	client := &http.Client{Transport: &Transport{Tape: x, Origins: map[string]string{srv.URL: "node"}, Live: http.DefaultTransport}}
	req, _ := http.NewRequest("GET", srv.URL+"/cosmos/bank/v1beta1/balances/fixture?pagination.limit=100", nil)
	req.Header.Set("x-cosmos-block-height", "123")
	resp, e := client.Do(req)
	if e != nil {
		t.Fatal("fixture capture failed")
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	r := frozen(t, x)
	srv.Close()
	client.Transport = &Transport{Tape: r, Origins: map[string]string{"https://offline.invalid": "node"}, Live: panicTransport{}}
	req, _ = http.NewRequest("GET", "https://offline.invalid/cosmos/bank/v1beta1/balances/fixture?pagination.limit=100", nil)
	req.Header.Set("x-cosmos-block-height", "123")
	resp, e = client.Do(req)
	if e != nil {
		t.Fatal("offline replay failed")
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if e = r.Finish(false); e != nil || calls != 1 {
		t.Fatal("replay did not consume exact input")
	}
	req.Header.Set("x-cosmos-block-height", "124")
	if _, e = client.Do(req); e == nil {
		t.Fatal("wrong height accepted")
	}
	if r.Finish(false) == nil {
		t.Fatal("swallowed fallback error accepted")
	}
}

type panicTransport struct{}

func (panicTransport) RoundTrip(*http.Request) (*http.Response, error) {
	panic("offline transport attempted a network fallback")
}
func TestSQLReplayNeverOpensProvidedDSN(t *testing.T) {
	x := fixture(t)
	req := SQLRequest{Query: "SELECT fixture"}
	_ = x.Record("sql-query", encoded(req), encoded(SQLResponse{Columns: []string{"fixture"}, DatabaseTypes: []string{"INT8"}, Rows: [][]Value{{{Kind: "int", Text: "7"}}}}))
	r := frozen(t, x)
	db, e := OpenSQL(r, "invalid DSN that must never be parsed")
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var n int
	if e = db.QueryRow("SELECT fixture").Scan(&n); e != nil || n != 7 {
		t.Fatal("offline SQL replay failed")
	}
	if e = r.Finish(false); e != nil {
		t.Fatal(e)
	}
	if e = db.QueryRow("SELECT missing").Scan(&n); e == nil {
		t.Fatal("missing SQL accepted")
	}
}
func TestSQLMutationAndUnsafeTransactionRefuse(t *testing.T) {
	r := frozen(t, fixture(t))
	db, e := OpenSQL(r, "")
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec("COMMIT"); e == nil {
		t.Fatal("source transaction escape accepted")
	}
	if r.Finish(false) == nil {
		t.Fatal("swallowed write refusal accepted")
	}
}
func TestValueRoundTrip(t *testing.T) {
	for _, v := range []driver.Value{nil, int64(9), float64(1.25), true, "fixture", []byte("fixture")} {
		encoded, e := value(v)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = encoded.decode(); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := (Value{Kind: "int", Text: "1 trailing"}).decode(); e == nil {
		t.Fatal("malformed integer accepted")
	}
}
func TestSourceAndAdmissionFailClosedBeforeConnecting(t *testing.T) {
	if _, e := Export(context.Background(), "host=example.invalid dbname=query_proof_scratch_fixture", "scratch", nil); e == nil {
		t.Fatal("nonloopback scratch accepted")
	}
	if _, e := Export(context.Background(), "host=127.0.0.1 dbname=query_proof_scratch_fixture", "admitted-source", nil); e == nil {
		t.Fatal("missing admission accepted")
	}
	x := fixture(t)
	x.Refuse(errors.New("fixture refusal"))
	if _, e := x.Save(filepath.Join(t.TempDir(), "failed")); e == nil {
		t.Fatal("failed capture published")
	}
}
func TestBoundAndUnknownFields(t *testing.T) {
	x := fixture(t)
	if e := x.Record("http", []byte(`{}`), make([]byte, MaxResponseBytes+1)); e == nil {
		t.Fatal("oversized response accepted")
	}
	r := fixture(t)
	dir := filepath.Join(t.TempDir(), "capture")
	_, e := r.Save(dir)
	if e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(dir, "bundle.private.json")
	b, _ := os.ReadFile(p)
	var obj map[string]any
	_ = json.Unmarshal(b, &obj)
	obj["Unknown"] = true
	b, _ = json.Marshal(obj)
	_ = os.WriteFile(p, b, 0600)
	if _, e = Load(p, Digest(b)); e == nil {
		t.Fatal("unknown bundle field accepted")
	}
}
