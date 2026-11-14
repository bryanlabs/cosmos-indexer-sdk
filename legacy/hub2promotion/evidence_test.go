package hub2promotion

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func bindingReceipt(m Manifest) FinancialReceipt {
	return FinancialReceipt{Version: 1, Profile: CompleteProfile, First: 1, Last: FinalHeight, Canonical: CanonicalHeight, Hub3GenesisSHA: m.Hub3GenesisSHA, Complete: true, AllCoins: true, SourcePreserved: true, Modules: map[string]Pin{}, Stores: map[string]Pin{}, Checks: map[string]Pin{}, CanonicalRows: m.CanonicalRows, CanonicalProjectionSHA: m.CanonicalProjectionSHA, GenesisSHA: m.Genesis.SHA256, InitialRoot: m.Components[0].Opening, SourceSHA: m.Source.SHA256, BuildSHA: m.BuildSHA, BuildIdentitySHA: m.BuildIdentity.SHA256, BinarySHA: m.BinarySHA, ComponentsSHA: valueSHA(m.Components), ProjectionsSHA: valueSHA(m.Tables), MigrationLast: m.MigrationLast, TailFirst: m.TailFirst, TailLast: m.TailLast}
}
func evidenceBuild() BuildReceipt {
	return BuildReceipt{ImageDigest: "sha256:" + digest([]byte("synthetic-native-image")), GasProfile: Pin{SHA256: digest([]byte("synthetic-gas-profile"))}}
}
func envelope(m Manifest, kind, name string) EvidenceReceipt {
	return EvidenceReceipt{Version: 1, Profile: CompleteProfile, Kind: kind, Name: name, Chain: Chain, First: 1, Last: FinalHeight, Complete: true, AllCoins: true, GenesisSHA: m.Genesis.SHA256, SourceSHA: m.Source.SHA256, BuildSHA: m.BuildSHA, BinarySHA: m.BinarySHA, Canonical: m.Canonical, MigrationLast: m.MigrationLast, TailFirst: m.TailFirst, TailLast: m.TailLast, CanonicalRows: m.CanonicalRows, CanonicalProjectionSHA: m.CanonicalProjectionSHA}
}
func jsonPin(t *testing.T, f fixture, name string, v any) Pin {
	b, e := json.Marshal(v)
	require.NoError(t, e)
	return savePin(t, f.dir, name+"-"+randomID()+".json", b)
}
func strictEnvelopeFixture(t *testing.T) (fixture, Manifest, FinancialReceipt) {
	f := newFixture(t)
	m := cloneManifest(f.m)
	m.Fixture = false
	m.Profile = CompleteProfile
	m.Last = FinalHeight
	m.Canonical = CanonicalHeight
	m.MigrationLast = CanonicalHeight
	m.TailFirst = CanonicalHeight + 1
	m.TailLast = FinalHeight
	m.Components[0].Last = FinalHeight
	// These are only synthetic envelope-format fixtures. The actual complete
	// validator must still reject them, including their binary and partial SQL.
	artifact := m.Source
	m.BuildIdentity = savePin(t, f.dir, "different-build-identity.json", []byte(`{"synthetic_build":true}`))
	s := envelope(m, "source", "raw-cache")
	s.SourceSHA = ""
	s.Artifacts = []Pin{artifact}
	m.Source = jsonPin(t, f, "source-envelope", s)
	c := envelope(m, "component", "historical")
	c.HistoricalOnly = true
	c.Opening = m.Components[0].Opening
	c.Closing = m.Components[0].Closing
	c.Artifacts = []Pin{artifact}
	m.Components[0].Receipt = jsonPin(t, f, "component-envelope", c)
	r := bindingReceipt(m)
	scopes := []string{evidenceScope("source", "raw-cache", 1, FinalHeight), evidenceScope("component", "historical", 1, FinalHeight)}
	for _, group := range []struct {
		kind  string
		names []string
		pins  map[string]Pin
	}{{"module", requiredModules, r.Modules}, {"store", requiredStores, r.Stores}, {"check", requiredChecks, r.Checks}} {
		for _, name := range group.names {
			v := envelope(m, group.kind, name)
			if group.kind == "check" {
				switch name {
				case "initial_genesis":
					v.Opening = r.InitialRoot
					v.Closing = r.InitialRoot
				case "component_seams":
					v.Opening = r.InitialRoot
					v.Closing = m.Components[len(m.Components)-1].Closing
					v.ComponentsSHA = r.ComponentsSHA
				case "native_binary_image":
					v.ImageDigest = evidenceBuild().ImageDigest
				case "gas_profile":
					v.GasProfileSHA = evidenceBuild().GasProfile.SHA256
				}
			}
			scopes = append(scopes, evidenceScope(group.kind, name, 1, FinalHeight))
			v.Artifacts = []Pin{artifact}
			group.pins[name] = jsonPin(t, f, group.kind+"-"+name, v)
		}
	}
	r.AcceptedArtifacts = map[string]ArtifactAttestation{artifact.SHA256: {File: artifact, Role: "native-proof", Scopes: scopes}}
	m.Financial = jsonPin(t, f, "financial-envelope", r)
	return f, m, r
}
func TestStrictEvidenceEnvelopeRejectsGenericFixturePartialAndSeamForgery(t *testing.T) {
	f, m, r := strictEnvelopeFixture(t)
	require.NoError(t, m.validateEvidence(r, evidenceBuild()), "only envelope format, not owner-signed whole-chain proof")
	require.Error(t, m.ValidateComplete(), "synthetic envelope metadata is not financial acceptance")
	for _, kind := range []string{"generic-json", "fixture-profile", "partial-range", "false-complete", "missing-allcoin", "wrong-seam", "reused-receipt", "wrong-genesis", "wrong-source", "missing-artifact"} {
		t.Run(kind, func(t *testing.T) {
			v := cloneManifest(m)
			rr := r
			rr.Modules = map[string]Pin{}
			for k, p := range r.Modules {
				rr.Modules[k] = p
			}
			switch kind {
			case "generic-json":
				rr.Modules[requiredModules[0]] = f.m.Source
			case "reused-receipt":
				rr.Modules[requiredModules[0]] = rr.Modules[requiredModules[1]]
			case "wrong-seam":
				var e EvidenceReceipt
				require.NoError(t, readPinnedJSON(v.Components[0].Receipt, &e))
				e.Opening = digest([]byte("forged-seam"))
				v.Components[0].Receipt = jsonPin(t, f, "forged-seam", e)
			default:
				var e EvidenceReceipt
				require.NoError(t, readPinnedJSON(rr.Modules[requiredModules[0]], &e))
				switch kind {
				case "fixture-profile":
					e.Fixture = true
					e.Profile = FixtureProfile
				case "partial-range":
					e.Last = 1600000
				case "false-complete":
					e.Complete = false
				case "missing-allcoin":
					e.AllCoins = false
				case "wrong-genesis":
					e.GenesisSHA = digest([]byte("different-genesis"))
				case "wrong-source":
					e.SourceSHA = digest([]byte("different-source"))
				case "missing-artifact":
					e.Artifacts = nil
				}
				rr.Modules[requiredModules[0]] = jsonPin(t, f, kind, e)
			}
			require.Error(t, v.validateEvidence(rr, evidenceBuild()))
		})
	}
}
func TestFinancialReceiptBindsEntireProjectionAndProvenance(t *testing.T) {
	f, m, _ := strictEnvelopeFixture(t)
	for _, kind := range []string{"genesis", "build", "build-receipt", "binary", "component", "projection"} {
		t.Run(kind, func(t *testing.T) {
			v := cloneManifest(m)
			switch kind {
			case "genesis":
				v.Genesis = savePin(t, f.dir, "changed-genesis-"+randomID(), []byte(`{"changed":true}`))
			case "build":
				v.BuildSHA = digest([]byte("different-code"))
			case "build-receipt":
				v.BuildIdentity = savePin(t, f.dir, "substituted-native-build-"+randomID(), []byte(`{"native_binaries_substituted":true}`))
			case "binary":
				v.BinarySHA = digest([]byte("different-binary"))
			case "component":
				v.Components[0].Closing = digest([]byte("different-terminal"))
			case "projection":
				p := v.Tables[0].File
				b, e := os.ReadFile(p.Path)
				require.NoError(t, e)
				v.Tables[0].File = savePin(t, f.dir, "changed-projection-"+randomID(), append(b, ' '))
			}
			require.ErrorContains(t, v.ValidateComplete(), "financial receipt/package bindings differ")
		})
	}
}
func TestPinnedJSONParsesOnlyExactlyHashedBytes(t *testing.T) {
	dir := artifactDir(t)
	p := savePin(t, dir, "pinned.json", []byte(`{"version":1}`))
	var v struct {
		Version int `json:"version"`
	}
	require.NoError(t, readPinnedJSON(p, &v))
	require.Equal(t, 1, v.Version)
	require.NoError(t, os.WriteFile(p.Path, []byte(`{"version":2}`), 0600))
	require.ErrorContains(t, readPinnedJSON(p, &v), "SHA mismatch")
	p = savePin(t, dir, "extra.json", []byte(`{"version":1} {"version":2}`))
	require.Error(t, readPinnedJSON(p, &v))
	p = savePin(t, dir, "unknown.json", []byte(`{"version":1,"synthetic":true}`))
	require.Error(t, readPinnedJSON(p, &v))
	_, e := ReadManifestPinned(p.Path, strings.Repeat("a", 64))
	require.Error(t, e)
}
