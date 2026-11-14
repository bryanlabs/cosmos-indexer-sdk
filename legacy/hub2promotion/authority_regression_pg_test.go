package hub2promotion

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNativeAttestationsBindActualScopesRootsSeamsImageAndGas(t *testing.T) {
	f, m, r := strictEnvelopeFixture(t)
	for _, kind := range []string{"no-owner-attestation", "different-scope", "package-substitution", "input-not-proof", "initial-root", "seam-list", "terminal-root", "native-image", "native-gas"} {
		t.Run(kind, func(t *testing.T) {
			rr := r
			rr.Checks = map[string]Pin{}
			for k, p := range r.Checks {
				rr.Checks[k] = p
			}
			rr.Modules = map[string]Pin{}
			for k, p := range r.Modules {
				rr.Modules[k] = p
			}
			rr.AcceptedArtifacts = map[string]ArtifactAttestation{}
			for k, v := range r.AcceptedArtifacts {
				v.Scopes = append([]string(nil), v.Scopes...)
				rr.AcceptedArtifacts[k] = v
			}
			switch kind {
			case "no-owner-attestation":
				rr.AcceptedArtifacts = nil
			case "different-scope":
				for k, v := range rr.AcceptedArtifacts {
					v.Scopes = []string{"unrelated"}
					rr.AcceptedArtifacts[k] = v
				}
			case "input-not-proof":
				for k, v := range rr.AcceptedArtifacts {
					v.Role = "input"
					rr.AcceptedArtifacts[k] = v
				}
			case "package-substitution":
				name := requiredModules[0]
				var e EvidenceReceipt
				require.NoError(t, readPinnedJSON(rr.Modules[name], &e))
				e.Artifacts = []Pin{m.Genesis}
				rr.Modules[name] = jsonPin(t, f, "bad-package-proof", e)
				rr.AcceptedArtifacts[m.Genesis.SHA256] = ArtifactAttestation{File: m.Genesis, Role: "native-proof", Scopes: []string{evidenceScope("module", name, 1, FinalHeight)}}
			default:
				name := map[string]string{"initial-root": "initial_genesis", "seam-list": "component_seams", "terminal-root": "component_seams", "native-image": "native_binary_image", "native-gas": "gas_profile"}[kind]
				var e EvidenceReceipt
				require.NoError(t, readPinnedJSON(rr.Checks[name], &e))
				switch kind {
				case "initial-root":
					e.Opening = digest([]byte("wronginitial"))
				case "seam-list":
					e.ComponentsSHA = digest([]byte("wrongseamlist"))
				case "terminal-root":
					e.Closing = digest([]byte("wrongterminal"))
				case "native-image":
					e.ImageDigest = "sha256:" + digest([]byte("wrongimage"))
				case "native-gas":
					e.GasProfileSHA = digest([]byte("wronggas"))
				}
				rr.Checks[name] = jsonPin(t, f, kind, e)
			}
			require.Error(t, m.validateEvidence(rr, evidenceBuild()))
		})
	}
}
func TestPG3AuditorsPinSettingsAgainstNonUTCAndExactPublicOverloads(t *testing.T) {
	f := newFixture(t)
	f.owner.SetMaxOpenConns(1)
	mustExec(t, f.owner, `CREATE FUNCTION public.row_to_json(pg_catalog.pg_proc) RETURNS json LANGUAGE sql AS $$ SELECT '"spoofed-function"'::json $$; REVOKE ALL ON FUNCTION public.row_to_json(pg_catalog.pg_proc) FROM PUBLIC`)
	refreshIdentity(t, &f)
	mustExec(t, f.owner, `SET timezone='Pacific/Honolulu'; SET search_path=public,pg_catalog`)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, e := MergeFixture(ctx, f.loader, f.owner, f.m, f.runtime)
	require.NoError(t, e)
	require.Equal(t, int64(15), result.Rows)
	outcome, e := AuditOutcome(ctx, f.owner, f.m, f.runtime)
	require.NoError(t, e)
	require.Equal(t, OutcomeExact, outcome)
	var zone, path string
	require.NoError(t, f.owner.QueryRowContext(context.Background(), `SHOW timezone`).Scan(&zone))
	require.Equal(t, "Pacific/Honolulu", zone)
	require.NoError(t, f.owner.QueryRow(`SHOW search_path`).Scan(&path))
	require.Equal(t, "public, pg_catalog", path)
}
