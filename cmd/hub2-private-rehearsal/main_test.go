package main

import (
	"github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	"testing"
)

func validManifest() Manifest {
	m := Manifest{Version: 1, First: 1, Last: 1400000, SourceCommit: "c3444f7f1d48bb48fe596b80c79bb049a10e04b0"}
	for i := int64(0); i < 7; i++ {
		first, last := i*200000+1, (i+1)*200000
		m.Components = append(m.Components, Component{First: first, Last: last, Prefix: i == 0, HistoricalRows: 1, Transactions: 1, RecordsSHA: "pin", Proof: replayrecords.Proof{Chain: "cosmoshub-2", Release: replayrecords.Release034, From: first, Through: last, VerifiedThrough: last, SHA256: "pin"}})
	}
	return m
}
func TestSealedLayoutExactNoGapDuplicateOrValidationTail(t *testing.T) {
	if validateLayout(validManifest()) != nil {
		t.Fatal("valid exact prefix refused")
	}
	for _, change := range []func(*Manifest){func(m *Manifest) { m.Components[1].First-- }, func(m *Manifest) { m.Components[1].First++ }, func(m *Manifest) { m.Components[6].Last++ }, func(m *Manifest) { m.Components[0].Proof.Export = true }, func(m *Manifest) { m.Components[0].Proof.Chain = "cosmoshub-1" }, func(m *Manifest) { m.Components[0].Prefix = false }, func(m *Manifest) { m.SourceCommit = "wrong" }, func(m *Manifest) { m.Components = m.Components[:6] }, func(m *Manifest) { m.Components[1].Proof.VerifiedThrough = 1 }, func(m *Manifest) { m.Components[1].RecordsSHA = "wrong" }} {
		m := validManifest()
		change(&m)
		if validateLayout(m) == nil {
			t.Fatal("invalid layout admitted")
		}
	}
}
func TestOnlyExactPrivateIdentity(t *testing.T) {
	if guardDSN(dsn(importerRole), database) != nil {
		t.Fatal("private identity refused")
	}
	for _, s := range []string{"", "host=127.0.0.1 port=15432 user=" + importerRole + " dbname=" + database, "host=10.0.0.1 port=25461 user=" + importerRole + " dbname=" + database, "host=127.0.0.1 port=25461 user=postgres dbname=" + database, "host=127.0.0.1 port=25461 user=" + importerRole + " dbname=postgres"} {
		if guardDSN(s, database) == nil {
			t.Fatal("unsafe identity admitted")
		}
	}
}
