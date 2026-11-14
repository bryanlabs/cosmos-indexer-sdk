package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	rr "github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
)

func TestPrivateGasGateStillRequiresActualFinancialMovements(t *testing.T) {
	path := os.Getenv("EXACT_GAS_PRIVATE_PROFILE")
	root := os.Getenv("EXACT_GAS_PRIVATE_CACHE")
	if path == "" || root == "" {
		t.Skip("private retained inputs not supplied")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal("profile unreadable")
	}
	gas, e := rr.LoadApprovedGasProfile(bytes.NewReader(raw))
	if e != nil {
		t.Fatal("profile refused")
	}
	var input struct {
		Markers   []byte          `json:"original_marker_bytes"`
		Recording rr.GasRecording `json:"financial_recording_lineage"`
	}
	if json.Unmarshal(raw, &input) != nil {
		t.Fatal("profile shape")
	}
	dir, e := os.MkdirTemp("", "gas-gate-private-retained-")
	if e != nil {
		t.Fatal("private directory")
	}
	differences := filepath.Join(dir, "differences.txt")
	if os.WriteFile(differences, input.Markers, 0600) != nil {
		t.Fatal("private evidence")
	}
	p := rr.Proof{Chain: "cosmoshub-2", Release: rr.Release034, From: 400001, Through: 482090, VerifiedThrough: 482090, ResultDifferencesSHA256: "2ff60b284b334e2b29ff2523a9ab7d961a6ed2ec20f29d2976a294bcc6fb36a6", FinancialRecording: &input.Recording}
	status := replayStatus{Chain: "cosmoshub-2", Release: rr.Release034, From: 400001, Committed: 482090, Verified: 482090, Diffs: 3387}
	beforeStatus := status
	if admitStatusGas(&p, status, differences, gas) != nil || p.GasDiagnostic == nil || len(p.GasDiagnostic.Diagnostics) != 32 {
		t.Fatal("exact gas evidence not admitted at difference gate")
	}
	if status != beforeStatus {
		t.Fatal("status overwritten")
	}
	roundtrip, _ := json.Marshal(p)
	var restored rr.Proof
	_ = json.Unmarshal(roundtrip, &restored)
	if admitStatusGas(&restored, status, differences, gas) != nil {
		t.Fatal("receipt does not roundtrip")
	}
	if admitStatus(&restored, status, differences) == nil {
		t.Fatal("receipt bypassed strict default")
	}
	restored.GasDiagnostic.Diagnostics[0].NativeGasUsed++
	if admitStatusGas(&restored, status, differences, gas) == nil {
		t.Fatal("mutated diagnostic admitted")
	}
	if gas.CheckCachedFiles(root, 479755) != nil {
		t.Fatal("retained full envelopes do not match")
	}
	h, e := capture.LoadHeight(root, "cosmoshub-2", 479755, 16<<20)
	if e != nil {
		t.Fatal("retained cached inputs unreadable")
	}
	txs := make([][]byte, len(h.Block.Block.Txs))
	for i, x := range h.Block.Block.Txs {
		txs[i] = x
	}
	original := bytes.Clone(h.Results.Legacy.RawJSON)
	used := h.Results.TxsResults[0].GasUsed
	if gas.CheckCachedInputs(479755, txs, original) != nil {
		t.Fatal("original cached input refused")
	}
	changed := append(bytes.Clone(original), ' ')
	if gas.CheckCachedInputs(479755, txs, changed) == nil {
		t.Fatal("rebuilt result envelope accepted")
	}
	changedTxs := append([][]byte(nil), txs...)
	changedTxs[0] = append(bytes.Clone(txs[0]), 0)
	if gas.CheckCachedInputs(479755, changedTxs, original) == nil {
		t.Fatal("changed signed transaction accepted")
	}
	// All height completion markers exist, but this actual known successful
	// transaction has no native ante/message/fee/principal movements. Exercise
	// the SAME per-height function used by run(), with and without the profile.
	incomplete := rr.Height{Number: 479755, Records: []rr.Record{
		{Kind: "accounting", Height: 479755, Tx: -1, Message: -1, Phase: "begin_block", Status: "committed"},
		{Kind: "accounting", Height: 479755, Tx: -1, Message: -1, Phase: "end_block", Status: "committed"},
		{Kind: "height_completed", Height: 479755, Tx: -1, Message: -1, Phase: "block", Status: "committed"},
	}}
	baseline, ordinaryErr := reconcileReplayHeight(h, incomplete, nil)
	profiled, gasErr := reconcileReplayHeight(h, incomplete, gas)
	if ordinaryErr == nil || gasErr == nil || ordinaryErr != gasErr || !reflect.DeepEqual(baseline, profiled) {
		t.Fatal("valid gas profile bypassed ordinary missing-financial-movement refusal")
	}
	if h.Results.TxsResults[0].GasUsed != used || !bytes.Equal(h.Results.Legacy.RawJSON, original) || !reflect.DeepEqual(status, beforeStatus) {
		t.Fatal("cached canonical metadata changed")
	}
	retained, e := os.ReadFile(differences)
	if e != nil || !bytes.Equal(retained, input.Markers) {
		t.Fatal("raw discrepancy bytes changed")
	}
}
