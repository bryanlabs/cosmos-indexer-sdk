package books_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/legacy/books"
	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema/pgtest"
	"github.com/stretchr/testify/require"
)

// Two heights of a chain move coins every way the ledger stores. Every
// address whose history is complete closes against the next genesis file,
// the one whose balance there is off by a coin is reported, and only the
// reconciliation table changes.
func TestCloseExplainsTheNextGenesisFromTheStoredHistory(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	conn, err := db.Importer.DB()
	require.NoError(t, err)
	exec := func(statement string, args ...any) {
		t.Helper()
		require.NoError(t, db.Owner.Exec(statement, args...).Error)
	}
	exec(`UPDATE legacy.eras SET export_height = 2 WHERE chain_id = 'cosmoshub-3'`)
	for _, chain := range []string{"cosmoshub-3", "cosmoshub-4"} {
		exec(`INSERT INTO legacy.genesis_files (chain_id, sha256, genesis_time, accounts, delegations, unbonding_entries, validators, withdraw_addresses, genesis_txs)
VALUES (?, ?, now(), 0, 0, 0, 0, 0, 0)`, chain, strings.Repeat("0a", 32))
	}
	account := func(chain, address, kind string, liquid int) {
		t.Helper()
		var position int
		require.NoError(t, db.Owner.Raw(`SELECT count(*) FROM legacy.genesis_accounts WHERE chain_id = ?`, chain).Scan(&position).Error)
		exec(`INSERT INTO legacy.genesis_accounts (chain_id, address, file_position, account_number, sequence, kind, file_coins, liquid, delegated, unbonding)
VALUES (?, ?, ?, ?, 0, ?, ?, ?, 0, 0)`, chain, address, position, position, kind, liquid, liquid)
	}
	// Opening balances.
	account("cosmoshub-3", "alice", "base", 1000)
	account("cosmoshub-3", "bob", "base", 500)
	account("cosmoshub-3", "carol", "continuous_vesting", 100)
	account("cosmoshub-3", "pool", "module", 9000)
	account("cosmoshub-3", "idle", "base", 77)
	// Closing balances: what the movements below give, except that erin
	// holds one coin the history does not explain and idle holds two fewer.
	account("cosmoshub-4", "alice", "base", 1000-53-5-200-64-4)
	account("cosmoshub-4", "bob", "base", 500+53+512+40-6-3)
	account("cosmoshub-4", "carol", "continuous_vesting", 100+7+9)
	account("cosmoshub-4", "dave", "base", 10+3)
	account("cosmoshub-4", "erin", "base", 1)
	account("cosmoshub-4", "pool", "module", 1)
	account("cosmoshub-4", "idle", "base", 75)

	hash := strings.Repeat("0A", 32)
	row := func(phase, tx, msg, sub int, category, amount, from, to, detail string) ledger.Row {
		r := ledger.Row{Phase: phase, TxIndex: tx, MsgIndex: msg, SubIndex: sub, Category: category, Amount: amount, Denom: "uatom",
			From: from, To: to, Detail: detail, Source: "event"}
		if phase == ledger.PhaseTx {
			r.TxHash = hash
		}
		return r
	}
	inclusion := func(index int, fee, payer string, charged bool) ledger.Inclusion {
		return ledger.Inclusion{Index: index, Hash: hash, CodeEvidence: "explicit", Fee: fee, FeePayer: payer, FeeCharged: charged,
			FeeEvidence: "the transaction succeeded", Signers: []string{payer}, Raw: []byte{1}}
	}
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	blocks := []ledger.Block{
		{Chain: "cosmoshub-3", Height: 1, Time: when, Hash: strings.Repeat("AB", 32), Proposer: "AA",
			Inclusions: []ledger.Inclusion{inclusion(0, "5uatom", "alice", true), inclusion(1, "900uatom", "bob", false), inclusion(2, "3uatom,1other", "bob", true)},
			Rows: []ledger.Row{
				row(ledger.PhaseTx, 0, 0, 0, "transfer", "53", "alice", "bob", ""),
				row(ledger.PhaseTx, 0, 1, 0, "reward", "7", "", "alice", `{"paid_to":"carol"}`),
				row(ledger.PhaseTx, 0, 1, 1, "delegate", "200", "alice", "", ""),
				row(ledger.PhaseTx, 0, 2, 0, "undelegate", "30", "", "alice", ""),
				row(ledger.PhaseTx, 0, 3, 0, "redelegate", "30", "alice", "alice", ""),
				row(ledger.PhaseTx, 0, 4, 0, "gov_deposit", "64", "alice", "", ""),
				row(ledger.PhaseTx, 2, 0, 0, "transfer", "10", "", "dave", ""),
				row(ledger.PhaseTx, 2, 0, 1, "transfer", "4", "alice", "", ""),
				row(ledger.PhaseTx, 2, 0, 2, "transfer", "6", "bob", "", ""),
				row(ledger.PhaseTx, 2, 1, 0, "transfer", "8", "bob", "bob", ""),
			}},
		{Chain: "cosmoshub-3", Height: 2, Time: when.Add(time.Second), Hash: strings.Repeat("CD", 32), Proposer: "AA",
			Inclusions: []ledger.Inclusion{inclusion(0, "0", "carol", true)},
			Rows: []ledger.Row{
				row(ledger.PhaseTx, 0, 0, 0, "commission", "9", "", "carol", ""),
				row(ledger.PhaseTx, 0, 0, 1, "reward", "3", "", "dave", ""),
				row(ledger.PhaseEnd, 0, 0, 0, "unbonding_complete", "40", "", "bob", ""),
				row(ledger.PhaseEnd, 0, 0, 1, "gov_deposit_refund", "512", "pool", "bob", ""),
				row(ledger.PhaseEnd, 0, 0, 2, "income", "1", "pool", "pool", ""),
			}},
		// After the export height: not part of what the next chain started with.
		{Chain: "cosmoshub-3", Height: 3, Time: when.Add(2 * time.Second), Hash: strings.Repeat("EF", 32), Proposer: "AA",
			Inclusions: []ledger.Inclusion{inclusion(0, "1uatom", "alice", true)},
			Rows:       []ledger.Row{row(ledger.PhaseTx, 0, 0, 0, "transfer", "500", "alice", "bob", "")}},
	}
	_, err = ledger.Write(ctx, conn, blocks[:1])
	require.NoError(t, err)

	partial, err := books.Close(ctx, conn, "cosmoshub-3")
	require.NoError(t, err)
	require.False(t, partial.Complete)
	require.Equal(t, int64(1), partial.HeightsStored)
	require.ErrorContains(t, books.Record(ctx, conn, partial), "every height")

	_, err = ledger.Write(ctx, conn, blocks[1:])
	require.NoError(t, err)
	public := pgtest.Snapshot(t, db.Owner, "public")
	result, err := books.Close(ctx, conn, "cosmoshub-3")
	require.NoError(t, err)
	require.True(t, result.Complete)
	require.Equal(t, "cosmoshub-4", result.Next)
	require.Equal(t, int64(2), result.Through)
	require.Equal(t, int64(2), result.HeightsStored)
	require.Equal(t, 1, result.ModuleAccounts)
	require.Equal(t, 6, result.Addresses)
	require.Equal(t, 4, result.Closed)
	require.Equal(t, 2, result.Open)
	require.Equal(t, "1", result.Short.String())
	require.Equal(t, "2", result.Over.String())
	require.Equal(t, int64(1), result.OtherDenomRows)
	require.Equal(t, int64(2), result.Fees)
	require.Equal(t, map[string]int{
		"the next genesis holds more: no stored movement, not in the opening genesis": 1,
		"the next genesis holds less: no stored movement":                             1,
	}, result.OpenByCause)
	require.Equal(t, map[string]int{"under 10 uatom": 2}, result.OpenBySize)
	require.Equal(t, 1, result.Touched["reward paid to another address"])
	require.Equal(t, 1, result.Touched["reward of another address in"])
	require.Equal(t, 1, result.Touched["vesting account"])
	require.Equal(t, 2, result.Touched["fee out"])
	require.Zero(t, result.TouchedOpen["fee out"])

	require.NoError(t, books.Record(ctx, conn, result))
	require.NoError(t, books.Record(ctx, conn, result), "recording again replaces the same lines")
	var statuses []string
	require.NoError(t, db.Owner.Raw(`SELECT address || ' ' || status || ' ' || difference::bigint FROM legacy.reconciliation
WHERE chain_id = 'cosmoshub-3' AND component = 'liquid' ORDER BY address`).Scan(&statuses).Error)
	require.Equal(t, []string{"alice closed 0", "bob closed 0", "carol closed 0", "dave closed 0", "erin open 1", "idle open -2"}, statuses)
	pgtest.RequireUntouched(t, public, pgtest.Snapshot(t, db.Owner, "public"))

	// A category nobody gave a balance rule stops the check.
	exec(`INSERT INTO legacy.taxable_events (chain_id, height, phase, tx_index, msg_index, sub_index, category, amount, denom, "timestamp", amount_source)
VALUES ('cosmoshub-3', 2, 3, 0, 0, 9, 'swap', '5', 'uatom', now(), 'state')`)
	_, err = books.Close(ctx, conn, "cosmoshub-3")
	require.ErrorContains(t, err, "no rule for")

	// A chain whose export paid rewards cannot be closed this way.
	_, err = books.Close(ctx, conn, "cosmoshub-2")
	require.ErrorContains(t, err, "paid pending rewards")
}
