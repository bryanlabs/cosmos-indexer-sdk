package replayrecords

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
)

func proofFor(raw []byte, from, through int64) Proof {
	s := sha256.Sum256(raw)
	return Proof{Chain: "cosmoshub-2", Release: Release034, From: from, Through: through, VerifiedThrough: through, SHA256: hex.EncodeToString(s[:])}
}
func TestStreamValidatesBeforeVisiting(t *testing.T) {
	raw, p := fixture()
	p.SHA256 = fmt.Sprintf("%064d", 0)
	calls := 0
	if e := Stream(bytes.NewReader(raw), p, func(Height) error { calls++; return nil }); e == nil || calls != 0 {
		t.Fatal("invalid digest reached consumer")
	}
}
func TestStreamManyHeightsAndAbortedEvidence(t *testing.T) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	const count = 20000
	for i := int64(1); i <= count; i++ {
		for _, r := range []Record{{Kind: "accounting", Height: i, Tx: -1, Message: -1, Phase: "begin_block", Status: "committed"}, {Kind: "movement", Height: i, Tx: 0, Message: 0, Phase: "message", Label: "attempt", Address: "synthetic", Denom: "uatom", Amount: "1", Status: "aborted"}, {Kind: "accounting", Height: i, Tx: -1, Message: -1, Phase: "end_block", Status: "committed"}, {Kind: "height_completed", Height: i, Tx: -1, Message: -1, Phase: "block", Status: "committed"}} {
			if e.Encode(r) != nil {
				t.Fatal("encode")
			}
		}
	}
	calls := 0
	if err := Stream(bytes.NewReader(b.Bytes()), proofFor(b.Bytes(), 1, count), func(h Height) error {
		calls++
		if len(h.Records) != 4 || h.Number != int64(calls) || h.Records[1].Status != "aborted" {
			t.Fatal("height evidence lost")
		}
		return nil
	}); err != nil || calls != count {
		t.Fatal("stream failed")
	}
}
func TestExactByteVerifiedPrefixKeepsOriginalTail(t *testing.T) {
	var original bytes.Buffer
	encoder := json.NewEncoder(&original)
	encoder.SetIndent("", "")
	prefixEnd := 0
	for h := int64(9); h <= 10; h++ {
		for _, r := range []Record{
			{Kind: "accounting", Height: h, Tx: -1, Message: -1, Phase: "begin_block", Status: "committed"},
			{Kind: "accounting", Height: h, Tx: -1, Message: -1, Phase: "end_block", Status: "committed"},
			{Kind: "height_completed", Height: h, Tx: -1, Message: -1, Phase: "block", Status: "committed"},
		} {
			if err := encoder.Encode(r); err != nil {
				t.Fatal(err)
			}
		}
		if h == 9 {
			prefixEnd = original.Len()
		}
	}
	before := sha256.Sum256(original.Bytes())
	prefix := append([]byte(nil), original.Bytes()[:prefixEnd]...)
	p := proofFor(prefix, 9, 9)
	if err := Stream(bytes.NewReader(prefix), p, nil); err != nil {
		t.Fatal(err)
	}
	full := proofFor(original.Bytes(), 9, 10)
	full.VerifiedThrough = 9
	if err := Stream(bytes.NewReader(original.Bytes()), full, nil); err == nil {
		t.Fatal("unverified original tail admitted")
	}
	if err := Stream(bytes.NewReader(original.Bytes()), p, nil); err == nil {
		t.Fatal("tail silently ignored")
	}
	if before != sha256.Sum256(original.Bytes()) || !bytes.Equal(prefix, original.Bytes()[:prefixEnd]) {
		t.Fatal("original evidence changed")
	}
}

func TestStreamBoundedLineAndExportClosure(t *testing.T) {
	raw := append(bytes.Repeat([]byte{'x'}, (1<<20)+1), '\n')
	if e := Stream(bytes.NewReader(raw), proofFor(raw, 1, 1), nil); e == nil {
		t.Fatal("oversize line accepted")
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	for _, r := range []Record{{Kind: "movement", Height: 9, Tx: -1, Message: -1, Phase: "zero_height_export", Status: "exported", Label: "delegator_reward/bank.add", Address: "synthetic", Validator: "validator", Delegator: "owner", Denom: "uatom", Amount: "1"}, {Kind: "accounting", Height: 9, Tx: -1, Message: -1, Phase: "zero_height_export", Status: "exported"}, {Kind: "export_completed", Height: 9, Tx: -1, Message: -1, Phase: "zero_height_export", Status: "exported"}} {
		e.Encode(r)
	}
	p := proofFor(b.Bytes(), 9, 9)
	p.Export = true
	if e := Stream(bytes.NewReader(b.Bytes()), p, nil); e != nil {
		t.Fatal("complete export rejected")
	}
	p.Export = false
	if e := Stream(bytes.NewReader(b.Bytes()), p, nil); e == nil {
		t.Fatal("export accepted as committed replay")
	}
}
