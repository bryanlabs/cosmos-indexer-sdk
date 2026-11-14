package hub2promotion

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOwnerProofCannotBeSourceSiblingEnvelopeOrNativeBuildInput(t *testing.T) {
	f, m, r := strictEnvelopeFixture(t)
	for _, kind := range []string{"source-package", "sibling-envelope", "gas-input", "native-binary-input"} {
		t.Run(kind, func(t *testing.T) {
			build := evidenceBuild()
			rr := r
			rr.AcceptedArtifacts = map[string]ArtifactAttestation{}
			var v EvidenceReceipt
			scopeKind, name, role := "module", requiredModules[0], "native-proof"
			p := r.Checks[requiredChecks[0]]
			switch kind {
			case "source-package":
				scopeKind = "source"
				name = "raw-cache"
				role = "source-evidence"
				p = m.Genesis
				require.NoError(t, readPinnedJSON(m.Source, &v))
			case "gas-input":
				p = savePin(t, f.dir, "gas-input-"+randomID(), []byte(`{"fixture_gas_input":true}`))
				build.GasProfile = p
			case "native-binary-input":
				p = savePin(t, f.dir, "native-input-"+randomID(), []byte(`{"fixture_native_input":true}`))
				build.NativeBinaries = []Pin{p}
			}
			if kind != "source-package" {
				require.NoError(t, readPinnedJSON(rr.Modules[name], &v))
			}
			v.Artifacts = []Pin{p}
			rr.AcceptedArtifacts[p.SHA256] = ArtifactAttestation{File: p, Role: role, Scopes: []string{evidenceScope(scopeKind, name, 1, FinalHeight)}}
			require.ErrorContains(t, v.validate(m, rr, build, scopeKind, name, 1, FinalHeight), "package/input pin")
		})
	}
}
func TestFinancialCanonicalJSONRejectsMergedDuplicateAndCaseHiddenClaims(t *testing.T) {
	f, m, r := strictEnvelopeFixture(t)
	canonical, e := r.CanonicalBytes()
	require.NoError(t, e)
	for _, kind := range []string{"pretty", "case", "merged-duplicate"} {
		t.Run(kind, func(t *testing.T) {
			var bytes []byte
			switch kind {
			case "pretty":
				bytes, e = json.MarshalIndent(r, "", "  ")
				require.NoError(t, e)
			case "case":
				bytes = []byte(strings.Replace(string(canonical), `"complete":true`, `"Complete":true`, 1))
			case "merged-duplicate":
				bytes = []byte(strings.TrimSuffix(string(canonical), "}") + `,"accepted_artifacts":{}}`)
			}
			mm := cloneManifest(m)
			mm.Financial = savePin(t, f.dir, "ambiguous-"+kind+"-"+randomID(), bytes)
			require.ErrorContains(t, mm.verifyFinancialAuthority(), "canonical JSON")
		})
	}
}
