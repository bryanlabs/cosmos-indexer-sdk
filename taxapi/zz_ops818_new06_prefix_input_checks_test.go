//go:build ops818new06input && ops818opening06storedv3 && ops818harness && ops818cohorts0607

package taxapi

import (
	"github.com/DefiantLabs/cosmos-indexer/tax"
	"testing"
	"time"
)

func TestOPS818New06CompositionChecks(t *testing.T) {
	s := n6Scope{Address: "synthetic", Since: flowTestStart, PrefixEnd: flowTestStart.AddDate(0, 0, 1), End: flowTestStart.AddDate(0, 0, 2)}
	row := tax.TaxableEvent{Timestamp: s.Since, Amount: "999", Category: "nft_sale"}
	prefix := reportInput{Wallets: []walletRows{{Events: []tax.TaxableEvent{row, row}, Memos: map[string]string{"prefix": "exact"}}}}
	tail := reportInput{Wallets: []walletRows{{Events: []tax.TaxableEvent{{Timestamp: s.PrefixEnd}}, Memos: map[string]string{"tail": "retained"}}}}
	joined, e := n6Join(prefix, tail, s)
	if e != nil || len(joined.Wallets[0].Events) != 3 || len(joined.Wallets[0].Memos) != 2 {
		t.Fatal("lossless disjoint composition failed")
	}
	raw := []n6CreditRow{{MessageID: 1, Timestamp: s.Since, Receiver: s.Address, Amount: "2uatom,3uother"}}
	credits, e := n6Credits(raw, s)
	if e != nil || !credits[flowCredit{1, "2uatom"}] || credits[flowCredit{1, "999uatom"}] {
		t.Fatal("actual literal credit map replaced by tax gross")
	}
	raw[0].Receiver = "other"
	if _, e = n6Credits(raw, s); e == nil {
		t.Fatal("wrong raw native actor accepted")
	}
	raw[0].Receiver = s.Address
	raw[0].Timestamp = s.PrefixEnd
	if _, e = n6Credits(raw, s); e == nil {
		t.Fatal("captured overlap accepted")
	}
	tail.Wallets[0].Memos["prefix"] = "conflict"
	if _, e = n6Join(prefix, tail, s); e == nil {
		t.Fatal("conflicting retained memo accepted")
	}
	_ = time.Second
}
