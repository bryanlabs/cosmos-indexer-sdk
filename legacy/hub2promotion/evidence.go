package hub2promotion

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// EvidenceReceipt is a strict binding envelope produced from the independent
// owners' accepted proofs. Its artifacts retain those original proofs verbatim.
// It is not a substitute for running or accepting native financial proofs.
// Fixture/prefix receipts cannot masquerade as complete envelopes.
type EvidenceReceipt struct {
	Version                int    `json:"version"`
	Profile                string `json:"profile"`
	Fixture                bool   `json:"fixture"`
	Kind                   string `json:"kind"`
	Name                   string `json:"name"`
	Chain                  string `json:"chain"`
	First                  int64  `json:"first"`
	Last                   int64  `json:"last"`
	Complete               bool   `json:"complete"`
	AllCoins               bool   `json:"all_coins"`
	GenesisSHA             string `json:"genesis_sha"`
	SourceSHA              string `json:"source_sha"`
	BuildSHA               string `json:"build_sha"`
	BinarySHA              string `json:"binary_sha"`
	Opening                string `json:"opening"`
	Closing                string `json:"closing"`
	HistoricalOnly         bool   `json:"historical_only"`
	Canonical              int64  `json:"canonical"`
	MigrationLast          int64  `json:"migration_last"`
	TailFirst              int64  `json:"tail_first"`
	TailLast               int64  `json:"tail_last"`
	CanonicalRows          int64  `json:"canonical_rows"`
	CanonicalProjectionSHA string `json:"canonical_projection_sha"`
	Artifacts              []Pin  `json:"artifacts"`
	ComponentsSHA          string `json:"components_sha"`
	ImageDigest            string `json:"image_digest"`
	GasProfileSHA          string `json:"gas_profile_sha"`
}

func valueSHA(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type ArtifactAttestation struct {
	File   Pin      `json:"file"`
	Role   string   `json:"role"`
	Scopes []string `json:"scopes"`
}

func evidenceScope(kind, name string, first, last int64) string {
	return fmt.Sprintf("%s:%s:%d..%d", kind, name, first, last)
}

func (r EvidenceReceipt) validate(m Manifest, financial FinancialReceipt, build BuildReceipt, kind, name string, first, last int64) error {
	if r.Version != 1 || r.Profile != CompleteProfile || r.Fixture || r.Kind != kind || r.Name != name || r.Chain != Chain || r.First != first || r.Last != last || !r.Complete || !r.AllCoins || r.GenesisSHA != m.Genesis.SHA256 || r.BuildSHA != m.BuildSHA || r.BinarySHA != m.BinarySHA || r.Canonical != m.Canonical || r.MigrationLast != m.MigrationLast || r.TailFirst != m.TailFirst || r.TailLast != m.TailLast {
		return errors.New("partial/fixture/unbound native evidence envelope")
	}
	if kind == "source" {
		if r.SourceSHA != "" {
			return errors.New("source envelope must not self-reference")
		}
	} else if r.SourceSHA != m.Source.SHA256 {
		return errors.New("native source binding mismatch")
	}
	if len(r.Artifacts) == 0 {
		return errors.New("native proof artifacts absent")
	}
	seen := map[string]bool{}
	hasProof := false
	for _, p := range r.Artifacts {
		if seen[p.Path] {
			return errors.New("duplicate native artifact")
		}
		seen[p.Path] = true
		if e := hashFile(p); e != nil {
			return e
		}
		attested, ok := financial.AcceptedArtifacts[p.SHA256]
		if !ok || attested.File != p {
			return errors.New("artifact not attested by signed financial owner")
		}
		scopeOK := false
		for _, scope := range attested.Scopes {
			if scope == evidenceScope(kind, name, first, last) {
				scopeOK = true
			}
		}
		if !scopeOK {
			return errors.New("native artifact attested for different evidence scope")
		}
		if (attested.Role == "native-proof" || attested.Role == "source-evidence") && isPackagePin(m, financial, build, p) {
			return errors.New("package/input pin cannot substitute for native/source proof")
		}
		if attested.Role == "native-proof" {
			hasProof = true
		} else if attested.Role == "source-evidence" && kind == "source" {
			hasProof = true
		} else if attested.Role != "input" {
			return errors.New("invalid native artifact role")
		}
	}
	if !hasProof {
		return errors.New("scope has no owner-attested native proof")
	}
	if kind == "check" {
		switch name {
		case "initial_genesis":
			if r.Opening != financial.InitialRoot || r.Closing != financial.InitialRoot {
				return errors.New("initial native root binding mismatch")
			}
		case "component_seams":
			if r.Opening != financial.InitialRoot || r.Closing != m.Components[len(m.Components)-1].Closing || r.ComponentsSHA != financial.ComponentsSHA {
				return errors.New("native seam list binding mismatch")
			}
		case "native_binary_image":
			if r.ImageDigest != build.ImageDigest {
				return errors.New("native image binding mismatch")
			}
		case "gas_profile":
			if r.GasProfileSHA != build.GasProfile.SHA256 {
				return errors.New("native gas profile binding mismatch")
			}
		}
	}
	if kind != "component" && !(kind == "check" && (name == "initial_genesis" || name == "component_seams")) && (r.Opening != "" || r.Closing != "" || r.HistoricalOnly) {
		return errors.New("unrepresented native root/scope fields")
	}
	if kind == "check" && name == "canonical_payout" && (r.CanonicalRows != m.CanonicalRows || r.CanonicalProjectionSHA != m.CanonicalProjectionSHA) {
		return errors.New("canonical native payout evidence mismatch")
	}
	return nil
}
func isPackagePin(m Manifest, r FinancialReceipt, build BuildReceipt, p Pin) bool {
	pins := []Pin{m.Genesis, m.Source, m.Financial, m.FinancialSignature, m.BuildIdentity, build.Binary, build.GasProfile}
	pins = append(pins, build.SourceFiles...)
	pins = append(pins, build.NativeBinaries...)
	for _, group := range []map[string]Pin{r.Modules, r.Stores, r.Checks} {
		for _, v := range group {
			pins = append(pins, v)
		}
	}
	for _, c := range m.Components {
		pins = append(pins, c.Receipt)
	}
	for _, t := range m.Tables {
		pins = append(pins, t.File)
	}
	for _, v := range pins {
		if p.Path == v.Path || p.SHA256 == v.SHA256 {
			return true
		}
	}
	return false
}

func (m Manifest) validateEvidence(r FinancialReceipt, build BuildReceipt) error {
	seenPath, seenSHA := map[string]bool{}, map[string]bool{}
	distinct := func(p Pin) error {
		if seenPath[p.Path] || seenSHA[p.SHA256] {
			return errors.New("receipt reused across different evidence scopes")
		}
		seenPath[p.Path] = true
		seenSHA[p.SHA256] = true
		return nil
	}
	for _, p := range []Pin{m.Financial, m.BuildIdentity, m.Source} {
		if e := distinct(p); e != nil {
			return e
		}
	}
	var source EvidenceReceipt
	if e := readPinnedJSON(m.Source, &source); e != nil {
		return e
	}
	if e := source.validate(m, r, build, "source", "raw-cache", 1, FinalHeight); e != nil {
		return e
	}
	for _, c := range m.Components {
		if e := distinct(c.Receipt); e != nil {
			return e
		}
		var v EvidenceReceipt
		if e := readPinnedJSON(c.Receipt, &v); e != nil {
			return e
		}
		if e := v.validate(m, r, build, "component", "historical", c.First, c.Last); e != nil {
			return e
		}
		if !v.HistoricalOnly || v.Opening != c.Opening || v.Closing != c.Closing {
			return errors.New("component receipt seam/scope mismatch")
		}
	}
	for _, group := range []struct {
		kind  string
		names []string
		pins  map[string]Pin
	}{{"module", requiredModules, r.Modules}, {"store", requiredStores, r.Stores}, {"check", requiredChecks, r.Checks}} {
		for _, name := range group.names {
			p := group.pins[name]
			if e := distinct(p); e != nil {
				return e
			}
			var v EvidenceReceipt
			if e := readPinnedJSON(p, &v); e != nil {
				return e
			}
			if e := v.validate(m, r, build, group.kind, name, 1, FinalHeight); e != nil {
				return e
			}
		}
	}
	return nil
}
