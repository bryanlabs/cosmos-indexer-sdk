package taxapi

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The pre-check's reads share one archive budget, however many windows and
// blocks it settles, so it never reads more than one walk's worth.
func TestEarlyDelegationsAtSharesOneBudget(t *testing.T) {
	wallet, val := bech(t, "cosmos", 1), bech(t, "cosmosvaloper", 2)
	fake := &fakeArchive{delegations: map[int64]map[string]int64{}}
	for h := int64(1); h <= archiveCallLimit+5; h++ {
		fake.delegations[h] = map[string]int64{val: 100 + h}
	}
	srv := fake.serve(t)
	defer srv.Close()
	at := earlyDelegationsAt(&ledgerNode{archive: testArchive(t, srv, 1000)}, wallet)
	for h := int64(1); h <= archiveCallLimit; h++ {
		d, err := at(h)
		require.NoError(t, err, "call %d", h)
		require.Equal(t, big100(h), d[val].String())
	}
	_, err := at(archiveCallLimit + 1)
	require.ErrorIs(t, err, errArchiveCeiling)
	require.Len(t, fake.calls, archiveCallLimit)
	require.Nil(t, earlyDelegationsAt(nil, wallet), "no node, no reads, as the walk")
	// A second pre-check has its own budget, as each walk does.
	_, err = earlyDelegationsAt(&ledgerNode{archive: testArchive(t, srv, 1000)}, wallet)(1)
	require.NoError(t, err)
}

func big100(h int64) string { return bi(100 + h).String() }
