package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rr "github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
)

func windowFixture(t *testing.T) (map[string]string, string, string, rr.Proof, string) {
	t.Helper()
	dir := t.TempDir()
	paths := map[string]string{}
	var prefix, selected bytes.Buffer
	for h := int64(1); h <= 3; h++ {
		var records []rr.Record
		records = append(records, rr.Record{Kind: "accounting", Height: h, Tx: -1, Message: -1, Phase: "begin_block", Status: "committed"})
		if h == 2 {
			records = append(records, rr.Record{Kind: "movement", Height: h, Tx: 0, Message: -1, Phase: "ante", Label: "ante_fee", Status: "committed", Address: "fixture", Denom: "asset", Amount: "-1"}, rr.Record{Kind: "principal", Height: h, Tx: 0, Message: 0, Phase: "message", Status: "aborted", Address: "fixture", Validator: "fixture", Delegator: "fixture", Denom: "asset", Amount: "0"}, rr.Record{Kind: "movement", Height: h, Tx: 0, Message: 0, Phase: "message", Status: "aborted", Address: "fixture", Denom: "other_asset", Amount: "2"})
		}
		records = append(records, rr.Record{Kind: "accounting", Height: h, Tx: -1, Message: -1, Phase: "end_block", Status: "committed"}, rr.Record{Kind: "height_completed", Height: h, Tx: -1, Message: -1, Phase: "block", Status: "committed"})
		for _, r := range records {
			raw, _ := json.Marshal(r)
			raw = append(raw, '\n')
			prefix.Write(raw)
			if h >= 2 {
				selected.Write(raw)
			}
		}
	}
	for kind, body := range map[string][]byte{"movements": prefix.Bytes(), "validators": []byte("{\"h\":0,\"u\":[{}]}\n{\"h\":2,\"u\":[{}]}\n"), "differences": []byte("1 tx:0:tags\n2 tx:0:tags\n")} {
		paths[kind] = filepath.Join(dir, kind)
		if os.WriteFile(paths[kind], body, 0600) != nil {
			t.Fatal("fixture write")
		}
	}
	records := filepath.Join(dir, "selected")
	differences := filepath.Join(dir, "selected-differences")
	os.WriteFile(records, selected.Bytes(), 0600)
	os.WriteFile(differences, []byte("2 tx:0:tags\n"), 0600)
	p := rr.Proof{Chain: "cosmoshub-2", Release: rr.Release034, From: 2, Through: 3, VerifiedThrough: 3, SHA256: windowHash(selected.Bytes()), ResultDifferencesSHA256: windowHash([]byte("2 tx:0:tags\n"))}
	return paths, records, differences, p, windowHash(prefix.Bytes())
}
func TestSealedWindowStreamsLosslessAndRefuse(t *testing.T) {
	paths, records, diffs, p, digest := windowFixture(t)
	if n, e := validateWindowStreams(paths, records, diffs, p, digest); e != nil || n != 1 {
		t.Fatal("complete fixture refused")
	}
	for _, name := range []string{"missing-genesis-context", "missing-height", "missing-end", "duplicate-height", "reordered-height", "partial-movement", "dropped-aborted", "changed-asset", "changed-principal", "extra-marker", "reordered-markers", "missing-validator-genesis", "reordered-validator", "null-validator", "unknown-validator", "partial-validator", "partial-marker", "partial-marker-after-window", "wrong-prefix-hash"} {
		t.Run(name, func(t *testing.T) {
			paths, records, diffs, p, digest := windowFixture(t)
			target := paths["movements"]
			raw, _ := os.ReadFile(target)
			switch name {
			case "missing-genesis-context":
				raw = bytes.Join(bytes.Split(raw, []byte{'\n'})[3:], []byte{'\n'})
			case "missing-height":
				raw = bytes.ReplaceAll(raw, []byte(`"height":2`), []byte(`"height":3`))
			case "missing-end":
				raw = bytes.Replace(raw, []byte(`"phase":"end_block"`), []byte(`"phase":"begin_block"`), 1)
			case "duplicate-height":
				raw = append(bytes.Clone(raw), raw...)
			case "reordered-height":
				raw = bytes.Replace(raw, []byte(`"height":1`), []byte(`"height":3`), 1)
			case "partial-movement":
				raw = raw[:len(raw)-1]
			case "dropped-aborted", "changed-asset", "changed-principal":
				target = records
				raw, _ = os.ReadFile(target)
				switch name {
				case "dropped-aborted":
					var kept [][]byte
					for _, line := range bytes.SplitAfter(raw, []byte{'\n'}) {
						if !bytes.Contains(line, []byte(`"status":"aborted"`)) {
							kept = append(kept, line)
						}
					}
					raw = bytes.Join(kept, nil)
				case "changed-asset":
					raw = bytes.ReplaceAll(raw, []byte("other_asset"), []byte("asset"))
				case "changed-principal":
					raw = bytes.Replace(raw, []byte(`"amount":"0"`), []byte(`"amount":"3"`), 1)
				}
				p.SHA256 = windowHash(raw)
			case "extra-marker":
				target = diffs
				raw = []byte("2 tx:0:tags\n3 tx:0:tags\n")
				p.ResultDifferencesSHA256 = windowHash(raw)
			case "reordered-markers":
				target = paths["differences"]
				raw = []byte("2 tx:0:tags\n1 tx:0:tags\n")
			case "partial-marker":
				target = paths["differences"]
				raw = []byte("1 tx:0:tags\n2 tx:0:tags")
			case "partial-marker-after-window":
				target = paths["differences"]
				raw = []byte("1 tx:0:tags\n2 tx:0:tags\n4 tx")
			case "partial-validator":
				target = paths["validators"]
				raw = []byte("{\"h\":0,\"u\":[{}]}")
			case "missing-validator-genesis":
				target = paths["validators"]
				raw = []byte("{\"h\":2,\"u\":[{}]}\n")
			case "reordered-validator":
				target = paths["validators"]
				raw = []byte("{\"h\":0,\"u\":[{}]}\n{\"h\":0,\"u\":[{}]}\n")
			case "null-validator":
				target = paths["validators"]
				raw = []byte("{\"h\":0,\"u\":null}\n")
			case "unknown-validator":
				target = paths["validators"]
				raw = []byte("{\"h\":0,\"u\":[{}],\"ignored\":1}\n")
			case "wrong-prefix-hash":
				digest = strings.Repeat("0", 64)
			}
			if os.WriteFile(target, raw, 0600) != nil {
				t.Fatal("fixture write")
			}
			if target == paths["movements"] && name != "wrong-prefix-hash" {
				digest = windowHash(raw)
			}
			if _, e := validateWindowStreams(paths, records, diffs, p, digest); e == nil {
				t.Fatal("incomplete/mutated evidence admitted")
			}
		})
	}
}
func TestSealedWindowPrivatePrefixBinding(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("private fixture\n")
	path := filepath.Join(dir, "run.prefix.log")
	os.WriteFile(path, raw, 0600)
	good := sealedFile{Name: "run.prefix.log", SHA: windowHash(raw), Bytes: int64(len(raw)), Inode: 3, Before: 100, After: 110, RemoteBefore: windowHash(raw), RemoteAfter: windowHash(raw)}
	if _, e := boundFile(dir, "log", good); e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"hash", "truncated", "changed-inode", "replaced-prefix", "unsafe-path", "missing-file", "public-permissions", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			x := good
			dir := t.TempDir()
			path := filepath.Join(dir, x.Name)
			os.WriteFile(path, raw, 0600)
			switch kind {
			case "hash":
				x.SHA = strings.Repeat("0", 64)
			case "truncated":
				x.After = x.Before - 1
			case "changed-inode":
				x.Inode = 0
			case "replaced-prefix":
				x.RemoteAfter = strings.Repeat("0", 64)
			case "unsafe-path":
				x.Name = "../run.prefix.log"
			case "missing-file":
				x.Name = "absent"
			case "public-permissions":
				os.Chmod(path, 0644)
			case "symlink":
				other := filepath.Join(dir, "other")
				os.WriteFile(other, raw, 0600)
				os.Rename(path, filepath.Join(dir, "preserved"))
				os.Symlink(other, path)
			}
			if _, e := boundFile(dir, "log", x); e == nil {
				t.Fatal("unsafe prefix binding admitted")
			}
		})
	}
}
func TestSealedWindowReceiptStrictJSON(t *testing.T) {
	var v struct {
		Kind string `json:"kind"`
	}
	if strictWindowJSON([]byte(`{"kind":"ok"}`), &v) != nil {
		t.Fatal("baseline")
	}
	for _, raw := range []string{`{"kind":"ok","kind":"evil"}`, `{"kind":"ok","Kind":"evil"}`, `{"kind":"ok","Kind":"evil"}`, `{"kind":"ok","unknown":1}`, `{"kind":"ok"} {}`, `{"kind":NaN}`} {
		if strictWindowJSON([]byte(raw), &v) == nil {
			t.Fatal("ambiguous receipt admitted")
		}
	}
}
func TestSealedWindowProductionSizeManifest(t *testing.T) {
	members := make(map[string]string, 164184)
	for h := 400000; h <= 482091; h++ {
		for _, kind := range []string{"block", "block_results"} {
			members[fmt.Sprintf("cosmoshub-2/%d/%d-%s.raw.gz", h/10000, h, kind)] = strings.Repeat("0", 64)
		}
	}
	raw, err := json.Marshal(members)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	var decoded map[string]string
	if strictWindowJSON(raw, &decoded) != nil || len(decoded) != 164184 {
		t.Fatal("complete production-size manifest refused")
	}
	t.Logf("all164184 keys decoded without quadratic pair comparisons in %s", time.Since(started))
}

func TestSealedWindowPinnedScopeRefusesInventedCompletion(t *testing.T) {
	good := windowReceipt{Version: "hub2-sealed-window-v1", Job: windowJob, UID: windowUID, PodUID: "fixture", Image: rr.GasFinancialImage, Source: rr.GasFinancialSource, Target: 2902000, From: 400001, Through: 482090, Component: "per-complete-height-financial-reconciliation-not-chain-books-acceptance", Files: map[string]sealedFile{"movements": {}, "validators": {}, "differences": {}, "status": {}, "log": {}}}
	p := rr.Proof{From: 400001, Through: 482090, VerifiedThrough: 482090}
	if validateWindowScope(good, p) != nil {
		t.Fatal("baseline refused")
	}
	for _, name := range []string{"version", "job", "uid", "pod", "image", "source", "target", "from", "through", "complete", "component", "streams", "proof-from", "proof-through", "proof-verified", "export"} {
		t.Run(name, func(t *testing.T) {
			w, q := good, p
			switch name {
			case "version":
				w.Version = "v2"
			case "job":
				w.Job = "other"
			case "uid":
				w.UID = "other"
			case "pod":
				w.PodUID = ""
			case "image":
				w.Image = rr.GasDiagnosticImage
			case "source":
				w.Source = rr.GasDiagnosticSource
			case "target":
				w.Target = 482090
			case "from":
				w.From--
			case "through":
				w.Through++
			case "complete":
				w.JobComplete = true
			case "component":
				w.Component = "whole-chain-accepted"
			case "streams":
				w.Files = map[string]sealedFile{}
			case "proof-from":
				q.From--
			case "proof-through":
				q.Through++
			case "proof-verified":
				q.VerifiedThrough++
			case "export":
				q.Export = true
			}
			if validateWindowScope(w, q) == nil {
				t.Fatal("mutation admitted")
			}
		})
	}
	// Ordinary completion validation still refuses the REAL longer status,
	// independently of discrepancy counts. Only explicit window mode projects it.
	s := replayStatus{Chain: "cosmoshub-2", Release: rr.Release034, From: 1, Committed: 500000, Verified: 499999}
	if verifyStatus(p, s) == nil {
		t.Fatal("ordinary status silently projected")
	}
}

func TestSealedWindowOriginalStatusAndDurableReport(t *testing.T) {
	good := map[string]any{"release": rr.Release034, "chain": "cosmoshub-2", "started": "2026-11-13T23:12:03Z", "updated": "2026-11-14T03:35:00Z", "first_height_this_run": 1, "last_height_committed": 482091, "state_hash_verified_through": 482090, "target": 2902000}
	log := []byte("2026-11-14T03:35:00Z" + originalReport(482091, 482090) + "30 run 30 txs 1 result-diffs 1 evidence 0 \n")
	raw, _ := json.Marshal(good)
	if validateOriginalWindow(raw, log) != nil {
		t.Fatal("baseline refused")
	}
	for key, value := range map[string]any{"release": "latest", "chain": "cosmoshub-1", "started": "invalid", "updated": "2026-11-12T00:00:00Z", "first_height_this_run": 400001, "last_height_committed": 482090, "state_hash_verified_through": 482089, "target": 482090, "stop_reason": "failed", "transactions_this_run": -1, "heights_where_results_differ_from_cache": -1, "evidence_items_this_run": -1, "unexpected": true} {
		t.Run(key, func(t *testing.T) {
			q := map[string]any{}
			for k, v := range good {
				q[k] = v
			}
			q[key] = value
			raw, _ := json.Marshal(q)
			if validateOriginalWindow(raw, log) == nil {
				t.Fatal("mutated original status admitted")
			}
		})
	}
	for _, broken := range [][]byte{nil, []byte("2026-11-14T03:35:00Z height 482090 verified 482089 rate 30\n"), log[:len(log)-1], bytes.Replace(log, []byte("03:35:00Z"), []byte("03:34:30Z"), 1)} {
		if validateOriginalWindow(raw, broken) == nil {
			t.Fatal("missing/wrong durable report admitted")
		}
	}
	duplicate := append(bytes.Clone(raw[:len(raw)-1]), []byte(`,"target":482090}`)...)
	if validateOriginalWindow(duplicate, log) == nil {
		t.Fatal("duplicate original status key admitted")
	}
}
