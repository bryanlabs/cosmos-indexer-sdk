package tax

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	sdkmath "cosmossdk.io/math"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/cosmos/events"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	cometAbciTypes "github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	disttypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
)

// The claim-multicoin fixture's delegator and validator.
const (
	multicoinWallet    = "cosmos1ts4vwmfccwjv0vehlzd4x5rw5xzljjnhgm7gnz"
	multicoinValidator = "cosmosvaloper1n229vhepft6wnkt5tjpwmxdmcnfz55jv3vp77d"
)

const sdk50IBCDenom = "ibc/27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2"

func abciEvent(typ string, msgIndex string, kv ...string) cometAbciTypes.Event {
	e := cometAbciTypes.Event{Type: typ}
	for i := 0; i+1 < len(kv); i += 2 {
		e.Attributes = append(e.Attributes, cometAbciTypes.EventAttribute{Key: kv[i], Value: kv[i+1]})
	}
	if msgIndex != "" {
		e.Attributes = append(e.Attributes, cometAbciTypes.EventAttribute{Key: "msg_index", Value: msgIndex})
	}
	return e
}

// An SDK 0.50 transaction with three messages, split into per-message logs
// the way the indexer splits them (by msg_index). Two claims from different
// validators are both paid to the withdraw address, the first in two denoms;
// a bank send to the same address and the untagged fee transfer are not
// distribution payouts. Each claim moves its own reward, per denom, in the
// order of its reward rows (sorted coins put the ibc denom first).
func TestWithdrawAddressSDK50MultiMessageTx(t *testing.T) {
	two := "300uatom,40" + sdk50IBCDenom
	txEvents := []cometAbciTypes.Event{
		abciEvent("transfer", "", "recipient", "cosmos1feecollectorxxxxxxxxxxxxxxxxxxxxxxx", "sender", del, "amount", "5000uatom"),
		abciEvent("message", "0", "action", "/cosmos.distribution.v1beta1.MsgWithdrawDelegatorReward", "sender", del, "module", "distribution"),
		abciEvent("transfer", "0", "recipient", withdrawTo, "sender", distributionModuleAddress, "amount", two),
		abciEvent("withdraw_rewards", "0", "amount", two, "validator", val, "delegator", del),
		abciEvent("message", "1", "action", "/cosmos.distribution.v1beta1.MsgWithdrawDelegatorReward", "sender", del, "module", "distribution"),
		abciEvent("transfer", "1", "recipient", withdrawTo, "sender", distributionModuleAddress, "amount", "200uatom"),
		abciEvent("withdraw_rewards", "1", "amount", "200uatom", "validator", otherValidator, "delegator", del),
		abciEvent("message", "2", "action", "/cosmos.bank.v1beta1.MsgSend", "sender", del, "module", "bank"),
		abciEvent("transfer", "2", "recipient", withdrawTo, "sender", del, "amount", "50uatom"),
	}
	msgs := []sdk.Msg{
		&disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val},
		&disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: otherValidator},
		&banktypes.MsgSend{FromAddress: del, ToAddress: withdrawTo, Amount: sdk.NewCoins(sdk.NewInt64Coin(fixtureNativeDenom, 50))},
	}
	logs, err := events.ParseTxEventsToMessageIndexEvents(len(msgs), txEvents)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]TaxableEvent{
		{moved(del, withdrawTo, "40", sdk50IBCDenom, actionClaim), moved(del, withdrawTo, "300", fixtureNativeDenom, actionClaim)},
		{moved(del, withdrawTo, "200", fixtureNativeDenom, actionClaim)},
		{},
	}
	for i, msg := range msgs {
		log := &txlog.LogMessage{MessageIndex: i, Events: events.StringEventstoNormalizedEvents(logs[i].Events)}
		data, err := (&Parser{ID: "sdk50"}).ParseMessage(msg, log, config.IndexConfig{})
		if err != nil {
			t.Fatal(err)
		}
		out := (*data).([]TaxableEvent)
		assertMovedLast(t, out, want[i])
		if i < 2 {
			rewards := withoutWithdrawAddress(out)
			if len(rewards) != len(want[i]) {
				t.Fatalf("msg %d rewards %+v", i, rewards)
			}
			for j, r := range rewards {
				if r.Category != string(CategoryReward) || r.ToAddr != del || r.Amount != want[i][j].Amount || r.Denom != want[i][j].Denom {
					t.Fatalf("msg %d reward %d %+v", i, j, r)
				}
			}
		}
	}
}

// redirectDistribution rewrites, in a copy of log, every distribution payout
// to delegator so it goes to to instead, as when a withdraw address is set.
// Attributes are read as recipient, sender, amount runs like the parser
// reads them. It reports the coins it redirected.
func redirectDistribution(log *txlog.LogMessage, delegator, to string) (*txlog.LogMessage, sdk.Coins) {
	out := &txlog.LogMessage{MessageIndex: log.MessageIndex}
	var moved sdk.Coins
	for _, e := range log.Events {
		c := txlog.LogMessageEvent{Type: e.Type, Attributes: append([]txlog.Attribute(nil), e.Attributes...)}
		if e.Type == string(CategoryTransfer) {
			for i := 0; i+2 < len(c.Attributes); i++ {
				r, s, a := c.Attributes[i], c.Attributes[i+1], c.Attributes[i+2]
				if r.Key == attributeRecipient && r.Value == delegator && s.Key == attributeSender && s.Value == distributionModuleAddress && a.Key == attributeAmount {
					coins, err := sdk.ParseCoinsNormalized(a.Value)
					if err == nil {
						moved = moved.Add(coins...)
						c.Attributes[i].Value = to
					}
				}
			}
		}
		out.Events = append(out.Events, c)
	}
	return out, moved
}

type archiveCase struct {
	name string
	msg  sdk.Msg
	log  *txlog.LogMessage
}

func archiveCases(t *testing.T) []archiveCase {
	t.Helper()
	entries, err := os.ReadDir("testdata/hub-legacy")
	if err != nil {
		t.Fatal(err)
	}
	var out []archiveCase
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".json")
		msgs, logs := hubLegacyFixture(t, name)
		for i := range msgs {
			out = append(out, archiveCase{fmt.Sprintf("%s/%d", name, i), msgs[i], logs[i]})
		}
	}
	// The SDK 0.50 mainnet claim paid in ATOM and USDC.
	out = append(out, archiveCase{"claim-multicoin", &disttypes.MsgWithdrawDelegatorReward{
		DelegatorAddress: multicoinWallet, ValidatorAddress: multicoinValidator,
	}, rewardFixture(t, "claim-multicoin")})
	return out
}

// On the real archive fixtures (legacy merged logs, authz, SDK 0.50), with
// every distribution payout to the one rewarded delegator redirected to a
// withdraw address: the existing rows are exactly the unredirected ones, and
// the moves, after them, carry per denom exactly the booked reward the
// redirected payouts cover, in reward-row order. Unredirected, no fixture
// moves anything.
func TestWithdrawAddressArchiveFixturesRedirected(t *testing.T) {
	redirected, multiDenom := 0, 0
	for _, c := range archiveCases(t) {
		base := parseLegacy(t, c.msg, c.log)
		if len(withoutWithdrawAddress(base)) != len(base) {
			t.Fatalf("%s: moves with no withdraw address", c.name)
		}
		delegators := map[string]bool{}
		for _, r := range base {
			if r.Category == string(CategoryReward) {
				delegators[r.ToAddr] = true
			}
		}
		if len(delegators) != 1 {
			continue
		}
		var delegator string
		for d := range delegators {
			delegator = d
		}
		log, paid := redirectDistribution(c.log, delegator, withdrawTo)
		if paid.IsZero() {
			continue
		}
		out := parseLegacy(t, c.msg, log)
		// A commission swept in the same message is booked to whoever
		// received it, so it follows the payout; every other row stays.
		expect := append([]TaxableEvent(nil), base...)
		for i := range expect {
			if expect[i].Category == string(CategoryCommission) && expect[i].ToAddr == delegator {
				expect[i].ToAddr = withdrawTo
			}
		}
		if got := withoutWithdrawAddress(out); !reflect.DeepEqual(got, expect) {
			t.Fatalf("%s: redirecting the payout changed the existing rows\n%s\n%s", c.name, hashedRows(got), hashedRows(expect))
		}
		var want []TaxableEvent
		left := paid
		for _, r := range base {
			if r.Category != string(CategoryReward) || r.ToAddr != delegator {
				continue
			}
			booked, ok := sdkmath.NewIntFromString(r.Amount)
			if !ok {
				t.Fatalf("%s: reward amount %q", c.name, r.Amount)
			}
			n := sdkmath.MinInt(booked, left.AmountOf(r.Denom))
			if !n.IsPositive() {
				continue
			}
			left = left.Sub(sdk.NewCoin(r.Denom, n))
			want = append(want, moved(delegator, withdrawTo, n.String(), r.Denom, r.RewardTrigger))
		}
		want = mergeMoves(want)
		assertMovedLast(t, out, want)
		redirected++
		if len(want) > 1 {
			multiDenom++
		}
	}
	t.Logf("%d fixture messages redirected, %d with moves in more than one denom", redirected, multiDenom)
	if redirected < 5 || multiDenom < 1 {
		t.Fatalf("only %d redirected, %d multi-denom", redirected, multiDenom)
	}
}

// mergeMoves sums moves of the same denom, keeping the first one's place.
func mergeMoves(in []TaxableEvent) []TaxableEvent {
	var out []TaxableEvent
	at := map[string]int{}
	for _, e := range in {
		if i, ok := at[e.Denom]; ok {
			a, _ := sdkmath.NewIntFromString(out[i].Amount)
			b, _ := sdkmath.NewIntFromString(e.Amount)
			out[i].Amount = a.Add(b).String()
			continue
		}
		at[e.Denom] = len(out)
		out = append(out, e)
	}
	return out
}

// hashedRows prints rows with every address as its hash12.
func hashedRows(rows []TaxableEvent) string {
	h := func(s string) string {
		if s == "" {
			return ""
		}
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])[:12]
	}
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "[%s %s %s %s from=%s to=%s val=%s] ", r.Category, r.Amount, r.Denom, r.RewardTrigger, h(r.FromAddr), h(r.ToAddr), h(r.ValidatorAddress))
	}
	return b.String()
}
