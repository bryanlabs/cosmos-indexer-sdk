package taxapi

import (
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// absentWallet delegated 50 tokens on ledgerTestVal at 30 and took them all
// back at 60. The undelegation names the tokens paid out, so the shares put
// back carry a token of rounding, and the walk below 30 holds between zero
// and about one share. ledgerTestVal2 is held throughout.
func absentWallet(samples []rateSample, absent map[AbsentAt]bool) ledgerInput {
	return ledgerInput{
		Address:   ledgerTestAddr,
		BondDenom: "uatom",
		Anchor: ledgerAnchor{
			Height:      100,
			Bank:        map[string]*big.Int{},
			Delegations: map[string]*big.Int{ledgerTestVal2: bi(10)},
			Shares:      map[string]decimal.Decimal{ledgerTestVal2: decimal.NewFromInt(10)},
			Rates:       map[string]validatorRate{ledgerTestVal: rate(999, 1000), ledgerTestVal2: unitRate(500)},
		},
		Samples: map[string][]rateSample{ledgerTestVal: samples},
		Events: []ledgerEvent{
			ev(1, 30, phaseTx, "delegate", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "50uatom", "new_shares", "50.050050050050050050"),
			ev(2, 60, phaseTx, "unbond", "delegator", ledgerTestAddr, "validator", ledgerTestVal, "amount", "50uatom"),
		},
		Absent: absent,
	}
}

// checkZeroOnAbsent walks to above and then absentAt, where ledgerTestVal is
// proved absent, and checks the zero there and the refusal without the proof.
func checkZeroOnAbsent(t *testing.T, in ledgerInput, above, absentAt int64) map[int64]ledgerHoldings {
	t.Helper()
	got, err := walkBack(in, []int64{above, absentAt})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[absentAt].Delegations[ledgerTestVal]; ok {
		t.Fatalf("a validator that did not exist shows a delegation: %v", got[absentAt].Delegations)
	}
	if _, ok := got[absentAt].DelegationError[ledgerTestVal]; ok {
		t.Fatalf("a validator that did not exist shows an error bound: %v", got[absentAt].DelegationError)
	}
	intsEqual(t, "other validator", got[absentAt].Delegations, map[string]int64{ledgerTestVal2: 10})

	// Without the proof the same date is refused, as before.
	in.Absent = nil
	refused, err := walkBack(in, []int64{above, absentAt})
	var unk ledgerUnknown
	if !errors.As(err, &unk) {
		t.Fatalf("want the rate refusal without a proof, got %v", err)
	}
	if _, ok := refused[absentAt]; ok {
		t.Fatal("the date was answered without a proof")
	}
	return got
}

// Before its creation block a validator holds no delegations, so a rebuilt
// span that allows zero is shown as zero there. The dates above are shown
// from the recorded rates as before.
func TestWalkBackZeroBeforeValidatorCreated(t *testing.T) {
	in := absentWallet([]rateSample{{40, rate(999, 1000)}}, map[AbsentAt]bool{{ledgerTestVal, 20}: true})
	got := checkZeroOnAbsent(t, in, 50, 20)
	if d := got[50].Delegations[ledgerTestVal]; d == nil || d.Cmp(bi(49)) < 0 {
		t.Fatalf("the delegation at 50 is shown from the recorded rates, got %v", got[50].Delegations)
	}
}

// After its removal a validator holds no delegations either. Here it was
// emptied by 5 and removed, and its operator address was created again
// before 30. The rate recorded at 5 has no delegator shares, so it cannot
// bound the date at 20.
func TestWalkBackZeroAfterValidatorRemoved(t *testing.T) {
	in := absentWallet([]rateSample{{5, rate(0, 0)}, {40, rate(999, 1000)}}, map[AbsentAt]bool{{ledgerTestVal, 20}: true})
	got := checkZeroOnAbsent(t, in, 50, 20)
	if d := got[50].Delegations[ledgerTestVal]; d == nil || d.Cmp(bi(49)) < 0 {
		t.Fatalf("the delegation at 50 is shown from the recorded rates, got %v", got[50].Delegations)
	}
}

// A proof is for one height. A neighbouring height is refused.
func TestWalkBackAbsentProofIsExactHeight(t *testing.T) {
	in := absentWallet([]rateSample{{40, rate(999, 1000)}}, map[AbsentAt]bool{{ledgerTestVal, 21}: true})
	got, err := walkBack(in, []int64{50, 20})
	var unk ledgerUnknown
	if !errors.As(err, &unk) {
		t.Fatalf("want the rate refusal, got %v", err)
	}
	if _, ok := got[20]; ok {
		t.Fatal("a height without its own proof was answered")
	}
}

// A rebuilt span that cannot be zero (the index lacks the delegation the
// undelegation took back) means the index and the chain disagree.
func TestWalkBackAbsentWithSharesRefusesTheWallet(t *testing.T) {
	in := absentWallet([]rateSample{{40, rate(999, 1000)}}, map[AbsentAt]bool{{ledgerTestVal, 20}: true})
	in.Events = in.Events[1:]
	got, err := walkBack(in, []int64{50, 20})
	var inc ledgerInconsistent
	if !errors.As(err, &inc) || got != nil {
		t.Fatalf("want inconsistent and nothing answered, got %v %v", got, err)
	}
}

// A staking record on the validator that names no delegator may be the
// wallet's, so the zero is refused.
func TestWalkBackAbsentRefusesUnnamedRecords(t *testing.T) {
	in := absentWallet([]rateSample{{40, rate(999, 1000)}}, map[AbsentAt]bool{{ledgerTestVal, 20}: true})
	in.Events = append(in.Events, ev(3, 45, phaseTx, "delegate", "validator", ledgerTestVal, "amount", "5uatom"))
	got, err := walkBack(in, []int64{50, 20})
	var unk ledgerUnknown
	if !errors.As(err, &unk) || !strings.Contains(unk.msg, "tokens per share") {
		t.Fatalf("want the rate refusal, got %v", err)
	}
	if _, ok := got[20]; ok {
		t.Fatal("the date below the unnamed record was answered")
	}
	if stakeRecordsNamed([]ledgerEvent{ev(3, 45, phaseTx, "unbond", "delegator", ledgerTestAddr, "amount", "5uatom")}, ledgerTestVal) {
		t.Fatal("a record naming no validator passed")
	}
	if !stakeRecordsNamed([]ledgerEvent{ev(3, 45, phaseTx, "delegate", "validator", ledgerTestVal2, "amount", "5uatom")}, ledgerTestVal) {
		t.Fatal("an unnamed record on another validator failed the check for this one")
	}
}

// Unbonding entries are kept as rebuilt on a proved height.
func TestWalkBackAbsentKeepsUnbonding(t *testing.T) {
	in := absentWallet([]rateSample{{40, rate(999, 1000)}}, map[AbsentAt]bool{{ledgerTestVal, 20}: true})
	in.Anchor.Unbonding = []unbondingEntry{{Validator: ledgerTestVal2, CreationHeight: 10, Balance: bi(7)}}
	got, err := walkBack(in, []int64{50, 20})
	if err != nil {
		t.Fatal(err)
	}
	intsEqual(t, "unbonding", got[20].Unbonding, map[string]int64{ledgerTestVal2: 7})
}

func TestLoadAbsentProofs(t *testing.T) {
	dir := t.TempDir()
	body := []byte(ledgerTestVal + "|20\n" + ledgerTestVal2 + "|31\n")
	p := filepath.Join(dir, "absent.txt")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAbsentProofs(p, privateSHA(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[AbsentAt{ledgerTestVal, 20}] || !got[AbsentAt{ledgerTestVal2, 31}] {
		t.Fatalf("got %v", got)
	}
	if _, err := LoadAbsentProofs(p, privateSHA([]byte("other"))); err == nil {
		t.Fatal("a wrong digest was accepted")
	}
	bad := []byte(ledgerTestVal + "|x\n")
	if err := os.WriteFile(p, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAbsentProofs(p, privateSHA(bad)); err == nil {
		t.Fatal("a bad line was accepted")
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAbsentProofs(link, privateSHA(bad)); err == nil {
		t.Fatal("a symlink was accepted")
	}
}

func TestBalanceReaderKeepsAbsentProofs(t *testing.T) {
	s := NewServer(nil, nil, DefaultNativeAsset)
	s.UseAbsentProofs(map[AbsentAt]bool{{"cosmosvaloper1a", 10}: true})
	r := s.balanceReader()
	if !r.readOnlyLedger || !r.ledgerAbsent[AbsentAt{"cosmosvaloper1a", 10}] {
		t.Fatalf("reader readOnly %v absent %v", r.readOnlyLedger, r.ledgerAbsent)
	}
}
