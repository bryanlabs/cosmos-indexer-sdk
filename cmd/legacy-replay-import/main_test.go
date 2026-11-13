package main

import (
	rr "github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	"testing"
)

func TestRehearsalDestinationGuard(t *testing.T) {
	name := "legacy_replay_rehearsal_synthetic_12345678"
	good := "host=127.0.0.1 port=25439 user=synthetic dbname=" + name + " sslmode=disable"
	if guardDSN(good, name) != nil {
		t.Fatal("private named rehearsal refused")
	}
	for _, tc := range []struct{ dsn, name string }{
		{good, "taxindexer"}, {"host=127.0.0.1 port=15432 dbname=" + name, name},
		{"postgresql://synthetic@127.0.0.1:15432/" + name, name},
		{"host=127.0.0.1 port=25439 dbname=postgres", name},
		{good, "legacy_replay_rehearsal_short"},
		{"host=127.0.0.1,127.0.0.2 port=25439,15432 dbname=" + name, name},
	} {
		if guardDSN(tc.dsn, tc.name) == nil {
			t.Fatal("unsafe destination admitted")
		}
	}
}
func TestStatusRequiresNextHeaderNotOwnCommit(t *testing.T) {
	p := rr.Proof{Chain: "cosmoshub-2", Release: rr.Release034, From: 10, Through: 20, VerifiedThrough: 20}
	s := replayStatus{Chain: "cosmoshub-2", Release: rr.Release034, From: 1, Committed: 21, Verified: 20}
	if verifyStatus(p, s) != nil {
		t.Fatal("next-header status refused")
	}
	for _, name := range []string{"own commit", "wrong release", "wrong chain", "different bound", "results differ", "segment starts too late"} {
		q := s
		switch name {
		case "own commit":
			q.Committed = 20
			q.Verified = 19
		case "wrong release":
			q.Release = "latest"
		case "wrong chain":
			q.Chain = "cosmoshub-3"
		case "different bound":
			q.Verified = 21
		case "results differ":
			q.Diffs = 1
		case "segment starts too late":
			q.From = 11
		}
		if verifyStatus(p, q) == nil {
			t.Fatal("unproven status admitted")
		}
	}
}
