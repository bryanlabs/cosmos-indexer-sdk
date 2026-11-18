package balancedays_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	sdkmath "cosmossdk.io/math"
	"github.com/DefiantLabs/cosmos-indexer/legacy/balancedays"
	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema/pgtest"
	"github.com/stretchr/testify/require"
)

// Two days of cosmoshub-3: bob delegates, then the validator is slashed for
// downtime and alice undelegates at the lower price. Without the stored
// slash the replay does not reach the next genesis file and nothing passes;
// with it every validator, delegation and entry is exact, and each day end
// shows the slash on every delegation of the validator.
func TestExactReplayReachesTheNextGenesis(t *testing.T) {
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
	account := func(chain, address string, liquid int) {
		t.Helper()
		var position int
		require.NoError(t, db.Owner.Raw(`SELECT count(*) FROM legacy.genesis_accounts WHERE chain_id = ?`, chain).Scan(&position).Error)
		exec(`INSERT INTO legacy.genesis_accounts (chain_id, address, file_position, account_number, sequence, kind, file_coins, liquid, delegated, unbonding)
VALUES (?, ?, ?, ?, 0, 'base', ?, ?, 0, 0)`, chain, address, position, position, liquid, liquid)
	}
	account("cosmoshub-3", "alice", 5)
	account("cosmoshub-3", "bob", 1_000_000)
	account("cosmoshub-4", "alice", 5)
	account("cosmoshub-4", "bob", 0)
	exec(`INSERT INTO legacy.genesis_validators (chain_id, operator, tokens, delegator_shares, status, jailed) VALUES
('cosmoshub-3', 'val', 2000000, 2000000, 'BONDED', false), ('cosmoshub-4', 'val', 1999800, 2000000, 'BONDED', false)`)
	exec(`INSERT INTO legacy.genesis_delegations (chain_id, delegator, validator, shares, tokens) VALUES
('cosmoshub-3', 'alice', 'val', 2000000, 2000000), ('cosmoshub-4', 'alice', 'val', 1000000, 999900), ('cosmoshub-4', 'bob', 'val', 1000000, 999900)`)
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	exec(`INSERT INTO legacy.genesis_unbondings (chain_id, delegator, validator, entry, creation_height, completion_time, initial_balance, balance) VALUES
('cosmoshub-4', 'alice', 'val', 0, 0, ?, 999900, 999900)`, when.Add(24*time.Hour+21*24*time.Hour))

	hash := strings.Repeat("0A", 32)
	inclusion := ledger.Inclusion{Hash: hash, CodeEvidence: "explicit", Fee: "900uatom", FeePayer: "bob", FeeCharged: false,
		FeeEvidence: "the transaction succeeded", Signers: []string{"bob"}, Raw: []byte{1}}
	row := func(category, amount, from, to, detail string) ledger.Row {
		return ledger.Row{Phase: ledger.PhaseTx, Category: category, Amount: amount, Denom: "uatom", From: from, To: to, Validator: "val",
			Detail: detail, TxHash: hash, Source: "event"}
	}
	completion := when.Add(24*time.Hour + 21*24*time.Hour).Format(time.RFC3339Nano)
	_, err = ledger.Write(ctx, conn, []ledger.Block{
		{Chain: "cosmoshub-3", Height: 1, Time: when, Hash: strings.Repeat("AB", 32), Proposer: "AA",
			Inclusions: []ledger.Inclusion{inclusion}, Rows: []ledger.Row{row("delegate", "1000000", "bob", "", "")}},
		{Chain: "cosmoshub-3", Height: 2, Time: when.Add(24 * time.Hour), Hash: strings.Repeat("CD", 32), Proposer: "AA",
			Inclusions: []ledger.Inclusion{inclusion}, Rows: []ledger.Row{row("undelegate", "999900", "", "alice", `{"completion_time":"`+completion+`"}`)}},
	})
	require.NoError(t, err)

	build := func() (*balancedays.Result, []balancedays.Row) {
		t.Helper()
		var rows []balancedays.Row
		result, err := balancedays.Build(ctx, conn, "cosmoshub-3", func(r balancedays.Row) error {
			rows = append(rows, r)
			return nil
		})
		require.NoError(t, err)
		return result, rows
	}
	missing, _ := build()
	require.True(t, missing.ExactReplay)
	require.False(t, missing.Passed())
	require.Equal(t, 1, missing.ValidatorsDiffer)
	require.Equal(t, 2, missing.Delegations.Outside)

	balancedays.PlaceSlashed("valcons", "val")
	exec(`INSERT INTO legacy.block_events (chain_id, height, phase, event_index, type, attributes) VALUES
('cosmoshub-3', 2, 1, 0, 'slash', '[["address", "valcons"], ["power", "3"], ["reason", "missing_signature"], ["jailed", "valcons"]]')`)
	result, rows := build()
	require.True(t, result.Passed())
	require.Equal(t, 1, result.SlashesApplied)
	require.Equal(t, 1, result.ValidatorsExact)
	require.Equal(t, 2, result.SharesExact)
	require.Equal(t, 2, result.Delegations.Exact)
	require.Equal(t, 1, result.Unbonding.Exact)
	got := []string{}
	for _, r := range rows {
		got = append(got, r.Day.Format("2006-01-02")+" "+r.Address+" "+r.Holdings)
	}
	require.Equal(t, []string{
		`2020-01-02 alice {"liquid":{"uatom":"5"},"delegations":{"val":"2000000"}}`,
		`2020-01-02 bob {"delegations":{"val":"1000000"}}`,
		`2020-01-03 alice {"liquid":{"uatom":"5"},"delegations":{"val":"999900"},"unbonding":{"val":"999900"}}`,
		`2020-01-03 bob {"delegations":{"val":"999900"}}`,
	}, got)

	public := pgtest.Snapshot(t, db.Owner, "public")
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, balancedays.Write(ctx, tx, "cosmoshub-3", rows))
	require.NoError(t, balancedays.Record(ctx, tx, result))
	require.NoError(t, tx.Commit())
	var stored []string
	require.NoError(t, db.Owner.Raw(`SELECT address || ' ' || day::text || ' ' || (holdings->'delegations'->>'val') FROM legacy.balance_days
WHERE chain_id = 'cosmoshub-3' ORDER BY day, address`).Scan(&stored).Error)
	require.Equal(t, []string{"alice 2020-01-02 2000000", "bob 2020-01-02 1000000", "alice 2020-01-03 999900", "bob 2020-01-03 999900"}, stored)
	var lines []string
	require.NoError(t, db.Owner.Raw(`SELECT address || ' ' || component || ' ' || status FROM legacy.reconciliation
WHERE chain_id = 'cosmoshub-3' ORDER BY component, address`).Scan(&lines).Error)
	require.Equal(t, []string{"alice delegations closed", "bob delegations closed", "alice unbonding closed"}, lines)
	pgtest.RequireUntouched(t, public, pgtest.Snapshot(t, db.Owner, "public"))
}

// A uatom undelegated from a validator whose share price is below one
// releases no tokens, so the stored rows hold no row for it, yet its shares
// leave the delegation and the validator. The movement file replays it.
func TestAddedZeroTokenUndelegationReachesTheNextGenesis(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	conn, err := db.Importer.DB()
	require.NoError(t, err)
	exec := func(statement string, args ...any) {
		t.Helper()
		require.NoError(t, db.Owner.Exec(statement, args...).Error)
	}
	exec(`UPDATE legacy.eras SET export_height = 1 WHERE chain_id = 'cosmoshub-3'`)
	for _, chain := range []string{"cosmoshub-3", "cosmoshub-4"} {
		exec(`INSERT INTO legacy.genesis_files (chain_id, sha256, genesis_time, accounts, delegations, unbonding_entries, validators, withdraw_addresses, genesis_txs)
VALUES (?, ?, now(), 0, 0, 0, 0, 0, 0)`, chain, strings.Repeat("0a", 32))
		exec(`INSERT INTO legacy.genesis_accounts (chain_id, address, file_position, account_number, sequence, kind, file_coins, liquid, delegated, unbonding)
VALUES (?, 'alice', 0, 0, 0, 'base', 5, 5, 0, 0)`, chain)
	}
	// The SDK arithmetic of a 1 uatom undelegation at 1500002 tokens for
	// 2000000 shares.
	tokens, shares := sdkmath.NewInt(1500002), sdkmath.LegacyNewDec(2000000)
	moved := shares.MulInt(sdkmath.NewInt(1)).QuoInt(tokens)
	require.True(t, moved.MulInt(tokens).Quo(shares).TruncateInt().IsZero(), "the undelegation releases no tokens")
	left := shares.Sub(moved).String()
	exec(`INSERT INTO legacy.genesis_validators (chain_id, operator, tokens, delegator_shares, status, jailed) VALUES
('cosmoshub-3', 'val', 1500002, 2000000, 'BONDED', false), ('cosmoshub-4', 'val', 1500002, ?, 'BONDED', false)`, left)
	exec(`INSERT INTO legacy.genesis_delegations (chain_id, delegator, validator, shares, tokens) VALUES
('cosmoshub-3', 'alice', 'val', 2000000, 1500002), ('cosmoshub-4', 'alice', 'val', ?, 1500001)`, left)
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	completion := when.Add(21 * 24 * time.Hour)
	exec(`INSERT INTO legacy.genesis_unbondings (chain_id, delegator, validator, entry, creation_height, completion_time, initial_balance, balance) VALUES
('cosmoshub-4', 'alice', 'val', 0, 1, ?, 0, 0)`, completion)
	_, err = ledger.Write(ctx, conn, []ledger.Block{{Chain: "cosmoshub-3", Height: 1, Time: when, Hash: strings.Repeat("AB", 32), Proposer: "AA"}})
	require.NoError(t, err)

	build := func() *balancedays.Result {
		t.Helper()
		result, err := balancedays.Build(ctx, conn, "cosmoshub-3", func(balancedays.Row) error { return nil })
		require.NoError(t, err)
		return result
	}
	without := build()
	require.True(t, without.ExactReplay)
	require.False(t, without.Passed())
	require.Equal(t, 1, without.ValidatorsDiffer)

	hash := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	balancedays.ExtraMoves["cosmoshub-3"] = []balancedays.MoveRecord{{Height: 1, Kind: "undelegate", Delegator: hash("alice"), Validator: hash("val"),
		Tokens: "1", Completion: completion.Format(time.RFC3339Nano)}}
	balancedays.MessageTokens["cosmoshub-3"] = map[[3]int64]string{{1, 0, 0}: "1"}
	t.Cleanup(func() {
		delete(balancedays.ExtraMoves, "cosmoshub-3")
		delete(balancedays.MessageTokens, "cosmoshub-3")
	})
	result := build()
	require.True(t, result.Passed())
	require.Equal(t, int64(1), result.AddedMoves)
	require.Equal(t, int64(0), result.EventRows)
	require.Equal(t, 1, result.ValidatorsExact)
	require.Equal(t, 1, result.SharesExact)
	require.Zero(t, result.Unbonding.Outside, "an entry of no tokens matches the empty genesis entry")

	balancedays.ExtraMoves["cosmoshub-3"][0].Delegator = hash("nobody")
	_, err = balancedays.Build(ctx, conn, "cosmoshub-3", func(balancedays.Row) error { return nil })
	require.ErrorContains(t, err, "names a delegator or validator the replay does not hold")
}
