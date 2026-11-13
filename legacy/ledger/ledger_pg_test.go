package ledger_test

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema/pgtest"
	"github.com/stretchr/testify/require"
)

func sample(chain string, height int64) ledger.Block {
	hash := strings.Repeat("0A", 32)
	return ledger.Block{Chain: chain, Height: height, Time: time.Date(2020, 1, 2, 3, 4, 5, 123456789, time.FixedZone("x", 3600)),
		Hash: strings.Repeat("AB", 32), Proposer: "AA11",
		Inclusions: []ledger.Inclusion{
			{Index: 0, Hash: hash, Code: 0, CodeEvidence: "explicit", GasWanted: 200000, GasUsed: 51234, Fee: "5000uatom", FeePayer: "payer",
				FeeCharged: true, FeeEvidence: "the transaction succeeded", Memo: "hello", Signers: []string{"payer", "second"}, Raw: []byte{1, 2, 0, 3}},
			{Index: 1, Hash: hash, Code: 12, Codespace: "sdk", CodeEvidence: "explicit", GasWanted: 1, GasUsed: 2, Fee: "", FeePayer: "payer",
				FeeCharged: false, FeeEvidence: "it ran out of gas before the fee was taken", Signers: []string{"payer"}, Raw: []byte{9}},
		},
		Rows: []ledger.Row{
			{Phase: ledger.PhaseTx, TxIndex: 0, MsgIndex: 0, SubIndex: 0, Category: "transfer", Amount: "53", Denom: "uatom", From: "payer", To: "other", TxHash: hash, Source: "message"},
			{Phase: ledger.PhaseTx, TxIndex: 0, MsgIndex: 1, SubIndex: 0, Category: "reward", Amount: "7", Denom: "uatom", To: "payer", Validator: "val", Trigger: "delegate", TxHash: hash, Source: "event"},
			{Phase: ledger.PhaseEnd, SubIndex: 0, Category: "gov_deposit_refund", Amount: "512", Denom: "uatom", From: "gov", To: "payer", Source: "event"},
		},
		Events: []ledger.Event{
			{Phase: ledger.PhaseBegin, Index: 4, Type: "slash", Attributes: [][2]string{{"address", "c"}, {"reason", "double_sign"}, {"address", "again"}}},
			{Phase: ledger.PhaseEnd, Index: 0, Type: "complete_unbonding", Attributes: [][2]string{}},
		}}
}

func importer(t *testing.T, db pgtest.Database) *sql.DB {
	t.Helper()
	out, err := db.Importer.DB()
	require.NoError(t, err)
	return out
}

func TestWriteStoresHeightsAsTheImporterAndNeverRewritesThem(t *testing.T) {
	db := pgtest.OpenRetained(t)
	ctx := context.Background()
	conn := importer(t, db)
	before := pgtest.Snapshot(t, db.Owner, "public")

	first, empty := sample("cosmoshub-3", 9), ledger.Block{Chain: "cosmoshub-3", Height: 10, Time: time.Date(2020, 1, 2, 3, 4, 12, 0, time.UTC), Hash: strings.Repeat("CD", 32), Proposer: "BB22"}
	added, err := ledger.Write(ctx, conn, []ledger.Block{first, empty})
	require.NoError(t, err)
	require.Equal(t, 2, added)

	stored, err := ledger.Load(ctx, conn, "cosmoshub-3", []int64{9, 10, 11})
	require.NoError(t, err)
	require.Len(t, stored, 2, "a height that is not stored is absent")
	want := sample("cosmoshub-3", 9)
	want.Time = want.Time.UTC().Truncate(time.Microsecond)
	require.Equal(t, want, stored[0])
	require.Equal(t, int64(10), stored[1].Height)
	require.Empty(t, stored[1].Inclusions)

	// The same heights again add nothing, alone or beside a new height.
	legacyBefore := pgtest.Snapshot(t, db.Owner, "legacy")
	added, err = ledger.Write(ctx, conn, []ledger.Block{sample("cosmoshub-3", 9), empty})
	require.NoError(t, err)
	require.Zero(t, added)
	pgtest.RequireUntouched(t, legacyBefore, pgtest.Snapshot(t, db.Owner, "legacy"))

	// A height number means nothing without its chain.
	added, err = ledger.Write(ctx, conn, []ledger.Block{sample("cosmoshub-2", 9)})
	require.NoError(t, err)
	require.Equal(t, 1, added)
	legacyBefore = pgtest.Snapshot(t, db.Owner, "legacy")

	// Any difference from what is stored is refused, and a run that holds
	// one such height stores none of its other heights.
	changes := map[string]func(*ledger.Block){
		"time":            func(b *ledger.Block) { b.Time = b.Time.Add(time.Second) },
		"block hash":      func(b *ledger.Block) { b.Hash = strings.Repeat("EF", 32) },
		"fewer txs":       func(b *ledger.Block) { b.Inclusions = b.Inclusions[:1]; b.Rows = b.Rows[:2] },
		"code":            func(b *ledger.Block) { b.Inclusions[1].Code = 0 },
		"fee verdict":     func(b *ledger.Block) { b.Inclusions[1].FeeCharged = true },
		"signers":         func(b *ledger.Block) { b.Inclusions[0].Signers = []string{"payer"} },
		"bytes":           func(b *ledger.Block) { b.Inclusions[0].Raw = []byte{1, 2, 3} },
		"amount":          func(b *ledger.Block) { b.Rows[0].Amount = "54" },
		"recipient":       func(b *ledger.Block) { b.Rows[0].To = "another" },
		"missing row":     func(b *ledger.Block) { b.Rows = b.Rows[:2] },
		"event":           func(b *ledger.Block) { b.Events[0].Attributes[1][1] = "downtime" },
		"missing event":   func(b *ledger.Block) { b.Events = b.Events[:1] },
		"attribute order": func(b *ledger.Block) { a := b.Events[0].Attributes; a[0], a[2] = a[2], a[0] },
	}
	for name, change := range changes {
		different := sample("cosmoshub-3", 9)
		change(&different)
		_, err = ledger.Write(ctx, conn, []ledger.Block{different, {Chain: "cosmoshub-3", Height: 11, Time: empty.Time, Hash: empty.Hash, Proposer: "BB22"}})
		require.ErrorIs(t, err, ledger.ErrStoredDiffers, name)
	}
	pgtest.RequireUntouched(t, legacyBefore, pgtest.Snapshot(t, db.Owner, "legacy"))
	pgtest.RequireUntouched(t, before, pgtest.Snapshot(t, db.Owner, "public"))

	// A row worked out later by another step does not make a rerun differ.
	require.NoError(t, db.Owner.Exec(`INSERT INTO legacy.taxable_events (chain_id, height, phase, tx_index, msg_index, sub_index, category, amount, denom, "timestamp", amount_source)
VALUES ('cosmoshub-3', 9, 3, 0, 0, 1, 'unbonding_complete', '5', 'uatom', now(), 'state')`).Error)
	added, err = ledger.Write(ctx, conn, []ledger.Block{sample("cosmoshub-3", 9)})
	require.NoError(t, err)
	require.Zero(t, added)

	// The importer role cannot change or remove what it stored.
	for _, statement := range []string{
		`UPDATE legacy.blocks SET proposer = 'x'`, `DELETE FROM legacy.blocks`, `UPDATE legacy.tx_inclusions SET code = 0`,
		`DELETE FROM legacy.tx_inclusions`, `UPDATE legacy.taxable_events SET amount = '1'`, `DELETE FROM legacy.taxable_events`,
		`UPDATE legacy.block_events SET type = 'x'`, `DELETE FROM legacy.block_events`, `TRUNCATE legacy.block_events`,
	} {
		_, err := conn.ExecContext(ctx, statement)
		require.Error(t, err, statement)
	}
}

func TestWriteRefusesWhatTheLedgerDoesNotHold(t *testing.T) {
	db := pgtest.OpenRetained(t)
	ctx := context.Background()
	conn := importer(t, db)
	bad := map[string]func(*ledger.Block){
		"cosmoshub-4":             func(b *ledger.Block) { b.Chain = "cosmoshub-4" },
		"no height":               func(b *ledger.Block) { b.Height = 0 },
		"no time":                 func(b *ledger.Block) { b.Time = time.Time{} },
		"lower case hash":         func(b *ledger.Block) { b.Hash = strings.ToLower(b.Hash) },
		"gap in transactions":     func(b *ledger.Block) { b.Inclusions[1].Index = 2 },
		"no fee evidence":         func(b *ledger.Block) { b.Inclusions[0].FeeEvidence = "" },
		"no signers":              func(b *ledger.Block) { b.Inclusions[0].Signers = nil },
		"memo with a NUL":         func(b *ledger.Block) { b.Inclusions[0].Memo = "a\x00b" },
		"row for a missing tx":    func(b *ledger.Block) { b.Rows[1].TxIndex = 5 },
		"rows out of order":       func(b *ledger.Block) { b.Rows[0], b.Rows[1] = b.Rows[1], b.Rows[0] },
		"repeated row":            func(b *ledger.Block) { b.Rows[1] = b.Rows[0] },
		"amount with a fraction":  func(b *ledger.Block) { b.Rows[0].Amount = "5.5" },
		"no amount":               func(b *ledger.Block) { b.Rows[0].Amount = "" },
		"source worked out later": func(b *ledger.Block) { b.Rows[0].Source = "state" },
		"export payout phase":     func(b *ledger.Block) { b.Rows[2].Phase = 4 },
		"block row with position": func(b *ledger.Block) { b.Rows[2].MsgIndex = 1 },
		"event in a transaction":  func(b *ledger.Block) { b.Events[0].Phase = 2 },
		"events out of order":     func(b *ledger.Block) { b.Events[0], b.Events[1] = b.Events[1], b.Events[0] },
	}
	for name, change := range bad {
		block := sample("cosmoshub-3", 9)
		change(&block)
		_, err := ledger.Write(ctx, conn, []ledger.Block{block})
		require.Error(t, err, name)
	}
	_, err := ledger.Write(ctx, conn, []ledger.Block{sample("cosmoshub-3", 9), sample("cosmoshub-2", 10)})
	require.Error(t, err, "two chains in one run")
	_, err = ledger.Write(ctx, conn, []ledger.Block{sample("cosmoshub-3", 9), sample("cosmoshub-3", 9)})
	require.Error(t, err, "the same height twice")
	stored, err := ledger.Load(ctx, conn, "cosmoshub-3", []int64{9, 10})
	require.NoError(t, err)
	require.Empty(t, stored)
	require.Equal(t, "a�b", ledger.Text([]byte{'a', 0xff, 0, 'b'}))
}

func TestARangeBelongsToOneImporterAndResumesWhereItStopped(t *testing.T) {
	db := pgtest.OpenRetained(t)
	ctx := context.Background()
	conn := importer(t, db)
	r := ledger.Range{Chain: "cosmoshub-3", From: 1, To: 100}
	claim, progress, err := ledger.Begin(ctx, conn, r, "build-a")
	require.NoError(t, err)
	require.Equal(t, int64(1), progress.Next)

	_, _, err = ledger.Begin(ctx, conn, r, "build-a")
	require.ErrorContains(t, err, "another importer is working on this range")
	for _, other := range []ledger.Range{{Chain: "cosmoshub-3", From: 50, To: 150}, {Chain: "cosmoshub-3", From: 100, To: 100}} {
		_, _, err = ledger.Begin(ctx, conn, other, "build-a")
		require.ErrorContains(t, err, "overlaps another recorded range", other)
	}
	for _, bad := range []ledger.Range{{Chain: "cosmoshub-3", From: 0, To: 5}, {Chain: "cosmoshub-3", From: 9, To: 8}} {
		_, _, err = ledger.Begin(ctx, conn, bad, "build-a")
		require.Error(t, err)
	}
	// The same numbers on another chain are another range.
	other, _, err := ledger.Begin(ctx, conn, ledger.Range{Chain: "cosmoshub-2", From: 1, To: 100}, "build-a")
	require.NoError(t, err)
	other.Release()
	next, _, err := ledger.Begin(ctx, conn, ledger.Range{Chain: "cosmoshub-3", From: 101, To: 200}, "build-a")
	require.NoError(t, err)
	next.Release()

	require.Error(t, claim.Record(ctx, ledger.Progress{Status: "done", Next: 60}), "done needs every height stored")
	require.Error(t, claim.Record(ctx, ledger.Progress{Status: "done", Next: 101, Held: 1}))
	require.NoError(t, claim.Record(ctx, ledger.Progress{Status: "failed", Next: 60, Stored: 59, Held: 2, Transactions: 7, Rows: 11}))
	claim.Release()

	_, _, err = ledger.Begin(ctx, conn, ledger.Range{Chain: "cosmoshub-3", From: 1, To: 90}, "build-b")
	require.ErrorContains(t, err, "different last height")
	claim, progress, err = ledger.Begin(ctx, conn, r, "build-b")
	require.NoError(t, err)
	require.Equal(t, ledger.Progress{Status: "running", Next: 60, Stored: 59, Transactions: 7, Rows: 11}, progress)
	progress.Next, progress.Stored, progress.Status = 101, 100, "done"
	require.NoError(t, claim.Record(ctx, progress))
	claim.Release()
	var recorded string
	require.NoError(t, db.Owner.Raw(`SELECT status || ' ' || next_height || ' ' || heights_stored || ' ' || heights_held || ' ' || build FROM legacy.import_ranges WHERE chain_id = 'cosmoshub-3' AND from_height = 1`).Scan(&recorded).Error)
	require.Equal(t, "done 101 100 0 build-b", recorded)

	// A range keeps the evidence files it was started with. Another set,
	// or none, cannot continue it; the same set can, under any build.
	files := ledger.Range{Chain: "cosmoshub-3", From: 201, To: 300, Evidence: "fee verdicts aa removal verdicts bb"}
	claim, _, err = ledger.Begin(ctx, conn, files, "build-a")
	require.NoError(t, err)
	claim.Release()
	for _, other := range []string{"", "fee verdicts aa removal verdicts cc", "fee verdicts aa"} {
		changed := files
		changed.Evidence = other
		_, _, err = ledger.Begin(ctx, conn, changed, "build-a")
		require.ErrorContains(t, err, "started with other evidence files", other)
	}
	_, _, err = ledger.Begin(ctx, conn, ledger.Range{Chain: "cosmoshub-3", From: 1, To: 100, Evidence: "fee verdicts aa"}, "build-b")
	require.ErrorContains(t, err, "started with other evidence files", "a range started without evidence")
	_, _, err = ledger.Begin(ctx, conn, files, "build | evidence x")
	require.Error(t, err)
	claim, _, err = ledger.Begin(ctx, conn, files, "build-c")
	require.NoError(t, err)
	claim.Release()
	require.NoError(t, db.Owner.Raw(`SELECT build FROM legacy.import_ranges WHERE chain_id = 'cosmoshub-3' AND from_height = 201`).Scan(&recorded).Error)
	require.Equal(t, "build-c | evidence fee verdicts aa removal verdicts bb", recorded)
}

func TestAReleasedRangeIsFreeForAnotherSession(t *testing.T) {
	db := pgtest.OpenRetained(t)
	ctx := context.Background()
	pool := importer(t, db)
	r := ledger.Range{Chain: "cosmoshub-3", From: 1, To: 100}
	claim, _, err := ledger.Begin(ctx, pool, r, "build-a")
	require.NoError(t, err)
	claim.Release()
	// Keep every idle session of the pool busy, so the next claim runs on a
	// session that never held the range.
	for i := 0; i < 3; i++ {
		busy, err := pool.Conn(ctx)
		require.NoError(t, err)
		defer busy.Close()
	}
	again, _, err := ledger.Begin(ctx, pool, r, "build-a")
	require.NoError(t, err, "a released range must not stay locked by a pooled session")
	// A claim that is refused leaves nothing locked either.
	_, _, err = ledger.Begin(ctx, pool, ledger.Range{Chain: "cosmoshub-3", From: 1, To: 90}, "build-a")
	require.Error(t, err)
	again.Release()
	var held int
	require.NoError(t, db.Owner.Raw(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&held).Error)
	require.Zero(t, held)
}

func TestOverlappingRangesClaimedAtOnceGiveOneImporter(t *testing.T) {
	for _, isolation := range []string{"read committed", "repeatable read"} {
		t.Run(isolation, func(t *testing.T) {
			db := pgtest.OpenRetained(t)
			pool := importer(t, db)
			// New sessions of the importer start with this default.
			require.NoError(t, db.Owner.Exec("ALTER ROLE "+db.Role+" SET default_transaction_isolation TO '"+isolation+"'").Error)
			pool.SetMaxIdleConns(0)
			pool.SetMaxIdleConns(4)
			var current string
			require.NoError(t, pool.QueryRow("SHOW default_transaction_isolation").Scan(&current))
			require.Equal(t, isolation, current)
			claimOverlappingRangesAtOnce(t, db, pool)
		})
	}
}

func claimOverlappingRangesAtOnce(t *testing.T, db pgtest.Database, pool *sql.DB) {
	ctx := context.Background()
	for round := int64(0); round < 40; round++ {
		base := round * 1000
		ranges := []ledger.Range{{Chain: "cosmoshub-2", From: base + 1, To: base + 100}, {Chain: "cosmoshub-2", From: base + 50, To: base + 150}}
		errs := make([]error, len(ranges))
		start := make(chan struct{})
		var wait sync.WaitGroup
		for i := range ranges {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				claim, _, err := ledger.Begin(ctx, pool, ranges[i], "build-a")
				errs[i] = err
				if err == nil {
					claim.Release()
				}
			}()
		}
		close(start)
		wait.Wait()
		require.True(t, (errs[0] == nil) != (errs[1] == nil), "round %d: exactly one of two overlapping ranges may be claimed", round)
	}
	var recorded int
	require.NoError(t, db.Owner.Raw(`SELECT count(*) FROM legacy.import_ranges`).Scan(&recorded).Error)
	require.Equal(t, 40, recorded)
}

// moved builds a height whose transactions carry the given tax rows.
func moved(chain string, height int64, rows ...ledger.Row) ledger.Block {
	b := ledger.Block{Chain: chain, Height: height, Time: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC), Hash: strings.Repeat("AB", 32), Proposer: "AA11"}
	for _, row := range rows {
		for row.TxIndex >= len(b.Inclusions) {
			b.Inclusions = append(b.Inclusions, ledger.Inclusion{Index: len(b.Inclusions), Hash: strings.Repeat("0A", 32), CodeEvidence: "explicit",
				FeePayer: "payer", FeeCharged: true, FeeEvidence: "the transaction succeeded", Signers: []string{"payer"}, Raw: []byte{1}})
		}
		row.Phase, row.Amount, row.Denom = ledger.PhaseTx, "5", "uatom"
		b.Rows = append(b.Rows, row)
	}
	return b
}

func TestLastLeavesListsEveryMessageThatCouldHaveRemovedAValidator(t *testing.T) {
	db := pgtest.OpenRetained(t)
	ctx := context.Background()
	conn := importer(t, db)
	leave := func(tx, msg int, validator string) ledger.Row {
		return ledger.Row{TxIndex: tx, MsgIndex: msg, Category: "undelegate", From: "who", To: "who", Validator: validator, Detail: `{"completion_time":"x"}`, Source: "event"}
	}
	move := func(tx, msg int, from, to string) ledger.Row {
		return ledger.Row{TxIndex: tx, MsgIndex: msg, Category: "redelegate", From: "who", Validator: to, Source: "event",
			Detail: `{"source_validator":"` + from + `","destination_validator":"` + to + `","completion_time":"x"}`}
	}
	blocks := []ledger.Block{
		moved("cosmoshub-3", 5, leave(0, 0, "a")),
		// A reward of the same message is not a second move.
		moved("cosmoshub-3", 6, move(0, 0, "a", "b"), ledger.Row{TxIndex: 0, MsgIndex: 0, SubIndex: 1, Category: "reward", To: "who", Validator: "a", Trigger: "redelegate", Source: "event"}, leave(0, 1, "b")),
		// The first row creates validator a again. A plain delegation to b
		// creates nothing, so b's leave stays the last one.
		moved("cosmoshub-3", 7, ledger.Row{TxIndex: 0, Category: "delegate", From: "who", Validator: "a", Source: "message"},
			ledger.Row{TxIndex: 1, Category: "delegate", From: "who", Validator: "b", Source: "event"}),
		moved("cosmoshub-3", 8, leave(0, 0, "a"), leave(1, 0, "a")),
		moved("cosmoshub-3", 9, leave(0, 0, "c")),
	}
	_, err := ledger.Write(ctx, conn, blocks)
	require.NoError(t, err)
	// Another chain's heights are not this chain's.
	_, err = ledger.Write(ctx, conn, []ledger.Block{moved("cosmoshub-2", 8, leave(2, 0, "a"))})
	require.NoError(t, err)

	for want, r := range map[string]ledger.Range{
		"cosmoshub-3/6/0/0 cosmoshub-3/6/0/1 cosmoshub-3/8/1/0 cosmoshub-3/9/0/0": {Chain: "cosmoshub-3", From: 1, To: 9},
		"cosmoshub-3/6/0/0 cosmoshub-3/6/0/1 cosmoshub-3/8/1/0":                   {Chain: "cosmoshub-3", From: 5, To: 8},
		// A range that ends early lists the last leave it holds.
		"cosmoshub-3/5/0/0":                   {Chain: "cosmoshub-3", From: 1, To: 5},
		"cosmoshub-3/6/0/0 cosmoshub-3/6/0/1": {Chain: "cosmoshub-3", From: 6, To: 7},
		"cosmoshub-2/8/2/0":                   {Chain: "cosmoshub-2", From: 1, To: 9},
		"":                                    {Chain: "cosmoshub-3", From: 10, To: 20},
	} {
		got, err := ledger.LastLeaves(ctx, conn, r)
		require.NoError(t, err)
		require.Equal(t, want, strings.Join(got, " "), r)
	}
}
