package taxapi

import (
	"context"
	"math/big"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestHoldingsRoundTrip(t *testing.T) {
	h := ledgerHoldings{
		Liquid:      map[string]*big.Int{"uatom": big.NewInt(1500), "ibc/AB": big.NewInt(7)},
		Delegations: map[string]*big.Int{"cosmosvaloper1x": new(big.Int).Lsh(big.NewInt(1), 80)},
	}
	text, err := encodeHoldings(h)
	require.NoError(t, err)
	again, err := encodeHoldings(h)
	require.NoError(t, err)
	require.Equal(t, text, again, "encoding is canonical")
	got, err := decodeHoldings(42, text)
	require.NoError(t, err)
	require.Equal(t, int64(42), got.Height)
	require.Equal(t, "1500", got.Liquid["uatom"].String())
	require.Equal(t, h.Delegations["cosmosvaloper1x"].String(), got.Delegations["cosmosvaloper1x"].String())
	require.Empty(t, got.Unbonding)

	_, err = decodeHoldings(1, `{"liquid":{"uatom":"1.5"}}`)
	require.Error(t, err, "a non-integer amount must not decode")

	// Postgres returns jsonb in its own key order.
	require.True(t, sameJSON(`{"liquid": {"uatom": "1500", "ibc/AB": "7"}}`, `{"liquid":{"ibc/AB":"7","uatom":"1500"}}`))
	require.False(t, sameJSON(`{"liquid":{"uatom":"1500"}}`, `{"liquid":{"uatom":"1501"}}`))
}

func TestChangedDays(t *testing.T) {
	day := func(d int, status, holdings string) BalanceDay {
		return BalanceDay{Day: time.Date(2025, 1, d, 0, 0, 0, 0, time.UTC), Status: status, Holdings: holdings}
	}
	a, b := `{"liquid":{"uatom":"1"}}`, `{"liquid":{"uatom":"2"}}`
	all := []BalanceDay{day(1, "", a), day(2, "", a), day(3, "", b), day(4, balanceIncompleteS, "{}"), day(5, balanceIncompleteS, "{}"), day(6, "", b)}

	days := func(rows []BalanceDay) []int {
		var out []int
		for _, r := range rows {
			out = append(out, r.Day.Day())
		}
		return out
	}
	require.Equal(t, []int{1, 3, 4, 6}, days(changedDays(nil, all)), "the first day is always stored")
	before := day(31, "", a)
	require.Equal(t, []int{3, 4, 6}, days(changedDays(&before, all)), "a span continuing an equal row stores no first row")
	empty := []BalanceDay{day(1, "", "{}"), day(2, "", "{}")}
	require.Equal(t, []int{1}, days(changedDays(nil, empty)), "an empty wallet still stores its first day")
}

func testAddress(t *testing.T, n byte) string {
	t.Helper()
	data := make([]byte, 20)
	data[0] = n
	addr, err := bech32.ConvertAndEncode("cosmos", data)
	require.NoError(t, err)
	return addr
}

func balanceDaysTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres balance day verification")
	}
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	connection.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	database = database.WithContext(ctx)
	for _, sql := range []string{
		"SET search_path = pg_temp",
		// A session zone west of UTC would shift a date written from a UTC midnight.
		"SET TIME ZONE 'America/New_York'",
		"CREATE TEMP TABLE message_event_attributes (message_event_id bigint, value text)",
		"CREATE TEMP TABLE block_event_attributes (block_event_id bigint, block_event_attribute_key_id bigint, value text)",
		"CREATE INDEX ON message_event_attributes (left(value, 100))",
		"CREATE INDEX ON block_event_attributes (left(value, 100))",
		"CREATE TEMP TABLE tax_balance_days (chain text, address text, day date, height bigint, anchor_height bigint, status text, reason text, holdings jsonb, PRIMARY KEY (chain, address, day))",
		"CREATE TEMP TABLE tax_balance_builds (chain text PRIMARY KEY, from_day date, to_day date, building_from date, building_to date, cursor text, appending_to date, updated_at timestamptz)",
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	return database
}

func TestLedgerAddressesPostgres(t *testing.T) {
	database := balanceDaysTestDB(t)
	s := &Server{db: database, native: DefaultNativeAsset}

	var want []string
	for i := byte(1); i <= 9; i++ {
		want = append(want, testAddress(t, i*16))
	}
	sort.Strings(want)
	// Odd addresses only in message events, even only in block events, one
	// in both and repeated, plus values that are not account addresses.
	for i, a := range want {
		table := "message_event_attributes"
		if i%2 == 1 {
			table = "block_event_attributes"
		}
		require.NoError(t, database.Exec("INSERT INTO "+table+" (value) VALUES (?), (?)", a, a).Error)
	}
	require.NoError(t, database.Exec("INSERT INTO block_event_attributes (value) VALUES (?)", want[0]).Error)
	for _, junk := range []string{"cosmos1notbech32", "cosmos1zzzz", "cosmosvaloper1abc", "1000uatom", want[2] + ",x"} {
		require.NoError(t, database.Exec("INSERT INTO message_event_attributes (value) VALUES (?)", junk).Error)
	}

	for _, limit := range []int{1, 2, 3, 50} {
		var got []string
		cursor := ""
		for pages := 0; ; pages++ {
			require.Less(t, pages, 100, "paging must end")
			addrs, next, err := s.LedgerAddresses("cosmos", cursor, limit)
			require.NoError(t, err)
			got = append(got, addrs...)
			if next == "" {
				break
			}
			require.Greater(t, next, cursor)
			cursor = next
		}
		require.Equal(t, want, got, "limit %d", limit)
	}
}

func TestStoredBalanceDayPostgres(t *testing.T) {
	database := balanceDaysTestDB(t)
	s := &Server{db: database, native: DefaultNativeAsset}
	addr := testAddress(t, 1)
	d := func(day int) time.Time { return time.Date(2025, 3, day, 0, 0, 0, 0, time.UTC) }

	build, err := s.balanceBuild("cosmoshub")
	require.NoError(t, err)
	require.Nil(t, build, "no build yet")
	_, ok, err := s.storedBalanceDay(build, addr, d(5))
	require.NoError(t, err)
	require.False(t, ok)

	from, to := d(3), d(20)
	require.NoError(t, database.Save(&BalanceBuild{Chain: "cosmoshub", FromDay: &from, ToDay: &to}).Error)
	require.NoError(t, database.Create([]BalanceDay{
		{Chain: "cosmoshub", Address: addr, Day: d(1), Holdings: `{"liquid":{"uatom":"1"}}`},
		{Chain: "cosmoshub", Address: addr, Day: d(3), Holdings: `{"liquid":{"uatom":"3"}}`},
		{Chain: "cosmoshub", Address: addr, Day: d(10), Status: balanceIncompleteS, Holdings: "{}"},
	}).Error)
	var stored string
	require.NoError(t, database.Raw("SELECT min(day)::text FROM tax_balance_days").Scan(&stored).Error)
	require.Equal(t, "2025-03-01", stored, "the stored date is the UTC day")

	build, err = s.balanceBuild("cosmoshub")
	require.NoError(t, err)
	require.NotNil(t, build)
	cases := []struct {
		day    time.Time
		found  bool
		liquid string
		status string
	}{
		{d(2), false, "", ""},                     // before the built span
		{d(3), true, "3", ""},                     // the span's first row
		{d(9), true, "3", ""},                     // inherits day 3
		{d(10), true, "", balanceIncompleteS},     // a status row
		{d(15), true, "", balanceIncompleteS},     // inherits the status
		{d(21), false, "", ""},                    // after the span
		{d(9).Add(23 * time.Hour), true, "3", ""}, // a time inside the day
	}
	for _, c := range cases {
		row, ok, err := s.storedBalanceDay(build, addr, c.day)
		require.NoError(t, err)
		require.Equal(t, c.found, ok, c.day)
		if !ok {
			continue
		}
		require.Equal(t, c.status, row.Status, c.day)
		if c.liquid != "" {
			h, err := decodeHoldings(1, row.Holdings)
			require.NoError(t, err)
			require.Equal(t, c.liquid, h.Liquid["uatom"].String(), c.day)
		}
	}
	_, ok, err = s.storedBalanceDay(build, testAddress(t, 2), d(9))
	require.NoError(t, err)
	require.False(t, ok, "an address with no rows falls back to the live walk")
}

// A column added to tax_balance_builds by a newer image's migration must not
// break a running API's cached statements (SQLSTATE 0A000, 2026-11-08).
func TestBalanceBuildSurvivesAddedColumnPostgres(t *testing.T) {
	database := balanceDaysTestDB(t)
	s := &Server{db: database, native: DefaultNativeAsset}
	from, to := time.Date(2025, 3, 3, 0, 0, 0, 0, time.UTC), time.Date(2025, 3, 20, 0, 0, 0, 0, time.UTC)
	require.NoError(t, database.Save(&BalanceBuild{Chain: "cosmoshub", FromDay: &from, ToDay: &to}).Error)

	read := func() {
		t.Helper()
		build, err := s.balanceBuild("cosmoshub")
		require.NoError(t, err)
		require.NotNil(t, build)
		require.NoError(t, s.lockBalanceBuild("cosmoshub", func(*gorm.DB, *BalanceBuild) error { return nil }))
	}
	read()
	require.NoError(t, database.Exec("ALTER TABLE tax_balance_builds ADD COLUMN later_column text").Error)
	read()
}
