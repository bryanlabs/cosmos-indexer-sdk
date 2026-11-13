package replayrecords

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func fixture() ([]byte, Proof) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	for _, r := range []Record{{Kind: "accounting", Height: 1, Phase: "begin_block", Status: "committed", Tx: -1, Message: -1}, {Kind: "movement", Height: 1, Phase: "message", Status: "committed", Address: "test-account", Denom: "uatom", Amount: "7"}, {Kind: "accounting", Height: 1, Phase: "end_block", Status: "committed", Tx: -1, Message: -1}, {Kind: "height_completed", Height: 1, Phase: "block", Status: "committed", Tx: -1, Message: -1}} {
		e.Encode(r)
	}
	sum := sha256.Sum256(b.Bytes())
	return b.Bytes(), Proof{Chain: "cosmoshub-2", Release: Release034, From: 1, Through: 1, VerifiedThrough: 1, SHA256: hex.EncodeToString(sum[:])}
}
func TestEvidence(t *testing.T) {
	raw, p := fixture()
	h, e := Read(bytes.NewReader(raw), p)
	if e != nil || len(h) != 1 {
		t.Fatal("complete segment rejected")
	}
	for _, tc := range []struct {
		name   string
		data   []byte
		change func(*Proof)
	}{
		{"own commit not next header", raw, func(p *Proof) { p.VerifiedThrough = 0 }},
		{"wrong chain", raw, func(p *Proof) { p.Chain = "cosmoshub-3" }},
		{"unpinned release", raw, func(p *Proof) { p.Release = "latest" }},
		{"wrong digest", raw, func(p *Proof) { p.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000" }},
		{"partial line", raw[:len(raw)-1], func(*Proof) {}},
		{"missing height", raw, func(p *Proof) { p.Through = 2; p.VerifiedThrough = 2 }},
		{"missing accounting", bytes.ReplaceAll(raw, []byte("begin_block"), []byte("unexpected")), func(p *Proof) {}},
		{"noncanonical amount", bytes.ReplaceAll(raw, []byte(`"amount":"7"`), []byte(`"amount":"07"`)), func(p *Proof) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := p
			tc.change(&q)
			if _, e := Read(bytes.NewReader(tc.data), q); e == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
}
