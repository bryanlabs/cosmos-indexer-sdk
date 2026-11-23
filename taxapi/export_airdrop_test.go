package taxapi

import (
	"strings"
	"testing"
)

// An airdrop the indexer books as income reads "Airdrop (income)" in the
// export's description, so a user can find it; other income keeps its
// category.
func TestAirdropDescription(t *testing.T) {
	r := Row{Category: TagIncome, RewardTrigger: TagAirdrop, Direction: "in"}
	if d := r.description(); !strings.HasPrefix(d, "Airdrop (income) | source=airdrop") {
		t.Fatalf("airdrop: %q", d)
	}
	r.RewardTrigger = "dao_payment"
	if d := r.description(); !strings.HasPrefix(d, "income | source=dao_payment") {
		t.Fatalf("payment: %q", d)
	}
}
