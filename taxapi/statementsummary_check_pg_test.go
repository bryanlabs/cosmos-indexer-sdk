package taxapi

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStatementSummaryCheckFindsOrphanPostgres(t *testing.T) {
	for _, kind := range []string{"days", "recipients"} {
		t.Run(kind, func(t *testing.T) {
			s := summaryPGServer(t)
			ctx := context.Background()
			_, err := FillStatementSummary(ctx, s.db, false, summaryOpts(summaryDay(2026, 11, 13)))
			require.NoError(t, err)
			orphan := flowPGValoper(98)
			if kind == "days" {
				summaryExec(t, s.db, "INSERT INTO statement_days(validator_address,day,category,denom,amount,events) VALUES(?,'2025-01-01','reward','uatom',1,1)", orphan)
			} else {
				summaryExec(t, s.db, "INSERT INTO statement_delegators(validator_address,month,to_addr) VALUES(?,'2025-01-01','synthetic-orphan-recipient')", orphan)
			}
			res, err := CheckStatementSummary(ctx, s.db, 0, 1, StatementSummaryOptions{})
			require.NoError(t, err)
			require.Len(t, res.Mismatches, 1)
			require.Equal(t, orphan, res.Mismatches[0].Validator)
			require.False(t, summaryPlan(t, s, orphan).Summary, "a failed comparison withdraws the summary without deleting its rows")
			require.Positive(t, summaryCount(t, s.db, "SELECT count(*) FROM statement_days"))
		})
	}
}
