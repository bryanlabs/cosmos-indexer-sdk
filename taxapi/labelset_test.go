package taxapi

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func labelSetLine(addr, basis string) string {
	return `{"set_version":"v1","chain":"mainnet","address":"` + addr + `","name":"n","kind":"interchain_account","source":"derived:ica-handshake/v1","evidence":"e","rule":"derived:ica-handshake/v1","basis":"` + basis + `","ref":"node","license":"own","as_of":"2026-11-21","tags":["interchain_account"],"created_at":"2026-11-21T00:00:00Z"}`
}

func TestReadLabelSet(t *testing.T) {
	a := moduleAddress("distribution")
	b := moduleAddress("gov")
	rows, sum, err := ReadLabelSet(strings.NewReader(labelSetLine(a, "derived") + "\n" + labelSetLine(b, "published") + "\n"))
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Len(t, sum, 64)
	require.Equal(t, []string{"interchain_account"}, rows[0].Tags)

	_, _, err = ReadLabelSet(strings.NewReader(labelSetLine(a, "inferred")))
	require.ErrorContains(t, err, "neither derived nor published")
	_, _, err = ReadLabelSet(strings.NewReader(labelSetLine(a, "derived") + "\n" + labelSetLine(a, "derived")))
	require.ErrorContains(t, err, "listed twice")
	_, _, err = ReadLabelSet(strings.NewReader(labelSetLine("osmo1xyz", "derived")))
	require.ErrorContains(t, err, "valid cosmos address")
	_, _, err = ReadLabelSet(strings.NewReader(strings.Replace(labelSetLine(a, "derived"), `"ref":"node"`, `"ref":""`, 1)))
	require.ErrorContains(t, err, "required")
}
