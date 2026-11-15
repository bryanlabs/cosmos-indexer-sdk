//go:build ops818harness

package taxapi

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

func TestOPS818GATASemanticDependency(t *testing.T) {
	s := connect(t, "jit=off", "statement_timeout=20s", "lock_timeout=2s")
	s.readOnlyLedger = true
	var scope struct{ Address, Since string }
	b, err := os.ReadFile(os.Getenv("H_SEMANTIC_INPUT"))
	if err != nil { t.Fatal(err) }
	if err = json.Unmarshal(b, &scope); err != nil { t.Fatal(err) }
	var events []tax.TaxableEvent
	if err = s.scopedTaxEvents(apiMainnet).Select("taxable_events.*").Where("(from_addr = ? OR to_addr = ?)", scope.Address, scope.Address).Where("category = ? OR reward_trigger IN ?", string(tax.CategoryIBCOut), []string{ibcTimeoutRefund, ibcErrorAckRefund}).Find(&events).Error; err != nil { t.Fatal(err) }
	if err = s.retainLegacyOutboundIdentities(events); err != nil { t.Fatal(err) }
	incomplete, err := s.incompleteIBCRefunds(apiMainnet, []string{scope.Address})
	if err != nil { t.Fatal(err) }
	events = append(events, incomplete...)
	var native []tax.TaxableEvent
	for _, e := range events { if e.Denom == DefaultNativeAsset.Denom || flowSingleHopNative(e, DefaultNativeAsset.Denom) { native = append(native, e) } }
	end := time.Date(2026,11,13,0,0,0,0,time.UTC)
	allReady, nativeReady := ibcBasisReadiness(events,end), ibcBasisReadiness(native,end)
	pairs := matchIBCRefunds(events)
	var unmatched []tax.TaxableEvent
	for _, e := range events { if isIBCRefund(e.RewardTrigger) && pairs[e.ID] == "" { unmatched = append(unmatched,e) } }
	result := struct{ Full, Native ReportReadiness; Events, UnmatchedRefunds []tax.TaxableEvent }{allReady,nativeReady,events,unmatched}
	b, err = json.MarshalIndent(result,"","  ")
	if err != nil { t.Fatal(err) }
	if err = os.WriteFile(os.Getenv("H_SEMANTIC_OUTPUT"),b,0600); err != nil { t.Fatal(err) }
	t.Log("full semantic pending", allReady.PendingSemanticReviews, "native-denom subset pending", nativeReady.PendingSemanticReviews, "unmatched refunds",len(unmatched))
}
