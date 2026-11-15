//go:build ops818opening06storedv3 && ops818opening06archivev2

package taxapi

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOPS818Archive06V2ActualBoundPositiveAndRejections(t *testing.T) {
	if b6Dir == "" {
		t.Skip("actual private proof not supplied")
	}
	var p b6Proof
	if e := v6Read(filepath.Join(b6Dir, "archive-opening-000001.proof.private.json"), &p); e != nil {
		t.Fatal("actual source proof unavailable")
	}
	b, e := os.ReadFile(filepath.Join(v6Root, "extra-bank-request.private.jsonl"))
	if e != nil {
		t.Fatal("actual request unavailable")
	}
	var request v6Request
	for _, line := range bytes.Split(b, []byte("\n")) {
		var r v6Request
		if json.Unmarshal(line, &r) == nil && r.Kind == "retained-opening" && r.Request.Address == p.Address && r.Request.Day.Equal(p.Day) {
			request = r
			break
		}
	}
	if e = b6Validate(p, request); e != nil {
		t.Fatal("actual positive source proof rejected")
	}
	for _, name := range []string{"wrong_address", "wrong_day", "wrong_source", "readonly_off", "assigned_xid", "wrong_kind", "wrong_height", "wrong_receipt_hash", "wrong_raw_file_hash", "not_replayed"} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(p)
			var q b6Proof
			if json.Unmarshal(raw, &q) != nil {
				t.Fatal("proof fixture clone failed")
			}
			switch name {
			case "wrong_address":
				q.Address = "synthetic-other"
			case "wrong_day":
				q.Day = q.Day.AddDate(0, 0, 1)
			case "wrong_source":
				q.Source = "old-source"
			case "readonly_off":
				q.Controls.Readonly = "off"
			case "assigned_xid":
				n := int64(1)
				q.Controls.AssignedXID = &n
			case "wrong_kind":
				q.Holdings.Source.Kind = "ledger_approximate"
			case "wrong_height":
				q.Boundary.Height++
			case "wrong_receipt_hash":
				q.Receipts[0].SHA256 = "wrong"
			case "wrong_raw_file_hash":
				for path := range q.RawHTTP {
					q.RawHTTP[path] = "wrong"
					break
				}
			case "not_replayed":
				q.IndependentlyReplayed = false
			}
			if e := b6Validate(q, request); e == nil {
				t.Fatal("invalid source proof accepted")
			}
		})
	}
}
