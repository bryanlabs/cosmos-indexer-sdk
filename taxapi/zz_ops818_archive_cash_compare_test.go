//go:build ops818harness

package taxapi

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
)

func TestOPS818ArchiveCashCompare(t *testing.T) {
	var scope struct {
		Wallets []struct {
			Address string
			Since   time.Time
		}
		Archive []FlowArchiveReceipt
	}
	b, err := os.ReadFile(os.Getenv("H_ARCHIVE_COMPARE_SCOPE"))
	if err != nil {
		t.Fatal("private compare scope missing")
	}
	if json.Unmarshal(b, &scope) != nil {
		t.Fatal("compare scope invalid")
	}
	out := os.Getenv("H_ARCHIVE_COMPARE_DIR")
	if os.Mkdir(out, 0700) != nil {
		t.Fatal("unique private output required")
	}
	db, err := indexerDB.PostgresDbConnect("127.0.0.1", "15438", "cosmostax", "postgres", os.Getenv("H_PASS"), "silent", "default_transaction_read_only=on", "application_name=ops818-archive-cash-compare-readonly", "jit=off", "statement_timeout=30s", "lock_timeout=2s", "effective_io_concurrency=200")
	if err != nil {
		t.Fatal("source read connection failed")
	}
	s := NewServer(db, NewOracle(os.Getenv("ORACLE_URL"), os.Getenv("NODE_REST_API")), DefaultNativeAsset)
	s.readOnlyLedger = true
	upstream, err := newFlowArchive(os.Getenv("H_ARCHIVE_RPC"))
	if err != nil {
		t.Fatal("archive config failed")
	}
	cache := map[string]string{}
	for _, r := range scope.Archive {
		if fmt.Sprintf("%x", sha256.Sum256([]byte(r.Response))) != r.SHA256 {
			t.Fatal("retained source hash mismatch")
		}
		key := fmt.Sprintf("%s|%d|%s|%s", r.Method, r.Height, r.QueryPath, r.RequestData)
		if old, ok := cache[key]; ok && old != r.Response {
			t.Fatal("conflicting retained source response")
		}
		cache[key] = r.Response
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/")
		q := r.URL.Query()
		key := fmt.Sprintf("%s|%s|%s|%s", method, q.Get("height"), q.Get("path"), q.Get("data"))
		if raw, ok := cache[key]; ok {
			_, _ = w.Write([]byte(raw))
			return
		}
		var raw json.RawMessage
		if e := upstream.rpc.GetJSON(method, q, &raw); e != nil {
			http.Error(w, "archive read failed", 502)
			return
		}
		_, _ = w.Write(raw)
	}))
	defer proxy.Close()
	archive, err := newFlowArchive(proxy.URL)
	if err != nil {
		t.Fatal("cache endpoint failed")
	}
	end := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	for i, w := range scope.Wallets {
		opening, e1 := archive.holdingsAt(s, apiMainnet, w.Address, w.Since.AddDate(0, 0, -1))
		closing, e2 := archive.holdingsAt(s, apiMainnet, w.Address, end)
		result := map[string]any{"address": w.Address, "since": w.Since, "closing_day": end, "opening": opening, "closing": closing, "opening_error": fmt.Sprint(e1), "closing_error": fmt.Sprint(e2), "archive": archive.evidence(map[string]bool{w.Address: true})}
		if err = privateJSON(filepath.Join(out, walletFile(w.Address)+".archive-compare.private.json"), result); err != nil {
			t.Fatal("private receipt save failed")
		}
		_ = privateAppend(filepath.Join(out, "progress.private.jsonl"), map[string]any{"ordinal": i + 1, "opening_ok": e1 == nil, "closing_ok": e2 == nil, "at": time.Now().UTC()})
	}
	t.Log("all scoped material archive comparisons captured", len(scope.Wallets))
}
