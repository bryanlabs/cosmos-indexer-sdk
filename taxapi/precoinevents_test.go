package taxapi

import (
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
)

func TestPreCoinEventsCharge(t *testing.T) {
	const fee = "2500uatom"
	cases := []struct {
		name    string
		r       failedTxResult
		charged string
		ok      bool
	}{
		{"message failed", failedTxResult{Code: 13, Codespace: "channel", Log: "failed to execute message; message index: 1: receive packet verification failed: packet sequence already received: redundant relay"}, fee, true},
		{"message failed with insufficient funds", failedTxResult{Code: 5, Codespace: "sdk", Log: "failed to execute message; message index: 0: 5887uatom is smaller than 9000uatom: insufficient funds"}, fee, true},
		{"message without a route", failedTxResult{Code: 6, Codespace: "sdk", Log: "unrecognized message service method: /a.b.Msg; message index: 0: unknown request"}, fee, true},
		{"wrong sequence", failedTxResult{Code: 32, Codespace: "sdk", Log: "account sequence mismatch, expected 7, got 6: incorrect account sequence"}, "", true},
		{"bad signature", failedTxResult{Code: 4, Codespace: "sdk", Log: "signature verification failed; please verify account number (1) and chain-id (cosmoshub-4): unauthorized"}, "", true},
		{"fee above the balance", failedTxResult{Code: 5, Codespace: "sdk", Log: "1000uatom is smaller than 2500uatom: insufficient funds: insufficient funds"}, "", true},
		{"no block gas left", failedTxResult{Code: 11, Codespace: "sdk", Log: "no block gas left to run tx: out of gas"}, "", true},
		{"out of gas in a message", failedTxResult{Code: 11, Codespace: "sdk", Log: "out of gas in location: WritePerByte; gasWanted: 300000, gasUsed: 300120: out of gas", GasWanted: 300000, TxBytes: 400, Signers: 1}, fee, true},
		{"out of gas below the ante bound", failedTxResult{Code: 11, Codespace: "sdk", Log: "out of gas in location: WriteFlat; gasWanted: 60000, gasUsed: 61000: out of gas", GasWanted: 60000, TxBytes: 400, Signers: 1}, "", false},
		{"out of gas with two signers", failedTxResult{Code: 11, Codespace: "sdk", Log: "out of gas in location: ReadFlat; gasWanted: 150000, gasUsed: 151000: out of gas", GasWanted: 150000, TxBytes: 400, Signers: 2}, "", false},
		{"redacted internal error", failedTxResult{Code: 1, Codespace: "undefined", Log: "internal"}, "", false},
		{"redacted panic", failedTxResult{Code: 111222, Codespace: "undefined", Log: "panic message redacted to hide potentially sensitive system info: panic"}, "", false},
	}
	for _, c := range cases {
		c.r.Fee, c.r.FeeRead = fee, true
		charged, ok := preCoinEventsCharge(c.r)
		if charged != c.charged || ok != c.ok {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, charged, ok, c.charged, c.ok)
		}
	}
	// A message failure whose bytes did not decode stays unsettled.
	if charged, ok := preCoinEventsCharge(failedTxResult{Code: 13, Codespace: "channel", Log: msgFailurePrefix + "0: x"}); ok || charged != "" {
		t.Errorf("message failure without readable bytes: got (%q, %v)", charged, ok)
	}
	// A transaction that declared no fee had nothing to charge.
	for _, log := range []string{msgFailurePrefix + "0: x", outOfGasPrefix + "WriteFlat; gasWanted: 10, gasUsed: 11: out of gas"} {
		if charged, ok := preCoinEventsCharge(failedTxResult{Code: 11, Codespace: "sdk", Log: log, FeeRead: true}); !ok || charged != "" {
			t.Errorf("zero fee %q: got (%q, %v)", log, charged, ok)
		}
	}
}

func testTxBytes(t *testing.T, memo string, signers int) string {
	t.Helper()
	return testTxBytesFee(t, memo, signers, sdk.NewCoins(sdk.NewInt64Coin("uatom", 2500)))
}

func testTxBytesFee(t *testing.T, memo string, signers int, fee sdk.Coins) string {
	t.Helper()
	body, err := (&txtypes.TxBody{Memo: memo}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	auth, err := (&txtypes.AuthInfo{Fee: &txtypes.Fee{Amount: fee, GasLimit: 300000}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	raw := txtypes.TxRaw{BodyBytes: body, AuthInfoBytes: auth}
	for i := 0; i < signers; i++ {
		raw.Signatures = append(raw.Signatures, []byte{byte(i + 1)})
	}
	b, err := raw.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Before Gaia v6 neither the block nor a failed transaction carries coin
// events, so the outcome comes from where each one failed.
func TestBlockTxsBeforeCoinEvents(t *testing.T) {
	dir := t.TempDir()
	const height = 6000000
	msgTx, seqTx, gasTx := testTxBytes(t, "message", 1), testTxBytes(t, "sequence", 1), testTxBytes(t, "gas", 1)
	writeCached(t, dir, height, "block", map[string]any{
		"block": map[string]any{
			"header": map[string]any{"height": "6000000"},
			"data":   map[string]any{"txs": []string{b64(msgTx), b64(seqTx), b64(gasTx)}},
		},
	})
	writeCached(t, dir, height, "block_results", map[string]any{
		"height": "6000000",
		"txs_results": []map[string]any{
			{"code": 13, "codespace": "channel", "gas_wanted": "200000", "log": "failed to execute message; message index: 0: packet sequence already received: redundant relay"},
			{"code": 32, "codespace": "sdk", "gas_wanted": "200000", "log": "account sequence mismatch, expected 7, got 6: incorrect account sequence"},
			{"code": 11, "codespace": "sdk", "gas_wanted": "50000", "log": "out of gas in location: WriteFlat; gasWanted: 50000, gasUsed: 51000: out of gas"},
		},
		"finalize_block_events": []event{{Type: "mint"}, {Type: "transfer"}},
	})
	node := &ledgerNode{cache: dir}
	txs, err := node.blockTxs(height)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		charged string
		known   bool
	}{{"2500uatom", true}, {"", true}, {"", false}}
	for i, w := range want {
		if txs[i].Charged != w.charged || txs[i].ChargeKnown != w.known {
			t.Errorf("tx %d read as charged %q known %v, want %q %v", i, txs[i].Charged, txs[i].ChargeKnown, w.charged, w.known)
		}
	}
	charges, err := node.failedTxCharges(height)
	if err != nil {
		t.Fatal(err)
	}
	if len(charges) != 2 || charges[txHash(msgTx)] != "2500uatom" || charges[txHash(seqTx)] != "" {
		t.Fatalf("charges %v", charges)
	}
	if _, ok := charges[txHash(gasTx)]; ok {
		t.Fatal("an out-of-gas failure the gas limit does not settle must be left out")
	}
	index := []backTx{
		{Height: height, Indexed: true, Hash: txHash(msgTx), Code: 13, TxID: 1},
		{Height: height, Indexed: true, Hash: txHash(seqTx), Code: 32, TxID: 2},
		{Height: height, Indexed: true, Hash: txHash(gasTx), Code: 11, TxID: 3},
	}
	b := scanBackBlock(height, txs, index)
	if len(b.fees) != 2 || b.fees[0] != (FailedTxFee{TxID: 1, Charged: "2500uatom"}) || b.fees[1] != (FailedTxFee{TxID: 2}) {
		t.Fatalf("scan fees %+v", b.fees)
	}
}

// From Gaia v6 the block's own events carry coin events, and a failed
// transaction with none paid nothing whatever its log says.
func TestBlockTxsWithCoinEvents(t *testing.T) {
	dir := t.TempDir()
	const height = 9000000
	tx := testTxBytes(t, "rejected", 1)
	writeCached(t, dir, height, "block", map[string]any{
		"block": map[string]any{
			"header": map[string]any{"height": "9000000"},
			"data":   map[string]any{"txs": []string{b64(tx)}},
		},
	})
	writeCached(t, dir, height, "block_results", map[string]any{
		"height": "9000000",
		"txs_results": []map[string]any{
			{"code": 11, "codespace": "sdk", "gas_wanted": "900000", "log": "out of gas in location: WriteFlat; gasWanted: 900000, gasUsed: 901000: out of gas"},
		},
		"begin_block_events": []event{{Type: "coin_received"}, {Type: "coinbase"}},
	})
	txs, err := (&ledgerNode{cache: dir}).blockTxs(height)
	if err != nil {
		t.Fatal(err)
	}
	if txs[0].Charged != "" || !txs[0].ChargeKnown || !txs[0].Rejected {
		t.Fatalf("read as %+v", txs[0])
	}
}

func TestDeclaredFee(t *testing.T) {
	fee, signers, ok := declaredFee([]byte(testTxBytes(t, "x", 2)))
	if fee != "2500uatom" || signers != 2 || !ok {
		t.Fatalf("got %q %d %v", fee, signers, ok)
	}
	// A zero coin is no fee.
	fee, _, ok = declaredFee([]byte(testTxBytesFee(t, "x", 1, sdk.Coins{sdk.NewInt64Coin("uatom", 0)})))
	if fee != "" || !ok {
		t.Fatalf("zero fee read as %q %v", fee, ok)
	}
	if _, _, ok := declaredFee([]byte("not a transaction")); ok {
		t.Fatal("bytes that do not decode must not read")
	}
}
