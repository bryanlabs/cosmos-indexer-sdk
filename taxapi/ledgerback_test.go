package taxapi

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	backTestSigner  = "cosmos1qypqxpq9qcrsszg2pvxq6rs0zqg3yyc5lzv7xu"
	backTestGranter = "cosmos1zg69v7ys40x77y352eufp27daufrg4ncnjqz7q"
)

func writeCached(t *testing.T, dir string, height int64, method string, result any) {
	t.Helper()
	folder := filepath.Join(dir, strconv.FormatInt(height/10000, 10))
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(folder, strconv.FormatInt(height, 10)+"-"+method+".raw.gz"))
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	if err := json.NewEncoder(gz).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func txHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

type attr struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type event struct {
	Type       string `json:"type"`
	Attributes []attr `json:"attributes"`
}

// cacheBlock writes a block with a successful granted transaction (plain
// attributes) and a failed charged one (base64 attributes, as older CometBFT
// releases wrote them).
func cacheBlock(t *testing.T, dir string, height int64) (okTx, failedTx string) {
	okTx, failedTx = "tx-ok "+backTestSigner, "tx-failed "+backTestSigner
	h := strconv.FormatInt(height, 10)
	writeCached(t, dir, height, "block", map[string]any{
		"block": map[string]any{
			"header": map[string]any{"height": h},
			"data":   map[string]any{"txs": []string{b64(okTx), b64(failedTx)}},
		},
	})
	writeCached(t, dir, height, "block_results", map[string]any{
		"height": h,
		"txs_results": []map[string]any{
			{"code": 0, "events": []event{
				{Type: "use_feegrant", Attributes: []attr{{"granter", backTestGranter}, {"grantee", backTestSigner}}},
				{Type: "coin_spent", Attributes: []attr{{"spender", backTestGranter}, {"amount", "500uatom"}}},
			}},
			{"code": 5, "events": []event{
				{Type: "coin_spent", Attributes: []attr{{b64("spender"), b64(backTestSigner)}, {b64("amount"), b64("2500uatom")}}},
			}},
		},
	})
	return okTx, failedTx
}

func TestBlockTxsFromCache(t *testing.T) {
	dir := t.TempDir()
	okTx, failedTx := cacheBlock(t, dir, 23763459)
	node := &ledgerNode{cache: dir}

	txs, err := node.blockTxs(23763459)
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 2 {
		t.Fatalf("got %d txs, want 2", len(txs))
	}
	if txs[0].Hash != txHash(okTx) || txs[0].Code != 0 || txs[0].Granter != backTestGranter || txs[0].Charged != "" {
		t.Fatalf("granted tx read as %+v", txs[0])
	}
	if txs[1].Hash != txHash(failedTx) || txs[1].Code != 5 || txs[1].Charged != "2500uatom" || txs[1].Granter != "" {
		t.Fatalf("failed tx read as %+v", txs[1])
	}
	if strings.Join(txs[0].Accounts, ",") != backTestSigner+","+backTestGranter {
		t.Fatalf("granted tx accounts %v", txs[0].Accounts)
	}

	charges, err := node.failedTxCharges(23763459)
	if err != nil {
		t.Fatal(err)
	}
	if len(charges) != 1 || charges[txHash(failedTx)] != "2500uatom" {
		t.Fatalf("failed charges %v", charges)
	}
}

func TestBlockTxsFromCacheRefusesMissingAndMislabelled(t *testing.T) {
	dir := t.TempDir()
	node := &ledgerNode{cache: dir}
	if _, err := node.blockTxs(100); err == nil {
		t.Fatal("a height the cache does not hold must fail")
	}
	cacheBlock(t, dir, 200)
	// A block_results file for another height under this height's name.
	writeCached(t, dir, 200, "block_results", map[string]any{"height": "201", "txs_results": []any{}})
	if _, err := node.blockTxs(200); err == nil || !strings.Contains(err.Error(), "201") {
		t.Fatalf("a response for another height must fail, got %v", err)
	}
}

func TestScanBackBlock(t *testing.T) {
	chain := []nodeTx{
		{Hash: "AA", Code: 0, Granter: backTestGranter, Accounts: []string{backTestGranter, backTestSigner}},
		{Hash: "BB", Code: 5, Charged: "2500uatom", Accounts: []string{backTestSigner}},
		{Hash: "BB", Code: 5, Charged: "2500uatom", Accounts: []string{backTestSigner}},
	}
	index := []backTx{
		{Height: 7, Indexed: true, Hash: "aa", Code: 0, Messages: true, TxID: 11},
		{Height: 7, Indexed: true, Hash: "bb", Code: 5, TxID: 12},
	}
	b := scanBackBlock(7, chain, index)
	if b.chainTxs != 0 || len(b.accounts) != 0 {
		t.Fatalf("complete block recorded a hole: %+v", b)
	}
	if len(b.fees) != 1 || b.fees[0] != (FailedTxFee{TxID: 12, Charged: "2500uatom"}) {
		t.Fatalf("fees %+v", b.fees)
	}
	if len(b.grants) != 1 || b.grants[0] != (FeeGrantUse{TxID: 11, Granter: backTestGranter}) {
		t.Fatalf("grants %+v", b.grants)
	}

	// The successful transaction is indexed without its messages.
	index[0].Messages = false
	b = scanBackBlock(7, chain, index)
	if b.chainTxs != 2 || strings.Join(b.accounts, ",") != backTestSigner+","+backTestGranter {
		t.Fatalf("hole %+v", b)
	}

	// A transaction the index lacks gets no fee or grant row.
	b = scanBackBlock(7, chain, index[1:])
	if b.chainTxs != 2 || len(b.grants) != 0 || len(b.fees) != 1 {
		t.Fatalf("missing tx %+v", b)
	}
}
