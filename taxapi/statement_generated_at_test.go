package taxapi

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The coverage statement carries the time it was generated, so two requests
// for the same data differ only there. Tests that compare responses byte for
// byte mask that one value, in each form the statement prints it, and compare
// everything else exactly.
var (
	statementGeneratedAtJSON   = regexp.MustCompile(`("statement":\{"generated_at":")[^"]*(")`)
	statementGeneratedAtText   = regexp.MustCompile(`(Generated )\d{4}-\d\d-\d\d \d\d:\d\d UTC( from the live Cosmos Tax index\.)`)
	statementGeneratedAtHeader = regexp.MustCompile(`(; generated )\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ`)
)

const statementGeneratedAtMask = "GENERATED_AT"

func withoutStatementGeneratedAt(s string) string {
	s = statementGeneratedAtJSON.ReplaceAllString(s, "${1}"+statementGeneratedAtMask+"${2}")
	s = statementGeneratedAtText.ReplaceAllString(s, "${1}"+statementGeneratedAtMask+"${2}")
	return statementGeneratedAtHeader.ReplaceAllString(s, "${1}"+statementGeneratedAtMask)
}

func TestWithoutStatementGeneratedAtMasksOnlyGeneratedAt(t *testing.T) {
	at := func(ts time.Time, height int64) *CoverageStatement {
		return &CoverageStatement{GeneratedAt: ts, IndexedThroughHeight: height,
			Chains: []StatementChain{{ChainID: apiHubChainID, FirstHeight: 5200791, InReports: true}}}
	}
	t1 := time.Date(2026, 11, 22, 1, 26, 54, 489011540, time.UTC)
	t2 := t1.Add(61 * time.Second)
	for _, render := range []func(*CoverageStatement) string{
		func(st *CoverageStatement) string { return mustStatementJSON(t, st) },
		func(st *CoverageStatement) string { return statementText(st, PeriodCoverage{}) },
		func(st *CoverageStatement) string { return statementHeader(st) },
	} {
		a, b := render(at(t1, 29500000)), render(at(t2, 29500000))
		require.NotEqual(t, a, b)
		require.Equal(t, withoutStatementGeneratedAt(a), withoutStatementGeneratedAt(b))
		require.True(t, strings.Contains(withoutStatementGeneratedAt(a), statementGeneratedAtMask), a)
		require.NotEqual(t, withoutStatementGeneratedAt(a), withoutStatementGeneratedAt(render(at(t1, 29500001))))
	}
}

func mustStatementJSON(t *testing.T, st *CoverageStatement) string {
	t.Helper()
	b, err := json.Marshal(struct {
		Statement *CoverageStatement `json:"statement"`
	}{st})
	require.NoError(t, err)
	return string(b)
}
