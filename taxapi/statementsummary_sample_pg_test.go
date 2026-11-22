package taxapi

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStatementSummarySampleSeedPostgres(t *testing.T) {
	s := summaryPGServer(t)
	ctx := context.Background()
	_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(summaryDay(2026, 11, 13)))
	require.NoError(t, err)
	for _, seed := range []int64{0, 1, -1, 1794635443940715695, 1<<63 - 1, -1 << 63} {
		t.Run(strconv.FormatInt(seed, 10), func(t *testing.T) {
			var sameText bool
			err := s.db.Raw("SELECT ?::bigint::text = ?::text", seed, strconv.FormatInt(seed, 10)).Row().Scan(&sameText)
			require.NoError(t, err)
			require.True(t, sameText, "typed binding preserves the seed text used by the sampling hash")
			for _, cells := range []int{1, 24} {
				res, err := CheckStatementSummary(ctx, s.db, cells, seed, StatementSummaryOptions{})
				require.NoError(t, err)
				require.Positive(t, res.Checked)
				require.Empty(t, res.Mismatches)
			}
		})
	}
}
