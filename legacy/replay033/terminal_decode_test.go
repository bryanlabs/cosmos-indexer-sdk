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

// Decode only the NEW untouched-Gaia fifteen-height execution, not the observer.
// The old 400000..400026 decoder and acceptance test remain byte-unchanged.
func TestTerminalNativeAuditDecode(t *testing.T) {
	dir := os.Getenv("HUB1_TERMINAL_AUDIT")
	if dir == "" {
		t.Skip("explicit new terminal execution required")
	}
	in, e := os.Open(filepath.Join(dir, "native-audit.ndjson"))
	if e != nil {
		t.Fatal("native audit unavailable")
	}
	defer in.Close()
	baseline, e := os.ReadFile(filepath.Join(dir, "native-audit.ndjson.source-checkpoint.json"))
	if e != nil {
		t.Fatal("source audit unavailable")
	}
	out, e := os.OpenFile(filepath.Join(dir, "native-all-store-values.ndjson"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal("fresh decoder output required")
	}
	defer out.Close()
	reader := bufio.NewReader(io.MultiReader(bytes.NewReader(baseline), in))
	count := 0
	for {
		raw, e := reader.ReadBytes('\n')
		if e == io.EOF && len(raw) == 0 {
			break
		}
		if e != nil {
			t.Fatal("incomplete audit")
		}
		var r struct {
			Height int64                 `json:"height"`
			Stores []nativeStoreEvidence `json:"all_persistent_stores"`
		}
		if json.Unmarshal(raw, &r) != nil || r.Height != 500000+int64(count) || len(r.Stores) != 9 {
			t.Fatal("complete terminal audit scope differs")
		}
		type decoded struct {
			Name   string       `json:"name"`
			Values []sdk.KVPair `json:"values"`
		}
		names := map[string]bool{"main": true, "acc": true, "staking": true, "slashing": true, "mint": true, "distr": true, "gov": true, "fee": true, "params": true}
		stores := make([]decoded, 0, 9)
		for _, s := range r.Stores {
			if !names[s.Name] || len(s.Partitions) != 256 {
				t.Fatal("complete raw store scope differs")
			}
			delete(names, s.Name)
			values := make([]sdk.KVPair, 0)
			for p, q := range s.Partitions {
				if q.Code != 0 || q.Height != r.Height || !bytes.Equal(q.Key, []byte{byte(p)}) {
					t.Fatal("native query identity differs")
				}
				var pairs []sdk.KVPair
				if cdc.UnmarshalBinaryLengthPrefixed(q.Value, &pairs) != nil {
					t.Fatal("native amino decode failed")
				}
				for _, kv := range pairs {
					if len(kv.Key) == 0 || kv.Key[0] != byte(p) || (len(values) > 0 && bytes.Compare(values[len(values)-1].Key, kv.Key) >= 0) {
						t.Fatal("full raw native ordering/prefix differs")
					}
					values = append(values, kv)
				}
			}
			stores = append(stores, decoded{s.Name, values})
		}
		if json.NewEncoder(out).Encode(struct {
			Height int64     `json:"height"`
			Stores []decoded `json:"stores"`
		}{r.Height, stores}) != nil {
			t.Fatal("raw decoder write failed")
		}
		count++
	}
	if count != 16 || out.Sync() != nil {
		t.Fatal("all sixteen source/terminal states required")
	}
}

// Keep a compile-time assertion that prefix evidence remains native ABCI.
var _ = abci.ResponseQuery{}
