package taxapi

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The length check plus the plain class must keep exactly the values the
// bounded pattern kept.
func TestEvidenceAddressRecordedMatchesBoundedPattern(t *testing.T) {
	bounded := regexp.MustCompile(`^[a-z][a-z0-9]{7,127}$`)
	values := []string{
		"", "a", "cosmos", "cosmos1", "cosmos1x", "cosmos1abc", "Cosmos1abcdefgh", "cosmos1ABCDEFG",
		"cosmos1é" + strings.Repeat("a", 10), "cosmos1abcdefgh ", " cosmos1abcdefgh", "cosmos1abc-defgh",
		"1cosmosabcdefgh", "cosmosabcdefgh", "cosmos1\nabcdefgh", "cosmosvaloper1abcdefgh",
	}
	for n := 1; n <= 130; n++ {
		values = append(values, "a1"+strings.Repeat("b", n), "ab"+strings.Repeat("c", n))
	}
	for _, v := range values {
		want := bounded.MatchString(v) && strings.Contains(v, "1")
		require.Equal(t, want, evidenceAddressRecorded(v), "%q", v)
	}
}
