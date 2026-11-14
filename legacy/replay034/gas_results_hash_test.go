package main

import (
	"bytes"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	abci "github.com/tendermint/tendermint/abci/types"
	cmn "github.com/tendermint/tendermint/libs/common"
	tm "github.com/tendermint/tendermint/types"
)

func TestHistorical031ResultsHashExcludesGasAndTagsButComparatorDoesNot(t *testing.T) {
	cached := &abci.ResponseDeliverTx{Code: 0, Data: []byte{1}, GasWanted: 200000, GasUsed: 114833, Log: "success", Tags: []cmn.KVPair{{Key: []byte("action"), Value: []byte("undelegate")}}}
	reference := []*abci.ResponseDeliverTx{cached, {Code: 1, Data: []byte{2}}}
	root := tm.NewResults(reference).Hash()
	for _, name := range []string{"gas", "tags", "log", "info", "codespace", "code", "data"} {
		t.Run(name, func(t *testing.T) {
			changed := *cached
			switch name {
			case "gas":
				changed.GasUsed += 1021
			case "tags":
				changed.Tags = []cmn.KVPair{{Key: []byte("action"), Value: []byte("changed")}}
			case "log":
				changed.Log = "changed"
			case "info":
				changed.Info = "changed"
			case "codespace":
				changed.Codespace = "changed"
			case "code":
				changed.Code = 1
			case "data":
				changed.Data = []byte{9}
			}
			native := []*abci.ResponseDeliverTx{&changed, reference[1]}
			equal := bytes.Equal(root, tm.NewResults(native).Hash())
			consensus := name == "code" || name == "data"
			if equal == consensus {
				t.Fatal("wrong historical Code/Data-only root behavior")
			}
			// This original signed comparator covers gas/tags/log/Code/Data, NOT Info
			// or Codespace. The new exact-profile non-gas guard separately tests both
			// constructor-constrained fields. Do not invent old comparator coverage.
			if name != "info" && name != "codespace" && differ(&cachedResults{deliver: reference}, abci.ResponseBeginBlock{}, native, abci.ResponseEndBlock{}) == "" {
				t.Fatal("independent comparator ignored excluded field")
			}
		})
	}
	if bytes.Equal(root, tm.NewResults([]*abci.ResponseDeliverTx{reference[1], reference[0]}).Hash()) {
		t.Fatal("Code/Data result order ignored")
	}
}
func TestFiniteGas1021CanChangeTransactionAndBlockOutcome(t *testing.T) {
	panics := func(f func()) (panicked bool) { defer func() { panicked = recover() != nil }(); f(); return }
	tx := sdk.NewGasMeter(1020)
	block := sdk.NewGasMeter(2000)
	block.ConsumeGas(1000, "previous transaction")
	if !panics(func() { tx.ConsumeGas(1021, "parameter Get") }) || !panics(func() { block.ConsumeGas(1021, "parameter Get") }) {
		t.Fatal("universal harmless-gas assumption")
	}
}
