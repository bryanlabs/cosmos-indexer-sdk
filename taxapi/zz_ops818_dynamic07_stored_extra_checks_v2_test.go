//go:build ops818dynamic07openingv2

package taxapi

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestOPS818StoredOpening07V2V3StoredPathChecks(t *testing.T) {
	day := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	r := v8Request{Source: v8Source}
	r.Request.Address = "synthetic"
	r.Request.Day = day
	good := v8Proof{Source: v8Source, Controls: v8Controls{Readonly: "on", Database: "cosmostax", Snapshot: "snapshot"}, Address: r.Request.Address, Day: day, Known: true, Error: "<nil>", Holdings: flowHoldings{Liquid: amt(1), Source: CommissionFlowHoldingSource{Kind: "ledger_exact", Height: 10}}, StoredBuild: &BalanceBuild{FromDay: &day, ToDay: &day}, StoredRow: BalanceDay{Chain: apiMainnet, Address: r.Request.Address, Day: day, Holdings: `{"liquid":{"uatom":"1"}}`}, Boundary: heightTime{Height: 10}}
	if e := v8Validate(good, r); e != nil {
		t.Fatal("valid stored decode rejected")
	}
	for _, name := range []string{"wrong_database", "missing_stored_build", "wrong_stored_address", "wrong_boundary_height", "native_query", "wrong_raw_stored_amount", "approximate_stored_stake"} {
		t.Run(name, func(t *testing.T) {
			p := good
			switch name {
			case "wrong_database":
				p.Controls.Database = "other"
			case "missing_stored_build":
				p.StoredBuild = nil
			case "wrong_stored_address":
				p.StoredRow.Address = "other"
			case "wrong_boundary_height":
				p.Boundary.Height = 11
			case "native_query":
				p.Queries = []string{"SELECT * FROM message_event_attributes"}
			case "wrong_raw_stored_amount":
				p.StoredRow.Holdings = `{"liquid":{"uatom":"2"}}`
			case "approximate_stored_stake":
				p.StoredRow.Holdings = `{"liquid":{"uatom":"1"},"delegation_error":{"validator":"1"}}`
			}
			if e := v8Validate(p, r); e == nil {
				t.Fatal("unproved stored path accepted")
			}
		})
	}
}
func TestOPS818StoredOpening07V2V3NoFallbackSourceGuard(t *testing.T) {
	b, e := os.ReadFile("zz_ops818_dynamic07_stored_v2_test.go")
	if e != nil {
		t.Fatal(e)
	}
	s := string(b)
	if strings.Contains(s, "privateReadSnapshotTimeout:") {
		t.Fatal("native timeout override returned")
	}
	pre := strings.Index(s, "build, err := s.balanceBuild(apiMainnet)")
	reject := strings.Index(s, "return v8MissingStored")
	invoke := strings.Index(s, "holdings, known, e := l.opening")
	if pre < 0 || reject < pre || invoke < reject {
		t.Fatal("production opening no longer guarded by stored-only preflight")
	}
 runner,e:=os.ReadFile("zz_ops818_dynamic07_runner_v2_test.go");if e!=nil {t.Fatal(e)}
	if !strings.Contains(string(runner), `os.Setenv("NODE_REST_API", "http://127.0.0.1:1")`) {
		t.Fatal("native HTTP escape disabled guard missing")
	}
}
