package taxapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/taxapi/internal/servicehttp"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// inclusionTestNode answers account state from a list of balance changes:
// every wallet starts with 1000uatom and holds no stake.
type inclusionTestNode struct {
	deltas map[string]map[int64]int64
}

func (f inclusionTestNode) bank(addr string, h int64) int64 {
	v := int64(1000)
	for at, d := range f.deltas[addr] {
		if at <= h {
			v += d
		}
	}
	return v
}

func (f inclusionTestNode) serve(t *testing.T, cache string) *ledgerNode {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, err := strconv.ParseInt(r.Header.Get(heightHeader), 10, 64)
		require.NoError(t, err)
		parts := strings.Split(r.URL.Path, "/")
		page := map[string]any{}
		var body any
		switch {
		case strings.HasPrefix(r.URL.Path, "/cosmos/bank/v1beta1/balances/"):
			body = map[string]any{"balances": []any{map[string]any{"denom": apiUatom, apiAmount: strconv.FormatInt(f.bank(parts[5], h), 10)}}, incPageKey: page}
		case strings.HasPrefix(r.URL.Path, "/cosmos/staking/v1beta1/delegations/"):
			body = map[string]any{"delegation_responses": []any{}, incPageKey: page}
		case strings.HasSuffix(r.URL.Path, "/unbonding_delegations"):
			body = map[string]any{"unbonding_responses": []any{}, incPageKey: page}
		default:
			http.NotFound(w, r)
			return
		}
		require.NoError(t, json.NewEncoder(w).Encode(body))
	}))
	t.Cleanup(srv.Close)
	rest, err := servicehttp.New(http.DefaultClient, srv.URL)
	require.NoError(t, err)
	return &ledgerNode{rest: rest, cache: cache}
}

// walletTx is transaction bytes whose body names the wallets and whose auth
// info declares the fee.
func walletTx(t *testing.T, memo, fee string, wallets ...string) []byte {
	t.Helper()
	body := txtypes.TxBody{Memo: memo + " " + strings.Join(wallets, " ")}
	bodyBytes, err := body.Marshal()
	require.NoError(t, err)
	auth := txtypes.AuthInfo{Fee: &txtypes.Fee{Amount: coins(t, fee), GasLimit: 200000}}
	authBytes, err := auth.Marshal()
	require.NoError(t, err)
	raw := txtypes.TxRaw{BodyBytes: bodyBytes, AuthInfoBytes: authBytes, Signatures: [][]byte{[]byte("sig")}}
	out, err := raw.Marshal()
	require.NoError(t, err)
	return out
}

type inclusionFixture struct {
	db   *gorm.DB
	node *ledgerNode
	w    map[string]string
	tx   map[string][]byte
}

// Heights of the fixture. From Gaia v7 (10085397) failed transactions carry
// events; before it their fees are settled from the next block's payout.
const (
	incHeldA   = 10100005 // A turned away (wrong sequence); the index row sits here
	incHoleA   = 10100010 // A ran
	incHoleB   = 10100020 // B ran
	incHeldB   = 10100030 // B turned away later; the index row sits here
	incHeldC   = 10100035 // C turned away
	incHoleC   = 10100040 // C failed after paying its fee
	incHoleX   = 10100050 // X ran and is not in the index at all
	incHeldP   = 9100005  // before v7: P turned away
	incHoleP   = 9100010  // P failed after paying its fee
	incHoleU   = 9100020  // two equal failed fees, one paid: not decidable
	incBetween = 10100025 // a date between B's run and the copy the index dated it at
	incFeeX    = "50uatom"
	incPageKey = "pagination"
)

func newInclusionFixture(t *testing.T) inclusionFixture {
	t.Helper()
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres repeated inclusion verification")
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

	w := map[string]string{}
	for i, name := range []string{"A", "RA", "B", "RB", "C", "X", "P", "U"} {
		w[name] = testAddress(t, byte(100+i))
	}
	tx := map[string][]byte{
		"A":  walletTx(t, "a", "10uatom", w["A"], w["RA"]),
		"B":  walletTx(t, "b", "10uatom", w["B"], w["RB"]),
		"C":  walletTx(t, "c", "10uatom", w["C"]),
		"X":  walletTx(t, "x", incFeeX, w["X"]),
		"P":  walletTx(t, "p", "40uatom", w["P"]),
		"U1": walletTx(t, "u1", "40uatom", w["U"]),
		"U2": walletTx(t, "u2", "40uatom", w["U"]),
	}
	dir := t.TempDir()
	fc := feeCollectorAddress()
	cache := func(height int64, txs [][]byte, results []pgTxResult, begin []event) {
		h := strconv.FormatInt(height, 10)
		raw := make([]string, len(txs))
		for i, x := range txs {
			raw[i] = base64.StdEncoding.EncodeToString(x)
		}
		var blk cachedBlockBody
		blk.Block.Header.Height, blk.Block.Data.Txs = h, raw
		writeCached(t, dir, height, cachedBlock, blk)
		if results == nil {
			results = []pgTxResult{}
		}
		writeCached(t, dir, height, "block_results", map[string]any{pgHeightKey: h, pgResultsKey: results, "begin_block_events": begin})
	}
	collected := func(fees int64) []event {
		return []event{
			{Type: eventCoinReceived, Attributes: []attr{{attrReceiver, fc}, {apiAmount, testMintAmount}}},
			{Type: eventCoinSpent, Attributes: []attr{{attrSpender, fc}, {apiAmount, fmt.Sprintf("%duatom", 1000+fees)}}},
		}
	}
	send := func(from, to string) pgTxResult {
		return pgTxResult{Events: []event{
			{Type: eventCoinSpent, Attributes: []attr{{attrSpender, from}, {apiAmount, "100uatom"}}},
			{Type: eventCoinReceived, Attributes: []attr{{attrReceiver, to}, {apiAmount, "100uatom"}}},
		}}
	}
	seq := pgTxResult{Code: 32, Codespace: codespaceSDK}
	cache(incHeldA, [][]byte{tx["A"]}, []pgTxResult{seq}, nil)
	cache(incHoleA, [][]byte{tx["A"]}, []pgTxResult{send(w["A"], w["RA"])}, nil)
	cache(incHoleB, [][]byte{tx["B"]}, []pgTxResult{send(w["B"], w["RB"])}, nil)
	cache(incHeldB, [][]byte{tx["B"]}, []pgTxResult{seq}, nil)
	cache(incHeldC, [][]byte{tx["C"]}, []pgTxResult{seq}, nil)
	cache(incHoleC, [][]byte{tx["C"]}, []pgTxResult{{Code: 5, Codespace: codespaceSDK, Events: []event{{Type: eventCoinSpent, Attributes: []attr{{attrSpender, w["C"]}, {apiAmount, "10uatom"}}}}}}, nil)
	cache(incHoleX, [][]byte{tx["X"]}, []pgTxResult{{Events: []event{{Type: eventCoinSpent, Attributes: []attr{{attrSpender, w["X"]}, {apiAmount, incFeeX}}}}}}, nil)
	cache(incHeldP, [][]byte{tx["P"]}, []pgTxResult{seq}, nil)
	cache(incHeldP+1, nil, nil, collected(0))
	cache(incHoleP, [][]byte{tx["P"]}, []pgTxResult{{Code: 5, Codespace: codespaceSDK}}, nil)
	cache(incHoleP+1, nil, nil, collected(40))
	cache(incHoleU, [][]byte{tx["U1"], tx["U2"]}, []pgTxResult{{Code: 5, Codespace: codespaceSDK}, {Code: 5, Codespace: codespaceSDK}}, nil)
	cache(incHoleU+1, nil, nil, collected(40))

	heights := []int64{incHeldA, incHoleA, incHoleB, incBetween, incHeldB, incHeldC, incHoleC, incHoleX, incHeldP, incHeldP + 1, incHoleP, incHoleP + 1, incHoleU, incHoleU + 1}
	var blocks []string
	for _, h := range heights {
		blocks = append(blocks, fmt.Sprintf("(%d,1,%d,timestamptz '2022-03-01' + %d * interval '6 seconds',true,true)", h, h, h-9000000))
	}
	hash := func(k string) string { return txHash(string(tx[k])) }
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
		tempAddressesTable,
		tempDenomsTable,
		"CREATE TEMP TABLE fees (tx_id bigint, denomination_id bigint, amount numeric, payer_address_id bigint)",
		"CREATE TEMP TABLE tax_fee_grants (tx_id bigint PRIMARY KEY, granter text)",
		"CREATE TEMP TABLE tax_failed_tx_fees (tx_id bigint PRIMARY KEY, charged text)",
		tempHolesKeyed,
		tempHoleAddrsKeyed,
		"CREATE TEMP TABLE tax_validator_rates (chain text, operator text, height bigint, tokens numeric, shares numeric)",
		tempHubChainRow,
		"INSERT INTO blocks VALUES " + strings.Join(blocks, ","),
		"INSERT INTO denoms VALUES (1,'uatom')",
		"INSERT INTO message_event_types VALUES (1,'coin_spent'),(2,'coin_received')",
		// A begin-block payout to B's receiver between B's run and the copy
		// the index dated B at.
		"INSERT INTO block_event_types VALUES (1,'coin_received')",
		"INSERT INTO block_event_attribute_keys VALUES (1,'receiver'),(2,'amount')",
		fmt.Sprintf("INSERT INTO block_events VALUES (1,%d,1,0)", incBetween),
		"INSERT INTO message_event_attribute_keys VALUES (1,'spender'),(2,'receiver'),(3,'amount')",
		// Each index row is dated at a copy the chain turned away, with the
		// code that copy failed with, as the upsert left it. A and B keep the
		// messages and events of the copy that ran.
		fmt.Sprintf("INSERT INTO txes VALUES (1,%d,'%s',32),(2,%d,'%s',32),(3,%d,'%s',32),(4,%d,'%s',32),(5,%d,'%s',5),(6,%d,'%s',5)",
			incHeldA, hash("A"), incHeldB, hash("B"), incHeldC, hash("C"), incHeldP, hash("P"), incHoleU, hash("U1"), incHoleU, hash("U2")),
		"INSERT INTO messages VALUES (1,1),(2,2)",
		"INSERT INTO message_events VALUES (1,1,1,0),(2,1,2,1),(3,2,1,0),(4,2,2,1)",
		fmt.Sprintf("INSERT INTO message_event_attributes VALUES (1,1,'%s',0),(1,3,'100uatom',1),(2,2,'%s',0),(2,3,'100uatom',1),(3,1,'%s',0),(3,3,'100uatom',1),(4,2,'%s',0),(4,3,'100uatom',1)",
			w["A"], w["RA"], w["B"], w["RB"]),
		fmt.Sprintf("INSERT INTO block_event_attributes VALUES (1,1,'%s',0),(1,2,'5uatom',1)", w["RB"]),
		fmt.Sprintf("INSERT INTO addresses VALUES (1,'%s'),(2,'%s'),(3,'%s'),(4,'%s'),(5,'%s')", w["A"], w["B"], w["C"], w["P"], w["U"]),
		"INSERT INTO fees VALUES (1,1,10,1),(2,1,10,2),(3,1,10,3),(4,1,40,4),(5,1,40,5),(6,1,40,5)",
		// The turned-away copies paid nothing.
		"INSERT INTO tax_failed_tx_fees VALUES (1,''),(2,''),(3,''),(4,'')",
		fmt.Sprintf("INSERT INTO tax_index_holes VALUES ('mainnet',%d,1,true),('mainnet',%d,1,true),('mainnet',%d,1,true),('mainnet',%d,1,true),('mainnet',%d,1,true),('mainnet',%d,2,true)",
			incHoleA, incHoleB, incHoleC, incHoleX, incHoleP, incHoleU),
		fmt.Sprintf("INSERT INTO tax_index_hole_addresses VALUES ('mainnet',%d,'%s'),('mainnet',%d,'%s'),('mainnet',%d,'%s'),('mainnet',%d,'%s'),('mainnet',%d,'%s'),('mainnet',%d,'%s'),('mainnet',%d,'%s'),('mainnet',%d,'%s')",
			incHoleA, w["A"], incHoleA, w["RA"], incHoleB, w["B"], incHoleB, w["RB"], incHoleC, w["C"], incHoleX, w["X"], incHoleP, w["P"], incHoleU, w["U"]),
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	require.NoError(t, inclusionsMigrate(database))
	fake := inclusionTestNode{deltas: map[string]map[int64]int64{
		w["A"]: {incHoleA: -110}, w["RA"]: {incHoleA: 100},
		w["B"]: {incHoleB: -110}, w["RB"]: {incHoleB: 100, incBetween: 5},
		w["C"]: {incHoleC: -10},
		w["X"]: {incHoleX: -100},
		w["P"]: {incHoleP: -40},
		w["U"]: {incHoleU: -40},
	}}
	return inclusionFixture{db: database, node: fake.serve(t, dir), w: w, tx: tx}
}

func (f inclusionFixture) plans(t *testing.T, sum RepeatedInclusionsSummary, backup string) map[int64]inclusionHole {
	t.Helper()
	raw, err := os.ReadFile(backup)
	require.NoError(t, err)
	out := map[int64]inclusionHole{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var p inclusionHole
		require.NoError(t, json.Unmarshal([]byte(line), &p))
		out[p.Height] = p
	}
	require.Len(t, out, sum.Holes)
	return out
}

func liquid(t *testing.T, h ledgerHoldings) int64 {
	t.Helper()
	v, ok := h.Liquid[apiUatom]
	require.True(t, ok, "no uatom balance")
	return v.Int64()
}

// The chain included transactions twice: A ran after an earlier copy was
// turned away, B ran before a later copy was, C failed after paying its fee
// with an earlier copy turned away, and P did the same before Gaia v7. The
// index dates each at the copy turned away. The dry run plans each at the
// copy that took effect, proves its block and the block the row sat at
// against chain state, and writes nothing; a transaction missing from the
// index and an undecidable pre-v7 fee keep their holes. The apply records the
// plan and clears only the proven holes.
func TestRepairRepeatedInclusionsPostgres(t *testing.T) {
	f := newInclusionFixture(t)
	s := &Server{db: f.db, native: DefaultNativeAsset}
	ids := []int64{1}

	// Before the repair, A's transfer is dated at the copy turned away, so
	// walking back across the block it ran misses it, and walking back across
	// the turned-away copy's block applies it there.
	var rates rateCache
	reason, err := s.checkAccountAcross("mainnet", ids, f.node, f.w["A"], incHoleA, &rates)
	require.NoError(t, err)
	require.Equal(t, "a liquid balance differs", reason)
	reason, err = s.checkAccountAcross("mainnet", ids, f.node, f.w["A"], incHeldA, &rates)
	require.NoError(t, err)
	require.Equal(t, "a liquid balance differs", reason)
	// B's balance between its run and the later copy is answered wrongly and
	// without any refusal: the later copy undoes a transfer that already ran.
	at, err := f.node.anchor(f.w["B"], incHeldB)
	require.NoError(t, err)
	at.Time = time.Now()
	held, why, err := s.walkAcross("mainnet", ids, f.node, f.w["B"], at, []int64{incBetween}, &rates)
	require.NoError(t, err)
	require.Empty(t, why)
	require.Equal(t, int64(990), liquid(t, held[incBetween]), "the misdated row undoes B's transfer before it is reached, and its fee is lost")

	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	dry := filepath.Join(t.TempDir(), "dry.jsonl")
	sum, err := s.RepairRepeatedInclusions("mainnet", f.node, RepeatedInclusionsOptions{BackupPath: dry, Workers: 3}, logf)
	require.NoError(t, err, strings.Join(logs, "\n"))
	require.Equal(t, 6, sum.Holes)
	require.Equal(t, 4, sum.Proven, strings.Join(logs, "\n"))
	require.Equal(t, 4, sum.Placements)
	require.Equal(t, map[string]int{inclusionRanBefore: 1, inclusionRanAfter: 1, inclusionPaidFee: 1, inclusionPrePaid: 1}, sum.Kinds)
	require.Equal(t, 2, sum.AccountFailures, "only X's and U's accounts fail their checks: %v", sum.Blockers)
	require.Equal(t, 1, sum.Blockers["account check: a liquid balance differs"])
	require.Equal(t, 1, sum.Blockers["account check: a failed transaction has no recorded fee outcome"])
	require.Equal(t, 8, sum.NamedAccounts)
	require.Equal(t, 6, sum.Unrefused, "X and U stay refused")
	plans := f.plans(t, sum, dry)

	for _, h := range []int64{incHoleA, incHoleB, incHoleC, incHoleP} {
		p := plans[h]
		require.True(t, p.Proven, "%d: %v %v %v", h, p.PlaceBlockers, p.Blockers, p.Checks)
		require.Len(t, p.Effective, 1)
		require.Len(t, p.Held, 1)
		require.True(t, p.Effective[0].Effective)
		require.False(t, p.Held[0].Effective)
		require.Equal(t, p.Effective[0].TxID, p.Held[0].TxID)
	}
	require.Equal(t, int64(incHeldA), plans[incHoleA].Held[0].Height)
	require.Equal(t, int64(incHeldB), plans[incHoleB].Held[0].Height)
	require.Equal(t, int64(0), plans[incHoleA].Effective[0].Code)
	require.Equal(t, int64(5), plans[incHoleC].Effective[0].Code)
	require.Equal(t, "10uatom", plans[incHoleC].Effective[0].Charged)
	require.Equal(t, "40uatom", plans[incHoleP].Effective[0].Charged)
	require.Equal(t, int64(32), plans[incHoleP].Held[0].Code)
	// Each moved row is checked at the block it sat at too.
	checkedHeld := 0
	for _, c := range plans[incHoleA].Checks {
		if c.Height == incHeldA {
			checkedHeld++
			require.True(t, c.OK)
		}
	}
	require.Equal(t, 2, checkedHeld, "A's sender and receiver at the turned-away copy")

	x := plans[incHoleX]
	require.False(t, x.Proven)
	require.Empty(t, x.Effective)
	require.Contains(t, x.Blockers, "a transaction the chain ran is not in the index")
	u := plans[incHoleU]
	require.False(t, u.Proven)
	require.Contains(t, u.Blockers, "a failed transaction's fee outcome could not be settled")

	// The dry run wrote nothing.
	var n int64
	require.NoError(t, f.db.Model(&TxInclusion{}).Count(&n).Error)
	require.Zero(t, n)
	require.NoError(t, f.db.Model(&IndexHoleClearance{}).Count(&n).Error)
	require.Zero(t, n)
	require.NoError(t, f.db.Model(&FailedTxFee{}).Count(&n).Error)
	require.Equal(t, int64(4), n)
	// Logs name no wallet.
	for _, w := range f.w {
		require.NotContains(t, strings.Join(logs, "\n"), w)
	}

	// No double counting: with the plan in place each transfer and fee is
	// counted once, at the block it took effect, and nowhere else.
	preview := &inclusionPreview{cleared: map[int64]bool{}}
	for _, p := range plans {
		if p.Accepted {
			preview.effective = append(preview.effective, p.Effective...)
		}
	}
	pv := &Server{db: f.db, native: DefaultNativeAsset, preview: preview}
	for _, k := range []string{"A", "B"} {
		events, _, err := pv.ledgerEvents(ids, f.w[k], "", 10100000, 10100100)
		require.NoError(t, err)
		require.Len(t, events, 1, k)
		fees, unknown, err := pv.ledgerFees(ids, f.w[k], 10100000, 10100100, nil)
		require.NoError(t, err)
		require.Zero(t, unknown)
		require.Len(t, fees, 1, k)
		require.Equal(t, events[0].Height, fees[0].Height)
	}
	events, _, err := pv.ledgerEvents(ids, f.w["A"], "", 10100000, 10100100)
	require.NoError(t, err)
	require.Equal(t, int64(incHoleA), events[0].Height)
	fees, _, err := pv.ledgerFees(ids, f.w["P"], 9100000, 9100100, nil)
	require.NoError(t, err)
	require.Len(t, fees, 1)
	require.Equal(t, int64(incHoleP), fees[0].Height)
	require.True(t, fees[0].Coins.AmountOf(apiUatom).Equal(coins(t, "40uatom").AmountOf(apiUatom)))
	requireSameBlockReads(t, f, pv)
	// The silent window now answers B's real balance.
	held, why, err = pv.walkAcross("mainnet", ids, f.node, f.w["B"], at, []int64{incBetween}, &rates)
	require.NoError(t, err)
	require.Empty(t, why)
	require.Equal(t, int64(890), liquid(t, held[incBetween]))

	// The apply records the plan and the proven clearances only.
	_, err = s.RepairRepeatedInclusions("mainnet", f.node, RepeatedInclusionsOptions{Apply: true}, logf)
	require.ErrorContains(t, err, "requires --backup")
	_, err = s.RepairRepeatedInclusions("mainnet", f.node, RepeatedInclusionsOptions{Apply: true, BackupPath: dry}, logf)
	require.ErrorContains(t, err, "create repeated-inclusion backup")
	applied := filepath.Join(t.TempDir(), "apply.jsonl")
	sum, err = s.RepairRepeatedInclusions("mainnet", f.node, RepeatedInclusionsOptions{Apply: true, BackupPath: applied, Workers: 3}, logf)
	require.NoError(t, err, strings.Join(logs, "\n"))
	require.Equal(t, 4, sum.Proven)
	require.NoError(t, f.db.Model(&TxInclusion{}).Count(&n).Error)
	require.Equal(t, int64(8), n)
	require.NoError(t, f.db.Model(&TxInclusion{}).Where("effective").Count(&n).Error)
	require.Equal(t, int64(4), n)
	var cleared []int64
	require.NoError(t, f.db.Model(&IndexHoleClearance{}).Order("height").Pluck("height", &cleared).Error)
	require.Equal(t, []int64{incHoleP, incHoleA, incHoleB, incHoleC}, cleared)
	// The hole rows themselves stay.
	require.NoError(t, f.db.Model(&IndexHole{}).Count(&n).Error)
	require.Equal(t, int64(6), n)
	// The ledger no longer refuses A before its hole, and still refuses X.
	hs, err := s.holeHeights("mainnet", f.w["A"], 0, 20000000)
	require.NoError(t, err)
	require.Empty(t, hs)
	hs, err = s.holeHeights("mainnet", f.w["X"], 0, 20000000)
	require.NoError(t, err)
	require.Equal(t, []int64{incHoleX}, hs)
	requireSameBlockReads(t, f, s)
	// The ledger reads the written rows as the preview did.
	reason, err = s.checkAccountAcross("mainnet", ids, f.node, f.w["A"], incHoleA, &rates)
	require.NoError(t, err)
	require.Empty(t, reason)
	reason, err = s.checkAccountAcross("mainnet", ids, f.node, f.w["A"], incHeldA, &rates)
	require.NoError(t, err)
	require.Empty(t, reason)

	// A second run finds only the unproven holes and adds nothing.
	again := filepath.Join(t.TempDir(), "again.jsonl")
	sum, err = s.RepairRepeatedInclusions("mainnet", f.node, RepeatedInclusionsOptions{Apply: true, BackupPath: again, Workers: 3}, logf)
	require.NoError(t, err)
	require.Equal(t, 2, sum.Holes)
	require.Zero(t, sum.Proven)
	require.NoError(t, f.db.Model(&TxInclusion{}).Count(&n).Error)
	require.Equal(t, int64(8), n)
}

// A moved row that does not check out at the block it sat at keeps its
// placement out of the plan, so the block it would clear stays a hole.
func TestRepairRepeatedInclusionsRefusesUnprovenHeldBlockPostgres(t *testing.T) {
	f := newInclusionFixture(t)
	// Someone else's transfer to A's receiver at the turned-away block is
	// on chain but not in the index.
	f.node = inclusionTestNode{deltas: map[string]map[int64]int64{
		f.w["A"]: {incHoleA: -110}, f.w["RA"]: {incHoleA: 100, incHeldA: 7},
		f.w["B"]: {incHoleB: -110}, f.w["RB"]: {incHoleB: 100, incBetween: 5},
		f.w["C"]: {incHoleC: -10}, f.w["X"]: {incHoleX: -100},
		f.w["P"]: {incHoleP: -40}, f.w["U"]: {incHoleU: -40},
	}}.serve(t, f.node.cache)
	s := &Server{db: f.db, native: DefaultNativeAsset}
	dry := filepath.Join(t.TempDir(), "dry.jsonl")
	sum, err := s.RepairRepeatedInclusions("mainnet", f.node, RepeatedInclusionsOptions{BackupPath: dry, Workers: 2}, func(string, ...any) {})
	require.NoError(t, err)
	plans := f.plans(t, sum, dry)
	p := plans[incHoleA]
	require.False(t, p.Accepted)
	require.False(t, p.Proven)
	require.Contains(t, p.PlaceBlockers, "a moved transaction's accounts do not check out at the block the index dated it at")
	require.True(t, plans[incHoleB].Proven)
	require.Equal(t, 3, sum.Proven)
	require.Equal(t, 3, sum.Placements)
}

// A copy turned away while another copy of the same bytes got past the ante
// handler is charged nothing before Gaia v7, even when its code alone would
// let it have paid, so an equal fee in the same block is decided.
func TestSettlePreEventReincluded(t *testing.T) {
	fc := feeCollectorAddress()
	next := readFeeCollectorBegin([]rpcEvent{
		fcEvent(eventCoinReceived, fc, testMintAmount, "", false),
		fcEvent(eventCoinSpent, fc, "1040uatom", "", false),
	}, nil)
	fresh := func(reincluded bool) scannedBlock {
		return scannedBlock{
			txs: []nodeTx{{Hash: "RR", Code: 5, Rejected: true}, {Hash: "QQ", Code: 5, Rejected: true}},
			raw: []rawTxInfo{
				{Codespace: codespaceSDK, Fee: coins(t, "40uatom"), FeeDecoded: true, Reincluded: reincluded},
				{Codespace: codespaceSDK, Fee: coins(t, "40uatom"), FeeDecoded: true},
			},
		}
	}
	b := fresh(false)
	settlePreEventBlock(9000000, b, next)
	require.True(t, b.txs[0].ChargeUnknown, "two equal fees, one paid: undecidable")
	require.True(t, b.txs[1].ChargeUnknown)

	b = fresh(true)
	settlePreEventBlock(9000000, b, next)
	require.Equal(t, nodeTx{Hash: "RR", Code: 5, Rejected: true}, b.txs[0])
	require.Equal(t, nodeTx{Hash: "QQ", Code: 5, Charged: "40uatom"}, b.txs[1])
}

// requireSameBlockReads proves the repair's block-driven event reads return
// what the wallet-driven reads the API uses return, for every wallet over
// every one-block window and over the whole range.
func requireSameBlockReads(t *testing.T, f inclusionFixture, s *Server) {
	t.Helper()
	ids := []int64{1}
	blocked := &Server{db: s.db, native: s.native, preview: s.preview, blockReads: true}
	var heights []int64
	require.NoError(t, f.db.Raw("SELECT height FROM blocks ORDER BY height").Scan(&heights).Error)
	windows := [][2]int64{{0, 20000000}}
	for _, h := range heights {
		windows = append(windows, [2]int64{h - 1, h})
	}
	seen := 0
	for _, w := range f.w {
		operator, _ := AccountOperator(w)
		for _, win := range windows {
			want, wantU, err := s.ledgerEvents(ids, w, operator, win[0], win[1])
			require.NoError(t, err)
			got, gotU, err := blocked.ledgerEvents(ids, w, operator, win[0], win[1])
			require.NoError(t, err)
			require.Equal(t, wantU, gotU)
			require.Equal(t, want, got, "window %v", win)
			seen += len(got)
		}
	}
	require.Positive(t, seen)
}
