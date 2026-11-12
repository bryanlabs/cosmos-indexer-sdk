package genesis

import (
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/legacy/schema/pgtest"
	"github.com/stretchr/testify/require"
)

func TestLoadStoresTheFileOnceAndLeavesEverythingElseUntouched(t *testing.T) {
	db := pgtest.Open(t)
	serving := pgtest.Snapshot(t, db.Owner, "public")

	two, err := Read(strings.NewReader(hubTwo("33.333333333333333333")))
	require.NoError(t, err)
	result, err := Load(db.Importer, two)
	require.NoError(t, err)
	require.False(t, result.AlreadyLoaded)
	require.Equal(t, two.Counts(), result.Stored)

	for _, text := range []string{hubOne(), hubFour(uatom("140"))} {
		file, err := Read(strings.NewReader(text))
		require.NoError(t, err)
		result, err := Load(db.Importer, file)
		require.NoError(t, err)
		require.Equal(t, file.Counts(), result.Stored)
	}

	var rows []string
	require.NoError(t, db.Owner.Raw(`SELECT concat_ws(' ', chain_id, file_position, kind, file_coins, liquid, delegated, unbonding, sequence)
		FROM legacy.genesis_accounts ORDER BY chain_id, file_position`).Scan(&rows).Error)
	require.Equal(t, []string{
		"cosmoshub-1 0 base 1000 600 400 0 0", "cosmoshub-1 1 delayed_vesting 50 50 0 0 0", "cosmoshub-1 2 continuous_vesting 70 70 0 0 0",
		"cosmoshub-2 0 base 1000 1000 30 16 7", "cosmoshub-2 1 base 0 0 60 0 0",
		"cosmoshub-4 0 base 0 0 0 0 3", "cosmoshub-4 1 continuous_vesting 70 70 30 0 0", "cosmoshub-4 2 delayed_vesting 40 40 0 0 0", "cosmoshub-4 3 module 30 30 0 0 0",
	}, rows)
	require.NoError(t, db.Owner.Raw(`SELECT concat_ws(' ', chain_id, shares, tokens, from_genesis_tx::text)
		FROM legacy.genesis_delegations ORDER BY chain_id, delegator`).Scan(&rows).Error)
	require.Equal(t, []string{"cosmoshub-1 400.000000000000000000 400 true", "cosmoshub-2 33.333333333333333333 30 false",
		"cosmoshub-2 66.666666666666666667 60 false", "cosmoshub-4 30.000000000000000000 30 false"}, rows)
	require.NoError(t, db.Owner.Raw(`SELECT concat_ws(' ', entry, balance, to_char(completion_time AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.US'))
		FROM legacy.genesis_unbondings ORDER BY entry`).Scan(&rows).Error)
	require.Equal(t, []string{"0 11 2019-05-01 10:00:00.123456", "1 5 2019-05-02 10:00:00.000000"}, rows)
	require.NoError(t, db.Owner.Raw(`SELECT concat_ws(' ', chain_id, outstanding_rewards, accumulated_commission, status)
		FROM legacy.genesis_validators WHERE chain_id = 'cosmoshub-4'`).Scan(&rows).Error)
	require.Equal(t, []string{"cosmoshub-4 12.500000000000000000 1.250000000000000000 bonded"}, rows)

	// Loading the same file again writes nothing.
	stored := pgtest.Snapshot(t, db.Owner, "legacy")
	result, err = Load(db.Importer, two)
	require.NoError(t, err)
	require.True(t, result.AlreadyLoaded)
	require.Equal(t, two.Counts(), result.Stored)
	pgtest.RequireUntouched(t, stored, pgtest.Snapshot(t, db.Owner, "legacy"))

	// Another file for a chain already loaded is refused and writes nothing.
	other, err := Read(strings.NewReader(strings.Replace(hubTwo("33.333333333333333333"), `"sequence_number":"7"`, `"sequence_number":"8"`, 1)))
	require.NoError(t, err)
	_, err = Load(db.Importer, other)
	require.ErrorContains(t, err, "another genesis file")
	// The same checksum with other content cannot pass either: every row is compared.
	other.SHA256 = two.SHA256
	_, err = Load(db.Importer, other)
	require.ErrorContains(t, err, "differ from the genesis file")
	// Nor can a file that differs only in its genesis time.
	later := *two
	later.Time = two.Time.Add(time.Second)
	_, err = Load(db.Importer, &later)
	require.ErrorContains(t, err, "another genesis file")
	pgtest.RequireUntouched(t, stored, pgtest.Snapshot(t, db.Owner, "legacy"))

	// A connection that could reach the serving tables is refused before any write.
	three, err := Read(strings.NewReader(strings.Replace(hubTwo("33.333333333333333333"), "cosmoshub-2", "cosmoshub-3", 1)))
	require.NoError(t, err)
	_, err = Load(db.Owner, three)
	require.ErrorContains(t, err, "not confined")
	pgtest.RequireUntouched(t, stored, pgtest.Snapshot(t, db.Owner, "legacy"))

	pgtest.RequireUntouched(t, serving, pgtest.Snapshot(t, db.Owner, "public"))
}
