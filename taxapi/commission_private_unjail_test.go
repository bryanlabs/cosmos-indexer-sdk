package taxapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPrivateUnjailProfileVersionBinding(t *testing.T) {
	p := CommissionPrivateProfile{Schema: privateUnjailSchema, SourceSHA256: privateSourceSealSHA, ParserSHA256: privateParserSourceSHA, EconomicProofSHA256: privateEconomicProofSHA, From: time.Date(2024, 3, 21, 0, 0, 0, 0, time.UTC), AsOf: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), UnjailSourcesSHA256: []string{privateUnjailSources[0].SourceSHA, privateUnjailSources[1].SourceSHA}}
	require.True(t, p.validSchema())
	scope, n := p.privateParserScope()
	require.EqualValues(t, 3, n)
	require.Contains(t, scope, "(10404,28657858,19312696)")
	require.Contains(t, scope, "(10535,29441609,19837518)")
	raw, e := json.Marshal(p)
	require.NoError(t, e)
	path := filepath.Join(t.TempDir(), "profile.json")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	loaded, e := LoadCommissionPrivateProfile(path, privateSHA(raw))
	require.NoError(t, e)
	require.NotEmpty(t, loaded.digest)
	require.Error(t, (&Server{commissionPrivateProfile: loaded}).validatePrivateProjection())
	for _, mutate := range []func(*CommissionPrivateProfile){
		func(p *CommissionPrivateProfile) { p.Schema = "commission-readonly-authz-projection-v1" },
		func(p *CommissionPrivateProfile) { p.UnjailSourcesSHA256 = nil },
		func(p *CommissionPrivateProfile) { p.UnjailSourcesSHA256 = append(p.UnjailSourcesSHA256, "other") },
		func(p *CommissionPrivateProfile) {
			p.UnjailSourcesSHA256 = []string{privateUnjailSources[1].SourceSHA, privateUnjailSources[0].SourceSHA}
		},
	} {
		q := p
		mutate(&q)
		require.False(t, q.validSchema())
		b, e := json.Marshal(q)
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(path, b, 0600))
		_, e = LoadCommissionPrivateProfile(path, privateSHA(b))
		require.Error(t, e)
	}
	p.Schema = "commission-readonly-authz-projection-v1"
	p.UnjailSourcesSHA256 = nil
	require.True(t, p.validSchema())
	scope, n = p.privateParserScope()
	require.EqualValues(t, 1, n)
	require.NotContains(t, scope, "10404")
}

func TestPrivateUnjailExactSourceQueries(t *testing.T) {
	for _, source := range privateUnjailSources {
		sql := privateUnjailSourceSQL(source)
		require.NotContains(t, sql, "m.id=27907375")
		require.Contains(t, sql, "t.code=0")
		require.Contains(t, sql, "'all_errors'")
		require.Contains(t, sql, "'fees'")
		require.Error(t, validatePrivateUnjailSource([]byte(`{}`), source))
	}
}

func TestPrivateUnjailRetainedSources(t *testing.T) {
	root := os.Getenv("COMMISSION_UNJAIL_TEST_ROOT")
	if root == "" {
		t.Skip("retained private sources not configured")
	}
	for i, source := range privateUnjailSources {
		b, e := os.ReadFile(filepath.Join(root, []string{"source10404.private.json", "source10535.private.json"}[i]))
		require.NoError(t, e)
		require.NoError(t, validatePrivateUnjailSource(b, source))
		for _, field := range []string{"message_bytes", "message_type", "message", "error", "tx", "block", "all_errors", "all_messages", "tax_rows", "events"} {
			var m map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(b, &m))
			m[field] = json.RawMessage(`null`)
			changed, e := json.Marshal(m)
			require.NoError(t, e)
			require.Error(t, validatePrivateUnjailSource(changed, source))
		}
		require.Error(t, validatePrivateUnjailSource(b, privateUnjailSources[1-i]))
		// Even re-sealing a forged monetary event must fail the semantic check.
		var m map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(b, &m))
		m["events"] = json.RawMessage(`[{"type":"coin_spent"}]`)
		changed, e := json.Marshal(m)
		require.NoError(t, e)
		forged := source
		forged.SourceSHA = privateSHA(changed)
		require.Error(t, validatePrivateUnjailSource(changed, forged))
		require.False(t, strings.Contains(privateUnjailSourceSQL(source), "UPDATE "))
	}
}
