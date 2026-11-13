package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	rr "github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
)

func TestStatusTagOnlyOriginalEvidence(t *testing.T) {
	dir, err := os.MkdirTemp("", "replay-tags-retained-")
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("1 tx0:tags(order)\n19 end:tags(value action;)\n21 begin:tags(+1 proposer;)\n")
	path := filepath.Join(dir, "result-differences.txt")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	p := rr.Proof{Chain: "cosmoshub-2", Release: rr.Release034, From: 10, Through: 20, VerifiedThrough: 20, ResultDifferencesSHA256: hex.EncodeToString(hash[:])}
	s := replayStatus{Chain: "cosmoshub-2", Release: rr.Release034, From: 10, Committed: 21, Verified: 20, Diffs: 2}
	original := s
	if admitStatus(&p, s, path) != nil {
		t.Fatal("tags-only original refused")
	}
	if s != original {
		t.Fatal("actual status changed")
	}
	if p.ResultDifferencesAdmission == nil || p.ResultDifferencesAdmission.RunLines != 2 || p.ResultDifferencesAdmission.HistoricalLines != 1 || p.ResultDifferencesAdmission.ClassificationSHA256 == "" {
		t.Fatal("missing classification receipt")
	}
	receipt, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip rr.Proof
	if err = json.Unmarshal(receipt, &roundtrip); err != nil || admitStatus(&roundtrip, s, path) != nil {
		t.Fatal("admitted proof cannot roundtrip")
	}
	bad := *roundtrip.ResultDifferencesAdmission
	bad.ClassificationSHA256 = "forged"
	roundtrip.ResultDifferencesAdmission = &bad
	if admitStatus(&roundtrip, s, path) == nil {
		t.Fatal("classification forgery admitted")
	}
	if admitStatus(&p, s, "") == nil {
		t.Fatal("receipt substituted for full raw evidence")
	}
	q := p
	q.ResultDifferencesAdmission = nil
	q.ResultDifferencesSHA256 = ""
	if admitStatus(&q, s, path) == nil {
		t.Fatal("unbound differences admitted")
	}
	q = p
	q.ResultDifferencesAdmission = nil
	wrong := s
	wrong.Diffs = 1
	if admitStatus(&q, wrong, path) == nil {
		t.Fatal("actual result count not reconciled")
	}
	retained, err := os.ReadFile(path)
	if err != nil || string(retained) != string(raw) {
		t.Fatal("original differences changed")
	}
	// Difference height 21 is classified although records end at verified 20.
	// It must not be ignored or used to extend the verified record range.
	tooFar := p
	tooFar.Through = 21
	if admitStatus(&tooFar, s, path) == nil {
		t.Fatal("unverified tail admitted")
	}
	zero := s
	zero.Diffs = 0
	old := rr.Proof{Chain: "cosmoshub-2", Release: rr.Release034, From: 10, Through: 20, VerifiedThrough: 20}
	if admitStatus(&old, zero, "") != nil {
		t.Fatal("existing zero-diff proof now requires extra file")
	}
	zero.Verified = 22
	if admitStatus(&old, zero, "") == nil {
		t.Fatal("verified beyond committed admitted")
	}
}
