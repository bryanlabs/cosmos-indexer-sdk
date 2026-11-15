//go:build ops818capture07v1

package taxapi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOPS818CaptureOnly07PublishStored(t *testing.T) {
	prior := v9Dir
	v9Dir = t.TempDir()
	defer func() { v9Dir = prior }()
	day := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	r := v9Request{Source: v9Source}
	r.Request.Address = "synthetic"
	r.Request.Day = day
	p := v9Proof{StoredBuild: &BalanceBuild{FromDay: &day, ToDay: &day}, StoredRow: BalanceDay{Chain: apiMainnet, Address: r.Request.Address, Day: day, Holdings: `{"liquid":{"uatom":"1"}}`}, Boundary: heightTime{Height: 1}, Source: v9Source, Controls: v9Controls{Readonly: "on", Snapshot: "fixture", Database: "cosmostax"}, Address: r.Request.Address, Day: day, Holdings: flowHoldings{Liquid: amt(1), Source: CommissionFlowHoldingSource{Kind: "ledger_exact", Height: 1}}, Known: true, Error: "<nil>"}
	if e := v9Publish(r, p, 1); e != nil {
		t.Fatal(e)
	}
	var got v9Proof
	if e := v9Read(filepath.Join(v9Dir, "opening-000001.proof.private.json"), &got); e != nil || v9Validate(got, r) != nil {
		t.Fatal("captured proof invalid")
	}
	if e := v9Publish(r, p, 1); e == nil {
		t.Fatal("immutable proof overwritten")
	}
	p.Controls.Readonly = "off"
	if e := v9Publish(r, p, 2); e == nil {
		t.Fatal("bad controls captured")
	}
	if _, e := os.Stat(filepath.Join(v9Dir, "opening-000002.proof.private.json")); !os.IsNotExist(e) {
		t.Fatal("rejected proof created")
	}
}

func TestOPS818CaptureOnly07NoPublicationAST(t *testing.T) {
	for _, name := range []string{"zz_ops818_capture07_stored_v1_test.go", "zz_ops818_capture07_archive_v1_test.go"} {
		f, e := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if e != nil {
			t.Fatal(e)
		}
		count := 0
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || (fn.Name.Name != "v9Publish" && fn.Name.Name != "b9Publish") {
				continue
			}
			count++
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && (id.Name == "v9Root" || id.Name == "Flock" || id.Name == "Rename") {
					t.Fatal("live mutation capability retained")
				}
				return true
			})
		}
		if count != 1 {
			t.Fatal("publisher missing")
		}
	}
}
