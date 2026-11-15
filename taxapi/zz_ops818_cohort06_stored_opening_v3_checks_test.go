//go:build ops818opening06storedv3

package taxapi

import (
	"testing"
	"time"
)

func TestOPS818StoredOpening06V3ProofChecks(t *testing.T) {
	r := v6Request{Source: v6Source}
	r.Request.Address = "synthetic"
	r.Request.Day = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	day := r.Request.Day
	base := v6Proof{StoredBuild: &BalanceBuild{FromDay: &day, ToDay: &day}, StoredRow: BalanceDay{Chain: apiMainnet, Address: r.Request.Address, Day: day, Holdings: `{"liquid":{"uatom":"1"}}`}, Boundary: heightTime{Height: 1}, Source: v6Source, Controls: v6Controls{Readonly: "on", Snapshot: "snapshot", Database: "cosmostax"}, Address: r.Request.Address, Day: r.Request.Day, Holdings: flowHoldings{Liquid: amt(1), Source: CommissionFlowHoldingSource{Kind: "ledger_exact", Height: 1}}, Known: true, Error: "<nil>"}
	if e := v6Validate(base, r); e != nil {
		t.Fatal("positive exact proof rejected")
	}
	for _, name := range []string{"wrong_address", "wrong_day", "wrong_source", "readonly_off", "assigned_xid", "unknown", "wrong_kind", "missing_height", "negative"} {
		t.Run(name, func(t *testing.T) {
			p := base
			switch name {
			case "wrong_address":
				p.Address = "other"
			case "wrong_day":
				p.Day = p.Day.AddDate(0, 0, 1)
			case "wrong_source":
				p.Source = "old"
			case "readonly_off":
				p.Controls.Readonly = "off"
			case "assigned_xid":
				v := int64(1)
				p.Controls.AssignedXID = &v
			case "unknown":
				p.Known = false
			case "wrong_kind":
				p.Holdings.Source.Kind = "ledger_approximate"
			case "missing_height":
				p.Holdings.Source.Height = 0
			case "negative":
				p.Holdings.Liquid = amt(-1)
			}
			if e := v6Validate(p, r); e == nil {
				t.Fatal("invalid proof admitted")
			}
		})
	}
}
