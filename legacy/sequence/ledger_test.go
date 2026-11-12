package sequence

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// signedAt stands in for a signature made with one account number and sequence.
func signedAt(signer string, number, seq uint64) Signed {
	return Signed{Signer: signer, Kind: "test", Try: func(n, s uint64) bool { return n == number && s == seq }}
}

func files(closing map[string]uint64) (*Genesis, *Genesis) {
	opening := &Genesis{Chain: "cosmoshub-1", Sequence: map[string]uint64{}, GenesisSigned: map[string]uint64{}, Number: map[string]uint64{}, Position: map[string]uint64{}}
	close := &Genesis{Chain: "cosmoshub-2", Sequence: closing, GenesisSigned: map[string]uint64{}, Number: map[string]uint64{}, Position: map[string]uint64{}}
	number := uint64(1)
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		if _, ok := closing[name]; ok {
			opening.Sequence[name], opening.Number[name], close.Number[name] = 0, number, number
			number++
		}
	}
	return opening, close
}

func TestLedgerSettlesFailuresBetweenTwoProvenSequences(t *testing.T) {
	opening, closing := files(map[string]uint64{"a": 2, "b": 1, "c": 2, "d": 2, "e": 1, "f": 3})
	l := NewLedger(opening, closing)
	// a: the failure signed 0 and the next fee paying transaction signed 1, so it paid.
	l.Add("k/1/0", "unknown", []Signed{signedAt("a", 1, 0)})
	l.Add("k/1/1", "charged", []Signed{signedAt("a", 1, 1)})
	// b: the next fee paying transaction signed 0 again, so the failure did not pay.
	l.Add("k/2/0", "unknown", []Signed{signedAt("b", 2, 0)})
	l.Add("k/2/1", "charged", []Signed{signedAt("b", 2, 0)})
	// c: two failures both signed 0 and one of them paid. Which one is not provable.
	l.Add("k/3/0", "unknown", []Signed{signedAt("c", 3, 0)})
	l.Add("k/3/1", "unknown", []Signed{signedAt("c", 3, 0)})
	l.Add("k/3/2", "charged", []Signed{signedAt("c", 3, 1)})
	// d: the first signed 0, the second signed 1 and the account then stood at 1:
	// the first paid and the second, though validly signed, did not.
	l.Add("k/4/0", "unknown", []Signed{signedAt("d", 4, 0)})
	l.Add("k/4/1", "unknown", []Signed{signedAt("d", 4, 1)})
	l.Add("k/4/2", "charged", []Signed{signedAt("d", 4, 1)})
	// e: nothing follows the failure, so the closing file settles it.
	l.Add("k/5/0", "unknown", []Signed{signedAt("e", 5, 0)})
	// f: a failure whose fee was proven not charged moves nothing.
	l.Add("k/6/0", "charged", []Signed{signedAt("f", 6, 0)})
	l.Add("k/6/1", "not charged", []Signed{signedAt("f", 6, 9)})
	l.Add("k/6/2", "charged", []Signed{signedAt("f", 6, 1)})
	l.Add("k/6/3", "charged", []Signed{signedAt("f", 6, 2)})

	report := l.Close(true)
	require.Equal(t, map[string]string{"k/1/0": "charged", "k/2/0": "not charged", "k/4/0": "charged", "k/4/1": "not charged", "k/5/0": "charged"}, report.Resolutions)
	require.Equal(t, 3, report.SettledCharged)
	require.Equal(t, 2, report.SettledNotCharged)
	require.Equal(t, 2, report.Unsettled, "the two failures of c")
	require.Equal(t, 6, report.Accounts)
	require.Equal(t, 6, report.Exact)
	require.Zero(t, report.Mismatched+report.Impossible+report.Loose+report.Conflicts)
	require.Equal(t, int64(7), l.Signatures["fee charged, signature verified"])
	require.Equal(t, int64(1), l.Signatures["fee not charged, signature not verified"])
	require.Equal(t, []string{"k/1/0", "k/2/0", "k/4/0", "k/4/1", "k/5/0"}, report.SortedResolutions())
}

func TestLedgerReportsWhatDoesNotAddUp(t *testing.T) {
	opening, closing := files(map[string]uint64{"a": 3, "b": 5, "c": 1, "d": 2, "e": 0, "f": 2})
	l := NewLedger(opening, closing)
	// a: two fee paying transactions, the closing file says three.
	l.Add("k/1/0", "charged", []Signed{signedAt("a", 1, 0)})
	l.Add("k/1/1", "charged", []Signed{signedAt("a", 1, 1)})
	// b: one failure cannot account for a move of five.
	l.Add("k/2/0", "unknown", []Signed{signedAt("b", 2, 0)})
	// c: a fee paying transaction whose signature does not verify.
	l.Add("k/3/0", "charged", []Signed{signedAt("c", 3, 7)})
	// d: a failure before an unverified fee paying transaction stays unsettled,
	// and the next verified one makes the account exact again.
	l.Add("k/4/0", "unknown", []Signed{signedAt("d", 4, 0)})
	l.Add("k/4/1", "charged", []Signed{{Signer: "d"}})
	l.Add("k/4/2", "charged", []Signed{signedAt("d", 4, 1)})
	// f: nothing verified follows its unverified fee paying transaction.
	l.Add("k/6/0", "unknown", []Signed{signedAt("f", 6, 0)})
	l.Add("k/6/1", "charged", []Signed{{Signer: "f"}})
	// A failure two accounts signed is settled by the one that can prove it.
	l.Add("k/5/0", "unknown", []Signed{signedAt("e", 5, 0), signedAt("c", 3, 1)})

	report := l.Close(true)
	require.Equal(t, 1, report.Mismatched, "a")
	require.Equal(t, 1, report.Impossible, "b")
	require.Equal(t, 3, report.Exact, "c, d and e: e did not move, so the shared failure did not pay")
	require.Equal(t, 1, report.Loose, "f")
	require.Equal(t, map[string]string{"k/5/0": "not charged"}, report.Resolutions)
	require.Equal(t, 3, report.Unsettled, "the failures of b, d and f")
	require.Equal(t, int64(1), l.Unverified["fee charged, genesis account, test"])
	require.Equal(t, int64(2), l.Signatures["fee charged, signature not checkable"])
}

func TestLedgerSettlesNothingAfterAGap(t *testing.T) {
	opening, closing := files(map[string]uint64{"a": 40})
	l := NewLedger(opening, closing)
	l.Add("k/1/0", "unknown", []Signed{signedAt("a", 1, 0)})
	l.Gap()
	// The account moved by an unknown amount in the missing heights.
	l.Add("k/9/0", "charged", []Signed{signedAt("a", 1, 30)})
	l.Add("k/9/1", "unknown", []Signed{signedAt("a", 1, 31)})
	l.Add("k/9/2", "charged", []Signed{signedAt("a", 1, 32)})
	report := l.Close(true)
	require.Equal(t, map[string]string{"k/9/1": "charged"}, report.Resolutions, "between two verified sequences a failure is still settled")
	require.Equal(t, 1, report.Unsettled)
	require.Zero(t, report.Exact+report.Mismatched, "no end of hub comparison after a gap")
	require.Equal(t, int64(2), l.Signatures["fee charged, signature verified"])
}

func TestLedgerFindsUnrecordedAccountNumbersFromCreationOrder(t *testing.T) {
	opening, closing := files(map[string]uint64{"a": 1})
	closing.Number = map[string]uint64{"a": 0}
	closing.Sequence["new-1"], closing.Sequence["new-2"] = 1, 1
	l := NewLedger(opening, closing)
	l.Add("k/1/0", "charged", []Signed{signedAt("a", 1, 0)})
	l.Received("a")
	l.Received("new-1")
	l.Received("new-2")
	l.Received("new-1")
	// One genesis account, so the first new account has number 1 by creation
	// order. The chain created two others first, which a signature shows.
	l.Add("k/2/0", "charged", []Signed{signedAt("new-1", 3, 0)})
	require.Equal(t, int64(2), l.offset)
	l.Add("k/2/1", "charged", []Signed{signedAt("new-2", 4, 0)})
	report := l.Close(true)
	require.Equal(t, 3, report.Exact)
	require.Equal(t, int64(3), l.Signatures["fee charged, signature verified"])
}
