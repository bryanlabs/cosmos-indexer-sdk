package main

import (
	"encoding/json"
	"github.com/DefiantLabs/cosmos-indexer/legacy/replay033/observeapp"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"os"
	"testing"
)

// Explicit local private inputs only. This test never writes chain or tax state.
func TestHistoricalImmutableCatalog(t *testing.T) {
	root := os.Getenv("HUB1_PRIVATE_CACHE")
	if root == "" {
		t.Skip("private immutable historical inputs not supplied")
	}
	decoder := auth.DefaultTxDecoder(observeapp.MakeCodec())
	counts := map[string]int{}
	first := map[string]int64{}
	txs := 0
	for h := int64(400001); h <= 401000; h++ {
		c := load(root, "cosmoshub-1", h, true)
		if c.err != nil {
			t.Fatal("historical immutable capture failed strict load")
		}
		for _, raw := range c.block.Data.Txs {
			tx, e := decoder(raw)
			if e != nil {
				t.Fatal("native033 transaction decoding failed")
			}
			txs++
			for _, m := range tx.GetMsgs() {
				k := m.Route() + "/" + m.Type()
				counts[k]++
				if first[k] == 0 {
					first[k] = h
				}
			}
		}
	}
	b, e := json.Marshal(struct {
		From, To            int64
		Transactions        int
		Messages            map[string]int
		FirstMessageHeights map[string]int64
	}{400001, 401000, txs, counts, first})
	if e != nil {
		t.Fatal("aggregate catalog failed")
	}
	t.Log(string(b))
}
