package hub2promotion

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The whole-chain finalizer is the only writer of
// full_chain_books_tax_acceptance=true. It reads every truth value from the
// pinned evidence files themselves. The input list carries paths and hashes
// only, and a component receipt is never edited or relabelled: the finalizer
// writes a new report beside new index files, or it refuses and names the
// failing input.

const FinalizationSchema = "legacy-whole-chain-finalization-v1"

// Starting genesis of each chain and the next chain's starting genesis. A
// different file cannot stand in for either end of a chain's history.
const acceptedHub1GenesisSHA = "73a866b21723ecbc28b6d15951b2eb3aa2f2443650ff6df489bf55ac5edceefa"
const acceptedHub3GenesisSHA = "5c2e311745a4cd3cfb4eaa7a7dace4cc116500ee65395b583444add9e72e1d1a"

// The reviewed build of cmd/hub2-migrate036. The comparison against the Hub 3
// starting balances is only as good as the migration that feeds it.
const acceptedMigrationBinarySHA = "75e659059f6b42b90e5b2b0209d8f34d5b76c73268c910236e9dcfd5717432c3"

var commitRE = regexp.MustCompile(`^[a-f0-9]{40}$`)

// ComponentInput names one independently sealed part of a chain's history.
type ComponentInput struct {
	Profile    string `json:"profile"`
	Acceptance Pin    `json:"acceptance"`
	Opening    *Pin   `json:"opening,omitempty"`
	Closing    *Pin   `json:"closing,omitempty"`
	Result     *Pin   `json:"result,omitempty"`
}

// FinalizeSpec is the complete ordered input list for one chain.
type FinalizeSpec struct {
	Chain           string           `json:"chain"`
	Genesis         Pin              `json:"genesis"`
	Components      []ComponentInput `json:"components"`
	Join            *Pin             `json:"join,omitempty"`
	CanonicalProof  *Pin             `json:"canonical_proof,omitempty"`
	CanonicalExport Pin              `json:"canonical_export"`
	ExportBinding   *Pin             `json:"export_binding,omitempty"`
	PayoutRecords   Pin              `json:"payout_records"`
	PayoutProof     Pin              `json:"payout_proof"`
	NextGenesis     Pin              `json:"next_genesis"`
	MigratedExport  *Pin             `json:"migrated_export,omitempty"`
	MigrationBinary *Pin             `json:"migration_binary,omitempty"`
	Projection      Pin              `json:"projection"`
	StagingReceipt  *Pin             `json:"staging_receipt,omitempty"`
	LoaderBinary    Pin              `json:"loader_binary"`
	BuildCommit     string           `json:"build_commit"`
}

// ProjectionIndex is written by the staging export and read by the finalizer
// and the manifest assembler. Staging identifies the private database the rows
// were read from.
type ProjectionIndex struct {
	Schema  string       `json:"schema"`
	Chain   string       `json:"chain"`
	Staging Identity     `json:"staging"`
	Tables  []Projection `json:"tables"`
}

const ProjectionIndexSchema = "legacy-staging-projection-index-v1"

type finalBounds struct {
	chain, next                string
	canonical, last            int64
	genesisSHA, nextGenesisSHA string
	migrationBinarySHA         string
	migrates, approved         bool
}

func approvedBounds(chain string) (finalBounds, error) {
	canonical, last, next, e := boundaries(chain)
	if e != nil {
		return finalBounds{}, e
	}
	b := finalBounds{chain: chain, next: next, canonical: canonical, last: last, approved: true}
	if chain == "cosmoshub-1" {
		b.genesisSHA, b.nextGenesisSHA = acceptedHub1GenesisSHA, acceptedHub2GenesisSHA
	} else {
		b.genesisSHA, b.nextGenesisSHA, b.migrationBinarySHA, b.migrates = acceptedHub2GenesisSHA, acceptedHub3GenesisSHA, acceptedMigrationBinarySHA, true
	}
	return b, nil
}

// SealedComponent is what the finalizer read from one component's own files.
type SealedComponent struct {
	Profile         string `json:"profile"`
	First           int64  `json:"first"`
	Last            int64  `json:"last"`
	Acceptance      Pin    `json:"acceptance"`
	Opening         string `json:"opening"`
	Closing         string `json:"closing"`
	OpeningProof    *Pin   `json:"opening_proof,omitempty"`
	ClosingProof    *Pin   `json:"closing_proof,omitempty"`
	Result          *Pin   `json:"result,omitempty"`
	Held            int64  `json:"held"`
	SourcePreserved bool   `json:"source_preserved"`
	ClosingBoundBy  string `json:"closing_bound_by"`
	Tail            bool   `json:"historical_tail_evidence_only"`
	PriorSeal       string `json:"recorded_prior_seal,omitempty"`
	continues       string
}

// ComponentChain is the verified join, or the verified prefix of one.
type ComponentChain struct {
	Chain      string            `json:"chain"`
	Components []SealedComponent `json:"components"`
	Seams      []int64           `json:"seams"`
	Through    int64             `json:"through"`
}

type refusal struct{ input, reason string }

func (r refusal) Error() string { return "refused: " + r.input + ": " + r.reason }
func refuse(input, format string, a ...any) error {
	return refusal{input, fmt.Sprintf(format, a...)}
}

func pinnedObject(p Pin, name string) (map[string]any, error) {
	b, e := readPinnedBytes(p, 256<<20)
	if e != nil {
		return nil, refuse(name, "file missing or its bytes differ from the pinned hash")
	}
	v, e := decodeUnique(b)
	if e != nil {
		return nil, refuse(name, "not well formed JSON with unique keys")
	}
	o, ok := v.(map[string]any)
	if !ok {
		return nil, refuse(name, "JSON object required")
	}
	return o, nil
}
func localObject(path string) (map[string]any, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	v, e := decodeUnique(b)
	if e != nil {
		return nil, e
	}
	o, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("JSON object required")
	}
	return o, nil
}
func whole(o map[string]any, key string) (int64, bool) {
	n, ok := o[key].(json.Number)
	if !ok {
		return 0, false
	}
	i, e := n.Int64()
	return i, e == nil
}
func eitherWhole(o map[string]any, keys ...string) (int64, bool) {
	found, out := 0, int64(0)
	for _, k := range keys {
		if _, present := o[k]; present {
			i, ok := whole(o, k)
			if !ok {
				return 0, false
			}
			found++
			out = i
		}
	}
	return out, found == 1
}

// containsString reports whether a document names the value anywhere.
func containsString(v any, want string) bool {
	switch x := v.(type) {
	case string:
		return x == want
	case map[string]any:
		for _, item := range x {
			if containsString(item, want) {
				return true
			}
		}
	case []any:
		for _, item := range x {
			if containsString(item, want) {
				return true
			}
		}
	}
	return false
}

// headerProof reads a native proof that the state after one height is the
// application hash carried by the following block header.
func headerProof(p Pin, name, chain string, height int64) (string, error) {
	o, e := pinnedObject(p, name)
	if e != nil {
		return "", e
	}
	at, ok1 := whole(o, "checkpoint_height")
	verified, ok2 := whole(o, "state_hash_verified_through")
	next, ok3 := whole(o, "next_header_height")
	hash, _ := o["application_hash"].(string)
	if o["kind"] != "checkpoint_next_header_only" || o["chain"] != chain || !ok1 || !ok2 || !ok3 || !shaRE.MatchString(hash) {
		return "", refuse(name, "not a next-header state proof for %s", chain)
	}
	if at != height || verified != height || next != height+1 {
		return "", refuse(name, "state proof is for height %d, wanted %d", at, height)
	}
	return hash, nil
}
func commitHash(v any) (int64, string, bool) {
	c, ok := v.(map[string]any)
	if !ok {
		return 0, "", false
	}
	version, ok := whole(c, "Version")
	if !ok {
		return 0, "", false
	}
	text, _ := c["Hash"].(string)
	if text == "" {
		return version, "", true
	}
	raw, e := base64.StdEncoding.Strict().DecodeString(text)
	if e != nil || len(raw) != 32 {
		return 0, "", false
	}
	return version, hex.EncodeToString(raw), true
}

var preservationKeys = []string{
	"original_source_and_jobs_preserved",
	"all_source_pre_post_and_local_member_hashes_equal",
	"all_cache_pre_post_and_local_hashes_match",
	"actual_source_original_native_job_pod_UID_specs_images_and_restart0_preserved",
	"source_closed_checkpoint_and_reference_and_cache_before_after_hashes_equal",
	"source_preserved_after_proof",
}

func preserved(o map[string]any) bool {
	seen := 0
	for _, k := range preservationKeys {
		if v, ok := o[k]; ok {
			if v != true {
				return false
			}
			seen++
		}
	}
	return seen > 0
}

func readComponent(chain string, n int, in ComponentInput, prior *SealedComponent, b finalBounds) (SealedComponent, error) {
	name := fmt.Sprintf("component %d (%s)", n+1, filepath.Base(filepath.Dir(in.Acceptance.Path)))
	out := SealedComponent{Profile: in.Profile, Acceptance: in.Acceptance, OpeningProof: in.Opening, ClosingProof: in.Closing, Result: in.Result, ClosingBoundBy: "input list hash and the next component's opening proof"}
	wantChain := map[string]string{"hub2-genesis-prefix-v1": "cosmoshub-2", "hub2-range-result-v1": "cosmoshub-2", "hub2-continuation-v1": "cosmoshub-2", "hub2-tail-v1": "cosmoshub-2", "hub1-part-v1": "cosmoshub-1", "hub1-tail-v1": "cosmoshub-1"}[in.Profile]
	if wantChain == "" {
		return out, refuse(name, "unknown component profile")
	}
	if wantChain != chain {
		return out, refuse(name, "profile belongs to %s, not %s", wantChain, chain)
	}
	// A component root is only ever named by the input list or by the next
	// part's recorded prior seal, never found by folder name: an interrupted
	// attempt can sit beside the accepted one under a similar name.
	root := filepath.Dir(filepath.Dir(in.Acceptance.Path))
	continuation := in.Profile == "hub2-continuation-v1" || in.Profile == "hub2-tail-v1"
	_, claimMissing := os.Lstat(filepath.Join(root, "execution-claim.json"))
	if _, missing := os.Lstat(in.Acceptance.Path); missing != nil && continuation && claimMissing == nil {
		return out, refuse(name, "this root was claimed but never sealed: an abandoned attempt is evidence, not a part")
	}
	a, e := pinnedObject(in.Acceptance, name+" receipt")
	if e != nil {
		return out, e
	}
	if continuation && claimMissing == nil {
		exit, e := localObject(filepath.Join(root, "execution-exit.json"))
		code, ok := whole(exit, "exit_code")
		if e != nil || exit["component_accepted"] != true || !ok || code != 0 {
			return out, refuse(name, "this root holds a claim with no accepted exit: an abandoned attempt is evidence, not a part")
		}
	}
	if a["full_chain_books_tax_acceptance"] == true || a["books_tax_acceptance"] == true || a["global_chain_books_tax_acceptance"] == true {
		return out, refuse(name, "a part cannot carry whole-chain acceptance")
	}
	opening := func() error {
		if in.Opening == nil {
			return refuse(name, "opening state proof missing")
		}
		out.Opening, e = headerProof(*in.Opening, name+" opening proof", chain, out.First-1)
		return e
	}
	closing := func() error {
		if in.Closing == nil {
			return refuse(name, "closing state proof missing")
		}
		out.Closing, e = headerProof(*in.Closing, name+" closing proof", chain, out.Last)
		if e != nil {
			return e
		}
		if containsString(a, out.Closing) || containsString(a, in.Closing.SHA256) {
			out.ClosingBoundBy = "its own receipt"
		}
		return nil
	}
	counted := func(accepted string) error {
		var ok1, ok2 bool
		out.First, ok1 = eitherWhole(a, "first", "from")
		out.Last, ok2 = eitherWhole(a, "last", "through")
		if !ok1 || !ok2 || out.First < 1 || out.Last < out.First {
			return refuse(name, "receipt does not state its first and last height")
		}
		if a[accepted] != true {
			return refuse(name, "receipt for heights %d to %d is not sealed (%s is not true)", out.First, out.Last, accepted)
		}
		held, ok := whole(a, "held")
		out.Held = held
		if !ok || held != 0 {
			return refuse(name, "receipt for heights %d to %d holds heights back", out.First, out.Last)
		}
		heights, ok := whole(a, "validated_heights")
		if !ok || heights != out.Last-out.First+1 {
			return refuse(name, "receipt does not cover every height from %d to %d", out.First, out.Last)
		}
		out.SourcePreserved = preserved(a)
		if !out.SourcePreserved {
			return refuse(name, "receipt does not show the original recordings unchanged")
		}
		return nil
	}
	switch in.Profile {
	case "hub2-genesis-prefix-v1":
		if e = counted("component_accepted"); e != nil {
			return out, e
		}
		g, _ := a["native_genesis_opening"].(map[string]any)
		if out.First != 1 || in.Opening != nil || g == nil || g["full_native_all10modules_exact_equal_official_genesis"] != true || g["header1_application_hash_genuinely_empty"] != true || g["opening_from_zeroheight_export"] != false || a["synthetic_zeroheight_payouts_counted_as_income_or_opening"] != false {
			return out, refuse(name, "first part does not open from the official genesis")
		}
		out.Opening = b.genesisSHA
		if e = closing(); e != nil {
			return out, e
		}
		at, ok := whole(a, "normal_terminal_height")
		if !ok || at != out.Last || a["normal_native_root"] != out.Closing {
			return out, refuse(name, "closing state proof differs from the receipt's own end state")
		}
	case "hub2-range-result-v1":
		if e = counted("component_validated"); e != nil {
			return out, e
		}
		if code, ok := whole(a, "exit_code"); !ok || code != 0 {
			return out, refuse(name, "validation did not exit cleanly")
		}
		if e = opening(); e != nil {
			return out, e
		}
		if e = closing(); e != nil {
			return out, e
		}
	case "hub2-continuation-v1", "hub2-tail-v1":
		if in.Profile == "hub2-tail-v1" {
			if a["rehearsal"] != false || a["schema"] != "hub2-historical-tail-seal-v1" {
				return out, refuse(name, "receipt is not the sealed Hub 2 tail: a rehearsal or any other kind of receipt is not accepted here")
			}
			scope, _ := a["complete_scope"].([]any)
			at, ok := whole(a, "canonical_migration_height")
			if len(scope) != 2 || scope[0] != json.Number(strconv.FormatInt(b.canonical+1, 10)) || scope[1] != json.Number(strconv.FormatInt(b.last, 10)) || !ok || at != b.canonical {
				return out, refuse(name, "tail seal does not cover exactly heights %d to %d after the migration height %d", b.canonical+1, b.last, b.canonical)
			}
			if a["terminal_next_header_proof"] != false || a["component_accepted"] != true {
				return out, refuse(name, "tail seal is not accepted, or claims a later block that does not exist")
			}
			proof, _ := a["opening_state_proof"].(map[string]any)
			if in.Opening == nil || proof["sha256"] != in.Opening.SHA256 {
				return out, refuse(name, "tail seal opens from a different state proof than the one listed")
			}
			if in.Closing != nil {
				return out, refuse(name, "the tail has no closing state proof: no block exists after the final height")
			}
		}
		if e = counted("component_accepted"); e != nil {
			return out, e
		}
		if want, present := a["prior_acceptance_sha256"]; present {
			out.continues, _ = want.(string)
			if out.continues == "" {
				return out, refuse(name, "receipt does not name the receipt it continues from")
			}
		} else if prior != nil && prior.Profile == in.Profile {
			return out, refuse(name, "receipt does not name the receipt it continues from")
		}
		if config, e := localObject(filepath.Join(root, "component-config.json")); e == nil {
			first, ok1 := whole(config, "first")
			last, ok2 := whole(config, "last")
			seal, _ := config["prior_seal"].(string)
			if !ok1 || !ok2 || first != out.First || last != out.Last || seal == "" {
				return out, refuse(name, "this root's recorded range or earlier seal does not match its receipt")
			}
			out.PriorSeal = filepath.Clean(seal)
		}
		if e = opening(); e != nil {
			return out, e
		}
		if in.Profile == "hub2-tail-v1" {
			out.Closing = in.Acceptance.SHA256
			out.ClosingBoundBy = "no next block exists after the final height, so the tail is closed by its receipt hash"
		} else if e = closing(); e != nil {
			return out, e
		}
		if in.Profile == "hub2-continuation-v1" && out.ClosingBoundBy != "its own receipt" {
			if listed, _ := a["mandatory_artifact_sha256"].(map[string]any); listed[in.Closing.Path] != in.Closing.SHA256 {
				return out, refuse(name, "closing state proof is not one of the files the receipt sealed")
			}
			out.ClosingBoundBy = "its own receipt"
		}
	case "hub1-part-v1":
		if a["schema"] != "native033-complete-profile-seal-v1" || a["source_preserved_after_proof"] != true || a["actual_negative_controls_passed"] != true {
			return out, refuse(name, "receipt is not a sealed Hub 1 part with the original recordings unchanged")
		}
		if in.Result == nil || in.Result.SHA256 != a["result_sha256"] || in.Opening != nil || in.Closing != nil {
			return out, refuse(name, "result file is not the one the seal names")
		}
		r, e := pinnedObject(*in.Result, name+" result")
		if e != nil {
			return out, e
		}
		if r["schema"] != "native033-complete-profile-financial-v1" && r["schema"] != "native033-split-range-financial-v1" {
			return out, refuse(name, "result is not a complete Hub 1 financial result")
		}
		var ok1, ok2 bool
		out.First, ok1 = whole(r, "first")
		out.Last, ok2 = whole(r, "through")
		heights, ok3 := whole(r, "heights")
		if !ok1 || !ok2 || !ok3 || out.First < 1 || out.Last < out.First || heights != out.Last-out.First+1 {
			return out, refuse(name, "result does not cover every height of its range")
		}
		openAt, openHash, ok4 := commitHash(r["opening_commit"])
		closeAt, closeHash, ok5 := commitHash(r["ending_commit"])
		if !ok4 || !ok5 || openAt != out.First-1 || closeAt != out.Last || closeHash == "" || (openHash == "") != (out.First == 1) {
			return out, refuse(name, "result does not state its start and end state")
		}
		out.Opening, out.Closing, out.SourcePreserved, out.ClosingBoundBy = openHash, closeHash, true, "its own sealed result"
		if out.First == 1 {
			out.Opening = b.genesisSHA
		}
	case "hub1-tail-v1":
		scope, _ := a["complete_scope"].([]any)
		at, ok := whole(a, "canonical_migration_height")
		if len(scope) != 2 || !ok || at != b.canonical || a["terminal_next_header_proof"] != false || a["prior500000_export_or_payout_reexecuted"] != false {
			return out, refuse(name, "receipt is not the separate Hub 1 tail seal")
		}
		first, ok1 := scope[0].(json.Number)
		last, ok2 := scope[1].(json.Number)
		if !ok1 || !ok2 || in.Opening != nil || in.Closing != nil {
			return out, refuse(name, "tail seal does not state its heights")
		}
		out.First, _ = first.Int64()
		out.Last, _ = last.Int64()
		out.SourcePreserved = true
		out.Closing = in.Acceptance.SHA256
		out.ClosingBoundBy = "no next block exists after the final height, so the tail is closed by its receipt hash"
		if prior != nil {
			out.Opening = prior.Closing
		}
	}
	out.Tail = out.First > b.canonical
	return out, nil
}

// VerifyComponents checks an ordered component list for one chain: the
// genuine starting genesis, every receipt sealed with nothing held and its
// source preserved, no gap or overlap, and each part's end state equal to the
// next part's start state. It does not require the list to be complete, so it
// also reports how far a chain's sealed history currently reaches.
func VerifyComponents(spec FinalizeSpec) (ComponentChain, error) {
	b, e := approvedBounds(spec.Chain)
	if e != nil {
		return ComponentChain{}, e
	}
	return verifyComponents(spec, b)
}
func verifyComponents(spec FinalizeSpec, b finalBounds) (ComponentChain, error) {
	out := ComponentChain{Chain: spec.Chain}
	if spec.Chain != b.chain {
		return out, refuse("input list", "chain differs from the requested chain")
	}
	if spec.Genesis.SHA256 != b.genesisSHA {
		return out, refuse("starting genesis", "hash is not the genuine %s genesis", b.chain)
	}
	g, e := pinnedObject(spec.Genesis, "starting genesis")
	if e != nil {
		return out, e
	}
	if g["chain_id"] != b.chain {
		return out, refuse("starting genesis", "file is for a different chain")
	}
	if len(spec.Components) == 0 {
		return out, refuse("input list", "no components listed")
	}
	seen := map[string]bool{}
	var prior *SealedComponent
	for n, in := range spec.Components {
		if seen[in.Acceptance.SHA256] || seen[in.Acceptance.Path] {
			return out, refuse(fmt.Sprintf("component %d", n+1), "the same receipt is listed twice")
		}
		seen[in.Acceptance.SHA256], seen[in.Acceptance.Path] = true, true
		c, e := readComponent(spec.Chain, n, in, prior, b)
		if e != nil {
			return out, e
		}
		name := fmt.Sprintf("component %d (heights %d to %d)", n+1, c.First, c.Last)
		want := int64(1)
		if prior != nil {
			want = prior.Last + 1
		}
		if c.First > want {
			return out, refuse(name, "gap: heights %d to %d are not covered by any component", want, c.First-1)
		}
		if c.First < want {
			return out, refuse(name, "overlap: height %d is already covered by the component before it", c.First)
		}
		if c.continues != "" && (prior == nil || c.continues != prior.Acceptance.SHA256) {
			return out, refuse(name, "receipt continues from a different earlier receipt than the one listed before it")
		}
		if c.PriorSeal != "" && (prior == nil || c.PriorSeal != filepath.Clean(prior.Acceptance.Path)) {
			return out, refuse(name, "this root records a different earlier seal than the part listed before it")
		}
		if prior != nil && (c.Opening == "" || c.Opening != prior.Closing) {
			return out, refuse(name, "seam at height %d: this part's start state differs from the previous part's end state", prior.Last)
		}
		if c.First <= b.canonical && c.Last > b.canonical {
			return out, refuse(name, "a part must end exactly at the migration height %d", b.canonical)
		}
		if c.Tail != (in.Profile == "hub2-tail-v1" || in.Profile == "hub1-tail-v1") || (c.Tail && (c.First != b.canonical+1 || c.Last != b.last)) {
			return out, refuse(name, "only the separate tail %d to %d may follow the migration height", b.canonical+1, b.last)
		}
		if prior != nil {
			out.Seams = append(out.Seams, prior.Last)
		}
		out.Components = append(out.Components, c)
		out.Through = c.Last
		prior = &out.Components[len(out.Components)-1]
	}
	return out, nil
}

type payoutRow struct {
	Height    int64  `json:"height"`
	Phase     int    `json:"phase"`
	Tx        int64  `json:"tx_index"`
	Msg       int64  `json:"msg_index"`
	Sub       int64  `json:"sub_index"`
	Category  string `json:"category"`
	Amount    string `json:"amount"`
	Denom     string `json:"denom"`
	To        string `json:"to_addr"`
	Validator string `json:"validator_address"`
	Source    string `json:"amount_source"`
}

func payoutKey(category, validator, to, amount, denom string) string {
	if category == "commission" {
		to = ""
	}
	return strings.Join([]string{category, validator, to, amount, denom}, "\x00")
}
func multisetSHA(m map[string]int64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%d\x00%s\n", m[k], k)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type projectionFacts struct {
	headers, payoutRows int64
	payoutSHA           string
	payouts             map[string]int64
	parts               *partRows
}

// scanProjection streams every exported row once. It applies the loader's own
// row checks and adds the two the loader leaves to the source owner: no tax
// row may sit in the tail above the migration height, and every restart
// payout row must sit at the migration height.
func scanProjection(tables []Projection, b finalBounds, genesisSHA string, parts []SealedComponent) (projectionFacts, error) {
	facts := projectionFacts{payouts: map[string]int64{}, parts: newPartRows(parts)}
	if len(tables) != len(inserts) {
		return facts, refuse("staging projection", "ten tables required")
	}
	m := Manifest{Last: b.last, Canonical: b.canonical, Scope: Scope{ChainID: b.chain}}
	payoutHash := sha256.New()
	keys := map[[5]int64]bool{}
	genesisRows := 0
	for n, t := range inserts {
		p := tables[n]
		name := "staging projection table " + t.name
		if p.Table != t.name || strings.Join(p.Columns, ",") != t.columns {
			return facts, refuse(name, "table order or columns differ from the loader's")
		}
		f, e := openPinned(p.File)
		if e != nil {
			return facts, refuse(name, "file missing")
		}
		stream := sha256.New()
		sc := bufio.NewScanner(io.TeeReader(f, stream))
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		var rows int64
		for sc.Scan() {
			raw := sc.Bytes()
			rows++
			if e = validateRow(raw, t, m); e != nil {
				f.Close()
				return facts, refuse(name, "row %d: %v", rows, e)
			}
			switch t.name {
			case "blocks":
				var row struct {
					Height int64 `json:"height"`
				}
				facts.headers++
				if json.Unmarshal(raw, &row) != nil || row.Height != facts.headers {
					f.Close()
					return facts, refuse(name, "block %d is missing or duplicated", facts.headers)
				}
				if !facts.parts.add(t.name, row.Height, raw) {
					f.Close()
					return facts, refuse(name, "row %d sits at a height no listed part covers", rows)
				}
			case "tx_inclusions", "block_events":
				var row struct {
					Height int64 `json:"height"`
				}
				if json.Unmarshal(raw, &row) != nil || !facts.parts.add(t.name, row.Height, raw) {
					f.Close()
					return facts, refuse(name, "row %d sits at a height no listed part covers", rows)
				}
			case "genesis_files":
				var row struct {
					SHA string `json:"sha256"`
				}
				genesisRows++
				if json.Unmarshal(raw, &row) != nil || row.SHA != genesisSHA {
					f.Close()
					return facts, refuse(name, "history was built from a different genesis file")
				}
			case "taxable_events":
				var row payoutRow
				if json.Unmarshal(raw, &row) != nil {
					f.Close()
					return facts, refuse(name, "row %d unreadable", rows)
				}
				if row.Height > b.canonical {
					f.Close()
					return facts, refuse(name, "a tax row sits in the tail above the migration height %d", b.canonical)
				}
				if (row.Phase == 4) != (row.Source == "export_payout") {
					f.Close()
					return facts, refuse(name, "a restart payout row is mislabelled")
				}
				if row.Phase == 4 {
					k := [5]int64{row.Height, int64(row.Phase), row.Tx, row.Msg, row.Sub}
					if row.Height != b.canonical || keys[k] {
						f.Close()
						return facts, refuse(name, "a restart payout row is duplicated or not at the migration height %d", b.canonical)
					}
					keys[k] = true
					facts.payoutRows++
					facts.payouts[payoutKey(row.Category, row.Validator, row.To, row.Amount, row.Denom)]++
					payoutHash.Write(raw)
					payoutHash.Write([]byte("\n"))
				} else if !facts.parts.add(t.name, row.Height, raw) {
					f.Close()
					return facts, refuse(name, "row %d sits at a height no listed part covers", rows)
				}
			}
		}
		e = sc.Err()
		f.Close()
		if e != nil {
			return facts, refuse(name, "unreadable")
		}
		if rows != p.Rows || hex.EncodeToString(stream.Sum(nil)) != p.File.SHA256 {
			return facts, refuse(name, "row count or bytes differ from the export index")
		}
	}
	if genesisRows != 1 {
		return facts, refuse("staging projection table genesis_files", "exactly one genesis row required")
	}
	if facts.headers != b.last {
		return facts, refuse("staging projection table blocks", "history holds %d blocks, the chain has %d", facts.headers, b.last)
	}
	facts.payoutSHA = hex.EncodeToString(payoutHash.Sum(nil))
	return facts, nil
}

type exportRecord struct {
	Validator string `json:"validator"`
	Delegator string `json:"delegator"`
	Kind      string `json:"kind"`
	Code      int64  `json:"code"`
	Height    int64  `json:"height"`
	Tx        int64  `json:"tx"`
	Message   int64  `json:"message"`
	Phase     string `json:"phase"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	Address   string `json:"address"`
	Denom     string `json:"denom"`
	Amount    string `json:"amount"`
	// The native observer (Hub 1) also records store evidence and a
	// classification on each movement. Hub 2's export carries none of these.
	Unit            string          `json:"unit,omitempty"`
	Before          json.RawMessage `json:"before,omitempty"`
	After           json.RawMessage `json:"after,omitempty"`
	Classification  string          `json:"classification,omitempty"`
	EvidenceQuality string          `json:"evidence_quality,omitempty"`
}

// exportEvidenceUnits are the native evidence kinds an observed export
// carries beside its payouts, with the one unit each is written in. They
// move no tokens; the importer reads them as evidence the same way.
var exportEvidenceUnits = map[string]string{"native_state": "native_store_bytes", "native_state_checkpoint": "native_store_bytes", "native_state_hash": "native_store_sha256", "native_execution": "native_execution_evidence"}

func present(m json.RawMessage) bool { return len(m) != 0 && string(m) != "null" }

// readPayoutRecords reads the native record of what the migration-height
// export actually paid out. Each paid reward and commission appears once.
func readPayoutRecords(records, proof Pin, b finalBounds) (map[string]int64, int64, error) {
	p, e := pinnedObject(proof, "restart payout proof")
	if e != nil {
		return nil, 0, e
	}
	from, ok1 := whole(p, "from")
	through, ok2 := whole(p, "through")
	verified, ok3 := whole(p, "state_hash_verified_through")
	if p["chain"] != b.chain || p["zero_height_export"] != true || !ok1 || !ok2 || !ok3 || from != b.canonical || through != b.canonical || verified != b.canonical {
		return nil, 0, refuse("restart payout proof", "not the export at the migration height %d", b.canonical)
	}
	if p["records_sha256"] != records.SHA256 {
		return nil, 0, refuse("restart payout records", "not the records the proof names")
	}
	f, e := openPinned(records)
	if e != nil {
		return nil, 0, refuse("restart payout records", "file missing")
	}
	defer f.Close()
	h := sha256.New()
	sc := bufio.NewScanner(io.TeeReader(f, h))
	// A store checkpoint line can run to several MiB.
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	out := map[string]int64{}
	var count, completed, accounting int64
	for sc.Scan() {
		d := json.NewDecoder(strings.NewReader(sc.Text()))
		d.DisallowUnknownFields()
		var r exportRecord
		// A native_execution record keeps the result code the chain returned
		// (a refused withdrawal pays nothing and has no movement); every other
		// record must be a success.
		if d.Decode(&r) != nil || r.Height != b.canonical || r.Phase != "zero_height_export" || r.Status != "exported" || r.Tx != -1 || r.Message != -1 || (r.Code != 0 && r.Kind != "native_execution") {
			return nil, 0, refuse("restart payout records", "a record is not from the migration-height export")
		}
		if unit, evidence := exportEvidenceUnits[r.Kind]; evidence {
			if r.Unit != unit {
				return nil, 0, refuse("restart payout records", "native evidence in another unit")
			}
			continue
		}
		if (r.Kind == "movement" && r.Unit != "" && r.Unit != "native_bank_tokens") || (r.Kind != "movement" && (r.Unit != "" || present(r.Before) || present(r.After) || r.Classification != "" || r.EvidenceQuality != "")) {
			return nil, 0, refuse("restart payout records", "an unknown or malformed payout record")
		}
		switch r.Kind {
		case "accounting":
			accounting++
		case "export_completed":
			completed++
		case "movement":
			category := map[string]string{"validator_commission/bank.add": "commission", "delegator_reward/bank.add": "reward"}[r.Label]
			if category == "" || r.Validator == "" || r.Denom == "" || r.Amount == "" || strings.Trim(r.Amount, "0123456789") != "" || strings.TrimLeft(r.Amount, "0") != r.Amount || (category == "reward") != (r.Delegator != "") {
				return nil, 0, refuse("restart payout records", "an unknown or malformed payout record")
			}
			out[payoutKey(category, r.Validator, r.Delegator, r.Amount, r.Denom)]++
			count++
		default:
			return nil, 0, refuse("restart payout records", "an unknown record kind")
		}
	}
	if sc.Err() != nil || hex.EncodeToString(h.Sum(nil)) != records.SHA256 {
		return nil, 0, refuse("restart payout records", "bytes differ from the pinned hash")
	}
	if completed != 1 || accounting < 1 {
		return nil, 0, refuse("restart payout records", "the export did not record its accounting and exactly one completion")
	}
	return out, count, nil
}

// FinalReport is the whole-chain acceptance the loader reads. Field names the
// loader checks are kept exactly; the rest records every input by hash.
type FinalReport struct {
	Schema                 string            `json:"schema"`
	Chain                  string            `json:"chain"`
	Accepted               bool              `json:"full_chain_books_tax_acceptance"`
	FixtureBounds          bool              `json:"fixture_bounds"`
	FixtureChecksPassed    bool              `json:"fixture_checks_passed"`
	SourcePreserved        bool              `json:"source_preserved"`
	First                  int64             `json:"first"`
	Last                   int64             `json:"last"`
	Held                   int64             `json:"held"`
	Canonical              int64             `json:"canonical"`
	TailFirst              int64             `json:"tail_first"`
	TailLast               int64             `json:"tail_last"`
	CanonicalExportSHA     string            `json:"canonical_export_sha256"`
	ProjectionsSHA         string            `json:"projections_sha"`
	GenesisSHA             string            `json:"genesis_sha"`
	SourceSHA              string            `json:"source_sha"`
	ComponentsSHA          string            `json:"components_sha"`
	BuildSHA               string            `json:"build_sha"`
	BinarySHA              string            `json:"binary_sha"`
	NextGenesisSHA         string            `json:"next_genesis_sha"`
	MigratedExportSHA      string            `json:"migrated_export_sha,omitempty"`
	MigrationBinarySHA     string            `json:"migration_binary_sha,omitempty"`
	CanonicalRows          int64             `json:"canonical_rows"`
	CanonicalProjectionSHA string            `json:"canonical_projection_sha"`
	StagingReceiptSHA      string            `json:"staging_receipt_sha256"`
	StagingFingerprintSHA  string            `json:"staging_fingerprint_all_tables_sha256"`
	PayoutRecords          int64             `json:"restart_payout_records"`
	PayoutMultisetSHA      string            `json:"restart_payout_multiset_sha"`
	TaxRowsAboveCanonical  int64             `json:"tax_rows_above_canonical"`
	Balance                BalanceComparison `json:"all_account_all_coin_comparison"`
	Seams                  []int64           `json:"seams"`
	Sealed                 []SealedComponent `json:"sealed_components"`
	Components             []Component       `json:"components"`
	Tables                 []Projection      `json:"tables"`
	Staging                Identity          `json:"staging"`
	Inputs                 map[string]Pin    `json:"inputs"`
	Outputs                map[string]Pin    `json:"outputs"`
	BuildCommit            string            `json:"build_commit"`
	FinalizedUTC           string            `json:"finalized_utc"`
}

// WriteNew writes one new private JSON file and returns its pin. It never
// replaces an existing file.
func WriteNew(path string, v any) (Pin, error) { return writeNew(path, v) }
func writeNew(path string, v any) (Pin, error) {
	b, e := json.MarshalIndent(v, "", " ")
	if e != nil {
		return Pin{}, e
	}
	b = append(b, '\n')
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return Pin{}, e
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	if c := f.Close(); e == nil {
		e = c
	}
	if e != nil {
		return Pin{}, e
	}
	sum := sha256.Sum256(b)
	return Pin{path, hex.EncodeToString(sum[:])}, nil
}

// Finalize checks one chain's complete evidence and, only when every check
// passes, writes the whole-chain acceptance into a NEW directory. Any failure
// returns an error naming the failing input and writes no acceptance.
func Finalize(ctx context.Context, spec FinalizeSpec, dir string) (FinalReport, Pin, error) {
	b, e := approvedBounds(spec.Chain)
	if e != nil {
		return FinalReport{}, Pin{}, e
	}
	return finalize(ctx, spec, dir, b)
}
func finalize(ctx context.Context, spec FinalizeSpec, dir string, b finalBounds) (FinalReport, Pin, error) {
	none := FinalReport{}
	if !filepath.IsAbs(dir) {
		return none, Pin{}, refuse("output directory", "absolute NEW directory required")
	}
	chain, e := verifyComponents(spec, b)
	if e != nil {
		return none, Pin{}, e
	}
	if chain.Through != b.last {
		return none, Pin{}, refuse("component list", "sealed history reaches height %d, the chain ends at %d", chain.Through, b.last)
	}
	var canonicalPart SealedComponent
	for _, c := range chain.Components {
		if c.Last == b.canonical {
			canonicalPart = c
		}
	}
	if canonicalPart.Last != b.canonical {
		return none, Pin{}, refuse("component list", "no part ends at the migration height %d", b.canonical)
	}
	inputs := map[string]Pin{"genesis": spec.Genesis, "canonical_export": spec.CanonicalExport, "payout_records": spec.PayoutRecords, "payout_proof": spec.PayoutProof, "next_genesis": spec.NextGenesis, "projection_index": spec.Projection, "loader_binary": spec.LoaderBinary}
	if spec.Chain == "cosmoshub-1" {
		if spec.Join == nil || spec.CanonicalProof == nil {
			return none, Pin{}, refuse("input list", "Hub 1 needs its whole-chain join receipt and its migration-height state proof")
		}
		j, e := pinnedObject(*spec.Join, "join receipt")
		if e != nil {
			return none, Pin{}, e
		}
		first, ok1 := whole(j, "first")
		through, ok2 := whole(j, "through")
		at, ok3 := whole(j, "canonical_migration_height")
		seals, _ := j["profile_seals"].([]any)
		parts := chain.Components[:len(chain.Components)-1]
		if j["schema"] != "native033-whole-source-financial-reconciliation-v1" || !ok1 || !ok2 || !ok3 || first != 1 || through != b.last || at != b.canonical || j["all_historical_sources_and_coinflows_reconciled"] != true || len(seals) != len(parts) || j["accepted_terminal_seal_sha256"] != chain.Components[len(chain.Components)-1].Acceptance.SHA256 {
			return none, Pin{}, refuse("join receipt", "does not join exactly the listed parts and tail")
		}
		for n, s := range seals {
			o, _ := s.(map[string]any)
			if o == nil || o["sha256"] != parts[n].Acceptance.SHA256 {
				return none, Pin{}, refuse("join receipt", "part %d differs from the listed seal", n+1)
			}
		}
		inputs["join"] = *spec.Join
	}
	if spec.CanonicalProof != nil {
		hash, e := headerProof(*spec.CanonicalProof, "migration-height state proof", spec.Chain, b.canonical)
		if e != nil {
			return none, Pin{}, e
		}
		if hash != canonicalPart.Closing {
			return none, Pin{}, refuse("migration-height state proof", "state differs from the end state of the part ending at %d", b.canonical)
		}
		inputs["canonical_proof"] = *spec.CanonicalProof
	}
	var index ProjectionIndex
	if e = readPinnedJSON(spec.Projection, &index); e != nil {
		return none, Pin{}, refuse("staging projection index", "missing, changed or malformed")
	}
	if index.Schema != ProjectionIndexSchema || index.Chain != spec.Chain || !strings.HasPrefix(index.Staging.Database, "legacy_replay_rehearsal_") {
		return none, Pin{}, refuse("staging projection index", "not a private staging export of %s", spec.Chain)
	}
	if e = ctx.Err(); e != nil {
		return none, Pin{}, e
	}
	facts, e := scanProjection(index.Tables, b, spec.Genesis.SHA256, chain.Components)
	if e != nil {
		return none, Pin{}, e
	}
	// The canonical export is what the old chain handed to the new one.
	_, exportRoot, e := readAllAccountCoins(spec.CanonicalExport)
	if e != nil {
		return none, Pin{}, refuse("canonical export", "%v", e)
	}
	if at, ok := whole(exportRoot, "source_height"); !ok || at != b.canonical || exportRoot["for_zero_height"] != true {
		return none, Pin{}, refuse("canonical export", "not the restart export taken at the migration height %d", b.canonical)
	}
	binding := canonicalPart.Acceptance
	if spec.ExportBinding != nil {
		binding = *spec.ExportBinding
		inputs["export_binding"] = binding
	}
	bound, e := pinnedObject(binding, "export binding")
	if e != nil {
		return none, Pin{}, e
	}
	if app, ok := exportRoot["source_app_hash"].(string); ok && app != canonicalPart.Closing {
		return none, Pin{}, refuse("canonical export", "taken from a different state than the end of the part ending at %d", b.canonical)
	}
	if !containsString(bound, spec.CanonicalExport.SHA256) {
		return none, Pin{}, refuse("canonical export", "its hash is not named by the sealed evidence for the migration height")
	}
	records, recordCount, e := readPayoutRecords(spec.PayoutRecords, spec.PayoutProof, b)
	if e != nil {
		return none, Pin{}, e
	}
	if !containsString(bound, spec.PayoutRecords.SHA256) && !containsString(bound, spec.PayoutProof.SHA256) {
		return none, Pin{}, refuse("restart payout records", "their hash is not named by the sealed evidence for the migration height")
	}
	if recordCount < 1 || facts.payoutRows != recordCount || multisetSHA(facts.payouts) != multisetSHA(records) {
		return none, Pin{}, refuse("restart payout", "history holds %d payout rows, the export paid %d: the payout must be included exactly once", facts.payoutRows, recordCount)
	}
	if spec.NextGenesis.SHA256 != b.nextGenesisSHA {
		return none, Pin{}, refuse("next genesis", "hash is not the genuine %s starting genesis", b.next)
	}
	_, nextRoot, e := readAllAccountCoins(spec.NextGenesis)
	if e != nil {
		return none, Pin{}, refuse("next genesis", "%v", e)
	}
	if nextRoot["chain_id"] != b.next {
		return none, Pin{}, refuse("next genesis", "file is for a different chain")
	}
	comparisonExport := spec.CanonicalExport
	if b.migrates {
		if spec.MigratedExport == nil || spec.MigrationBinary == nil {
			return none, Pin{}, refuse("input list", "Hub 2 needs the migrated export and the migration helper")
		}
		if spec.MigrationBinary.SHA256 != b.migrationBinarySHA || hashFile(*spec.MigrationBinary) != nil {
			return none, Pin{}, refuse("migration helper", "not the reviewed hub2-migrate036 build")
		}
		if e = hashFile(*spec.MigratedExport); e != nil {
			return none, Pin{}, refuse("migrated export", "file missing or changed")
		}
		run, cancel := context.WithTimeout(ctx, 5*time.Minute)
		cmd := exec.CommandContext(run, spec.MigrationBinary.Path, "-input", spec.CanonicalExport.Path, "-sha256", spec.CanonicalExport.SHA256, "-check-output", spec.MigratedExport.Path, "-check-sha256", spec.MigratedExport.SHA256)
		cmd.Env = []string{"GOMAXPROCS=2", "GOMEMLIMIT=512MiB"}
		e = cmd.Run()
		cancel()
		if e != nil {
			return none, Pin{}, refuse("migrated export", "the official 0.34 to 0.36 migration of the canonical export does not reproduce it")
		}
		comparisonExport = *spec.MigratedExport
		inputs["migrated_export"], inputs["migration_binary"] = *spec.MigratedExport, *spec.MigrationBinary
	} else if spec.MigratedExport != nil || spec.MigrationBinary != nil {
		return none, Pin{}, refuse("input list", "Hub 1 has no migration step")
	}
	balance, e := CompareAllAccounts(comparisonExport, spec.NextGenesis)
	if e != nil {
		return none, Pin{}, refuse("balance comparison", "%v", e)
	}
	if !balance.Pass {
		return none, Pin{}, refuse("balance comparison", "%d accounts checked against the %s starting balances: %d missing, %d extra, %d coin amounts differ", balance.AccountsChecked, b.next, balance.Missing, balance.Extra, balance.MismatchedDenominations)
	}
	if !commitRE.MatchString(spec.BuildCommit) || hashFile(spec.LoaderBinary) != nil {
		return none, Pin{}, refuse("loader build", "loader binary missing or build commit not a full commit id")
	}
	// Without the importer's receipt nothing ties these rows to the seals:
	// any well-formed projection would pass under the same sealed parts.
	if spec.StagingReceipt == nil {
		return none, Pin{}, refuse("staging receipt", "required: the staging importer's receipt is what ties the exported rows to the sealed parts")
	}
	receipt, e := checkStagingReceipt(*spec.StagingReceipt, spec, chain.Components, index.Tables, facts, b)
	if e != nil {
		return none, Pin{}, e
	}
	inputs["staging_receipt"] = *spec.StagingReceipt
	if e = os.Mkdir(dir, 0700); e != nil {
		return none, Pin{}, refuse("output directory", "must not exist yet")
	}
	report := FinalReport{Schema: FinalizationSchema, Chain: spec.Chain, FixtureBounds: !b.approved, SourcePreserved: true, First: 1, Last: b.last, Canonical: b.canonical, TailFirst: b.canonical + 1, TailLast: b.last,
		CanonicalExportSHA: spec.CanonicalExport.SHA256, GenesisSHA: spec.Genesis.SHA256, NextGenesisSHA: spec.NextGenesis.SHA256, CanonicalRows: facts.payoutRows, CanonicalProjectionSHA: facts.payoutSHA,
		PayoutRecords: recordCount, PayoutMultisetSHA: multisetSHA(records), Balance: balance, Seams: chain.Seams, Sealed: chain.Components, Tables: index.Tables, Staging: index.Staging, Inputs: inputs, Outputs: map[string]Pin{}, BuildCommit: spec.BuildCommit, FinalizedUTC: time.Now().UTC().Format(time.RFC3339)}
	type preservedEntry struct {
		First, Last int64
		Receipt     Pin
		Preserved   bool
	}
	var source []preservedEntry
	for _, c := range chain.Components {
		report.Components = append(report.Components, Component{c.First, c.Last, c.Acceptance, c.Opening, c.Closing, true})
		report.Held += c.Held
		report.SourcePreserved = report.SourcePreserved && c.SourcePreserved
		source = append(source, preservedEntry{c.First, c.Last, c.Acceptance, c.SourcePreserved})
	}
	if report.Held != 0 || !report.SourcePreserved {
		return none, Pin{}, refuse("component list", "a part holds heights back or lost its source")
	}
	sourcePin, e := writeNew(filepath.Join(dir, "source-preservation.json"), map[string]any{"schema": "legacy-whole-chain-source-preservation-v1", "chain": spec.Chain, "components": source})
	if e != nil {
		return none, Pin{}, e
	}
	commit := sha256.Sum256([]byte(spec.BuildCommit))
	report.BuildSHA, report.BinarySHA = hex.EncodeToString(commit[:]), spec.LoaderBinary.SHA256
	buildPin, e := writeNew(filepath.Join(dir, "build-identity.json"), map[string]any{"schema": "legacy-whole-chain-build-identity-v1", "build_commit": spec.BuildCommit, "build_sha": report.BuildSHA, "loader_binary": spec.LoaderBinary})
	if e != nil {
		return none, Pin{}, e
	}
	report.StagingReceiptSHA, report.StagingFingerprintSHA = spec.StagingReceipt.SHA256, receipt.Fingerprint
	report.SourceSHA, report.ComponentsSHA, report.ProjectionsSHA = sourcePin.SHA256, valueSHA(report.Components), valueSHA(report.Tables)
	report.Outputs["source"], report.Outputs["build_identity"] = sourcePin, buildPin
	if b.migrates {
		report.MigratedExportSHA, report.MigrationBinarySHA = spec.MigratedExport.SHA256, spec.MigrationBinary.SHA256
	}
	// Reduced test boundaries can prove the checks, never a real chain.
	report.Accepted, report.FixtureChecksPassed = b.approved, !b.approved
	pin, e := writeNew(filepath.Join(dir, "finalization-report.json"), report)
	if e != nil {
		return none, Pin{}, e
	}
	return report, pin, nil
}

// IsRefusal reports whether an error is a finalizer refusal whose text is safe
// to show: it names the failing input and never a wallet or an amount.
func IsRefusal(e error) bool {
	var r refusal
	return errors.As(e, &r)
}
