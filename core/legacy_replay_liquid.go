package core

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// ReplayNet is the net change of spendable balances at one height, keyed by
// address, a NUL byte and the denomination. A zero net is absent.
type ReplayNet map[string]string

var replayFeeCoin = regexp.MustCompile(`^([0-9]{1,78})([a-z][a-z0-9/]{0,127})?$`)

func replayNet(d replayDeltas) ReplayNet {
	out := ReplayNet{}
	for k, v := range d {
		if !v.IsZero() {
			out[k.address+"\x00"+k.denom] = v.String()
		}
	}
	return out
}

// LegacyReplayRecorded sums what the recorder saw the bank keeper commit at
// one height, including an actual zero-height export.
func LegacyReplayRecorded(heights ...replayrecords.Height) (ReplayNet, error) {
	d := replayDeltas{}
	for _, h := range heights {
		for _, r := range h.Records {
			if r.Kind != "movement" || (r.Status != "committed" && r.Status != "exported") {
				continue
			}
			n, ok := sdk.NewIntFromString(r.Amount)
			if !ok || r.Address == "" || (r.Denom == "" && !n.IsZero()) {
				return nil, heldRecord(r, "movement is not a signed whole amount of one address")
			}
			d.add(r.Address, r.Denom, n)
		}
	}
	return replayNet(d), nil
}

// LegacyReplayLiquid applies the rows and charged fees of one stored height
// to spendable balances. A category with no rule is refused by name, so a
// kind of movement the ledger cannot label can never pass as zero.
func LegacyReplayLiquid(b ledger.Block) (ReplayNet, error) {
	if !replayChain(b.Chain) {
		return nil, held()
	}
	d := replayDeltas{}
	for _, row := range b.Rows {
		n, ok := sdk.NewIntFromString(row.Amount)
		if !ok || n.IsNegative() {
			return nil, fmt.Errorf("%w: height %d row amount is not a whole number", replayHeld, b.Height)
		}
		paid := row.To
		if (row.Category == "reward" || row.Category == "commission") && row.Detail != "" {
			var detail struct {
				To string `json:"paid_to"`
			}
			if json.Unmarshal([]byte(row.Detail), &detail) != nil || detail.To == "" {
				return nil, fmt.Errorf("%w: height %d %s row names no paid address", replayHeld, b.Height, row.Category)
			}
			paid = detail.To
		}
		switch row.Category {
		case "transfer":
			if row.From != "" {
				d.add(row.From, row.Denom, n.Neg())
			}
			if row.To != "" {
				d.add(row.To, row.Denom, n)
			}
		case "reward", "commission", "unbonding_complete":
			d.add(paid, row.Denom, n)
		case "delegate", "expense":
			d.add(row.From, row.Denom, n.Neg())
		case "gov_deposit":
			d.add(row.From, row.Denom, n.Neg())
			d.add(replayGovEscrow(), row.Denom, n)
		case "gov_deposit_refund":
			d.add(row.To, row.Denom, n)
			d.add(replayGovEscrow(), row.Denom, n.Neg())
		case "gov_deposit_burn":
			d.add(replayGovEscrow(), row.Denom, n.Neg())
			d.add(replayGovBurn(), row.Denom, n)
		case "undelegate", "redelegate":
		default:
			return nil, fmt.Errorf("%w: height %d row category %q has no spendable balance rule", replayHeld, b.Height, row.Category)
		}
	}
	for _, inc := range b.Inclusions {
		if !inc.FeeCharged || inc.Fee == "" {
			continue
		}
		for _, part := range strings.Split(inc.Fee, ",") {
			found := replayFeeCoin.FindStringSubmatch(part)
			if found == nil {
				return nil, fmt.Errorf("%w: height %d stored fee is not readable", replayHeld, b.Height)
			}
			n, _ := sdk.NewIntFromString(found[1])
			if n.IsZero() {
				continue
			}
			if found[2] == "" || inc.FeePayer == "" {
				return nil, fmt.Errorf("%w: height %d charged fee without a denomination or payer", replayHeld, b.Height)
			}
			d.add(inc.FeePayer, found[2], n.Neg())
		}
	}
	return replayNet(d), nil
}

// Equal reports whether two heights moved the same balances.
func (n ReplayNet) Equal(other ReplayNet) bool {
	if len(n) != len(other) {
		return false
	}
	for k, v := range n {
		if other[k] != v {
			return false
		}
	}
	return true
}
