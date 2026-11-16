package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rr "github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"
	amino "github.com/tendermint/go-amino"
)

// hub1Fixture writes one closed Hub 1 recording of a single height holding
// one fee and one synthetic transfer.
func hub1Fixture(t *testing.T, edit func(records []map[string]any) []map[string]any) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "cache", "cosmoshub-1", "0")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	a, b := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20)
	codec := amino.NewCodec()
	codec.RegisterInterface((*replayTestMsg)(nil), nil)
	codec.RegisterConcrete(replayTestSend{}, "cosmos-sdk/MsgSend", nil)
	codec.RegisterConcrete(replayTestTx{}, "auth/StdTx", nil)
	raw, err := codec.MarshalBinaryLengthPrefixed(replayTestTx{Msgs: []replayTestMsg{replayTestSend{a, b, []replayTestCoin{{"uatom", "53"}}}}, Fee: replayTestFee{Amount: []replayTestCoin{{"uatom", "7"}}, Gas: 90000}})
	require.NoError(t, err)
	gz := func(path string, body []byte) {
		var buf bytes.Buffer
		z := gzip.NewWriter(&buf)
		_, err := z.Write(body)
		require.NoError(t, err)
		require.NoError(t, z.Close())
		require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
	}
	gz(filepath.Join(dir, "9-block.raw.gz"), []byte(fmt.Sprintf(`{"result":{"block_meta":{"block_id":{"hash":"%s"}},"block":{"header":{"chain_id":"cosmoshub-1","height":"9","time":"2019-03-14T00:00:00Z","proposer_address":"synthetic"},"data":{"txs":["%s"]}}}}`, strings.Repeat("AB", 32), base64.StdEncoding.EncodeToString(raw))))
	gz(filepath.Join(dir, "9-block_results.raw.gz"), []byte(`{"result":{"height":"9","results":{"DeliverTx":[{"code":0}]}}}`))
	from, to := sdk.AccAddress(a).String(), sdk.AccAddress(b).String()
	record := func(kind, phase string, tx, message int, label string) map[string]any {
		return map[string]any{"kind": kind, "code": 0, "height": 9, "tx": tx, "message": message, "phase": phase, "label": label, "status": "committed", "address": "", "denom": "", "amount": ""}
	}
	movement := func(phase string, message int, label, address, amount string) map[string]any {
		r := record("movement", phase, 0, message, label)
		r["unit"], r["address"], r["denom"], r["amount"] = "native_bank_tokens", address, "uatom", amount
		return r
	}
	baseline := record("native_state_checkpoint", "block", -1, -1, "segment_baseline/store:main:main")
	baseline["unit"], baseline["before"] = "native_store_bytes", []any{}
	records := []map[string]any{
		baseline,
		record("accounting", "begin_block", -1, -1, ""),
		movement("ante", -1, "ante_fee", from, "-7"),
		record("accounting", "ante", 0, -1, ""),
		movement("message", 0, "bank/send/bank.send", from, "-53"),
		movement("message", 0, "bank/send/bank.send", to, "53"),
		record("accounting", "message", 0, 0, "bank/send"),
		record("accounting", "transaction", 0, -1, ""),
		record("accounting", "end_block", -1, -1, ""),
		record("height_completed", "block", -1, -1, ""),
		record("observation_completed", "segment", -1, -1, ""),
	}
	if edit != nil {
		records = edit(records)
	}
	var stream bytes.Buffer
	enc := json.NewEncoder(&stream)
	for _, r := range records {
		require.NoError(t, enc.Encode(r))
	}
	gz(filepath.Join(root, "movements.source-closed.gz"), stream.Bytes())
	sum := sha256.Sum256(stream.Bytes())
	receipt := fmt.Sprintf(`{"schema":"native033-closed-source-capture-v1","first":9,"through":9,"source_whole_complete":true,"streams":{"movements":{"completion_kind":"observation_completed","raw_bytes":%d,"raw_sha256":"%s","records":%d}}}`,
		stream.Len(), hex.EncodeToString(sum[:]), len(records))
	require.NoError(t, os.WriteFile(filepath.Join(root, "capture.private.json"), []byte(receipt), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "recording-complete.private"), nil, 0o600))
	return root
}

func hub1Invoke(args ...string) error {
	oldFlags, oldArgs := flag.CommandLine, os.Args
	defer func() { flag.CommandLine, os.Args = oldFlags, oldArgs }()
	flag.CommandLine = flag.NewFlagSet("synthetic-hub1", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = append([]string{"legacy-replay-import"}, args...)
	return run()
}

func TestHub1CommandConvertsAClosedRecording(t *testing.T) {
	root := hub1Fixture(t, nil)
	require.NoError(t, hub1Invoke("--hub1-sources", root))
	// Rows go only to a database named for Hub 1 staging.
	for _, name := range []string{"legacy_replay_rehearsal_synthetic_12345678", "taxindexer", "legacy", ""} {
		require.Error(t, hub1Invoke("--hub1-sources", root, "--write", "--rehearsal-db", name))
	}
	require.True(t, hub1RehearsalName.MatchString("legacy_replay_rehearsal_hub1_full_20261116"))
	// The Hub 2 inputs never mix into a Hub 1 run.
	require.Error(t, hub1Invoke("--hub1-sources", root, "--cache", root))
	// An export is taken only at the canonical end of the chain.
	require.Error(t, hub1Invoke("--hub1-sources", root, "--export-records", filepath.Join(root, "capture.private.json"), "--export-proof", filepath.Join(root, "capture.private.json")))
}

func TestHub1CommandStopsOnWhatItCannotLabel(t *testing.T) {
	for name, edit := range map[string]func([]map[string]any) []map[string]any{
		"a movement the rows do not explain": func(r []map[string]any) []map[string]any {
			extra := map[string]any{}
			for k, v := range r[5] {
				extra[k] = v
			}
			extra["phase"], extra["tx"], extra["message"], extra["label"] = "end_block", -1, -1, "staking/slash/bank.burn"
			return append(append(append([]map[string]any{}, r[:9]...), extra), r[9:]...)
		},
		"an unknown record kind":    func(r []map[string]any) []map[string]any { r[3]["kind"] = "pool_sweep"; return r },
		"a receiver short one coin": func(r []map[string]any) []map[string]any { r[5]["amount"] = "52"; return r },
		"a height never closed": func(r []map[string]any) []map[string]any {
			return append(append([]map[string]any{}, r[:9]...), r[10])
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, hub1Invoke("--hub1-sources", hub1Fixture(t, edit)))
		})
	}
	// A recording that was never marked complete is not read at all.
	root := hub1Fixture(t, nil)
	require.NoError(t, os.Remove(filepath.Join(root, "recording-complete.private")))
	require.Error(t, hub1Invoke("--hub1-sources", root))
	var p rr.Proof
	require.Error(t, readJSON(filepath.Join(root, "capture.private.json"), &p, true))
}
