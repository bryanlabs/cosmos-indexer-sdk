package hub2promotion

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompleteAdmissionNeverAcceptsFixtureOrPartialReceipt(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.m.ValidateFixture())
	require.Error(t, f.m.ValidateComplete())
	for _, kind := range []string{"unchanged-fixture", "renamed-profile", "labelled-complete", "missing-module", "missing-store", "missing-check", "false-whole-flag", "canonical-tail-confusion"} {
		t.Run(kind, func(t *testing.T) {
			m := cloneManifest(f.m)
			if kind != "unchanged-fixture" {
				m.Fixture = false
				m.Profile = CompleteProfile
			}
			if kind != "renamed-profile" {
				m.Last = FinalHeight
				m.Canonical = CanonicalHeight
				m.MigrationLast = CanonicalHeight
				m.TailFirst = CanonicalHeight + 1
				m.TailLast = FinalHeight
				m.Components[0].Last = FinalHeight
			}
			if strings.HasPrefix(kind, "missing-") || kind == "false-whole-flag" {
				r := bindingReceipt(m)
				for _, k := range requiredModules {
					r.Modules[k] = m.Source
				}
				for _, k := range requiredStores {
					r.Stores[k] = m.Source
				}
				for _, k := range requiredChecks {
					r.Checks[k] = m.Source
				}
				switch kind {
				case "missing-module":
					delete(r.Modules, requiredModules[0])
				case "missing-store":
					delete(r.Stores, requiredStores[0])
				case "missing-check":
					delete(r.Checks, requiredChecks[0])
				case "false-whole-flag":
					r.Complete = false
				}
				b, e := json.Marshal(r)
				require.NoError(t, e)
				m.Financial = savePin(t, f.dir, kind+".json", b)
			}
			if kind == "canonical-tail-confusion" {
				m.MigrationLast = m.Last
			}
			e := m.ValidateComplete()
			require.Error(t, e)
			if strings.HasPrefix(kind, "missing-") {
				require.ErrorContains(t, e, "full native evidence set required")
			}
		})
	}
}
func TestBuildIdentityRejectsCounterfeitCleanAndUnboundClosure(t *testing.T) {
	dir := artifactDir(t)
	source := savePin(t, dir, "source.go", []byte("synthetic source closure"))
	binary := savePin(t, dir, "binary", []byte("synthetic binary"))
	native := savePin(t, dir, "native", []byte("synthetic native"))
	gas := savePin(t, dir, "profile.json", []byte(`{"synthetic":true}`))
	files := []Pin{source}
	b, _ := json.Marshal(files)
	m := Manifest{BinarySHA: binary.SHA256, BuildSHA: digest(b)}
	good := BuildReceipt{Version: 1, Profile: CompleteProfile, Binary: binary, SourceSHA: m.BuildSHA, SourceFiles: files, SignedCommit: strings.Repeat("a", 40), VCSRevision: strings.Repeat("a", 40), VCSModified: false, NativeBinaries: []Pin{native}, GasProfile: gas, ImageDigest: "sha256:" + strings.Repeat("c", 64)}
	require.ErrorContains(t, good.validate(m), "binary build metadata absent", "a synthetic file can never be an accepted complete binary")
	for _, kind := range []string{"counterfeit-clean", "dirty-build", "wrong-binary", "missing-native", "changed-profile", "wrong-closure"} {
		t.Run(kind, func(t *testing.T) {
			v := good
			switch kind {
			case "counterfeit-clean":
				v.VCSRevision = strings.Repeat("b", 40)
			case "dirty-build":
				v.VCSModified = true
			case "wrong-binary":
				v.Binary.SHA256 = strings.Repeat("d", 64)
			case "missing-native":
				v.NativeBinaries = nil
			case "changed-profile":
				v.GasProfile.SHA256 = strings.Repeat("e", 64)
			case "wrong-closure":
				v.SourceSHA = strings.Repeat("f", 64)
			}
			require.Error(t, v.validate(m))
		})
	}
	require.NoError(t, os.WriteFile(source.Path, []byte("mutated source"), 0600))
	require.Error(t, good.validate(m))
}
func TestStaticScopeSQLHasNoExistingSchemaRewrite(t *testing.T) {
	s := Scope{"h2p_s_synthetic", "h2p_l_synthetic", "h2p_v_synthetic", "h2p_k_synthetic", ""}
	text, e := s.SQL()
	require.NoError(t, e)
	for _, bad := range []string{"CREATE OR REPLACE", "ALTER TABLE legacy.", "DROP ", "TRUNCATE ", "DELETE ", "UPDATE legacy.", "REVOKE TEMP", "GRANT UPDATE ON {{all_tables}} TO {{loader}}"} {
		require.NotContains(t, text, bad)
	}
	require.Equal(t, 10, strings.Count(text, "WITH CASCADED CHECK OPTION"))
	require.Equal(t, 1, strings.Count(text, "SECURITY DEFINER"))
	require.Contains(t, text, "SET search_path = pg_catalog")
	require.Contains(t, text, "NOLOGIN")
	require.Contains(t, text, "REVOKE ALL ON FUNCTION "+s.Schema+".lock_legacy() FROM PUBLIC")
}
