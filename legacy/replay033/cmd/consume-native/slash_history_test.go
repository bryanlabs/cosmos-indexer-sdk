package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/secp256k1"
	tm "github.com/tendermint/tendermint/types"
	"os"
	"path/filepath"
	"testing"
)

func TestHistoricalValidatorUpdateDelayAndPin(t *testing.T) {
	pub := secp256k1.GenPrivKeySecp256k1([]byte("history-test")).PubKey()
	first := tm.TM2PB.ValidatorUpdate(tm.NewValidator(pub, 7))
	remove := first
	remove.Power = 0
	type line struct {
		H int64                  `json:"h"`
		U []abci.ValidatorUpdate `json:"u"`
	}
	body, e := json.Marshal(line{H: 0, U: []abci.ValidatorUpdate{first}})
	must(e)
	last, e := json.Marshal(line{H: 10, U: []abci.ValidatorUpdate{remove}})
	must(e)
	body = append(append(body, '\n'), append(last, '\n')...)
	path := filepath.Join(t.TempDir(), "history.jsonl")
	must(os.WriteFile(path, body, 0600))
	sum := sha256.Sum256(body)
	pin := hex.EncodeToString(sum[:])
	if historicalValidators(path, pin, 11).Size() != 1 || historicalValidators(path, pin, 12).Size() != 0 {
		t.Fatal("H+2 validator update delay differs")
	}
	refused(t, func() {
		historicalValidators(path, "0000000000000000000000000000000000000000000000000000000000000000", 11)
	})
}
