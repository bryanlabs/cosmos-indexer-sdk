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
	"sync"
	"testing"
	"time"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
	querytypes "github.com/cosmos/cosmos-sdk/types/query"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/shopspring/decimal"
)

type cashDayReceipt struct {
	Day      time.Time
	Height   int64
	Coins    map[string]string
	Receipts []FlowArchiveReceipt
}

func cashDayRead(s *Server, a *flowArchive, address string, day time.Time) (cashDayReceipt, error) {
	out := cashDayReceipt{Day: day, Coins: map[string]string{}}
	ids, err := s.chainRowIDs(apiMainnet)
	if err != nil {
		return out, err
	}
	last, err := s.dayEndBlock(ids, day)
	if err != nil {
		return out, err
	}
	out.Height = last.Height
	at, err := a.header(address, last.Height, &out.Receipts)
	if err != nil {
		return out, err
	}
	next, err := a.header(address, last.Height+1, &out.Receipts)
	if err != nil {
		return out, err
	}
	if !flowArchiveDayBoundary(at, next, last.TimeStamp, flowDay(day).AddDate(0, 0, 1)) {
		return out, fmt.Errorf("cash boundary not proven")
	}
	var key []byte
	seen := map[string]bool{}
	for {
		var response banktypes.QueryAllBalancesResponse
		err = a.query(address, last.Height, "/cosmos.bank.v1beta1.Query/AllBalances", &banktypes.QueryAllBalancesRequest{Address: address, Pagination: &querytypes.PageRequest{Key: key, Limit: 1000}}, &response, &out.Receipts)
		if err != nil {
			return out, err
		}
		if response.Pagination == nil {
			return out, fmt.Errorf("cash pagination absent")
		}
		for _, coin := range response.Balances {
			old := decimal.Zero
			if v, ok := out.Coins[coin.Denom]; ok {
				old = decimal.RequireFromString(v)
			}
			amount, err := decimal.NewFromString(coin.Amount.String())
			if err != nil {
				return out, err
			}
			out.Coins[coin.Denom] = old.Add(amount).String()
		}
		key = response.Pagination.NextKey
		if len(key) == 0 {
			break
		}
		if seen[string(key)] {
			return out, fmt.Errorf("cash pagination repeated")
		}
		seen[string(key)] = true
	}
	return out, nil
}
func cashLiquid(r cashDayReceipt) decimal.Decimal {
	if v, ok := r.Coins["uatom"]; ok {
		return decimal.RequireFromString(v)
	}
	return decimal.Zero
}
func TestOPS818CashTimeline(t *testing.T) {
	var scope struct {
		Wallets []struct {
			Address string
			Since   time.Time
			Daily   map[string]string
		}
		Archive []FlowArchiveReceipt
		AsOf    time.Time
	}
	b, err := os.ReadFile(os.Getenv("H_CASH_TIMELINE_SCOPE"))
	if err != nil || json.Unmarshal(b, &scope) != nil {
		t.Fatal("private timeline scope invalid")
	}
	out := os.Getenv("H_CASH_TIMELINE_DIR")
	if os.Mkdir(out, 0700) != nil {
		t.Fatal("unique timeline directory required")
	}
	db, err := indexerDB.PostgresDbConnect("127.0.0.1", "15438", "cosmostax", "postgres", os.Getenv("H_PASS"), "silent", "default_transaction_read_only=on", "application_name=ops818-cash-timeline-readonly", "jit=off", "statement_timeout=30s", "lock_timeout=2s")
	if err != nil {
		t.Fatal("source connection failed")
	}
	s := NewServer(db, NewOracle(os.Getenv("ORACLE_URL"), os.Getenv("NODE_REST_API")), DefaultNativeAsset)
	s.readOnlyLedger = true
	upstream, err := newFlowArchive(os.Getenv("H_ARCHIVE_RPC"))
	if err != nil {
		t.Fatal("archive config failed")
	}
	cache := map[string]string{}
	var mu sync.Mutex
	for _, r := range scope.Archive {
		if fmt.Sprintf("%x", sha256.Sum256([]byte(r.Response))) != r.SHA256 {
			t.Fatal("retained archive hash mismatch")
		}
		key := fmt.Sprintf("%s|%d|%s|%s", r.Method, r.Height, r.QueryPath, r.RequestData)
		if old, ok := cache[key]; ok && old != r.Response {
			t.Fatal("conflicting retained response")
		}
		cache[key] = r.Response
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/")
		q := r.URL.Query()
		key := fmt.Sprintf("%s|%s|%s|%s", method, q.Get("height"), q.Get("path"), q.Get("data"))
		mu.Lock()
		raw, ok := cache[key]
		mu.Unlock()
		if ok {
			_, _ = w.Write([]byte(raw))
			return
		}
		var body json.RawMessage
		if e := upstream.rpc.GetJSON(method, q, &body); e != nil {
			http.Error(w, "archive read failed", 502)
			return
		}
		mu.Lock()
		cache[key] = string(body)
		mu.Unlock()
		_, _ = w.Write(body)
	}))
	defer proxy.Close()
	a, err := newFlowArchive(proxy.URL)
	if err != nil {
		t.Fatal("cache endpoint failed")
	}
	for index, w := range scope.Wallets {
		dir := filepath.Join(out, walletFile(w.Address))
		if os.Mkdir(dir, 0700) != nil {
			t.Fatal("new wallet path required")
		}
		_ = privateJSON(filepath.Join(dir, "identity.private.json"), w)
		previous, err := cashDayRead(s, a, w.Address, w.Since.AddDate(0, 0, -1))
		if err != nil {
			_ = privateJSON(filepath.Join(dir, "error.private.json"), map[string]any{"error": err.Error()})
			continue
		}
		_ = privateJSON(filepath.Join(dir, "opening.private.json"), previous)
		start := w.Since
		for start.Before(scope.AsOf) {
			monthEnd := time.Date(start.Year(), start.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			if monthEnd.After(scope.AsOf) {
				monthEnd = scope.AsOf
			}
			closing, readErr := cashDayRead(s, a, w.Address, monthEnd.AddDate(0, 0, -1))
			if readErr != nil {
				_ = privateAppend(filepath.Join(dir, "errors.private.jsonl"), map[string]any{"day": monthEnd, "error": readErr.Error()})
				break
			}
			native := decimal.Zero
			for day := start; day.Before(monthEnd); day = day.AddDate(0, 0, 1) {
				if v, ok := w.Daily[day.Format(time.DateOnly)]; ok {
					native = native.Add(decimal.RequireFromString(v))
				}
			}
			residual := cashLiquid(previous).Add(native).Sub(cashLiquid(closing))
			_ = privateAppend(filepath.Join(dir, "months.private.jsonl"), map[string]any{"from": start, "to_exclusive": monthEnd, "native_net_base": native, "residual_base": residual, "closing": closing})
			{
				prior := previous
				for day := start; day.Before(monthEnd); day = day.AddDate(0, 0, 1) {
					current := closing
					var e error
					if !day.Equal(monthEnd.AddDate(0, 0, -1)) {
						current, e = cashDayRead(s, a, w.Address, day)
					}
					if e != nil {
						_ = privateAppend(filepath.Join(dir, "errors.private.jsonl"), map[string]any{"day": day, "error": e.Error()})
						break
					}
					net := decimal.Zero
					if v, ok := w.Daily[day.Format(time.DateOnly)]; ok {
						net = decimal.RequireFromString(v)
					}
					delta := cashLiquid(prior).Add(net).Sub(cashLiquid(current))
					_ = privateAppend(filepath.Join(dir, "days.private.jsonl"), map[string]any{"day": day, "native_net_base": net, "residual_base": delta, "closing": current})
					prior = current
				}
			}
			previous = closing
			start = monthEnd
		}
		_ = privateAppend(filepath.Join(out, "progress.private.jsonl"), map[string]any{"ordinal": index + 1, "at": time.Now().UTC()})
	}
	t.Log("all scoped cash timelines retained", len(scope.Wallets))
}
