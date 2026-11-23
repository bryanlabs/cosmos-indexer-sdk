package taxapi

import (
	base64enc "encoding/base64"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/stretchr/testify/require"
)

// Heights of the module account fixture.
const (
	modHoleOK      = 10100060 // every fee and movement indexed
	modHoleMissing = 10100070 // a fee-paying transaction is missing from the index
	modHoleUnknown = 10100080 // a failed transaction's charge is not recorded and it was not turned away
	modHolePre     = 9100060  // before v7: an unrecorded failed copy the ante handler turned away

	modBeginEventsKey = "begin_block_events"
)

func distributionAddress(t *testing.T) string {
	t.Helper()
	a, err := sdk.Bech32ifyAddressBytes(sdk.GetConfig().GetBech32AccountAddrPrefix(), authtypes.NewModuleAddress(moduleDistribution))
	require.NoError(t, err)
	return a
}

// The fee collector and the distribution module are balanced across each
// block: indexed fees and message movements plus the block's own events must
// equal the chain's change in balance. Walking them back through the ledger
// alone misses the mint, the payout and the allocation.
func TestModuleAccountCheckPostgres(t *testing.T) {
	f := newInclusionFixture(t)
	fc, dist := feeCollectorAddress(), distributionAddress(t)
	require.Equal(t, moduleFeeCollector, checkedModule(fc))
	require.Equal(t, moduleDistribution, checkedModule(dist))
	require.Empty(t, checkedModule(f.w["A"]))

	tx := map[string][]byte{
		"T1": walletTx(t, "t1", "7uatom", f.w["A"]),
		"T2": walletTx(t, "t2", "3uatom", f.w["B"]),
		"T3": walletTx(t, "t3", "5uatom", f.w["C"]),
		"T4": walletTx(t, "t4", "9uatom", f.w["X"]),
		"T5": walletTx(t, "t5", "4uatom", f.w["P"]),
		"T6": walletTx(t, "t6", "6uatom", f.w["U"]),
	}
	hash := func(k string) string { return txHash(string(tx[k])) }
	dir := f.node.cache
	block := func(height int64, keys []string, results []pgTxResult, begin []event) {
		raw := make([]string, len(keys))
		for i, k := range keys {
			raw[i] = base64enc.StdEncoding.EncodeToString(tx[k])
		}
		var blk cachedBlockBody
		blk.Block.Header.Height, blk.Block.Data.Txs = strconv.FormatInt(height, 10), raw
		writeCached(t, dir, height, cachedBlock, blk)
		if results == nil {
			results = []pgTxResult{}
		}
		writeCached(t, dir, height, "block_results", map[string]any{pgHeightKey: strconv.FormatInt(height, 10), pgResultsKey: results, modBeginEventsKey: begin})
	}
	coinEv := func(typ, key, who, amount string) event {
		return event{Type: typ, Attributes: []attr{{key, who}, {apiAmount, amount}}}
	}
	// The mint pays the fee collector, which pays its whole balance (the
	// previous block's fees plus the new coins) to distribution.
	payout := func(prev int64) []event {
		total := fmt.Sprintf("%duatom", 1000+prev)
		return []event{
			coinEv(eventCoinReceived, attrReceiver, fc, testMintAmount),
			coinEv(eventCoinSpent, attrSpender, fc, total),
			coinEv(eventCoinReceived, attrReceiver, dist, total),
		}
	}
	withdraw := pgTxResult{Events: []event{
		coinEv(eventCoinSpent, attrSpender, f.w["A"], "7uatom"),
		coinEv(eventCoinReceived, attrReceiver, fc, "7uatom"),
		coinEv(eventCoinSpent, attrSpender, dist, "20uatom"),
		coinEv(eventCoinReceived, attrReceiver, f.w["A"], "20uatom"),
	}}
	failedPaid := func(who, amount string) pgTxResult {
		return pgTxResult{Code: 5, Codespace: codespaceSDK, Events: []event{
			coinEv(eventCoinSpent, attrSpender, who, amount),
			coinEv(eventCoinReceived, attrReceiver, fc, amount),
		}}
	}
	seq := pgTxResult{Code: 32, Codespace: codespaceSDK}
	block(modHoleOK, []string{"T1", "T2", "T3"}, []pgTxResult{withdraw, failedPaid(f.w["B"], "3uatom"), seq}, payout(25))
	block(modHoleMissing, []string{"T4"}, []pgTxResult{{Events: []event{coinEv(eventCoinSpent, attrSpender, f.w["X"], "9uatom"), coinEv(eventCoinReceived, attrReceiver, fc, "9uatom")}}}, payout(10))
	block(modHoleUnknown, []string{"T5"}, []pgTxResult{failedPaid(f.w["P"], "4uatom")}, payout(0))
	block(modHolePre, []string{"T6"}, []pgTxResult{seq}, payout(0))

	for _, sql := range []string{
		fmt.Sprintf("INSERT INTO blocks VALUES (%[1]d,1,%[1]d,timestamptz '2022-03-01',true,true),(%[2]d,1,%[2]d,timestamptz '2022-03-01',true,true),(%[3]d,1,%[3]d,timestamptz '2022-03-01',true,true),(%[4]d,1,%[4]d,timestamptz '2021-12-01',true,true)",
			modHoleOK, modHoleMissing, modHoleUnknown, modHolePre),
		// T4 is not indexed.
		fmt.Sprintf("INSERT INTO txes VALUES (11,%d,'%s',0),(12,%d,'%s',5),(13,%d,'%s',32),(15,%d,'%s',5),(16,%d,'%s',32)",
			modHoleOK, hash("T1"), modHoleOK, hash("T2"), modHoleOK, hash("T3"), modHoleUnknown, hash("T5"), modHolePre, hash("T6")),
		"INSERT INTO messages VALUES (11,11)",
		"INSERT INTO message_events VALUES (11,11,1,0),(12,11,2,1)",
		fmt.Sprintf("INSERT INTO message_event_attributes VALUES (11,1,'%s',0),(11,3,'20uatom',1),(12,2,'%s',0),(12,3,'20uatom',1)", dist, f.w["A"]),
		"INSERT INTO fees VALUES (11,1,7,1),(12,1,3,2),(13,1,5,3),(15,1,4,4),(16,1,6,5)",
		// T2's charge is recorded; T3, T5 and T6 have none.
		"INSERT INTO tax_failed_tx_fees VALUES (12,'3uatom')",
		fmt.Sprintf("INSERT INTO tax_index_holes VALUES ('mainnet',%d,3,true),('mainnet',%d,1,true)", modHoleOK, modHoleMissing),
		fmt.Sprintf("INSERT INTO tax_index_hole_addresses VALUES ('mainnet',%[1]d,'%[3]s'),('mainnet',%[1]d,'%[4]s'),('mainnet',%[2]d,'%[3]s'),('mainnet',%[2]d,'%[4]s')",
			modHoleOK, modHoleMissing, fc, dist),
	} {
		require.NoError(t, f.db.Exec(sql).Error, sql)
	}
	f.node = inclusionTestNode{deltas: map[string]map[int64]int64{
		// 7 + 3 in fees, 1000 minted, 1025 paid out.
		fc: {modHoleOK: -15, modHoleMissing: -1, modHoleUnknown: 4},
		// 1025 allocated, 20 withdrawn.
		dist: {modHoleOK: 1005, modHoleMissing: 1010, modHoleUnknown: 1000, modHolePre: 1000},
	}}.serve(t, dir)

	s := &Server{db: f.db, native: DefaultNativeAsset}
	ids := []int64{1}
	check := func(addr string, h int64) string {
		var rates rateCache
		reason, err := s.checkAccountAcross("mainnet", ids, f.node, addr, h, &rates)
		require.NoError(t, err)
		return reason
	}
	require.Empty(t, check(fc, modHoleOK))
	require.Empty(t, check(dist, modHoleOK))
	// The missing transaction's fee shows as a shortfall.
	require.Equal(t, "the module account's balance change differs from the block's fees, messages and block events", check(fc, modHoleMissing))
	require.Empty(t, check(dist, modHoleMissing))
	require.Equal(t, "a failed transaction has no recorded fee outcome", check(fc, modHoleUnknown))
	require.Empty(t, check(fc, modHolePre))
	require.Empty(t, check(dist, modHolePre))

	// A wrong allocation is caught.
	f.node = inclusionTestNode{deltas: map[string]map[int64]int64{fc: {modHoleOK: -15}, dist: {modHoleOK: 1004}}}.serve(t, dir)
	require.NotEmpty(t, check(dist, modHoleOK))
	require.Empty(t, check(fc, modHoleOK))

	// Through the repair: the complete block is proven, the one missing a
	// transaction keeps its hole.
	f.node = inclusionTestNode{deltas: map[string]map[int64]int64{
		fc:   {modHoleOK: -15, modHoleMissing: -1},
		dist: {modHoleOK: 1005, modHoleMissing: 1010},
	}}.serve(t, dir)
	dry := filepath.Join(t.TempDir(), "dry.jsonl")
	var logs []string
	sum, err := s.RepairRepeatedInclusions("mainnet", f.node, RepeatedInclusionsOptions{BackupPath: dry, Heights: []int64{modHoleOK, modHoleMissing}, Workers: 2},
		func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
	require.NoError(t, err, strings.Join(logs, "\n"))
	require.Equal(t, 2, sum.Holes)
	plans := f.plans(t, sum, dry)
	require.True(t, plans[modHoleOK].Proven, "%v %v %v", plans[modHoleOK].PlaceBlockers, plans[modHoleOK].Blockers, plans[modHoleOK].Checks)
	require.False(t, plans[modHoleMissing].Proven)
	require.Contains(t, plans[modHoleMissing].Blockers, "a transaction the chain ran is not in the index")
}
