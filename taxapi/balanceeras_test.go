package taxapi

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Block 8694999 is the last without coin events, so a date ending there
// still undoes only blocks that have them.
func TestPreCoinEventsReason(t *testing.T) {
	require.Empty(t, preCoinEventsReason("", hubCoinEventsFrom-1))
	require.Empty(t, preCoinEventsReason(apiMainnet, 30000000))
	require.Contains(t, preCoinEventsReason("", hubCoinEventsFrom-2), "8695000")
	require.Contains(t, preCoinEventsReason("cosmoshub-4", 5200791), "Gaia v6")
	require.Empty(t, preCoinEventsReason(apiTestnet, 100))
}

func TestUnnamedStakingHeightsPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres ledger verification")
	}
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	database = database.WithContext(ctx)
	for _, sql := range []string{
		tempSearchPath,
		"CREATE TEMP TABLE taxable_events (category text, from_addr text, to_addr text, block_height bigint)",
		"INSERT INTO taxable_events VALUES " +
			"('delegate','cosmos1w','',9000000)," +
			"('undelegate','','cosmos1w',12000000)," +
			"('redelegate','cosmos1w','cosmos1w',12000000)," +
			"('reward','','cosmos1w',13000000)," +
			"('delegate','cosmos1other','',14000000)," +
			"('delegate','cosmos1w','',19639600)",
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	s := &Server{db: database, native: DefaultNativeAsset}
	got, err := s.unnamedStakingHeights("", "cosmos1w", 8700000)
	require.NoError(t, err)
	require.Equal(t, []int64{9000000, 12000000}, got)
	got, err = s.unnamedStakingHeights("", "cosmos1w", 9000000)
	require.NoError(t, err)
	require.Equal(t, []int64{12000000}, got)
	got, err = s.unnamedStakingHeights("", "cosmos1w", hubStakingDelegatorFrom-1)
	require.NoError(t, err)
	require.Empty(t, got)
	got, err = s.unnamedStakingHeights(apiTestnet, "cosmos1w", 1)
	require.NoError(t, err)
	require.Empty(t, got)
	require.False(t, strings.Contains(strings.Join(stakingCategories, ","), "reward"))
}
