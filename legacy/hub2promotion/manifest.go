package hub2promotion

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const FixtureProfile = "SYNTHETIC-ONLY-hub2-atomic-v1"
const CompleteProfile = "hub2-complete-native-allcoin-v1"
const PrivateScaleProfile = "PRIVATE-FIXTURE-prefix1400000-atomic-v1"

var shaRE = regexp.MustCompile(`^[a-f0-9]{64}$`)
var objectRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

type Pin struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type Component struct {
	First          int64  `json:"first"`
	Last           int64  `json:"last"`
	Receipt        Pin    `json:"receipt"`
	Opening        string `json:"opening"`
	Closing        string `json:"closing"`
	HistoricalOnly bool   `json:"historical_only"`
}
type Projection struct {
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
	Rows    int64    `json:"rows"`
	File    Pin      `json:"file"`
}
type Identity struct {
	Database      string `json:"database"`
	OID           uint32 `json:"oid"`
	SystemHash    string `json:"system_hash"`
	DeploymentUID string `json:"deployment_uid"`
	PodUID        string `json:"pod_uid"`
	PVCUID        string `json:"pvc_uid"`
	Endpoint      string `json:"endpoint"`
	CatalogSHA    string `json:"catalog_sha"`
	ScopeSHA      string `json:"scope_sha"`
}
type FinancialReceipt struct {
	Version                int                            `json:"version"`
	Profile                string                         `json:"profile"`
	First                  int64                          `json:"first"`
	Last                   int64                          `json:"last"`
	Canonical              int64                          `json:"canonical"`
	Hub3GenesisSHA         string                         `json:"hub3_genesis_sha"`
	Complete               bool                           `json:"complete"`
	AllCoins               bool                           `json:"all_coins"`
	SourcePreserved        bool                           `json:"source_preserved"`
	Modules                map[string]Pin                 `json:"modules"`
	Stores                 map[string]Pin                 `json:"stores"`
	Checks                 map[string]Pin                 `json:"checks"`
	AcceptedArtifacts      map[string]ArtifactAttestation `json:"accepted_artifacts"`
	CanonicalRows          int64                          `json:"canonical_rows"`
	CanonicalProjectionSHA string                         `json:"canonical_projection_sha"`
	GenesisSHA             string                         `json:"genesis_sha"`
	InitialRoot            string                         `json:"initial_root"`
	SourceSHA              string                         `json:"source_sha"`
	BuildSHA               string                         `json:"build_sha"`
	BuildIdentitySHA       string                         `json:"build_identity_sha"`
	BinarySHA              string                         `json:"binary_sha"`
	ComponentsSHA          string                         `json:"components_sha"`
	ProjectionsSHA         string                         `json:"projections_sha"`
	MigrationLast          int64                          `json:"migration_last"`
	TailFirst              int64                          `json:"tail_first"`
	TailLast               int64                          `json:"tail_last"`
}

// SDK034 has no separate bank/supply persistent store. Account coins and
// module supply are nevertheless required independently as financial checks.
var requiredModules = []string{"auth", "bank", "staking", "distribution", "mint", "slashing", "gov", "params", "crisis"}
var requiredStores = []string{"main", "acc", "staking", "mint", "distr", "slashing", "gov", "fee", "params"}
var requiredChecks = []string{"all_coin_balances", "fees", "staking_principal_shares", "rewards_commission_liabilities", "module_supply", "raw_typed_provenance", "canonical_payout", "hub3_genesis_equality", "tail_excluded_from_migration", "source_preservation", "native_binary_image", "gas_profile", "component_seams", "source_cache_raw", "initial_genesis"}

type BuildReceipt struct {
	Version        int    `json:"version"`
	Profile        string `json:"profile"`
	Binary         Pin    `json:"binary"`
	SourceSHA      string `json:"source_sha"`
	SourceFiles    []Pin  `json:"source_files"`
	SignedCommit   string `json:"signed_commit"`
	VCSRevision    string `json:"vcs_revision"`
	VCSModified    bool   `json:"vcs_modified"`
	NativeBinaries []Pin  `json:"native_binaries"`
	GasProfile     Pin    `json:"gas_profile"`
	ImageDigest    string `json:"image_digest"`
}

func (b BuildReceipt) validate(m Manifest) error {
	commitRE := regexp.MustCompile(`^[a-f0-9]{40}$`)
	if b.Version != 1 || b.Profile != CompleteProfile || !commitRE.MatchString(b.SignedCommit) || !commitRE.MatchString(b.VCSRevision) || (b.VCSModified || b.VCSRevision != b.SignedCommit) || b.Binary.SHA256 != m.BinarySHA || b.SourceSHA != m.BuildSHA || !shaRE.MatchString(strings.TrimPrefix(b.ImageDigest, "sha256:")) || !strings.HasPrefix(b.ImageDigest, "sha256:") || len(b.SourceFiles) == 0 || len(b.NativeBinaries) == 0 {
		return errors.New("incomplete/counterfeit build identity")
	}
	if e := hashFile(b.Binary); e != nil {
		return e
	}
	binaryBytes, e := readPinnedBytes(b.Binary, 256<<20)
	if e != nil {
		return e
	}
	info, e := buildinfo.Read(bytes.NewReader(binaryBytes))
	if e != nil {
		return errors.New("binary build metadata absent")
	}
	settings := map[string]string{}
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	if settings["vcs.revision"] != b.SignedCommit || settings["vcs.modified"] != "false" {
		return errors.New("actual binary is not sealed clean signed source")
	}
	if e := hashFile(b.GasProfile); e != nil {
		return e
	}
	files := append([]Pin(nil), b.SourceFiles...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	seen := map[string]bool{}
	for _, p := range append(files, b.NativeBinaries...) {
		if seen[p.Path] {
			return errors.New("duplicate build file")
		}
		seen[p.Path] = true
		if e := hashFile(p); e != nil {
			return e
		}
	}
	data, _ := json.Marshal(files)
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != b.SourceSHA {
		return errors.New("build source closure mismatch")
	}
	return nil
}

type Manifest struct {
	Version                int           `json:"version"`
	Profile                string        `json:"profile"`
	Fixture                bool          `json:"fixture"`
	First                  int64         `json:"first"`
	Last                   int64         `json:"last"`
	Canonical              int64         `json:"canonical"`
	MigrationLast          int64         `json:"migration_last"`
	TailFirst              int64         `json:"tail_first"`
	TailLast               int64         `json:"tail_last"`
	BuildSHA               string        `json:"build_sha"`
	BinarySHA              string        `json:"binary_sha"`
	BuildIdentity          Pin           `json:"build_identity"`
	Source                 Pin           `json:"source"`
	Financial              Pin           `json:"financial"`
	FinancialSignature     Pin           `json:"financial_signature"`
	Genesis                Pin           `json:"genesis"`
	Hub3GenesisSHA         string        `json:"hub3_genesis_sha"`
	Hub4GenesisSHA         string        `json:"hub4_genesis_sha"`
	Destination            Identity      `json:"destination"`
	Scope                  Scope         `json:"scope"`
	Components             []Component   `json:"components"`
	Tables                 []Projection  `json:"tables"`
	Protected              Snapshot      `json:"protected_non_hub2_physical"`
	CanonicalRows          int64         `json:"canonical_rows"`
	CanonicalProjectionSHA string        `json:"canonical_projection_sha"`
	Approved               *ApprovedLoad `json:"approved_load,omitempty"`
}

func readPinnedBytes(p Pin, limit int64) ([]byte, error) {
	f, e := openPinned(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > limit {
		return nil, errors.New("pinned artifact too large")
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != p.SHA256 {
		return nil, errors.New("parsed artifact SHA mismatch")
	}
	return b, nil
}
func readPinnedJSON(p Pin, v any) error {
	b, e := readPinnedBytes(p, 16<<20)
	if e != nil {
		return e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func ReadManifestPinned(path, sha string) (Manifest, error) {
	var m Manifest
	e := readPinnedJSON(Pin{path, sha}, &m)
	return m, e
}
func openPinned(p Pin) (*os.File, error) {
	if !shaRE.MatchString(p.SHA256) || !filepath.IsAbs(p.Path) {
		return nil, errors.New("unbound file pin")
	}
	st, e := os.Lstat(p.Path)
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("pin is not a regular file")
	}
	f, e := os.Open(p.Path)
	if e != nil {
		return nil, e
	}
	opened, e := f.Stat()
	if e != nil || !os.SameFile(st, opened) {
		f.Close()
		return nil, errors.New("pin changed before open")
	}
	return f, nil
}
func hashFile(p Pin) error {
	if !shaRE.MatchString(p.SHA256) || !filepath.IsAbs(p.Path) {
		return errors.New("unbound file pin")
	}
	st, e := os.Lstat(p.Path)
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() {
		return errors.New("pin is not a regular file")
	}
	f, e := os.Open(p.Path)
	if e != nil {
		return e
	}
	defer f.Close()
	opened, e := f.Stat()
	if e != nil || !os.SameFile(st, opened) {
		return errors.New("pin changed before open")
	}
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return e
	}
	if hex.EncodeToString(h.Sum(nil)) != p.SHA256 {
		return errors.New("source SHA mismatch")
	}
	return nil
}
func validateIdentity(i Identity) error {
	if i.Database == "" || i.OID == 0 || !shaRE.MatchString(i.SystemHash) || !shaRE.MatchString(i.CatalogSHA) || !shaRE.MatchString(i.ScopeSHA) || i.DeploymentUID == "" || i.PodUID == "" || i.PVCUID == "" || i.Endpoint == "" {
		return errors.New("destination identity incomplete")
	}
	return nil
}

// ValidateComplete never accepts a fixture. Validation does not authorize execution.
// Component receipts are pinned inputs owned by the independent financial pipeline.
func (m Manifest) ValidateComplete() error {
	if m.Profile != CompleteProfile || m.Scope.chain() != Chain {
		return errors.New("original Hub2 complete profile required")
	}
	return m.validate(false)
}

// ValidateFixture is deliberately separate and cannot attest whole-chain coverage.
func (m Manifest) ValidateFixture() error {
	if m.Profile != FixtureProfile {
		return errors.New("synthetic fixture profile required")
	}
	return m.validate(true)
}
func (m Manifest) ValidatePrivateScale() error {
	if m.Profile != PrivateScaleProfile {
		return errors.New("private prefix scale profile required")
	}
	return m.validate(true)
}
func (m Manifest) validate(fixture bool) error {
	scale := fixture && m.Profile == PrivateScaleProfile
	approved := !fixture && m.Profile == ApprovedProfile
	if m.Version != 1 || m.Fixture != fixture || m.First != 1 || m.Last < m.First {
		return errors.New("wrong version/coverage/mode")
	}
	if scale {
		if m.Last != 1400000 || m.Canonical != 0 || m.MigrationLast != 0 || m.TailFirst != 0 || m.TailLast != 0 || m.CanonicalRows != 0 || m.CanonicalProjectionSHA != valueEmptySHA() {
			return errors.New("private prefix is not a canonical migration")
		}
	} else if !approved && (m.Canonical < 1 || m.MigrationLast != m.Canonical || m.TailFirst != m.Canonical+1 || m.TailLast != m.Last) {
		return errors.New("wrong canonical/tail policy")
	}
	if fixture {
		if (!scale && (m.Profile != FixtureProfile || m.Last > 100)) || !strings.HasPrefix(m.Destination.Database, "legacy_replay_rehearsal_") {
			return errors.New("fixture profile/destination refused")
		}
	} else if !approved && (m.Scope.chain() != Chain || m.Profile != CompleteProfile || m.Last != FinalHeight || m.Canonical != CanonicalHeight) {
		return errors.New("complete chain receipt required")
	}
	if e := validateIdentity(m.Destination); e != nil {
		return e
	}
	if e := m.Scope.valid(); e != nil {
		return e
	}
	if len(m.Protected) != len(allTables) {
		return errors.New("all14 protected fingerprints required")
	}
	for _, t := range allTables {
		p, ok := m.Protected[t.name]
		if !ok || p.Rows < 0 || !shaRE.MatchString(p.SHA256) {
			return errors.New("invalid protected fingerprint")
		}
	}
	if !shaRE.MatchString(m.BuildSHA) || !shaRE.MatchString(m.BinarySHA) || !shaRE.MatchString(m.Hub3GenesisSHA) || !shaRE.MatchString(m.Hub4GenesisSHA) {
		return errors.New("missing code/binary/genesis pins")
	}
	for _, p := range []Pin{m.Source, m.Genesis, m.BuildIdentity, m.Financial} {
		if e := hashFile(p); e != nil {
			return e
		}
	}
	next := int64(1)
	var closing string
	for _, c := range m.Components {
		if c.First != next || c.Last < c.First || c.Last > m.Last || !c.HistoricalOnly || !shaRE.MatchString(c.Opening) || !shaRE.MatchString(c.Closing) || (closing != "" && closing != c.Opening) {
			return errors.New("component gap, duplicate or wrong join")
		}
		if e := hashFile(c.Receipt); e != nil {
			return e
		}
		next = c.Last + 1
		closing = c.Closing
	}
	if next != m.Last+1 {
		return errors.New("missing component tail")
	}
	if len(m.Tables) != len(inserts) {
		return errors.New("ten exact projection tables required")
	}
	for n, t := range inserts {
		p := m.Tables[n]
		if p.Table != t.name || strings.Join(p.Columns, ",") != t.columns || p.Rows < 0 {
			return fmt.Errorf("projection layout refused: %s", t.name)
		}
		if e := hashFile(p.File); e != nil {
			return e
		}
	}
	if m.Tables[0].Rows != 1 {
		return errors.New("one genuine genesis required")
	}
	if !fixture && !approved {
		var r FinancialReceipt
		if e := readPinnedJSON(m.Financial, &r); e != nil {
			return e
		}
		if r.GenesisSHA != m.Genesis.SHA256 || r.SourceSHA != m.Source.SHA256 || r.BuildSHA != m.BuildSHA || r.BuildIdentitySHA != m.BuildIdentity.SHA256 || r.BinarySHA != m.BinarySHA || r.ComponentsSHA != valueSHA(m.Components) || r.ProjectionsSHA != valueSHA(m.Tables) || !shaRE.MatchString(r.InitialRoot) || r.InitialRoot != m.Components[0].Opening || r.MigrationLast != m.MigrationLast || r.TailFirst != m.TailFirst || r.TailLast != m.TailLast {
			return errors.New("financial receipt/package bindings differ")
		}
		if r.Version != 1 || r.Profile != CompleteProfile || r.First != 1 || r.Last != FinalHeight || r.Canonical != CanonicalHeight || !r.Complete || !r.AllCoins || !r.SourcePreserved || r.Hub3GenesisSHA != m.Hub3GenesisSHA || r.CanonicalRows != m.CanonicalRows || r.CanonicalProjectionSHA != m.CanonicalProjectionSHA || m.CanonicalRows < 1 || !shaRE.MatchString(m.CanonicalProjectionSHA) {
			return errors.New("whole native/all-coin financial acceptance missing")
		}
		if len(r.Modules) != len(requiredModules) || len(r.Stores) != len(requiredStores) || len(r.Checks) != len(requiredChecks) {
			return errors.New("full native evidence set required")
		}
		for _, names := range []struct {
			names []string
			pins  map[string]Pin
		}{{requiredModules, r.Modules}, {requiredStores, r.Stores}, {requiredChecks, r.Checks}} {
			for _, name := range names.names {
				p, ok := names.pins[name]
				if !ok {
					return errors.New("required native pin absent")
				}
				if e := hashFile(p); e != nil {
					return e
				}
			}
		}
		var build BuildReceipt
		if e := readPinnedJSON(m.BuildIdentity, &build); e != nil {
			return e
		}
		if e := build.validate(m); e != nil {
			return e
		}
		if e := m.verifyFinancialAuthority(); e != nil {
			return e
		}
		if e := m.validateEvidence(r, build); e != nil {
			return e
		}
		if e := m.verifyOfflineProjection(); e != nil {
			return e
		}
	}
	return nil
}

// verifyOfflineProjection streams the actual SQL package, not just its labels.
// It is deliberately required for complete plans even when no DB is opened.
func (m Manifest) verifyOfflineProjection() error {
	var headers, payout int64
	payoutHash := sha256.New()
	for i, t := range inserts {
		p := m.Tables[i]
		f, e := openPinned(p.File)
		if e != nil {
			return e
		}
		streamHash := sha256.New()
		sc := bufio.NewScanner(io.TeeReader(f, streamHash))
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		var rows int64
		for sc.Scan() {
			raw := sc.Bytes()
			if e = validateRow(raw, t, m); e != nil {
				f.Close()
				return e
			}
			var row map[string]json.RawMessage
			if e = json.Unmarshal(raw, &row); e != nil {
				f.Close()
				return e
			}
			rows++
			if t.name == "blocks" {
				var height int64
				_ = json.Unmarshal(row["height"], &height)
				headers++
				if height != headers {
					f.Close()
					return errors.New("noncontiguous/duplicate offline header")
				}
			}
			if t.name == "genesis_files" {
				var sha string
				_ = json.Unmarshal(row["sha256"], &sha)
				if sha != m.Genesis.SHA256 {
					f.Close()
					return errors.New("initial genesis source mismatch")
				}
			}
			if t.name == "taxable_events" {
				var phase int
				_ = json.Unmarshal(row["phase"], &phase)
				if phase == 4 {
					payout++
					payoutHash.Write(raw)
					payoutHash.Write([]byte("\n"))
				}
			}
		}
		e = sc.Err()
		f.Close()
		if e != nil {
			return e
		}
		if rows != p.Rows || hex.EncodeToString(streamHash.Sum(nil)) != p.File.SHA256 {
			return errors.New("offline table count/tail/SHA mismatch")
		}
	}
	if headers != m.Last || payout != m.CanonicalRows || hex.EncodeToString(payoutHash.Sum(nil)) != m.CanonicalProjectionSHA {
		return errors.New("offline complete projection/payout mismatch")
	}
	return nil
}

// Recheck pins immediately before COMMIT as well as before streaming.
func (m Manifest) Recheck() error {
	if m.Profile == ApprovedProfile {
		return m.ValidateApproved()
	}
	if m.Fixture {
		if m.Profile == PrivateScaleProfile {
			return m.ValidatePrivateScale()
		}
		return m.ValidateFixture()
	}
	return m.ValidateComplete()
}
func valueEmptySHA() string { h := sha256.Sum256(nil); return hex.EncodeToString(h[:]) }
