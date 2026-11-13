package replayrecords

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestZeroAnteFeeScope(t *testing.T) {
	for _, name := range []string{"normal", "aborted", "empty denom", "message", "end", "label", "principal empty denom", "negative zero", "leading zero", "export"} {
		t.Run(name, func(t *testing.T) {
			r := Record{Kind: "movement", Height: 1, Tx: 0, Message: -1, Phase: "ante", Label: "ante_fee", Status: "committed", Address: "synthetic", Denom: "uatom", Amount: "0"}
			valid := name == "normal" || name == "aborted" || name == "empty denom"
			switch name {
			case "aborted":
				r.Status = "aborted"
			case "empty denom":
				r.Denom = ""
			case "message":
				r.Phase, r.Message = "message", 0
			case "end":
				r.Phase, r.Tx = "end_block", -1
			case "label":
				r.Label = "other"
			case "principal empty denom":
				r.Kind, r.Phase, r.Message, r.Denom = "principal", "message", 0, ""
			case "negative zero":
				r.Amount = "-0"
			case "leading zero":
				r.Amount = "00"
			}
			records := []Record{{Kind: "accounting", Height: 1, Tx: -1, Message: -1, Phase: "begin_block", Status: "committed"}, r, {Kind: "accounting", Height: 1, Tx: -1, Message: -1, Phase: "end_block", Status: "committed"}, {Kind: "height_completed", Height: 1, Tx: -1, Message: -1, Phase: "block", Status: "committed"}}
			if name == "export" {
				r.Phase, r.Status, r.Tx = "zero_height_export", "exported", -1
				records = []Record{r, {Kind: "accounting", Height: 1, Tx: -1, Message: -1, Phase: r.Phase, Status: r.Status}, {Kind: "export_completed", Height: 1, Tx: -1, Message: -1, Phase: r.Phase, Status: r.Status}}
			}
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			for _, rec := range records {
				if err := enc.Encode(rec); err != nil {
					t.Fatal(err)
				}
			}
			sum := sha256.Sum256(buf.Bytes())
			proof := Proof{Chain: "cosmoshub-2", Release: Release034, From: 1, Through: 1, VerifiedThrough: 1, SHA256: hex.EncodeToString(sum[:]), Export: name == "export"}
			_, err := Read(bytes.NewReader(buf.Bytes()), proof)
			if (err == nil) != valid {
				t.Fatalf("accepted=%v wanted=%v", err == nil, valid)
			}
		})
	}
}
