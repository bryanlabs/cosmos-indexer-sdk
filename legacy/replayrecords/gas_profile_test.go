package replayrecords

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestGasCanonicalInt64Lexemes(t *testing.T) {
	for _, raw := range []string{`0`, `1`, `"1"`, `-1`, `"-1"`, `9223372036854775807`, `"-9223372036854775808"`} {
		var n GasInt
		if strictGasJSON([]byte(raw), &n) != nil {
			t.Fatalf("valid canonical integer refused")
		}
	}
	for _, raw := range []string{`-0`, `"-0"`, `01`, `"01"`, `+1`, `"+1"`, `1e0`, `"1e0"`, `1.0`, `"1.0"`, `true`, `false`, `null`, `9223372036854775808`, `-9223372036854775809`, `" 1"`, `"1 "`} {
		var n GasInt
		if strictGasJSON([]byte(raw), &n) == nil {
			t.Fatalf("invalid integer accepted: %s", raw)
		}
	}
	var alias struct {
		S GasInt `json:"s"`
		K GasInt `json:"k"`
	}
	for _, raw := range []string{`{"s":1,"ſ":2}`, `{"k":1,"K":2}`} {
		if strictGasJSON([]byte(raw), &alias) == nil {
			t.Fatal("Unicode case-alias duplicate accepted")
		}
	}
	var v struct {
		N GasInt `json:"n"`
	}
	for _, raw := range []string{`{"n":1,"n":1}`, `{"n":1,"N":2}`, `{"n":1,"unknown":1}`, `{"n":1} {"n":1}`, `{"n":-0}`, `{"n":1e0}`} {
		if strictGasJSON([]byte(raw), &v) == nil {
			t.Fatal("ambiguous JSON accepted")
		}
	}
}
func TestGasSemanticPairNegatives(t *testing.T) {
	good := gasPair{479755, 0, strings.Repeat("a", 64), 0, 0, 200000, 200000, 115854, 114833}
	if validateGasPair(good, 200000) != nil {
		t.Fatal("positive pair refused")
	}
	tests := map[string]func(*gasPair){
		"native-code": func(p *gasPair) { p.NativeCode = 1 }, "cached-code": func(p *gasPair) { p.CachedCode = 1 },
		"delta": func(p *gasPair) { p.NativeUsed++ }, "cached-used": func(p *gasPair) { p.CachedUsed++ },
		"wanted": func(p *gasPair) { p.NativeWanted++ }, "cache-wanted": func(p *gasPair) { p.CachedWanted++ },
		"near-tx-limit":   func(p *gasPair) { p.NativeUsed = 199999; p.CachedUsed = 198978 },
		"late-out-of-gas": func(p *gasPair) { p.NativeUsed = 200001; p.CachedUsed = 198980 },
		"unknown-height":  func(p *gasPair) { p.Height = 482100 }, "negative-index": func(p *gasPair) { p.Index = -1 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			p := good
			mutate(&p)
			if validateGasPair(p, 200000) == nil {
				t.Fatal("semantic pair guard did not refuse")
			}
		})
	}
	if validateGasPair(good, 200001) == nil || validateGasPair(good, 1<<63) == nil {
		t.Fatal("signed gas guard bypassed")
	}
}
func TestGasIndependentNonGasGuard(t *testing.T) {
	good := gasResponse{Code: 0, Data: []byte{1, 2}, Log: "success", Wanted: 200000, Used: 114833, Tags: []gasTag{{[]byte("action"), []byte("undelegate")}, {[]byte("id"), []byte("synthetic")}}}
	clone := func() gasResponse {
		raw, _ := json.Marshal(good)
		var x gasResponse
		if strictGasJSON(raw, &x) != nil {
			t.Fatal("fixture")
		}
		return x
	}
	native := clone()
	native.Used += 1021
	root, err := historicalGasResultsRoot([]gasResponse{good})
	if err != nil {
		t.Fatal("historical projection failed")
	}
	gasRoot, err := historicalGasResultsRoot([]gasResponse{native})
	if err != nil || gasRoot != root {
		t.Fatal("historical gas exclusion changed")
	}
	tagMutation := clone()
	tagMutation.Tags[0].Value[0]++
	tagRoot, err := historicalGasResultsRoot([]gasResponse{tagMutation})
	if err != nil || tagRoot != root || validateNonGas(good, tagMutation) == nil {
		t.Fatal("ResultsHash substituted for independent tag semantics")
	}
	if validateNonGas(good, native) != nil {
		t.Fatal("only GasUsed should be excluded")
	}
	mutations := map[string]func(*gasResponse){"code": func(x *gasResponse) { x.Code = 1 }, "data": func(x *gasResponse) { x.Data[0]++ }, "log": func(x *gasResponse) { x.Log += "changed" }, "info": func(x *gasResponse) { x.Info = "changed" }, "codespace": func(x *gasResponse) { x.Codespace = "changed" }, "wanted": func(x *gasResponse) { x.Wanted++ }, "tag-key": func(x *gasResponse) { x.Tags[0].Key[0]++ }, "tag-value": func(x *gasResponse) { x.Tags[0].Value[0]++ }, "tag-order": func(x *gasResponse) { x.Tags[0], x.Tags[1] = x.Tags[1], x.Tags[0] }, "extra-tag": func(x *gasResponse) { x.Tags = append(x.Tags, x.Tags[0]) }, "missing-tag": func(x *gasResponse) { x.Tags = x.Tags[1:] }}
	for name, f := range mutations {
		t.Run(name, func(t *testing.T) {
			x := clone()
			f(&x)
			if validateNonGas(good, x) == nil {
				t.Fatal("independent non-gas guard did not refuse")
			}
		})
	}
	raw, _ := json.Marshal(good)
	raw = bytes.Replace(raw, []byte(`"code":0`), []byte(`"unknown":1,"code":0`), 1)
	var x gasResponse
	if strictGasJSON(raw, &x) == nil {
		t.Fatal("unknown response field accepted")
	}
}

// Opt-in private integration uses the actual approved retained inputs. No
// identities, addresses, quantities, raw bytes or private paths are logged.
func privateGas(t *testing.T) ([]byte, *ApprovedGasProfile) {
	t.Helper()
	path := os.Getenv("EXACT_GAS_PRIVATE_PROFILE")
	if path == "" {
		t.Skip("private profile not supplied")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal("private profile unreadable")
	}
	p, e := LoadApprovedGasProfile(bytes.NewReader(raw))
	if e != nil {
		t.Fatal("approved retained-input profile refused")
	}
	return raw, p
}
func TestPrivateExactGas32Profile(t *testing.T) {
	raw, p := privateGas(t)
	if len(p.profile.Cases) != 32 || len(p.profile.Blocks) != 31 {
		t.Fatal("incomplete profile")
	}
	if _, e := LoadApprovedGasProfile(bytes.NewReader(append(bytes.Clone(raw), ' '))); e == nil {
		t.Fatal("untrusted arbitrary profile accepted")
	}
	a, g, e := AdmitDifferences(bytes.NewReader(p.profile.RawMarkers), gasMarkerSHA, 400001, 482090, 3387, p, &p.profile.Recording)
	if e != nil || a.RunLines != 3387 || len(g.Diagnostics) != 32 || g.NativeRawRetained {
		t.Fatal("bounded source-attested diagnostic admission failed")
	}
	if _, _, e = AdmitDifferences(bytes.NewReader(p.profile.RawMarkers), gasMarkerSHA, 400001, 482090, 3387, nil, nil); e == nil {
		t.Fatal("default became permissive")
	}
	if _, _, e = AdmitDifferences(bytes.NewReader(p.profile.RawMarkers), gasMarkerSHA, 400001, 482090, 3387, p, nil); e == nil {
		t.Fatal("unattributed recording accepted")
	}
	badRecording := p.profile.Recording
	badRecording.Image = GasDiagnosticImage
	if _, _, e = AdmitDifferences(bytes.NewReader(p.profile.RawMarkers), gasMarkerSHA, 400001, 482090, 3387, p, &badRecording); e == nil {
		t.Fatal("diagnostic runtime misrepresented as observer")
	}
	before := bytes.Clone(raw)
	d := p.Diagnostic()
	d.Diagnostics[0].NativeGasUsed++
	after, _ := os.ReadFile(os.Getenv("EXACT_GAS_PRIVATE_PROFILE"))
	if !bytes.Equal(before, after) || reflect.DeepEqual(d, p.Diagnostic()) {
		t.Fatal("mutable diagnostic leaked into source")
	}
	// Structural complete-height guard only, not a financial reconciliation proof.
	proof := Proof{Chain: "cosmoshub-2", Release: Release034, From: 400001, Through: 482090, VerifiedThrough: 482090, SHA256: gasSHA(nil), GasDiagnostic: g, FinancialRecording: &p.profile.Recording}
	if Stream(bytes.NewReader(nil), proof, func(Height) error { t.Fatal("missing movement input reached callback"); return nil }) == nil {
		t.Fatal("missing financial records admitted")
	}
}
func TestPrivateExactGasSemanticNegatives(t *testing.T) {
	raw, _ := privateGas(t)
	// Prove the unmodified reserialized, hash-rebound baseline succeeds first.
	// Otherwise a serialization-only failure could make every negative vacuous.
	var baseline gasProfile
	if strictGasJSON(raw, &baseline) != nil {
		t.Fatal("baseline decode")
	}
	reserialized, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal("baseline serialization")
	}
	if _, err := loadGasProfile(reserialized, gasSHA(reserialized)); err != nil {
		t.Fatal("hash-rebound unmodified baseline refused")
	}
	// Rebind ONLY the test loader's hash to each mutated fixture so source,
	// completion, field and identity validators really execute beyond SHA checks.
	tests := map[string]func(*gasProfile){
		"chain": func(p *gasProfile) { p.Chain = "cosmoshub-3" }, "release": func(p *gasProfile) { p.Release += "future" }, "source": func(p *gasProfile) { p.Source = GasFinancialSource }, "image": func(p *gasProfile) { p.Image = GasFinancialImage },
		"financial-source": func(p *gasProfile) { p.Recording.Source = p.Source }, "financial-image": func(p *gasProfile) { p.Recording.Image = p.Image }, "observer-package": func(p *gasProfile) { p.Recording.ObserverTree = "future" }, "sdk-mod": func(p *gasProfile) { p.Recording.SDKMod = "future" }, "sdk-sum": func(p *gasProfile) { p.Recording.SDKSum = "future" },
		"range": func(p *gasProfile) { p.Completion.From++ }, "verified-height": func(p *gasProfile) { p.Completion.Verified-- }, "incomplete-header": func(p *gasProfile) { p.Completion.Final = false }, "next-executed": func(p *gasProfile) { p.Completion.NextExecuted = true }, "state-proof": func(p *gasProfile) { p.Completion.State = false }, "results-proof": func(p *gasProfile) { p.Completion.Results = false }, "validator-proof": func(p *gasProfile) { p.Completion.Validators = false }, "parent-proof": func(p *gasProfile) { p.Completion.Parent = false }, "invented-native-raw": func(p *gasProfile) { p.Completion.NativeRawRetained = true }, "invented-physical-headers": func(p *gasProfile) { p.Completion.FreshPhysicalHeaders = true },
		"missing-pair": func(p *gasProfile) { p.Cases = p.Cases[:31] }, "unknown-33rd": func(p *gasProfile) { p.Cases = append(p.Cases, p.Cases[0]) }, "duplicate": func(p *gasProfile) { p.Cases[1] = p.Cases[0] }, "reorder": func(p *gasProfile) { p.Cases[0], p.Cases[1] = p.Cases[1], p.Cases[0] }, "missing-block": func(p *gasProfile) { p.Blocks = p.Blocks[:30] }, "block-order": func(p *gasProfile) { p.Blocks[0], p.Blocks[1] = p.Blocks[1], p.Blocks[0] },
		"signed-fee": func(p *gasProfile) { p.Cases[0].SignedFee[0] ^= 1 }, "raw-signed-tx": func(p *gasProfile) { p.Cases[0].SignedTx[0] ^= 1 },
		"nongas-code": func(p *gasProfile) { p.Cases[0].SourceAttestedNonGas.Code = 1 }, "nongas-data": func(p *gasProfile) { p.Cases[0].SourceAttestedNonGas.Data[0] ^= 1 }, "nongas-log": func(p *gasProfile) { p.Cases[0].SourceAttestedNonGas.Log += "changed" }, "nongas-info": func(p *gasProfile) { p.Cases[0].SourceAttestedNonGas.Info = "changed" }, "nongas-codespace": func(p *gasProfile) { p.Cases[0].SourceAttestedNonGas.Codespace = "changed" }, "nongas-tag-identity": func(p *gasProfile) { p.Cases[0].SourceAttestedNonGas.Tags[1].Value[0] ^= 1 }, "nongas-tag-key": func(p *gasProfile) { p.Cases[0].SourceAttestedNonGas.Tags[1].Key[0] ^= 1 }, "nongas-tag-order": func(p *gasProfile) {
			x := &p.Cases[0].SourceAttestedNonGas
			x.Tags[0], x.Tags[1] = x.Tags[1], x.Tags[0]
		},
		"finite-block-gas": func(p *gasProfile) { p.Blocks[0].GenesisMaxGas = p.Blocks[0].NativeBlockGas }, "changed-block-meter-total": func(p *gasProfile) { p.Blocks[0].NativeBlockGas++ },
		"changed-results-root": func(p *gasProfile) { p.Blocks[0].ResultsRoot = strings.Repeat("0", 64) },
		"validator-update": func(p *gasProfile) {
			p.Blocks[0].ValidatorUpdates = json.RawMessage(`[{"pub_key":{"type":"synthetic","data":"AQ=="},"power":1}]`)
		},
		"cached-code": func(p *gasProfile) {
			p.Blocks[0].RawResults = bytes.Replace(p.Blocks[0].RawResults, []byte(`"gasUsed"`), []byte(`"code"`), 1)
		},
		"cached-data": func(p *gasProfile) {
			p.Blocks[0].RawResults = bytes.Replace(p.Blocks[0].RawResults, []byte(`"data":`), []byte(`"unknownData":`), 1)
		}, "unknown-response-field": func(p *gasProfile) {
			p.Blocks[0].RawResults = bytes.Replace(p.Blocks[0].RawResults, []byte(`"gasUsed"`), []byte(`"unknown"`), 1)
		},
	}
	for name, f := range tests {
		t.Run(name, func(t *testing.T) {
			var p gasProfile
			if strictGasJSON(raw, &p) != nil {
				t.Fatal("fixture")
			}
			f(&p)
			mutated, _ := json.Marshal(p)
			if _, e := loadGasProfile(mutated, gasSHA(mutated)); e == nil {
				t.Fatal("isolated semantic mutation accepted")
			}
		})
	}
	// The retained native diagnostic and whitelist must move together even in the
	// test-only hash-rebound fixture, then the numeric and raw-block guards run.
	for _, name := range []string{"failed-code", "delta", "wanted", "signed-gas", "near-limit"} {
		t.Run(name, func(t *testing.T) {
			var p gasProfile
			_ = strictGasJSON(raw, &p)
			q := &p.Cases[0].Pair
			switch name {
			case "failed-code":
				q.NativeCode = 1
			case "delta":
				q.NativeUsed++
			case "wanted":
				q.NativeWanted++
			case "signed-gas":
				q.NativeWanted++
				q.CachedWanted++
			case "near-limit":
				q.NativeUsed = q.NativeWanted - 1
				q.CachedUsed = q.NativeUsed - 1021
			}
			lines := bytes.Split(bytes.TrimSuffix(p.NativeDetails, []byte{'\n'}), []byte{'\n'})
			lines[0], _ = json.Marshal(q)
			p.NativeDetails = append(bytes.Join(lines, []byte{'\n'}), '\n')
			mutated, _ := json.Marshal(p)
			if _, e := loadGasProfile(mutated, gasSHA(mutated)); e == nil {
				t.Fatal("pair semantic mutation accepted")
			}
		})
	}
	var p gasProfile
	_ = strictGasJSON(raw, &p)
	markerMutations := [][]byte{bytes.Replace(p.RawMarkers, []byte(":gas"), []byte(":data"), 1), append(bytes.Clone(p.RawMarkers), []byte("482090 tx999:gas\n")...)}
	for _, m := range markerMutations {
		if validateGasMarkers(m, p.Cases) == nil {
			t.Fatal("marker semantic mutation accepted")
		}
	}
}
