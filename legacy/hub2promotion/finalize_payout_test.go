package hub2promotion

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The Hub 1 native observer writes store and execution evidence beside the
// payouts. The reader skips that evidence and counts only the movements.
func TestReadPayoutRecordsSkipsNativeEvidence(t *testing.T) {
	b := finalBounds{chain: "cosmoshub-1", canonical: 500000}
	base := map[string]any{"height": 500000, "tx": -1, "message": -1, "phase": "zero_height_export", "status": "exported", "code": 0, "address": "", "denom": "", "amount": ""}
	rec := func(extra map[string]any) map[string]any {
		r := map[string]any{}
		for k, v := range base {
			r[k] = v
		}
		for k, v := range extra {
			r[k] = v
		}
		return r
	}
	good := []map[string]any{
		rec(map[string]any{"kind": "native_state_checkpoint", "label": "export/store:bank", "unit": "native_store_bytes", "before": "ab"}),
		rec(map[string]any{"kind": "native_state", "label": "", "unit": "native_store_bytes", "before": "ab", "after": "cd", "validator": "val"}),
		rec(map[string]any{"kind": "native_state_hash", "label": "x", "unit": "native_store_sha256", "before": "ab", "after": "cd"}),
		rec(map[string]any{"kind": "native_execution", "label": "native_withdrawal_result", "unit": "native_execution_evidence", "code": 105, "validator": "val", "delegator": "a"}),
		rec(map[string]any{"kind": "accounting", "label": "delegator_reward", "validator": "val", "delegator": "a"}),
		rec(map[string]any{"kind": "movement", "label": "delegator_reward/bank.add", "unit": "native_bank_tokens", "before": "1", "after": "6", "classification": "reward", "evidence_quality": "native", "validator": "val", "delegator": "a", "denom": "uatom", "amount": "5"}),
		rec(map[string]any{"kind": "movement", "label": "validator_commission/bank.add", "unit": "native_bank_tokens", "validator": "val", "denom": "uatom", "amount": "3"}),
		rec(map[string]any{"kind": "export_completed", "label": ""}),
	}
	read := func(t *testing.T, rows []map[string]any) (map[string]int64, int64, error) {
		dir := t.TempDir()
		var sb strings.Builder
		for _, r := range rows {
			line, e := json.Marshal(r)
			require.NoError(t, e)
			sb.Write(line)
			sb.WriteString("\n")
		}
		records := writePin(t, dir, "records.ndjson", []byte(sb.String()))
		proof, e := json.Marshal(map[string]any{"chain": "cosmoshub-1", "zero_height_export": true, "from": 500000, "through": 500000, "state_hash_verified_through": 500000, "records_sha256": records.SHA256})
		require.NoError(t, e)
		return readPayoutRecords(records, writePin(t, dir, "proof.json", proof), b)
	}

	out, count, e := read(t, good)
	require.NoError(t, e)
	require.Equal(t, int64(2), count)
	require.Len(t, out, 2)

	for name, bad := range map[string]map[string]any{
		"evidence in another unit":       rec(map[string]any{"kind": "native_state", "unit": "native_bank_tokens"}),
		"unknown kind":                   rec(map[string]any{"kind": "native_write", "unit": "native_store_bytes"}),
		"failed movement":                rec(map[string]any{"kind": "movement", "label": "delegator_reward/bank.add", "code": 105, "validator": "val", "delegator": "b", "denom": "uatom", "amount": "5"}),
		"movement in bond tokens":        rec(map[string]any{"kind": "movement", "label": "delegator_reward/bank.add", "unit": "native_bond_tokens", "validator": "val", "delegator": "b", "denom": "uatom", "amount": "5"}),
		"accounting with store evidence": rec(map[string]any{"kind": "accounting", "label": "delegator_reward", "before": "ab"}),
		"unknown field":                  rec(map[string]any{"kind": "native_state", "unit": "native_store_bytes", "extra": "x"}),
		"evidence at another height":     rec(map[string]any{"kind": "native_state", "unit": "native_store_bytes", "height": 499999}),
	} {
		t.Run(name, func(t *testing.T) {
			rows := append(append([]map[string]any{}, good...), bad)
			_, _, e := read(t, rows)
			require.Error(t, e)
		})
	}
}

func writePin(t *testing.T, dir, name string, data []byte) Pin {
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	sum := sha256.Sum256(data)
	return Pin{Path: path, SHA256: hex.EncodeToString(sum[:])}
}
