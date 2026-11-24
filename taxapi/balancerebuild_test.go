package taxapi

import (
	"bytes"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	"github.com/stretchr/testify/require"
)

func TestRebuildableBuild(t *testing.T) {
	d := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	later := d.AddDate(0, 0, 5)
	require.ErrorContains(t, rebuildableBuild(BalanceBuild{}), "no balance build")
	require.NoError(t, rebuildableBuild(BalanceBuild{FromDay: &d, ToDay: &later}))
	require.ErrorContains(t, rebuildableBuild(BalanceBuild{FromDay: &d, ToDay: &later, BuildingFrom: &d, BuildingTo: &later}), "unfinished")
	require.ErrorContains(t, rebuildableBuild(BalanceBuild{FromDay: &d, ToDay: &later, AppendingTo: &later}), "append")
}

func TestRebuildAddressesPostgres(t *testing.T) {
	database := balanceDaysTestDB(t)
	s := &Server{db: database, native: DefaultNativeAsset}
	valoper, err := bech32.ConvertAndEncode("cosmosvaloper", make([]byte, 20))
	require.NoError(t, err)
	a, b := testAddress(t, 2), testAddress(t, 1)
	require.NoError(t, database.Exec("CREATE TEMP TABLE addresses (id bigint PRIMARY KEY, address text)").Error)
	require.NoError(t, database.Exec("INSERT INTO addresses VALUES (10, ?), (11, ?), (12, ?)", a, b, valoper).Error)

	got, err := s.rebuildAddresses("cosmos", []int64{10, 11})
	require.NoError(t, err)
	want := []string{a, b}
	sort.Strings(want)
	require.Equal(t, want, got, "addresses come back sorted")
	_, err = s.rebuildAddresses("cosmos", []int64{10, 13})
	require.ErrorContains(t, err, "1 of 2 address ids exist")
	_, err = s.rebuildAddresses("cosmos", []int64{10, 10})
	require.ErrorContains(t, err, "listed twice")
	_, err = s.rebuildAddresses("cosmos", []int64{12})
	require.ErrorContains(t, err, "not an account")
	_, err = s.rebuildAddresses("cosmos", nil)
	require.Error(t, err)
}

// The rollback script puts back exactly the span's rows the rebuild
// replaced, leaving rows before the span and other wallets alone.
func TestRebuildRollbackPostgres(t *testing.T) {
	database := balanceDaysTestDB(t)
	a, other := testAddress(t, 1), testAddress(t, 2)
	day := func(n int) time.Time { return time.Date(2025, 3, n, 0, 0, 0, 0, time.UTC) }
	var days []time.Time
	for n := 3; n <= 9; n++ {
		days = append(days, day(n))
	}
	stored := []BalanceDay{
		{Chain: apiCosmoshub, Address: a, Day: day(1), Height: 1, Holdings: `{"liquid":{"uatom":"11"}}`},
		{Chain: apiCosmoshub, Address: a, Day: day(3), Height: 3, AnchorHeight: 90, Holdings: `{"liquid":{"uatom":"5"}}`},
		{Chain: apiCosmoshub, Address: a, Day: day(6), Height: 6, AnchorHeight: 90, Status: "incomplete", Reason: "the wallet's history isn't indexed", Holdings: "{}"},
		{Chain: apiCosmoshub, Address: other, Day: day(4), Height: 4, Holdings: `{"liquid":{"uatom":"7"}}`},
	}
	require.NoError(t, database.Create(stored).Error)
	snapshot := func() []BalanceDay {
		t.Helper()
		var rows []BalanceDay
		require.NoError(t, database.Order("address, day").Find(&rows).Error)
		for i := range rows {
			rows[i].Day = rows[i].Day.UTC()
			rows[i].Holdings = strings.ReplaceAll(rows[i].Holdings, " ", "")
		}
		return rows
	}
	want := snapshot()

	var old []BalanceDay
	require.NoError(t, database.Where("chain = ? AND address IN ? AND day BETWEEN ? AND ?", apiCosmoshub, []string{a}, "2025-03-03", "2025-03-09").Order("address, day").Find(&old).Error)
	require.Len(t, old, 2)
	var script bytes.Buffer
	require.NoError(t, writeRollback(&script, apiCosmoshub, []string{a}, days[0], days[len(days)-1], old))

	rebuilt := []BalanceDay{
		{Chain: apiCosmoshub, Address: a, Day: day(3), Height: 3, AnchorHeight: 95, Holdings: `{"liquid":{"uatom":"4"}}`},
		{Chain: apiCosmoshub, Address: a, Day: day(8), Height: 8, AnchorHeight: 95, Holdings: `{"liquid":{"uatom":"12"}}`},
	}
	require.NoError(t, replaceWalletDays(database, apiCosmoshub, a, days, rebuilt))
	require.NotEqual(t, want, snapshot())

	for _, stmt := range strings.Split(strings.TrimSpace(script.String()), ";\n") {
		stmt = strings.TrimSuffix(stmt, ";")
		if stmt == "BEGIN" || stmt == "COMMIT" {
			continue
		}
		require.NoError(t, database.Exec(stmt).Error, stmt)
	}
	require.Equal(t, want, snapshot())
	require.True(t, strings.HasPrefix(script.String(), "BEGIN;\n") && strings.HasSuffix(script.String(), "COMMIT;\n"))
}
