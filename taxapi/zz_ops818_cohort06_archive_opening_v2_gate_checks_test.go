//go:build ops818opening06storedv3 && ops818opening06archivev2

package taxapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOPS818Archive06V2ActualAdmissionAndExactMemo(t *testing.T) {
	source := os.Getenv("OPS818_ARCHIVE06_ORIGINAL_PROOF_ROOT")
	if source == "" {
		t.Skip("private actual source admission only")
	}
	var p b6Proof
	if v6Read(filepath.Join(source, "archive-opening-000001.proof.private.json"), &p) != nil {
		t.Fatal("actual archive proof unavailable")
	}
	r := v6Request{Kind: "retained-opening", Source: v6Source}
	r.Request.Address = p.Address
	r.Request.Day = p.Day
	if _, already, e := b6Admit(r); e != nil || !already {
		t.Fatal("already supplied actual request not skipped before HTTP")
	}
	r.Request.Day = r.Request.Day.In(time.FixedZone("synthetic-offset", 3600))
	if _, already, e := b6Admit(r); e != nil || !already {
		t.Fatal("timezone equivalent request bypasses duplicate guard")
	}
	r.Request.Address = "synthetic-never-requested"
	if missing, already, e := b6Admit(r); e != nil || already || missing != "" {
		t.Fatal("archive read admitted without exact stored missing memo")
	}
	raw, e := os.ReadFile(p.MissingSourceProof)
	if e != nil {
		t.Fatal("actual stored missing proof unavailable")
	}
	var memo map[string]any
	if json.Unmarshal(raw, &memo) != nil {
		t.Fatal("actual missing proof malformed")
	}
	outer := memo["request"].(map[string]any)
	inner := outer["request"].(map[string]any)
	inner["address"] = "synthetic-other-address"
	d, e := os.MkdirTemp(os.Getenv("TMPDIR"), "archive-exact-memo-")
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(d, "wrong-address.private.json")
	encoded, _ := json.Marshal(memo)
	if os.WriteFile(path, encoded, 0600) != nil {
		t.Fatal("fixture write failed")
	}
	wrong := p
	wrong.MissingSourceProof = path
	wrong.MissingSourceSHA = v6Hash(encoded)
	r.Request.Address = p.Address
	r.Request.Day = p.Day
	if e = b6Validate(wrong, r); e == nil {
		t.Fatal("another request's correctly hashed missing memo accepted")
	}
}
