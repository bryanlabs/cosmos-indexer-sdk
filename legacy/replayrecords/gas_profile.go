package replayrecords

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/DefiantLabs/cosmos-indexer/legacy"
)

// Only this private artifact is approved. There is deliberately no caller-chosen
// expected-hash argument, environment override, or universal gas-delta flag.
const ApprovedGasProfileSHA256 = "11b863d30db5b1f799e5d3bdd1b49325e5a2b1637252e5c33df0e44aedf5c236"
const GasDiagnosticSource = "09a9e29cf22fa2cf0798f85181459cdb0438bdfb"
const GasDiagnosticImage = "sha256:c9405f56f5f4da5505987a9594d446dc0741906ca58cd0d490c13fd6f44e1f91"
const GasFinancialSource = "3e087ab429b75f3b6204ce475908025f6c1bdcfe"
const GasFinancialImage = "sha256:656638fa97f3792c9ba3cddb4b7f11703da5025d51b22163cc0036dd280781bd"
const GasObserverTree = "6827545e750972766e164c2823379c5b6d4788e3"
const GasSDKModBlob = "e3b97c97ce70d21df4ba13f1eb4d2466d84aa25a"
const GasSDKSumBlob = "d9488514e1fcc1d26edb84ab72b1f2b1520596aa"
const gasMarkerSHA = "2ff60b284b334e2b29ff2523a9ab7d961a6ed2ec20f29d2976a294bcc6fb36a6"
const gasAttestation = "source-attested Data/Log/ordered Tags; Info/Codespace constrained by pinned successful SDK constructors; NOT two retained full native/cached raw responses"

// GasRecording identifies the explicitly accepted financial observer lineage,
// not the different uninstrumented diagnostic runtime. These are source/package
// bindings, not permission to accept a future image with a familiar version.
type GasRecording struct {
	Source       string `json:"source"`
	Image        string `json:"image"`
	ObserverTree string `json:"observer_git_tree"`
	SDKMod       string `json:"sdk_mod_git_blob"`
	SDKSum       string `json:"sdk_sum_git_blob"`
}

func (r GasRecording) valid() bool {
	return r == (GasRecording{GasFinancialSource, GasFinancialImage, GasObserverTree, GasSDKModBlob, GasSDKSumBlob})
}

type gasTag struct {
	Key   []byte `json:"key"`
	Value []byte `json:"value"`
}
type gasResponse struct {
	Code      GasInt   `json:"code"`
	Data      []byte   `json:"data"`
	Log       string   `json:"log"`
	Info      string   `json:"info"`
	Wanted    GasInt   `json:"gasWanted"`
	Used      GasInt   `json:"gasUsed"`
	Tags      []gasTag `json:"tags"`
	Codespace string   `json:"codespace"`
}
type gasPair struct {
	Height       GasInt `json:"height"`
	Index        GasInt `json:"tx_index"`
	Hash         string `json:"tx_sha256"`
	NativeCode   GasInt `json:"native_code"`
	CachedCode   GasInt `json:"cached_code"`
	NativeWanted GasInt `json:"native_gas_wanted"`
	CachedWanted GasInt `json:"cached_gas_wanted"`
	NativeUsed   GasInt `json:"native_gas_used"`
	CachedUsed   GasInt `json:"cached_gas_used"`
}
type gasCase struct {
	Pair      gasPair `json:"pair"`
	SignedTx  []byte  `json:"signed_tx"`
	SignedFee []byte  `json:"signed_fee"`
	// This is an attested semantic projection, NOT retained native response bytes.
	SourceAttestedNonGas gasResponse `json:"source_attested_nongas"`
}
type gasBlock struct {
	Height     GasInt `json:"height"`
	RawBlock   []byte `json:"raw_block_envelope"`
	RawResults []byte `json:"raw_results_envelope"`
	// Derived from all positions, including failed responses capped to signed gas.
	NativeBlockGas   GasInt          `json:"native_block_gas_consumed"`
	GenesisMaxGas    GasInt          `json:"genesis_max_gas_not_checkpoint_measurement"`
	ResultsRoot      string          `json:"historical_code_data_results_root"`
	ValidatorUpdates json.RawMessage `json:"retained_native_validator_updates"`
}
type gasCompletion struct {
	From                 GasInt `json:"from"`
	Through              GasInt `json:"through"`
	Verified             GasInt `json:"verified"`
	NextHeader           GasInt `json:"unexecuted_next_header"`
	State                bool   `json:"state_checked_by_completed_signed_run"`
	Results              bool   `json:"results_checked_by_completed_signed_run"`
	Validators           bool   `json:"validators_checked_by_completed_signed_run"`
	Parent               bool   `json:"parent_checked_by_completed_signed_run"`
	Final                bool   `json:"final_header_checked_by_completed_signed_run"`
	NextExecuted         bool   `json:"next_block_executed"`
	NativeRawRetained    bool   `json:"full_native_response_raw_retained"`
	FreshPhysicalHeaders bool   `json:"fresh_physical_all_header_verification"`
	Attestation          string `json:"nongas_evidence_method"`
}
type gasProfile struct {
	Version       string            `json:"version"`
	Chain         string            `json:"chain"`
	Release       string            `json:"release"`
	Source        string            `json:"diagnostic_source"`
	Image         string            `json:"diagnostic_image"`
	Recording     GasRecording      `json:"financial_recording_lineage"`
	Completion    gasCompletion     `json:"completion"`
	RawMarkers    []byte            `json:"original_marker_bytes"`
	NativeDetails []byte            `json:"retained_native_gas_ndjson"`
	Cases         []gasCase         `json:"cases"`
	Blocks        []gasBlock        `json:"blocks"`
	Evidence      map[string]string `json:"reviewed_artifact_sha256"`
}

// ApprovedGasProfile is opaque and immutable outside this package. Native gas
// remains attributed diagnostic data. It never edits a capture or an inclusion.
type ApprovedGasProfile struct {
	profile gasProfile
	digest  string
}

func gasSHA(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func LoadApprovedGasProfile(r io.Reader) (*ApprovedGasProfile, error) {
	raw, e := io.ReadAll(io.LimitReader(r, 32<<20+1))
	if e != nil || len(raw) > 32<<20 {
		return nil, invalid
	}
	return loadGasProfile(raw, ApprovedGasProfileSHA256)
}
func loadGasProfile(raw []byte, trusted string) (*ApprovedGasProfile, error) {
	if gasSHA(raw) != trusted {
		return nil, invalid
	}
	var p gasProfile
	if strictGasJSON(raw, &p) != nil || validateGasProfile(p) != nil {
		return nil, invalid
	}
	return &ApprovedGasProfile{p, trusted}, nil
}
func validateGasPair(p gasPair, signedGas uint64) error {
	if p.Height < 479755 || p.Height > 482089 || p.Index < 0 || p.NativeCode != 0 || p.CachedCode != 0 || signedGas > 1<<63-1 || p.CachedWanted != GasInt(signedGas) || p.NativeWanted != GasInt(signedGas) || p.CachedUsed <= 0 || p.NativeUsed <= p.CachedUsed || p.NativeUsed-p.CachedUsed != 1021 || p.NativeWanted <= p.NativeUsed || p.NativeWanted-p.NativeUsed < 36213 {
		return invalid
	}
	if len(p.Hash) != 64 {
		return invalid
	}
	if b, e := hex.DecodeString(p.Hash); e != nil || hex.EncodeToString(b) != p.Hash {
		return invalid
	}
	return nil
}
func validateNonGas(cached, attested gasResponse) error {
	// GasUsed is intentionally not a non-gas equality field. GasWanted IS.
	cached.Used = 0
	attested.Used = 0
	if cached.Code != 0 || cached.Info != "" || cached.Codespace != "" || !reflect.DeepEqual(cached, attested) {
		return invalid
	}
	return nil
}
func validateGasProfile(p gasProfile) error {
	c := p.Completion
	if p.Version != "hub2-exact32-source-attested-v1" || p.Chain != "cosmoshub-2" || p.Release != Release034 || p.Source != GasDiagnosticSource || p.Image != GasDiagnosticImage || !p.Recording.valid() || c.From != 400001 || c.Through != 482090 || c.Verified != 482090 || c.NextHeader != 482091 || !c.State || !c.Results || !c.Validators || !c.Parent || !c.Final || c.NextExecuted || c.NativeRawRetained || c.FreshPhysicalHeaders || c.Attestation != gasAttestation || len(p.Cases) != 32 || len(p.Blocks) != 31 || len(p.Evidence) != 4 {
		return invalid
	}
	expected := map[string]string{
		"control_archive":         "d4690deb56e38163da68336d36907c24b958cf25d1def4635201219d29fb1d2d",
		"metadata_archive":        "16bd9cfc2bcf30c464c7182cd9e18da09ed1d110a9b2c1d8b0070d7312ce0e9c",
		"cache_archive":           "ba0d700765519a08950be02d743c9a113638830990ca847a1c00046f9f86c8fb",
		"independent_calculation": "9c6fbbac720eafea573e0c2934b4c48d41bfe1c13dbd77c58df88f55197e9a16",
	}
	if !reflect.DeepEqual(p.Evidence, expected) {
		return invalid
	}
	if gasSHA(p.RawMarkers) != gasMarkerSHA {
		return invalid
	}
	lines := bytes.Split(bytes.TrimSuffix(p.NativeDetails, []byte{'\n'}), []byte{'\n'})
	if len(lines) != 32 || !bytes.HasSuffix(p.NativeDetails, []byte{'\n'}) {
		return invalid
	}
	var previous gasPair
	for i, x := range p.Cases {
		q := x.Pair
		var retained gasPair
		if strictGasJSON(lines[i], &retained) != nil || retained != q || i > 0 && (q.Height < previous.Height || q.Height == previous.Height && q.Index <= previous.Index) {
			return invalid
		}
		previous = q
		tx, e := legacy.Decode(p.Chain, x.SignedTx)
		if e != nil || len(tx.Issues) != 0 || !tx.SignersComplete || len(tx.Signatures) == 0 || gasSHA(x.SignedTx) != q.Hash || !bytes.Equal(tx.FeeRaw, x.SignedFee) || validateGasPair(q, tx.Gas) != nil {
			return invalid
		}
	}
	caseIndex := 0
	previousHeight := GasInt(0)
	for _, b := range p.Blocks {
		if b.Height <= previousHeight || caseIndex >= len(p.Cases) || p.Cases[caseIndex].Pair.Height != b.Height {
			return invalid
		}
		previousHeight = b.Height
		next := caseIndex
		for next < len(p.Cases) && p.Cases[next].Pair.Height == b.Height {
			next++
		}
		if validateGasBlock(b, p.Cases[caseIndex:next]) != nil {
			return invalid
		}
		caseIndex = next
	}
	if caseIndex != 32 {
		return invalid
	}
	return nil
}

func validateGasBlock(b gasBlock, cases []gasCase) error {
	// Parse with duplicate/lexeme checks before reading any numeric field. Entire
	// original envelopes stay in the profile and must match again at intake.
	var block map[string]json.RawMessage
	if strictGasJSON(b.RawBlock, &block) != nil {
		return invalid
	}
	var env struct {
		Result struct {
			Block struct {
				Header struct {
					Chain  string `json:"chain_id"`
					Height GasInt `json:"height"`
				} `json:"header"`
				Data struct {
					Txs [][]byte `json:"txs"`
				} `json:"data"`
			} `json:"block"`
		} `json:"result"`
	}
	// Blocks have many other legitimate historical header fields, retained as raw
	// bytes and pinned by the trusted profile, not silently normalized away.
	if json.Unmarshal(b.RawBlock, &env) != nil || env.Result.Block.Header.Chain != "cosmoshub-2" || env.Result.Block.Header.Height != b.Height {
		return invalid
	}
	var re struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			Height  GasInt `json:"height"`
			Results struct {
				Deliver []gasResponse `json:"DeliverTx"`
				Begin   struct {
					Tags []gasTag `json:"tags"`
				} `json:"BeginBlock"`
				End struct {
					Updates   json.RawMessage `json:"validator_updates"`
					Consensus json.RawMessage `json:"consensus_param_updates"`
					Tags      []gasTag        `json:"tags"`
				} `json:"EndBlock"`
			} `json:"results"`
		} `json:"result"`
	}
	if strictGasJSON(b.RawResults, &re) != nil || re.Result.Height != b.Height || len(re.Result.Results.Deliver) != len(env.Result.Block.Data.Txs) || len(re.Result.Results.Begin.Tags) != 0 || len(re.Result.Results.End.Tags) != 0 {
		return invalid
	}
	end := re.Result.Results.End
	if len(end.Consensus) > 0 && string(end.Consensus) != "null" {
		return invalid
	}
	// Ordered native-era validators, every field retained and checked.
	type update struct {
		Key struct {
			Type string `json:"type"`
			Data []byte `json:"data"`
		} `json:"pub_key"`
		Power GasInt `json:"power"`
	}
	var a, v []update
	if len(end.Updates) == 0 {
		end.Updates = json.RawMessage("[]")
	}
	if len(b.ValidatorUpdates) == 0 {
		return invalid
	}
	if strictGasJSON(end.Updates, &a) != nil || strictGasJSON(b.ValidatorUpdates, &v) != nil || len(a) != len(v) {
		return invalid
	}
	for i := range a {
		if !reflect.DeepEqual(a[i], v[i]) {
			return invalid
		}
	}
	nativeGas := GasInt(0)
	overrides := map[int]gasPair{}
	for _, x := range cases {
		q := x.Pair
		i := int(q.Index)
		if i < 0 || i >= len(re.Result.Results.Deliver) || q.Height != b.Height || !bytes.Equal(env.Result.Block.Data.Txs[i], x.SignedTx) {
			return invalid
		}
		r := re.Result.Results.Deliver[i]
		if r.Code != q.CachedCode || r.Wanted != q.CachedWanted || r.Used != q.CachedUsed || validateNonGas(r, x.SourceAttestedNonGas) != nil {
			return invalid
		}
		overrides[i] = q
	}
	for i, r := range re.Result.Results.Deliver {
		tx, e := legacy.Decode("cosmoshub-2", env.Result.Block.Data.Txs[i])
		if e != nil || tx.Gas > 1<<63-1 {
			return invalid
		}
		used := r.Used
		if q, ok := overrides[i]; ok {
			used = q.NativeUsed
		}
		if used < 0 {
			return invalid
		}
		limit := GasInt(tx.Gas)
		if used > limit {
			used = limit
		}
		if nativeGas > 1<<63-1-used {
			return invalid
		}
		nativeGas += used
	}
	// Genesis limit is explicitly not a freshly measured checkpoint parameter.
	// Completed outcome/state proof is also mandatory. No general block-gas rule.
	if b.GenesisMaxGas != 2000000 || nativeGas != b.NativeBlockGas || nativeGas >= b.GenesisMaxGas || nativeGas > 328238 || len(b.ResultsRoot) != 64 {
		return invalid
	}
	if _, e := hex.DecodeString(b.ResultsRoot); e != nil {
		return invalid
	}
	root, err := historicalGasResultsRoot(re.Result.Results.Deliver)
	if err != nil || root != b.ResultsRoot {
		return invalid
	}
	return nil
}

// GasDiagnostic is attributed immutable output, never canonical GasUsed.
type GasDiagnostic struct {
	Height        int64  `json:"height"`
	Index         int64  `json:"tx_index"`
	TxSHA256      string `json:"tx_sha256"`
	CachedGasUsed int64  `json:"cached_gas_used"`
	NativeGasUsed int64  `json:"native_gas_used_diagnostic"`
}
type GasAdmission struct {
	ProfileSHA256     string          `json:"approved_profile_sha256"`
	DiagnosticSource  string          `json:"diagnostic_source"`
	DiagnosticImage   string          `json:"diagnostic_image"`
	Method            string          `json:"nongas_evidence_method"`
	NativeRawRetained bool            `json:"full_native_response_raw_retained"`
	Diagnostics       []GasDiagnostic `json:"attributed_native_gas_diagnostics"`
}

func (p *ApprovedGasProfile) Diagnostic() GasAdmission {
	a := GasAdmission{ProfileSHA256: p.digest, DiagnosticSource: GasDiagnosticSource, DiagnosticImage: GasDiagnosticImage, Method: gasAttestation}
	for _, c := range p.profile.Cases {
		q := c.Pair
		a.Diagnostics = append(a.Diagnostics, GasDiagnostic{int64(q.Height), int64(q.Index), q.Hash, int64(q.CachedUsed), int64(q.NativeUsed)})
	}
	return a
}

// CheckCachedInputs binds the exact retained envelope and ALL signed transaction
// bytes of an affected height. It never rebuilds raw JSON or changes metadata.
func (p *ApprovedGasProfile) CheckCachedInputs(height int64, txs [][]byte, rawResultEnvelope []byte) error {
	for _, b := range p.profile.Blocks {
		if int64(b.Height) == height {
			var e struct {
				Result struct {
					Results json.RawMessage `json:"results"`
				} `json:"result"`
			}
			// rpc.Legacy.RawJSON is the raw Result object, not the outer RPC wrapper.
			var outer struct {
				Result json.RawMessage `json:"result"`
			}
			if json.Unmarshal(b.RawResults, &outer) != nil {
				return invalid
			}
			if !bytes.Equal(rawResultEnvelope, outer.Result) {
				return invalid
			}
			if json.Unmarshal(b.RawResults, &e) != nil {
				return invalid
			}
			var block struct {
				Result struct {
					Block struct {
						Data struct {
							Txs [][]byte `json:"txs"`
						} `json:"data"`
					} `json:"block"`
				} `json:"result"`
			}
			if json.Unmarshal(b.RawBlock, &block) != nil || !reflect.DeepEqual(txs, block.Result.Block.Data.Txs) {
				return invalid
			}
			return nil
		}
	}
	return nil
}

// AdmitDifferences remains strict by default. The approved path permits exactly
// the original 3387 lines and 32 gas positions, never filtered or rewritten text.
func AdmitDifferences(r io.Reader, pinnedSHA string, from, committed, diffs int64, p *ApprovedGasProfile, recording *GasRecording) (DifferenceAdmission, *GasAdmission, error) {
	if p == nil {
		if recording != nil {
			return DifferenceAdmission{}, nil, invalid
		}
		a, e := AdmitTagDifferences(r, pinnedSHA, from, committed, diffs)
		return a, nil, e
	}
	if recording == nil || !recording.valid() || from != 400001 || committed != 482090 || diffs != 3387 || pinnedSHA != gasMarkerSHA {
		return DifferenceAdmission{}, nil, invalid
	}
	raw, e := io.ReadAll(io.LimitReader(r, MaxDifferenceBytes+1))
	if e != nil || !bytes.Equal(raw, p.profile.RawMarkers) || gasSHA(raw) != pinnedSHA {
		return DifferenceAdmission{}, nil, invalid
	}
	// Independently check each grammar/position, not just a global file digest.
	if validateGasMarkers(raw, p.profile.Cases) != nil {
		return DifferenceAdmission{}, nil, invalid
	}
	a := DifferenceAdmission{Version: "runner034-exact32-gas-v1", RawSHA256: pinnedSHA, From: from, Committed: committed, StatusDiffs: diffs, TotalLines: diffs, RunLines: diffs}
	encoded, _ := json.Marshal(a)
	a.ClassificationSHA256 = gasSHA(encoded)
	g := p.Diagnostic()
	return a, &g, nil
}
func validateGasMarkers(raw []byte, cases []gasCase) error {
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte{'\n'}), []byte{'\n'})
	if len(lines) != 3387 || !bytes.HasSuffix(raw, []byte{'\n'}) {
		return invalid
	}
	previous := int64(0)
	i := 0
	for _, line := range lines {
		hs, detail, ok := strings.Cut(string(line), " ")
		h, good := positive(hs)
		if !ok || !good || h <= previous || h < 400001 || h > 482090 {
			return invalid
		}
		previous = h
		if strings.Contains(detail, ":gas") {
			expected := []string{}
			for i < len(cases) && int64(cases[i].Pair.Height) == h {
				expected = append(expected, fmt.Sprintf("tx%d:gas", cases[i].Pair.Index))
				i++
			}
			if len(expected) == 0 || detail != strings.Join(expected, ",") {
				return invalid
			}
		} else if !tagLine(detail) {
			return invalid
		}
	}
	if i != 32 {
		return invalid
	}
	return nil
}
