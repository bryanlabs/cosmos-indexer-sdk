//go:build ops818opening06

package taxapi

import (
	"testing"
	"time"
)

func TestOPS818Opening06ProofChecks(t *testing.T) {
	r := o6Request{Source: o6Source}
	r.Request.Address = "synthetic"
	r.Request.Day = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	base := o6Proof{Source: o6Source, Controls: o6Controls{Readonly: "on", Snapshot: "snapshot"}, Address: r.Request.Address, Day: r.Request.Day, Holdings: flowHoldings{Liquid: amt(1), Source: CommissionFlowHoldingSource{Kind: "ledger_exact", Height: 1}}, Known: true, Error: "<nil>"}
	if e := o6Validate(base, r); e != nil {
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
			if e := o6Validate(p, r); e == nil {
				t.Fatal("invalid proof admitted")
			}
		})
	}
}
