package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/query"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/gogoproto/proto"
)

// fakeRPC answers abci_query for AllBalances like an archive node, at the
// height it is told to claim.
func fakeRPC(t *testing.T, claim string, seen *banktypes.QueryAllBalancesRequest, seenHeight *string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatal(err)
		}
		if req.Method == "status" {
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"sync_info":{"latest_block_height":"27000000","latest_block_time":"2026-11-22T16:00:00Z"}}}`)
			return
		}
		if req.Method != "abci_query" || req.Params["path"] != "/cosmos.bank.v1beta1.Query/AllBalances" {
			t.Fatalf("unexpected call %s %v", req.Method, req.Params["path"])
		}
		data, _ := hex.DecodeString(req.Params["data"].(string))
		if err := proto.Unmarshal(data, seen); err != nil {
			t.Fatal(err)
		}
		*seenHeight, _ = req.Params["height"].(string)
		resp := &banktypes.QueryAllBalancesResponse{
			Balances:   sdk.NewCoins(sdk.NewCoin("uatom", sdkmath.NewInt(13002))),
			Pagination: &query.PageResponse{NextKey: []byte{1, 2, 3}},
		}
		bz, _ := proto.Marshal(resp)
		h := claim
		if h == "" {
			h = *seenHeight
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"response": map[string]any{
			"code": 0, "value": base64.StdEncoding.EncodeToString(bz), "height": h}}})
		_, _ = w.Write(out)
	}))
}

func get(t *testing.T, s *shim, path, height string) (int, map[string]any) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if height != "" {
		req.Header.Set(heightHeader, height)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v: %s", path, err, rec.Body.String())
	}
	return rec.Code, body
}

func TestBalancesAtHeightLikeTheGateway(t *testing.T) {
	var seen banktypes.QueryAllBalancesRequest
	var seenHeight string
	up := fakeRPC(t, "", &seen, &seenHeight)
	defer up.Close()
	s, err := newShim([]string{up.URL}, []float64{1000})
	if err != nil {
		t.Fatal(err)
	}
	code, body := get(t, s, "/cosmos/bank/v1beta1/balances/cosmos1abc?pagination.limit=1000&pagination.key="+base64.StdEncoding.EncodeToString([]byte{9, 9}), "9041673")
	if code != http.StatusOK {
		t.Fatalf("status %d %v", code, body)
	}
	if seen.Address != "cosmos1abc" || seen.Pagination == nil || seen.Pagination.Limit != 1000 || string(seen.Pagination.Key) != string([]byte{9, 9}) {
		t.Fatalf("request sent %+v", seen)
	}
	if seenHeight != "9041673" {
		t.Fatalf("height sent %q", seenHeight)
	}
	bal := body["balances"].([]any)[0].(map[string]any)
	if bal["denom"] != "uatom" || bal["amount"] != "13002" {
		t.Fatalf("balances %v", body["balances"])
	}
	if body["pagination"].(map[string]any)["next_key"] != base64.StdEncoding.EncodeToString([]byte{1, 2, 3}) {
		t.Fatalf("pagination %v", body["pagination"])
	}
}

func TestRefusesAnswerAtAnotherHeight(t *testing.T) {
	var seen banktypes.QueryAllBalancesRequest
	var seenHeight string
	up := fakeRPC(t, "27000000", &seen, &seenHeight)
	defer up.Close()
	s, _ := newShim([]string{up.URL}, []float64{1000})
	code, body := get(t, s, "/cosmos/bank/v1beta1/balances/cosmos1abc", "9041673")
	if code == http.StatusOK || !strings.Contains(body["message"].(string), "not 9041673") {
		t.Fatalf("status %d %v", code, body)
	}
}

func TestLatestHeightAndUnknownPath(t *testing.T) {
	var seen banktypes.QueryAllBalancesRequest
	var seenHeight string
	up := fakeRPC(t, "", &seen, &seenHeight)
	defer up.Close()
	s, _ := newShim([]string{up.URL}, []float64{1000})
	code, body := get(t, s, "/cosmos/base/tendermint/v1beta1/blocks/latest", "")
	if code != http.StatusOK || body["block"].(map[string]any)["header"].(map[string]any)["height"] != "27000000" {
		t.Fatalf("latest %d %v", code, body)
	}
	if code, _ := get(t, s, "/cosmos/gov/v1beta1/proposals", ""); code != http.StatusNotImplemented {
		t.Fatalf("unknown path status %d", code)
	}
}

func TestRouteNameHidesAccounts(t *testing.T) {
	if got := routeName("/cosmos/staking/v1beta1/validators/cosmosvaloper1xyz/delegations"); strings.Contains(got, "xyz") {
		t.Fatal(got)
	}
	if got := routeName("/cosmos/bank/v1beta1/balances/cosmos1xyz"); strings.Contains(got, "xyz") {
		t.Fatal(got)
	}
}

func TestReadOriginFromEnv(t *testing.T) {
	t.Setenv("SHIM_TEST_ORIGIN", "https://rpc.example")
	if got, err := readOrigin("env:SHIM_TEST_ORIGIN"); err != nil || got != "https://rpc.example" {
		t.Fatal(got, err)
	}
	if _, err := readOrigin("env:SHIM_TEST_MISSING"); err == nil {
		t.Fatal("an empty variable was accepted")
	}
}

func TestMovesToNextUpstreamWhenOneFails(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer down.Close()
	var seen banktypes.QueryAllBalancesRequest
	var seenHeight string
	up := fakeRPC(t, "", &seen, &seenHeight)
	defer up.Close()
	s, _ := newShim([]string{down.URL, up.URL}, []float64{1000, 1000})
	code, body := get(t, s, "/cosmos/bank/v1beta1/balances/cosmos1abc", "9041673")
	if code != http.StatusOK {
		t.Fatalf("status %d %v", code, body)
	}
}

func TestQueryErrorIsNotRetried(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"response":{"code":111222,"log":"invalid denom: : panic","height":"9041673"}}}`)
	}))
	defer up.Close()
	s, _ := newShim([]string{up.URL}, []float64{1000})
	code, body := get(t, s, "/cosmos/staking/v1beta1/delegations/cosmos1abc", "9041673")
	if code != http.StatusInternalServerError || !strings.Contains(body["message"].(string), "invalid denom") || calls != 1 {
		t.Fatalf("status %d calls %d %v", code, calls, body)
	}
}

func TestRawStoreQueryPassesThrough(t *testing.T) {
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.Unmarshal(raw, &req)
		got = req.Params
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"response":{"code":0,"value":"AQI=","height":"9041673"}}}`)
	}))
	defer up.Close()
	s, _ := newShim([]string{up.URL}, []float64{1000})
	code, body := get(t, s, `/abci_query?path=%22/store/staking/key%22&data=0x31aa&height=9041673`, "")
	if code != http.StatusOK || got["path"] != "/store/staking/key" || got["data"] != "31aa" || got["height"] != "9041673" {
		t.Fatalf("status %d params %v body %v", code, got, body)
	}
	if body["result"].(map[string]any)["response"].(map[string]any)["height"] != "9041673" {
		t.Fatalf("body %v", body)
	}
}

func TestFailedUpstreamRestsWhileAnotherAnswers(t *testing.T) {
	downCalls := 0
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downCalls++
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer down.Close()
	var seen banktypes.QueryAllBalancesRequest
	var seenHeight string
	up := fakeRPC(t, "", &seen, &seenHeight)
	defer up.Close()
	s, _ := newShim([]string{down.URL, up.URL}, []float64{1000, 1000})
	for i := 0; i < 6; i++ {
		if code, body := get(t, s, "/cosmos/bank/v1beta1/balances/cosmos1abc", "9041673"); code != http.StatusOK {
			t.Fatalf("status %d %v", code, body)
		}
	}
	if code, _ := get(t, s, "/cosmos/base/tendermint/v1beta1/blocks/latest", ""); code != http.StatusOK {
		t.Fatalf("latest status %d", code)
	}
	if downCalls != 1 {
		t.Fatalf("resting upstream called %d times", downCalls)
	}
}
