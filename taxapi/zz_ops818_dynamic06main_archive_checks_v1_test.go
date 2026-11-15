//go:build ops818dynamic06mainopeningv1

package taxapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	querytypes "github.com/cosmos/cosmos-sdk/types/query"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
)

func TestOPS818Archive07V3V2HTTPProofs(t *testing.T) {
	root, e := os.MkdirTemp(os.Getenv("TMPDIR"), "archive-http-proof-")
	if e != nil {
		t.Fatal(e)
	}
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/block" {
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"block": map[string]any{"header": map[string]any{"chain_id": "cosmoshub-4", "height": "10", "time": "2025-01-01T23:59:59Z"}}}})
			return
		}
		resp := banktypes.QueryAllBalancesResponse{Balances: sdk.NewCoins(sdk.NewInt64Coin("uatom", 3)), Pagination: &querytypes.PageResponse{}}
		b, _ := resp.Marshal()
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"response": map[string]any{"code": 0, "height": "10", "value": base64.StdEncoding.EncodeToString(b)}}})
	}))
	defer server.Close()
	tr := &b9Transport{Root: root, Origin: server.URL, Used: map[string]string{}}
	a, e := b9Archive(server.URL, tr)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.header("synthetic", 10); e != nil {
		t.Fatal(e)
	}
	var got banktypes.QueryAllBalancesResponse
	if e = a.query("synthetic", 10, "/cosmos.bank.v1beta1.Query/AllBalances", &banktypes.QueryAllBalancesRequest{Address: "synthetic", Pagination: &querytypes.PageRequest{Limit: 1000}}, &got); e != nil {
		t.Fatal(e)
	}
	if !got.Balances.AmountOf("uatom").Equal(sdk.NewInt(3)) {
		t.Fatal("actual protobuf state decode mismatch")
	}
	before := count
	offline := &b9Transport{Root: root, Origin: server.URL, Offline: true, Used: map[string]string{}}
	replay, e := b9Archive(server.URL, offline)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = replay.header("synthetic", 10); e != nil {
		t.Fatal(e)
	}
	if count != before || offline.Network != 0 {
		t.Fatal("offline raw proof made duplicate source read")
	}
	if _, e = replay.header("synthetic", 11); e == nil {
		t.Fatal("uncaptured historical request accepted offline")
	}
	if count != before {
		t.Fatal("missing offline cache read source")
	}
	files, e := filepath.Glob(filepath.Join(root, "*.http.private.json"))
	if e != nil {
		t.Fatal(e)
	}
	for _, p := range files {
		var rec b9HTTPRecord
		if v9Read(p, &rec) != nil {
			t.Fatal("cache unreadable")
		}
		if rec.Method == "block" {
			rec.Body = []byte(`{}`)
			b, _ := json.Marshal(rec)
			if os.WriteFile(p, b, 0600) != nil {
				t.Fatal("fixture tamper failed")
			}
		}
	}
	if _, e = replay.header("synthetic", 10); e == nil {
		t.Fatal("tampered source raw hash accepted")
	}
}
func TestOPS818Archive07V3V2WrongChainHeightRejected(t *testing.T) {
	for _, mode := range []string{"chain", "height"} {
		t.Run(mode, func(t *testing.T) {
			root, e := os.MkdirTemp(os.Getenv("TMPDIR"), "archive-header-proof-")
			if e != nil {
				t.Fatal(e)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				chain, height := "cosmoshub-4", "10"
				if mode == "chain" {
					chain = "other"
				} else {
					height = "11"
				}
				json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"block": map[string]any{"header": map[string]any{"chain_id": chain, "height": height, "time": "2025-01-01T23:59:59Z"}}}})
			}))
			defer server.Close()
			tr := &b9Transport{Root: root, Origin: server.URL, Used: map[string]string{}}
			a, e := b9Archive(server.URL, tr)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = a.header("synthetic", 10); e == nil {
				t.Fatal("wrong exact historical header accepted")
			}
		})
	}
}
