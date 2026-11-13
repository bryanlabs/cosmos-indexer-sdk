package books_test

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/legacy/books"
	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema/pgtest"
	"github.com/stretchr/testify/require"
)

// The next genesis emptied two source wallets and credited two recovery
// wallets in code. Without the list none of the four closes. With it they
// are counted on their own, a wallet whose difference is not exactly the
// listed one stays open, a wallet outside the list is never helped, and the
// tax rows of the change are stored once by the confined importer role.
func TestCloseCountsTheGenesisChangeOnItsOwn(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	conn, err := db.Importer.DB()
	require.NoError(t, err)
	exec := func(statement string, args ...any) {
		t.Helper()
		require.NoError(t, db.Owner.Exec(statement, args...).Error)
	}
	exec(`UPDATE legacy.eras SET export_height = 1 WHERE chain_id = 'cosmoshub-3'`)
	exec(`INSERT INTO legacy.genesis_files (chain_id, sha256, genesis_time, accounts, delegations, unbonding_entries, validators, withdraw_addresses, genesis_txs)
VALUES ('cosmoshub-3', ?, '2019-12-11 16:11:34+00', 0, 0, 0, 0, 0, 0), ('cosmoshub-4', ?, '2021-02-18 06:00:00+00', 0, 0, 0, 0, 0, 0)`,
		strings.Repeat("0a", 32), strings.Repeat("0b", 32))
	account := func(chain, address string, liquid int) {
		t.Helper()
		var position int
		require.NoError(t, db.Owner.Raw(`SELECT count(*) FROM legacy.genesis_accounts WHERE chain_id = ?`, chain).Scan(&position).Error)
		exec(`INSERT INTO legacy.genesis_accounts (chain_id, address, file_position, account_number, sequence, kind, file_coins, liquid, delegated, unbonding)
VALUES (?, ?, ?, ?, 0, 'base', ?, ?, 0, 0)`, chain, address, position, position, liquid, liquid)
	}
	account("cosmoshub-3", "source1", 1000)
	account("cosmoshub-3", "source2", 500)
	account("cosmoshub-3", "recovery2", 5)
	account("cosmoshub-3", "payer", 100)
	account("cosmoshub-3", "idle", 77)
	account("cosmoshub-4", "source1", 0)
	account("cosmoshub-4", "source2", 0)
	account("cosmoshub-4", "recovery1", 900)
	account("cosmoshub-4", "recovery2", 5+468)
	account("cosmoshub-4", "payer", 100-20)
	account("cosmoshub-4", "idle", 75)

	hash := strings.Repeat("0A", 32)
	block := ledger.Block{Chain: "cosmoshub-3", Height: 1, Time: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC), Hash: strings.Repeat("AB", 32), Proposer: "AA",
		Inclusions: []ledger.Inclusion{{Index: 0, Hash: hash, CodeEvidence: "explicit", Fee: "0", FeePayer: "payer", FeeCharged: true,
			FeeEvidence: "the transaction succeeded", Signers: []string{"payer"}, Raw: []byte{1}}},
		Rows: []ledger.Row{{Phase: ledger.PhaseTx, Category: "transfer", Amount: "20", Denom: "uatom", From: "payer", To: "source2", Source: "event", TxHash: hash}}}
	_, err = ledger.Write(ctx, conn, []ledger.Block{block})
	require.NoError(t, err)
	public := pgtest.Snapshot(t, db.Owner, "public")

	list := func(amount1 int64) *books.GenesisChange {
		return &books.GenesisChange{Chain: "cosmoshub-3", Name: "test genesis change", Repo: "example.org/repo", Tag: "v1", Path: "file.go", SHA256: strings.Repeat("0c", 32),
			Recoveries: []books.Recovery{{Source: "source1", Recovery: "recovery1", Amount: big.NewInt(amount1)}, {Source: "source2", Recovery: "recovery2", Amount: big.NewInt(468)}}}
	}

	plain, err := books.Close(ctx, conn, "cosmoshub-3")
	require.NoError(t, err)
	require.Equal(t, 1, plain.Closed)
	require.Equal(t, 5, plain.Open)
	require.Zero(t, plain.ClosedWithChange)
	_, err = books.RecordChange(ctx, conn, plain)
	require.ErrorContains(t, err, "without a genesis change")

	// One coin off the listed amount: that recovery wallet stays open and
	// nothing of the change is recorded.
	off, err := books.CloseWith(ctx, conn, "cosmoshub-3", list(901))
	require.NoError(t, err)
	require.Equal(t, 3, off.ClosedWithChange)
	require.Equal(t, 2, off.Open)
	require.Equal(t, 1, off.ChangeRecoveries)
	require.False(t, off.ChangeWhole)
	_, err = books.RecordChange(ctx, conn, off)
	require.ErrorContains(t, err, "accepted every wallet")

	// A source the next genesis did not empty is not accepted, and neither
	// is a recovery given more than its source held.
	wrong := list(900)
	wrong.Recoveries = append(wrong.Recoveries, books.Recovery{Source: "idle", Recovery: "payer", Amount: big.NewInt(1)})
	stays, err := books.CloseWith(ctx, conn, "cosmoshub-3", wrong)
	require.NoError(t, err)
	require.Equal(t, 4, stays.ClosedWithChange)
	require.Equal(t, 1, stays.Open)
	require.Equal(t, 1, stays.Closed)
	require.False(t, stays.ChangeWhole)
	more := list(900)
	more.Recoveries[0].Source, more.Recoveries[1].Source = "source2", "source1"
	swapped, err := books.CloseWith(ctx, conn, "cosmoshub-3", more)
	require.NoError(t, err)
	require.Equal(t, 4, swapped.ClosedWithChange)
	require.False(t, swapped.ChangeWhole, "source2 held 520 and its recovery was given 900")

	_, err = books.CloseWith(ctx, conn, "cosmoshub-2", list(900))
	require.ErrorContains(t, err, "another chain")

	result, err := books.CloseWith(ctx, conn, "cosmoshub-3", list(900))
	require.NoError(t, err)
	require.True(t, result.Complete)
	require.Equal(t, 6, result.Addresses)
	require.Equal(t, 1, result.Closed)
	require.Equal(t, 4, result.ClosedWithChange)
	require.Equal(t, 1, result.Open)
	require.Equal(t, 2, result.ChangeSources)
	require.Equal(t, 2, result.ChangeRecoveries)
	require.True(t, result.ChangeWhole)
	require.Equal(t, "1520", result.ChangeOut.String())
	require.Equal(t, "1368", result.ChangeIn.String())
	require.Equal(t, "152", result.ChangePool.String())
	require.Equal(t, "0", result.Short.String(), "the accepted wallets are in neither total")
	require.Equal(t, "2", result.Over.String())
	require.Equal(t, map[string]int{"the next genesis holds less: no stored movement": 1}, result.OpenByCause)
	require.Equal(t, int64(1), result.Rows)

	require.NoError(t, books.Record(ctx, conn, result))
	var statuses []string
	require.NoError(t, db.Owner.Raw(`SELECT address || ' ' || status || ' ' || difference::bigint FROM legacy.reconciliation
WHERE chain_id = 'cosmoshub-3' AND component = 'liquid' ORDER BY address`).Scan(&statuses).Error)
	require.Equal(t, []string{"idle open -2", "payer closed 0", "recovery1 " + books.StatusChange + " 900", "recovery2 " + books.StatusChange + " 468",
		"source1 " + books.StatusChange + " -1000", "source2 " + books.StatusChange + " -520"}, statuses)

	added, err := books.RecordChange(ctx, conn, result)
	require.NoError(t, err)
	require.Equal(t, 4, added)
	var stored []string
	require.NoError(t, db.Owner.Raw(`SELECT concat_ws(' ', height, phase, tx_index, msg_index, sub_index, category, amount, denom, 'from=' || from_addr, 'to=' || to_addr,
amount_source, to_char("timestamp" AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS'), (detail::json) ->> 'review', (detail::json) ->> 'community_pool',
coalesce((detail::json) ->> 'recovery_address', (detail::json) ->> 'source_address'), (detail::json) -> 'evidence' ->> 'sha256' = ?)
FROM legacy.taxable_events WHERE chain_id = 'cosmoshub-3' AND phase = 4 ORDER BY sub_index`, strings.Repeat("0c", 32)).Scan(&stored).Error)
	const when = "genesis 2021-02-18 06:00:00 needs review"
	require.Equal(t, []string{
		"1 4 0 0 0 " + books.Prop29Category + " 1000 uatom from=source1 to= " + when + " 100 recovery1 t",
		"1 4 0 0 1 " + books.Prop29Category + " 900 uatom from= to=recovery1 " + when + " 100 source1 t",
		"1 4 0 0 2 " + books.Prop29Category + " 520 uatom from=source2 to= " + when + " 52 recovery2 t",
		"1 4 0 0 3 " + books.Prop29Category + " 468 uatom from= to=recovery2 " + when + " 52 source2 t",
	}, stored)

	// The rows are stored once and move nothing: the same close gives the
	// same counts, a close without the list still leaves the four open, and
	// a rerun of the import over that height still finds its rows equal.
	added, err = books.RecordChange(ctx, conn, result)
	require.NoError(t, err)
	require.Zero(t, added)
	again, err := books.CloseWith(ctx, conn, "cosmoshub-3", list(900))
	require.NoError(t, err)
	require.Equal(t, int64(4), again.ChangeRows)
	require.Equal(t, int64(1), again.Rows)
	require.Equal(t, []int{1, 4, 1}, []int{again.Closed, again.ClosedWithChange, again.Open})
	plain, err = books.Close(ctx, conn, "cosmoshub-3")
	require.NoError(t, err)
	require.Equal(t, 5, plain.Open)
	_, err = ledger.Write(ctx, conn, []ledger.Block{block})
	require.NoError(t, err)

	// A close under another list cannot replace what is stored.
	other := list(900)
	other.Name = "another change"
	differs, err := books.CloseWith(ctx, conn, "cosmoshub-3", other)
	require.NoError(t, err)
	require.True(t, differs.ChangeWhole)
	_, err = books.RecordChange(ctx, conn, differs)
	require.ErrorContains(t, err, "not the genesis change this close accepted")
	var rows int
	require.NoError(t, db.Owner.Raw(`SELECT count(*) FROM legacy.taxable_events WHERE chain_id = 'cosmoshub-3'`).Scan(&rows).Error)
	require.Equal(t, 5, rows)
	pgtest.RequireUntouched(t, public, pgtest.Snapshot(t, db.Owner, "public"))
}
