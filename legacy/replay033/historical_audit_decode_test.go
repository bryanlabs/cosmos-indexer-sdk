package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	sdk "github.com/cosmos/cosmos-sdk/types"
	abci "github.com/tendermint/tendermint/abci/types"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Independently decode the untouched native Gaia ALL-store query replies with
// the pinned native amino codec. Retain every key/value and every store, never
// a financial-field projection. Original raw replies stay unchanged.
func TestHistoricalNativeAuditDecode(t *testing.T) {
	dir := os.Getenv("HUB1_PRIVATE_AUDIT_DECODE")
	if dir == "" {
		t.Skip("explicit private native audit not supplied")
	}
	in, e := os.Open(filepath.Join(dir, "native-audit.ndjson"))
	if e != nil {
		t.Fatal("native audit unavailable")
	}
	defer in.Close()
	out, e := os.OpenFile(filepath.Join(dir, "native-all-store-values.ndjson"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal("native decoded evidence must be exclusive")
	}
	defer out.Close()
	baseline, e := os.ReadFile(filepath.Join(dir, "native-audit.ndjson.source-checkpoint.json"))
	if e != nil {
		t.Fatal("native source checkpoint audit unavailable")
	}
	reader := bufio.NewReader(io.MultiReader(bytes.NewReader(baseline), in))
	count := 0
	for {
		line, e := reader.ReadBytes('\n')
		if e == io.EOF && len(line) == 0 {
			break
		}
		if e != nil {
			t.Fatal("native audit incomplete")
		}
		var record struct {
			Height int64 `json:"height"`
			Stores []struct {
				Name       string               `json:"name"`
				Partitions []abci.ResponseQuery `json:"partitions"`
			} `json:"all_persistent_stores"`
		}
		if json.Unmarshal(line, &record) != nil || record.Height != 400000+int64(count) || len(record.Stores) != 9 {
			t.Fatal("native whole-state scope differs")
		}
		type store struct {
			Name   string       `json:"name"`
			Values []sdk.KVPair `json:"values"`
		}
		stores := make([]store, 0, 9)
		names := map[string]bool{}
		for _, s := range record.Stores {
			if names[s.Name] || len(s.Partitions) != 256 {
				t.Fatal("native all-store exhaustive prefix scope differs")
			}
			names[s.Name] = true
			values := make([]sdk.KVPair, 0)
			for p, r := range s.Partitions {
				if r.Code != 0 || r.Height != record.Height || len(r.Key) != 1 || r.Key[0] != byte(p) {
					t.Fatal("native query identity differs")
				}
				var entries []sdk.KVPair
				if cdc.UnmarshalBinaryLengthPrefixed(r.Value, &entries) != nil {
					t.Fatal("native store amino response invalid")
				}
				for _, kv := range entries {
					if len(kv.Key) == 0 || kv.Key[0] != byte(p) || (len(values) > 0 && bytes.Compare(values[len(values)-1].Key, kv.Key) >= 0) {
						t.Fatal("native exhaustive raw keys invalid")
					}
					values = append(values, kv)
				}
			}
			stores = append(stores, store{s.Name, values})
		}
		if json.NewEncoder(out).Encode(struct {
			Height int64   `json:"height"`
			Stores []store `json:"stores"`
		}{record.Height, stores}) != nil {
			t.Fatal("native complete decoded evidence failed")
		}
		count++
	}
	if count != 27 || out.Sync() != nil {
		t.Fatal("native whole segment must be complete")
	}
}
