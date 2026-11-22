package taxapi

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// stakingTestDB opens a fresh schema in a throwaway database (COSMOS_TAX_TEST_DSN).
func stakingTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN to a throwaway Postgres for the staking tables proof")
	}
	schema := fmt.Sprintf("staking_%d", time.Now().UnixNano())
	boot, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, boot.Exec("CREATE SCHEMA "+schema).Error)
	t.Cleanup(func() {
		boot.Exec("DROP SCHEMA " + schema + " CASCADE")
		if c, err := boot.DB(); err == nil {
			_ = c.Close()
		}
	})
	sep := " "
	if strings.Contains(dsn, "://") {
		sep = "&"
		if !strings.Contains(dsn, "?") {
			sep = "?"
		}
		dsn += sep + "search_path=" + schema + "&timezone=America/New_York"
	} else {
		dsn += sep + "search_path=" + schema + " timezone=America/New_York"
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = pool.Close() })
	return db
}

func insertStakingRows(t *testing.T, db *gorm.DB, st StakingState, days []StakingDayRow, vals []StakingValidatorRow) {
	t.Helper()
	require.NoError(t, db.Exec(`INSERT INTO tax_staking_state VALUES (?, ?::date, ?, ?, ?::date, ?, ?, ?, ?)`,
		st.Chain, st.FirstDay, st.FirstHeight, st.FirstBlockTime, st.LastDay, st.LastHeight, st.UpdatedAt, st.Source, st.Version).Error)
	for i := 0; i < len(days); i += 500 {
		j := min(i+500, len(days))
		var ph []string
		var args []any
		for _, d := range days[i:j] {
			ph = append(ph, "(?, ?::date, ?, ?, ?, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::jsonb)")
			args = append(args, st.Chain, d.Day, d.FirstHeight, d.LastHeight, d.Blocks, d.Minted, d.CommunityPool, d.Stakers, d.Commission,
				d.RewardsEvents, d.CommissionEvents, d.SupplyStart, d.InflationStart, d.InflationAvg, d.BondedAvg, d.CommunityTax,
				d.WithdrawnCommission, d.WithdrawnRewards, d.Proposals)
		}
		require.NoError(t, db.Exec("INSERT INTO tax_staking_days VALUES "+strings.Join(ph, ","), args...).Error)
	}
	for i := 0; i < len(vals); i += 5000 {
		j := min(i+5000, len(vals))
		var ph []string
		var args []any
		for _, v := range vals[i:j] {
			ph = append(ph, "(?, ?::date, ?, ?::numeric, ?::numeric)")
			args = append(args, st.Chain, v.Day, v.Operator, v.Rewards, v.Commission)
		}
		require.NoError(t, db.Exec("INSERT INTO tax_staking_validator_days VALUES "+strings.Join(ph, ","), args...).Error)
	}
}

// The handlers answer the same from real Postgres tables as from the
// embedded files, and fall back to the files without tables or state.
func TestStakingTablesPostgres(t *testing.T) {
	db := stakingTestDB(t)
	useCompleteIncome(t)
	want := serveBodies(t, &Server{})

	// No tables: embedded files.
	sameBodies(t, want, serveBodies(t, &Server{staking: newStakingTables(db)}))

	require.NoError(t, db.Exec(StakingTablesDDL).Error)
	// Tables but no state row: embedded files.
	sameBodies(t, want, serveBodies(t, &Server{staking: newStakingTables(db)}))

	st, days, vals := stakingRowsFromEmbedded(t)
	t.Logf("%d day rows, %d validator rows", len(days), len(vals))
	insertStakingRows(t, db, st, days, vals)
	tb := newStakingTables(db)
	began := time.Now()
	if tb.current(t.Context()) == nil {
		t.Fatalf("tables not used: %s", tb.reason)
	}
	t.Logf("loaded and built in %s", time.Since(began).Round(time.Millisecond))
	sameBodies(t, want, serveBodies(t, &Server{staking: tb}))

	// Exact numerics survive the round trip.
	got, err := ReadStakingValidatorDays(db, st.Chain, "2021-02-18", "2021-02-18")
	require.NoError(t, err)
	require.NotEmpty(t, got)
	require.NoError(t, db.Exec(`UPDATE tax_staking_validator_days SET rewards_uatom = '23190967499.979998625158886778' WHERE day = '2021-02-18' AND operator = ?`, got[0].Operator).Error)
	got2, err := ReadStakingValidatorDays(db, st.Chain, "2021-02-18", "2021-02-18")
	require.NoError(t, err)
	require.Equal(t, "23190967499.979998625158886778", got2[0].Rewards)
}
