package taxapi

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const earlyTip int64 = 1_000_000

// earlyBlock is one block of the early-stop fixture: an authz exec by the
// wallet that names no delegator, delegating unnamed, and the wallet's own
// delegation of named in the same block. clean says whether the wallet's
// delegations either side of the block account for both.
type earlyBlock struct {
	h, unnamed, named int64
	// matched is a delegation in its own message that names no delegator
	// beside a reward withdrawal that names the wallet: attributeStakes
	// gives it to the wallet.
	matched int64
	clean   bool
	// placed, when set, dates the exec at these heights instead (effective
	// inclusions in other blocks), so one message is read in two windows.
	placed []int64
	// dirty is the one placed height the delegations do not account for.
	dirty int64
	// foreign is a second unnamed delegation in the exec, to another
	// validator, that is not the wallet's; foreignFirst puts it first.
	foreign      int64
	foreignFirst bool
}

// earlyFixture loads blocks into temp tables and returns the database, the
// wallet, and the wallet's delegations either side of each block.
func earlyFixture(t *testing.T, blocks []earlyBlock) (*gorm.DB, string, map[int64]map[string]int64) {
	t.Helper()
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres early-stop verification")
	}
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	connection.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	database = database.WithContext(ctx)

	w := testAddress(t, 173)
	const v1 = "cosmosvaloper1early"
	for _, sql := range []string{
		tempSearchPath,
		tempChainsTable,
		"CREATE TEMP TABLE blocks (id bigint, chain_id bigint, height bigint, time_stamp timestamptz, tx_indexed boolean, block_events_indexed boolean)",
		tempTxesTable,
		tempMsgsTable,
		"CREATE TEMP TABLE message_events (id bigint, message_id bigint, message_event_type_id bigint, index bigint)",
		"CREATE TEMP TABLE message_event_types (id bigint, type text)",
		"CREATE TEMP TABLE message_event_attributes (message_event_id bigint, message_event_attribute_key_id bigint, value text, index bigint)",
		"CREATE TEMP TABLE message_event_attribute_keys (id bigint, key text)",
		"CREATE TEMP TABLE block_events (id bigint, block_id bigint, block_event_type_id bigint, lifecycle_position bigint)",
		tempBlockEventTypes,
		"CREATE TEMP TABLE block_event_attributes (block_event_id bigint, block_event_attribute_key_id bigint, value text, index bigint)",
		tempBlockEventKeys,
		tempHubChainRow,
		"INSERT INTO message_event_types VALUES (1,'coin_spent'),(2,'coin_received'),(3,'message'),(4,'delegate'),(5,'unbond'),(6,'withdraw_rewards'),(7,'transfer')",
		"INSERT INTO message_event_attribute_keys VALUES (1,'spender'),(2,'receiver'),(3,'amount'),(4,'sender'),(5,'module'),(6,'validator'),(7,'new_shares'),(8,'completion_time'),(9,'delegator'),(10,'recipient'),(11,'action')",
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	require.NoError(t, inclusionsMigrate(database))
	// A txes row with two effective inclusions cannot be stored in production;
	// the fixture drops the guard to prove each message is read once.
	for _, ix := range []string{"idx_tax_tx_inclusions_effective_tx", "idx_tax_tx_inclusions_effective_hash"} {
		require.NoError(t, database.Exec("DROP INDEX IF EXISTS "+ix).Error)
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		q := fmt.Sprintf(sql, args...)
		require.NoError(t, database.Exec(q).Error, q)
	}
	block := func(h int64) {
		exec("INSERT INTO blocks SELECT %[1]d,1,%[1]d,timestamptz '2022-01-10' + %[1]d * interval '6 seconds',true,true WHERE NOT EXISTS (SELECT 1 FROM blocks WHERE id = %[1]d)", h)
	}
	states := map[int64]map[string]int64{}
	var stake int64 = 1000
	// Oldest first, so each block's state follows the one below it.
	for k := len(blocks) - 1; k >= 0; k-- {
		b := blocks[k]
		n := int64(k + 1)
		x, d := 2*n-1, 2*n
		block(b.h)
		exec("INSERT INTO txes VALUES (%d,%d,'X%d',0)", x, b.h, n)
		exec("INSERT INTO messages VALUES (%d,%d)", x, x)
		exec("INSERT INTO message_events VALUES (%[1]d0,%[1]d,3,0),(%[1]d1,%[1]d,4,2)", x)
		exec("INSERT INTO message_event_attributes VALUES (%[1]d0,11,'/cosmos.authz.v1beta1.MsgExec',0),(%[1]d0,4,'%[2]s',1),(%[1]d1,6,'%[3]s',0),(%[1]d1,3,'%[4]duatom',1)", x, w, v1, b.unnamed)
		if b.foreign > 0 {
			at := 3
			if b.foreignFirst {
				at = 1
			}
			exec("INSERT INTO message_events VALUES (%[1]d2,%[1]d,4,%[2]d)", x, at)
			exec("INSERT INTO message_event_attributes VALUES (%[1]d2,6,'cosmosvaloper1other',0),(%[1]d2,3,'%[2]duatom',1)", x, b.foreign)
		}
		for i, p := range b.placed {
			block(p)
			exec("INSERT INTO tax_tx_inclusions (chain, height, tx_index, hash, tx_id, block_id, code, effective, recorded_at) VALUES ('cosmoshub-4',%d,%d,'X%d',%d,%d,0,true,now())", p, i, n, x, p)
		}
		if b.named > 0 {
			exec("INSERT INTO txes VALUES (%d,%d,'D%d',0)", d, b.h, n)
			exec("INSERT INTO messages VALUES (%d,%d)", d, d)
			exec("INSERT INTO message_events VALUES (%[1]d0,%[1]d,1,0),(%[1]d1,%[1]d,3,1),(%[1]d2,%[1]d,4,2)", d)
			exec("INSERT INTO message_event_attributes VALUES (%[1]d0,1,'%[2]s',0),(%[1]d0,3,'%[4]duatom',1),(%[1]d1,5,'staking',0),(%[1]d1,4,'%[2]s',1),(%[1]d2,6,'%[3]s',0),(%[1]d2,3,'%[4]duatom',1),(%[1]d2,7,'%[4]d.000000000000000000',2)", d, w, v1, b.named)
		}
		if b.matched > 0 {
			r := 2*n + 1_000_001
			exec("INSERT INTO txes VALUES (%d,%d,'R%d',0)", r, b.h, n)
			exec("INSERT INTO messages VALUES (%d,%d)", r, r)
			exec("INSERT INTO message_events VALUES (%[1]d0,%[1]d,6,0),(%[1]d1,%[1]d,4,1)", r)
			exec("INSERT INTO message_event_attributes VALUES (%[1]d0,9,'%[2]s',0),(%[1]d0,6,'%[3]s',1),(%[1]d1,6,'%[3]s',0),(%[1]d1,3,'%[4]duatom',1)", r, w, v1, b.matched)
		}
		heights := b.placed
		if len(heights) == 0 {
			heights = []int64{b.h}
		}
		for _, h := range heights {
			before := stake
			stake += b.named + b.unnamed + b.matched
			after := stake
			if !b.clean || h == b.dirty {
				after += 7
				stake = after
			}
			states[h-1] = map[string]int64{v1: before}
			states[h] = map[string]int64{v1: after}
		}
	}
	return database, w, states
}

// walkStop is the walk's own stop over (lowest, tip]: its ledger read and
// settleUnnamedStakes, as rebuildHoldings runs them.
func walkStop(t *testing.T, s *Server, w string, lowest int64, at func(int64) (map[string]*big.Int, error)) int64 {
	t.Helper()
	events, unmatched, err := s.ledgerStakeEvents([]int64{1}, w, "", lowest, earlyTip)
	require.NoError(t, err)
	_, stop := settleUnnamedStakes(at, w, "", "uatom", events, unmatched)
	return stop
}

// budgeted is delegationsBy with a limit on the calls, as one archive budget
// limits a walk's: past it every call fails.
func budgeted(states map[int64]map[string]int64, limit int, read *[]int64) func(int64) (map[string]*big.Int, error) {
	at := delegationsBy(states, read)
	left := limit
	return func(h int64) (map[string]*big.Int, error) {
		if left <= 0 {
			return nil, errArchiveCeiling
		}
		left--
		return at(h)
	}
}

// The pre-check's stop is the walk's whenever the walk stops above every
// date, and 0 otherwise, for dates above, at and below each block and each
// window edge, with and without block reads.
func TestUnnamedStopAboveMatchesTheWalkPostgres(t *testing.T) {
	// Windows below the tip: (900000, 1000000], (700000, 900000],
	// (300000, 700000], then down to the floor.
	cases := []struct {
		name   string
		blocks []earlyBlock
	}{
		// A settled block above a stop exactly on a window edge, which needs
		// the named delegation in its own block to settle.
		{"stop on an edge below a settled block", []earlyBlock{
			{h: 950_000, unnamed: 50, named: 30, clean: true},
			{h: 900_000, unnamed: 40, named: 20, clean: false},
			{h: 650_000, unnamed: 10, clean: false},
			{h: 250_000, unnamed: 5, clean: true},
		}},
		// The edge block settles only with its named delegation, and the stop
		// is two windows down, below settled blocks in the first two.
		{"stop in a lower window below settled groups", []earlyBlock{
			{h: 950_000, unnamed: 50, named: 30, clean: true},
			{h: 900_000, unnamed: 40, named: 20, clean: true},
			{h: 700_001, unnamed: 15, named: 25, clean: true},
			{h: 650_000, unnamed: 10, clean: false},
			{h: 250_000, unnamed: 5, clean: false},
		}},
		// Nothing stops: every block settles.
		{"no stop", []earlyBlock{
			{h: 950_000, unnamed: 50, named: 30, clean: true},
			{h: 900_000, unnamed: 40, named: 20, clean: true},
			{h: 250_000, unnamed: 5, clean: true},
		}},
		// The first block down does not settle.
		{"stop in the first window", []earlyBlock{
			{h: 999_999, unnamed: 50, clean: false},
			{h: 900_000, unnamed: 40, named: 20, clean: false},
		}},
		// One exec dated at two heights in two windows: read once, its
		// records are settled in each window as the walk settles them.
		{"one message dated in two windows", []earlyBlock{
			{h: 960_000, unnamed: 50, named: 30, clean: true},
			{h: 10, unnamed: 20, clean: true, placed: []int64{940_000, 600_000}},
			{h: 400_000, unnamed: 9, clean: false},
		}},
		// The same, with the lower dating not settling: the stop is there.
		{"one message dated in two windows, stop at the lower", []earlyBlock{
			{h: 960_000, unnamed: 50, named: 30, clean: true},
			{h: 10, unnamed: 20, clean: true, placed: []int64{940_000, 600_000}, dirty: 600_000},
			{h: 400_000, unnamed: 9, clean: false},
		}},
		// A block whose exec holds the wallet's unnamed delegation and
		// someone else's, in either order: both are tried together.
		{"two unnamed records in one block, foreign last", []earlyBlock{
			{h: 960_000, unnamed: 50, foreign: 11, clean: true},
			{h: 400_000, unnamed: 9, clean: false},
		}},
		{"two unnamed records in one block, foreign first", []earlyBlock{
			{h: 960_000, unnamed: 50, foreign: 11, foreignFirst: true, clean: true},
			{h: 400_000, unnamed: 9, clean: false},
		}},
		// Blocks on and just above window edges that settle only with the
		// delegation attributeStakes gave the wallet in the same block.
		{"attributed records on window edges", []earlyBlock{
			{h: 900_001, unnamed: 40, matched: 20, clean: true},
			{h: 900_000, unnamed: 30, matched: 25, named: 5, clean: true},
			{h: 700_001, unnamed: 15, matched: 25, clean: true},
			{h: 650_000, unnamed: 10, clean: false},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			database, w, states := earlyFixture(t, c.blocks)
			var floors []int64
			for _, b := range c.blocks {
				for _, h := range append([]int64{b.h}, b.placed...) {
					floors = append(floors, h+1, h, h-1)
				}
			}
			floors = append(floors, earlyTip-1, 900_001, 900_000, 899_999, 700_001, 700_000, 699_999, 300_000, 1)
			for _, s := range []*Server{{db: database, native: DefaultNativeAsset}, {db: database, native: DefaultNativeAsset, blockReads: true}} {
				for _, floor := range floors {
					var wr, er []int64
					want := walkStop(t, s, w, floor, delegationsBy(states, &wr))
					if want <= floor {
						want = 0
					}
					got, err := s.unnamedStopAbove([]int64{1}, w, "", floor, earlyTip, delegationsBy(states, &er))
					require.NoError(t, err)
					require.Equal(t, want, got, "floor %d block reads %t", floor, s.blockReads)
				}
			}
		})
	}
}

// The expected stops of the fixtures above, so a pre-check and a walk that
// were both wrong the same way would still fail.
func TestUnnamedStopAboveStopsPostgres(t *testing.T) {
	database, w, states := earlyFixture(t, []earlyBlock{
		{h: 950_000, unnamed: 50, named: 30, clean: true},
		{h: 900_000, unnamed: 40, named: 20, clean: false},
		{h: 650_000, unnamed: 10, clean: false},
		{h: 250_000, unnamed: 5, clean: true},
	})
	for _, s := range []*Server{{db: database, native: DefaultNativeAsset}, {db: database, native: DefaultNativeAsset, blockReads: true}} {
		for floor, want := range map[int64]int64{
			960_000: 0, 950_000: 0, 949_999: 0, 900_001: 0, 900_000: 0,
			899_999: 900_000, 650_000: 900_000, 1: 900_000,
		} {
			var read []int64
			got, err := s.unnamedStopAbove([]int64{1}, w, "", floor, earlyTip, delegationsBy(states, &read))
			require.NoError(t, err)
			require.Equal(t, want, got, "floor %d block reads %t", floor, s.blockReads)
		}
		// No archive: the first block down holding an unnamed record stops.
		got, err := s.unnamedStopAbove([]int64{1}, w, "", 1, earlyTip, nil)
		require.NoError(t, err)
		require.Equal(t, int64(950_000), got)
	}
	database, w, states = earlyFixture(t, []earlyBlock{
		{h: 950_000, unnamed: 50, named: 30, clean: true},
		{h: 900_000, unnamed: 40, named: 20, clean: true},
		{h: 700_001, unnamed: 15, named: 25, clean: true},
		{h: 650_000, unnamed: 10, clean: false},
		{h: 250_000, unnamed: 5, clean: false},
	})
	var read []int64
	got, err := (&Server{db: database, native: DefaultNativeAsset}).unnamedStopAbove([]int64{1}, w, "", 1, earlyTip, delegationsBy(states, &read))
	require.NoError(t, err)
	require.Equal(t, int64(650_000), got, "the highest stop, below the settled groups")
	require.Equal(t, []int64{949_999, 950_000, 899_999, 900_000, 700_000, 700_001, 649_999, 650_000}, read, "blocks settled newest first, each once")
}

// The pre-check spends one budget across every window, as the walk does, so
// it stops where the walk runs out.
func TestUnnamedStopAboveSharesOneBudgetPostgres(t *testing.T) {
	database, w, states := earlyFixture(t, []earlyBlock{
		{h: 950_000, unnamed: 50, named: 30, clean: true},
		{h: 800_000, unnamed: 40, named: 20, clean: true},
		{h: 500_000, unnamed: 10, clean: true},
	})
	s := &Server{db: database, native: DefaultNativeAsset}
	for limit := 0; limit <= 7; limit++ {
		var wr, er []int64
		want := walkStop(t, s, w, 1, budgeted(states, limit, &wr))
		got, err := s.unnamedStopAbove([]int64{1}, w, "", 1, earlyTip, budgeted(states, limit, &er))
		require.NoError(t, err)
		require.Equal(t, want, got, "limit %d", limit)
		require.Equal(t, wr, er, "limit %d", limit)
	}
	var read []int64
	got, err := s.unnamedStopAbove([]int64{1}, w, "", 1, earlyTip, budgeted(states, 4, &read))
	require.NoError(t, err)
	require.Equal(t, int64(500_000), got, "the third block, in the third window, finds the budget spent")
}

// The fixtures' attributed and twice-dated records are what they claim to
// be, so the cases above exercise them.
func TestEarlyFixtureShapesPostgres(t *testing.T) {
	database, w, _ := earlyFixture(t, []earlyBlock{
		{h: 900_000, unnamed: 30, matched: 25, named: 5, clean: true},
		{h: 10, unnamed: 20, clean: true, placed: []int64{940_000, 600_000}},
	})
	s := &Server{db: database, native: DefaultNativeAsset}
	events, unmatched, err := s.ledgerStakeEvents([]int64{1}, w, "", 1, earlyTip)
	require.NoError(t, err)
	var heights []int64
	for _, ev := range unmatched {
		heights = append(heights, ev.Height)
	}
	require.ElementsMatch(t, []int64{900_000, 940_000, 600_000}, heights)
	matched := 0
	for _, ev := range events {
		if ev.Height == 900_000 && ev.Type == "delegate" {
			for _, r := range ev.records() {
				if r["amount"] == "25uatom" && r["delegator"] == w {
					matched++
				}
			}
		}
	}
	require.Equal(t, 1, matched, "the matched delegation is the wallet's event")

	database, w, states := earlyFixture(t, []earlyBlock{
		{h: 900_001, unnamed: 40, matched: 20, clean: true},
		{h: 900_000, unnamed: 30, matched: 25, named: 5, clean: true},
		{h: 700_001, unnamed: 15, matched: 25, clean: true},
		{h: 650_000, unnamed: 10, clean: false},
	})
	var read []int64
	got, err := (&Server{db: database, native: DefaultNativeAsset}).unnamedStopAbove([]int64{1}, w, "", 1, earlyTip, delegationsBy(states, &read))
	require.NoError(t, err)
	require.Equal(t, int64(650_000), got, "the edge blocks settle with their attributed delegations")
}
