package taxapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/DefiantLabs/cosmos-indexer/taxapi/internal/servicehttp"
)

// delegationsBy answers delegationsAt from fixed states by height, and
// records the heights read.
func delegationsBy(states map[int64]map[string]int64, read *[]int64) func(int64) (map[string]*big.Int, error) {
	return func(h int64) (map[string]*big.Int, error) {
		*read = append(*read, h)
		st, ok := states[h]
		if !ok {
			return nil, fmt.Errorf("no state at %d", h)
		}
		out := map[string]*big.Int{}
		for v, n := range st {
			out[v] = bi(n)
		}
		return out, nil
	}
}

func unmatchedStake(id uint64, h int64, typ string, kv ...string) ledgerEvent {
	return ev(id, h, phaseTx, typ, append(kv, "delegator", ledgerTestAddr)...)
}

func TestSettleUnnamedStakes(t *testing.T) {
	const h = 150
	redel := unmatchedStake(9, h, "redelegate", "source_validator", ledgerTestVal, "destination_validator", ledgerTestVal2, "amount", "300uatom")
	other := unmatchedStake(10, h, "delegate", "validator", ledgerTestVal2, "amount", "40uatom")
	settle := func(states map[int64]map[string]int64, events, unmatched []ledgerEvent) ([]ledgerEvent, int64, []int64) {
		var read []int64
		owned, unattributed := settleUnnamedStakes(delegationsBy(states, &read), ledgerTestAddr, "", "uatom", events, unmatched)
		return owned, unattributed, read
	}

	// Clean: the delegations moved by one record's amount, on its source
	// and destination, rounded down on the destination.
	owned, unattributed, read := settle(map[int64]map[string]int64{
		h - 1: {ledgerTestVal: 1000},
		h:     {ledgerTestVal: 700, ledgerTestVal2: 299},
	}, nil, []ledgerEvent{redel, other})
	require.Zero(t, unattributed)
	require.Equal(t, []int64{h - 1, h}, read, "the wallet's delegations are read either side of the block")
	require.Len(t, owned, 1)
	require.Equal(t, redel, owned[0])

	// The wallet's own named delegation in the block is taken off first: the
	// unnamed delegation of the same amount to the same validator is not the
	// wallet's, and the walk goes on without it.
	named := []ledgerEvent{ev(1, h, phaseTx, "delegate", "delegator", ledgerTestAddr, "validator", ledgerTestVal2, "amount", "40uatom")}
	owned, unattributed, _ = settle(map[int64]map[string]int64{
		h - 1: {ledgerTestVal: 1000},
		h:     {ledgerTestVal: 1000, ledgerTestVal2: 40},
	}, named, []ledgerEvent{other})
	require.Zero(t, unattributed)
	require.Empty(t, owned)
	// The same change with no named event is the unnamed record's.
	owned, unattributed, _ = settle(map[int64]map[string]int64{
		h - 1: {ledgerTestVal: 1000},
		h:     {ledgerTestVal: 1000, ledgerTestVal2: 40},
	}, nil, []ledgerEvent{other})
	require.Zero(t, unattributed)
	require.Equal(t, []ledgerEvent{other}, owned)
	// Someone else's named delegation in the block explains nothing.
	foreign := []ledgerEvent{ev(1, h, phaseTx, "delegate", "delegator", "cosmos1other", "validator", ledgerTestVal2, "amount", "40uatom")}
	owned, unattributed, _ = settle(map[int64]map[string]int64{
		h - 1: {ledgerTestVal: 1000},
		h:     {ledgerTestVal: 1000, ledgerTestVal2: 40},
	}, foreign, []ledgerEvent{other})
	require.Zero(t, unattributed)
	require.Equal(t, []ledgerEvent{other}, owned)

	// Zero difference: the records are someone else's.
	owned, unattributed, _ = settle(map[int64]map[string]int64{
		h - 1: {ledgerTestVal: 1000},
		h:     {ledgerTestVal: 1000},
	}, nil, []ledgerEvent{redel, other})
	require.Zero(t, unattributed)
	require.Empty(t, owned)

	// Unclean: a change no single record explains stays refused.
	for name, after := range map[string]map[string]int64{
		"no record's amount":          {ledgerTestVal: 930},
		"two records":                 {ledgerTestVal: 700, ledgerTestVal2: 340},
		"a validator no record names": {ledgerTestVal: 1000, "cosmosvaloper1c": 5},
	} {
		owned, unattributed, _ = settle(map[int64]map[string]int64{h - 1: {ledgerTestVal: 1000}, h: after}, nil, []ledgerEvent{redel, other})
		require.Equal(t, int64(h), unattributed, name)
		require.Empty(t, owned, name)
	}
	// Two combinations that fit, one record or two others together.
	split := []ledgerEvent{
		unmatchedStake(11, h, "delegate", "validator", ledgerTestVal, "amount", "100uatom"),
		unmatchedStake(12, h, "delegate", "validator", ledgerTestVal, "amount", "60uatom"),
		unmatchedStake(13, h, "delegate", "validator", ledgerTestVal, "amount", "40uatom"),
	}
	_, unattributed, _ = settle(map[int64]map[string]int64{h - 1: {ledgerTestVal: 1000}, h: {ledgerTestVal: 1100}}, nil, split)
	require.Equal(t, int64(h), unattributed, "an amount two combinations explain is not clean")
	// A record within rounding of nothing cannot be told from nothing.
	tiny := []ledgerEvent{unmatchedStake(14, h, "delegate", "validator", ledgerTestVal, "amount", "1uatom")}
	_, unattributed, _ = settle(map[int64]map[string]int64{h - 1: {ledgerTestVal: 1000}, h: {ledgerTestVal: 1000}}, nil, tiny)
	require.Equal(t, int64(h), unattributed)
	// A named event whose effect on the delegations cannot be read.
	redeem := []ledgerEvent{ev(1, h, phaseTx, "redeem_shares", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "5"+ledgerTestVal+"/1")}
	_, unattributed, _ = settle(map[int64]map[string]int64{h - 1: {ledgerTestVal: 1000}, h: {ledgerTestVal: 1000}}, redeem, []ledgerEvent{other})
	require.Equal(t, int64(h), unattributed)

	// No archive, or a read that fails: refused as before.
	owned, unattributed = settleUnnamedStakes(nil, ledgerTestAddr, "", "uatom", nil, []ledgerEvent{redel})
	require.Equal(t, int64(h), unattributed)
	require.Empty(t, owned)
	owned, unattributed, _ = settle(map[int64]map[string]int64{h: {ledgerTestVal: 700, ledgerTestVal2: 300}}, nil, []ledgerEvent{redel})
	require.Equal(t, int64(h), unattributed)
	require.Empty(t, owned)

	// Blocks are settled newest first, and the first that cannot be settled
	// is where the refusal starts; nothing below it is read.
	low := unmatchedStake(20, 120, "unbond", "validator", ledgerTestVal, "amount", "5uatom", "requested", "6uatom")
	lower := unmatchedStake(21, 110, "unbond", "validator", ledgerTestVal, "amount", "5uatom")
	owned, unattributed, read = settle(map[int64]map[string]int64{
		h - 1: {ledgerTestVal: 1000}, h: {ledgerTestVal: 700, ledgerTestVal2: 299},
		119: {ledgerTestVal: 1000}, 120: {ledgerTestVal: 990},
	}, nil, []ledgerEvent{lower, redel, low})
	require.Equal(t, int64(120), unattributed)
	require.Equal(t, []ledgerEvent{redel}, owned)
	require.Equal(t, []int64{h - 1, h, 119, 120}, read)
	// An unbond is matched on the tokens it asked for.
	owned, unattributed, _ = settle(map[int64]map[string]int64{119: {ledgerTestVal: 1000}, 120: {ledgerTestVal: 994}}, nil, []ledgerEvent{low})
	require.Zero(t, unattributed)
	require.Equal(t, []ledgerEvent{low}, owned)
}

// attributeStakes returns each record it cannot match in the form it takes
// as the wallet's, and attributeRedelegations still refuses at the highest.
func TestAttributeStakesReturnsUnmatchedRecords(t *testing.T) {
	bonded, notBonded := moduleAccount("bonded_tokens_pool"), moduleAccount("not_bonded_tokens_pool")
	rows := []ledgerAttrRow{
		// Names no delegator at all.
		redelRow(8, 80, 170, "message", "action", "/cosmos.staking.v1beta1.MsgUndelegate"),
		redelRow(8, 81, 170, "transfer", "recipient", notBonded),
		redelRow(8, 81, 170, "transfer", "sender", bonded),
		redelRow(8, 81, 170, "transfer", "amount", "4uatom"),
		redelRow(8, 82, 170, "unbond", "validator", ledgerTestVal),
		redelRow(8, 82, 170, "unbond", "amount", "5uatom"),
		// Two granters withdrawing from the same source.
		redelRow(4, 40, 130, "withdraw_rewards", "validator", ledgerTestVal),
		redelRow(4, 40, 130, "withdraw_rewards", "delegator", "cosmos1other"),
		redelRow(4, 41, 130, "withdraw_rewards", "validator", ledgerTestVal),
		redelRow(4, 41, 130, "withdraw_rewards", "delegator", ledgerTestAddr),
		redelRow(4, 42, 130, "redelegate", "source_validator", ledgerTestVal),
		redelRow(4, 42, 130, "redelegate", "destination_validator", ledgerTestVal2),
		redelRow(4, 42, 130, "redelegate", "amount", "1uatom"),
	}
	events, unmatched := attributeStakes(rows, ledgerTestAddr)
	require.Empty(t, events)
	require.Len(t, unmatched, 2)
	got := map[int64]map[string]string{}
	for _, e := range unmatched {
		require.Len(t, e.records(), 1)
		got[e.Height] = e.records()[0]
	}
	require.Equal(t, map[string]string{"validator": ledgerTestVal, "amount": "4uatom", "requested": "5uatom", "delegator": ledgerTestAddr}, got[170])
	require.Equal(t, map[string]string{"source_validator": ledgerTestVal, "destination_validator": ledgerTestVal2, "amount": "1uatom", "delegator": ledgerTestAddr}, got[130])
	_, unattributed := attributeRedelegations(rows, ledgerTestAddr)
	require.Equal(t, int64(170), unattributed)
}

// A record settled as the wallet's is walked like any of its own events.
func TestWalkBackWithSettledStake(t *testing.T) {
	const h = 150
	unbond := unmatchedStake(9, h, "unbond", "validator", ledgerTestVal, "amount", "50uatom", "completion_time", "2023-01-01T00:00:00Z")
	named := ev(1, h, phaseTx, "delegate", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "30uatom", "new_shares", "30")
	var read []int64
	owned, unattributed := settleUnnamedStakes(delegationsBy(map[int64]map[string]int64{h - 1: {ledgerTestVal: 100}, h: {ledgerTestVal: 80}}, &read), ledgerTestAddr, "", "uatom", []ledgerEvent{named}, []ledgerEvent{unbond})
	require.Zero(t, unattributed)
	in := ledgerInput{
		Address:   ledgerTestAddr,
		BondDenom: "uatom",
		Anchor: ledgerAnchor{
			Height:      200,
			Bank:        map[string]*big.Int{},
			Delegations: map[string]*big.Int{ledgerTestVal: bi(80)},
			Shares:      map[string]decimal.Decimal{ledgerTestVal: decimal.NewFromInt(80)},
			Rates:       map[string]validatorRate{ledgerTestVal: unitRate(1000)},
		},
		Events: append([]ledgerEvent{named}, owned...),
	}
	got, err := walkBack(in, []int64{100})
	require.NoError(t, err)
	intsEqual(t, "h100", got[100].Delegations, map[string]int64{ledgerTestVal: 100})
}

// A rate no recorded sample bounds is read from chain state at the block
// that bounds it and walked as a recorded one; without a read, or when the
// read fails, the walk stops as before.
func TestWalkBackReadsUnrecordedRates(t *testing.T) {
	base := func() ledgerInput {
		return ledgerInput{
			Address:   ledgerTestAddr,
			BondDenom: "uatom",
			Anchor: ledgerAnchor{
				Height:      100,
				Bank:        map[string]*big.Int{},
				Delegations: map[string]*big.Int{ledgerTestVal: bi(150), ledgerTestVal2: bi(10)},
				Shares:      map[string]decimal.Decimal{ledgerTestVal: decimal.RequireFromString("150.505050505050505050"), ledgerTestVal2: decimal.NewFromInt(10)},
				Rates:       map[string]validatorRate{ledgerTestVal: rate(999, 1000), ledgerTestVal2: unitRate(500)},
			},
			// Nothing is recorded for ledgerTestVal before height 25.
			Samples: map[string][]rateSample{ledgerTestVal: {{25, rate(990, 1000)}, {50, rate(998, 1000)}}},
			Events: []ledgerEvent{
				ev(1, 30, phaseTx, "delegate", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "50uatom", "new_shares", "50.505050505050505050"),
				ev(2, 20, phaseTx, "unbond", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "1000uatom"),
			},
		}
	}
	targets := []int64{50, 25, 12}

	// The same walk with the rates recorded where they are read is the
	// reference.
	recorded := base()
	recorded.Samples[ledgerTestVal] = append([]rateSample{{12, rate(985, 1000)}, {19, rate(985, 1000)}}, recorded.Samples[ledgerTestVal]...)
	want, err := walkBack(recorded, targets)
	require.NoError(t, err)

	in := base()
	var reads []string
	in.RateAt = func(v string, h int64) (validatorRate, error) {
		reads = append(reads, fmt.Sprintf("%s@%d", v, h))
		return rate(985, 1000), nil
	}
	got, err := walkBack(in, targets)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprint(want), fmt.Sprint(got))
	require.Equal(t, []string{ledgerTestVal + "@19", ledgerTestVal + "@12"}, reads, "only the sides no sample bounds are read")

	// A validator gone by the anchor has no anchor rate either: both sides
	// are read.
	gone := base()
	gone.Anchor.Rates = map[string]validatorRate{ledgerTestVal2: unitRate(500)}
	gone.Samples = nil
	reads = nil
	gone.RateAt = in.RateAt
	_, err = walkBack(gone, []int64{50})
	require.NoError(t, err)
	require.Equal(t, []string{ledgerTestVal + "@50"}, reads, "one read at the target bounds both sides")

	// No read, a failed read, or a read past the anchor: unknown as before,
	// with the same reason.
	for name, read := range map[string]func(string, int64) (validatorRate, error){
		"no archive": nil,
		"failed read": func(string, int64) (validatorRate, error) {
			return validatorRate{}, errors.New("version does not exist")
		},
	} {
		in := base()
		in.RateAt = read
		got, err := walkBack(in, targets)
		var unk ledgerUnknown
		require.ErrorAs(t, err, &unk, name)
		require.Contains(t, unk.msg, "are not recorded", name)
		require.Len(t, got, 2, name)
	}
	calls := 0
	in = base()
	in.RateAt = func(string, int64) (validatorRate, error) { calls++; return validatorRate{}, errors.New("pruned") }
	book := newRateBook(in)
	for range 2 {
		_, _, err = book.at(ledgerTestVal, 19, 20, 20)
		require.Error(t, err)
	}
	_, _, err = book.at(ledgerTestVal, 200, 201, 201)
	require.Error(t, err)
	require.Equal(t, 1, calls, "a failed read is not retried and nothing past the anchor is read")

	// A rate read with no shares is refused, never taken as one.
	in = base()
	in.RateAt = func(string, int64) (validatorRate, error) {
		return validatorRate{Tokens: bi(0), Shares: decimal.Zero}, nil
	}
	_, err = walkBack(in, targets)
	var unk ledgerUnknown
	require.ErrorAs(t, err, &unk)
	require.Contains(t, unk.msg, "has no delegator shares")
}

// The node reads one validator's rate and the wallet's delegations at a
// height, and a height it no longer holds is an error, not the latest state.
func TestLedgerNodeRateAndDelegationsAtHeight(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get(heightHeader)
		if h == "5" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":2,"message":"failed to load state at height 5; version does not exist"}`))
			return
		}
		switch {
		case r.URL.Path == "/cosmos/staking/v1beta1/validators/"+ledgerTestVal:
			_ = json.NewEncoder(w).Encode(map[string]any{"validator": map[string]any{"operator_address": ledgerTestVal, "tokens": "985", "delegator_shares": "1000.000000000000000000"}})
		case strings.HasPrefix(r.URL.Path, "/cosmos/staking/v1beta1/delegations/"):
			amt := map[string]string{"149": "1000", "150": "700"}[h]
			_ = json.NewEncoder(w).Encode(map[string]any{"delegation_responses": []any{map[string]any{
				"delegation": map[string]any{"validator_address": ledgerTestVal, "shares": amt + ".000000000000000000"},
				"balance":    map[string]any{"denom": "uatom", "amount": amt},
			}}, incPageKey: map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	rest, err := servicehttp.New(http.DefaultClient, srv.URL)
	require.NoError(t, err)
	node := &ledgerNode{rest: rest}

	r, err := node.rateAt(ledgerTestVal, 19)
	require.NoError(t, err)
	require.Equal(t, rate(985, 1000).Tokens.String(), r.Tokens.String())
	require.True(t, r.Shares.Equal(decimal.NewFromInt(1000)))
	_, err = node.rateAt(ledgerTestVal, 5)
	require.Error(t, err)
	_, err = node.rateAt(ledgerTestVal2, 19)
	require.Error(t, err)

	d, _, err := node.delegations(ledgerTestAddr, 150)
	require.NoError(t, err)
	require.Equal(t, "700", d[ledgerTestVal].String())
	_, _, err = node.delegations(ledgerTestAddr, 5)
	require.Error(t, err)

	read := func(h int64) (map[string]*big.Int, error) {
		d, _, err := node.delegations(ledgerTestAddr, h)
		return d, err
	}
	unbond := unmatchedStake(9, 150, "unbond", "validator", ledgerTestVal, "amount", "300uatom")
	owned, unattributed := settleUnnamedStakes(read, ledgerTestAddr, "", "uatom", nil, []ledgerEvent{unbond})
	require.Zero(t, unattributed)
	require.Equal(t, []ledgerEvent{unbond}, owned)
	pruned := unmatchedStake(9, 6, "unbond", "validator", ledgerTestVal, "amount", "300uatom")
	_, unattributed = settleUnnamedStakes(read, ledgerTestAddr, "", "uatom", nil, []ledgerEvent{pruned})
	require.Equal(t, int64(6), unattributed, "a block the node no longer holds stays refused")
}
