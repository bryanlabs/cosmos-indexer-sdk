package main

import (
	"github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	"strings"
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

func chainManifest(components int) Manifest {
	m := Manifest{Version: 2, First: 1, SourceCommit: "c3444f7f1d48bb48fe596b80c79bb049a10e04b0"}
	for i := 0; i < components; i++ {
		first := int64(i)*200000 + 1
		last := first + 199999
		if last > canonicalHeight {
			last = canonicalHeight
		}
		m.Components = append(m.Components, Component{First: first, Last: last, Prefix: i == 0, HistoricalRows: 1, Transactions: 1, RecordsSHA: "pin", Status: "s", CacheManifest: "c", Proof: replayrecords.Proof{Chain: "cosmoshub-2", Release: replayrecords.Release034, From: first, Through: last, VerifiedThrough: last, SHA256: "pin"}})
		m.Last = last
	}
	return m
}
func canonicalExport() *CanonicalExport {
	return &CanonicalExport{Records: "x", Rows: 1, Proof: replayrecords.Proof{Chain: "cosmoshub-2", Release: replayrecords.Release034, From: 2902000, Through: 2902000, VerifiedThrough: 2902000, Export: true, SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}}
}
func TestAnySealedPrefixAndTheCanonicalEnd(t *testing.T) {
	for n := 1; n <= 14; n++ {
		if validateLayout(chainManifest(n)) != nil {
			t.Fatal("contiguous sealed prefix refused", n)
		}
	}
	end := chainManifest(15)
	if end.Last != 2902000 || end.Components[14].First != 2800001 {
		t.Fatal("canonical end not reached by the fifteenth component")
	}
	if validateLayout(end) == nil {
		t.Fatal("canonical end admitted without its restart payout")
	}
	end.CanonicalExport = canonicalExport()
	if validateLayout(end) != nil {
		t.Fatal("complete chain with restart payout refused")
	}
	end.Tail = &Tail{First: 2902001, Last: 2902002, Cache: "c", CacheManifest: "m"}
	if validateLayout(end) != nil {
		t.Fatal("complete chain with tail refused")
	}
	for _, change := range []func(*Manifest){
		func(m *Manifest) { m.Components[14].Last = 3000000; m.Last = 3000000 },
		func(m *Manifest) { m.Components[14].Proof.Export = true },
		func(m *Manifest) { m.CanonicalExport.Proof.Export = false },
		func(m *Manifest) { m.CanonicalExport.Proof.From = 2800000 },
		func(m *Manifest) { m.CanonicalExport.Proof.Through = 2902002 },
		func(m *Manifest) { m.CanonicalExport.Rows = 0 },
		func(m *Manifest) { m.Tail.First = 2902000 },
		func(m *Manifest) { m.Tail.Last = 2902003 },
		func(m *Manifest) { m.Version = 1 },
		func(m *Manifest) { m.Components = append(m.Components, m.Components[14]) },
	} {
		m := chainManifest(15)
		m.CanonicalExport = canonicalExport()
		m.Tail = &Tail{First: 2902001, Last: 2902002, Cache: "c", CacheManifest: "m"}
		change(&m)
		if validateLayout(m) == nil {
			t.Fatal("invalid complete chain admitted")
		}
	}
	// An intermediate zero-height export can never be listed as the payout,
	// and the tail cannot arrive before the canonical end.
	for n := 1; n <= 14; n++ {
		m := chainManifest(n)
		m.CanonicalExport = canonicalExport()
		m.CanonicalExport.Proof.From, m.CanonicalExport.Proof.Through, m.CanonicalExport.Proof.VerifiedThrough = m.Last, m.Last, m.Last
		if validateLayout(m) == nil {
			t.Fatal("intermediate export admitted as restart payout", n)
		}
		m = chainManifest(n)
		m.Tail = &Tail{First: 2902001, Last: 2902002, Cache: "c", CacheManifest: "m"}
		if validateLayout(m) == nil {
			t.Fatal("tail admitted before the canonical end", n)
		}
	}
}
func TestStoredRowsOfAComponentDoNotDependOnTheManifest(t *testing.T) {
	short, long := chainManifest(2), chainManifest(10)
	short.Pins, long.Pins = map[string]string{"s": "a", "c": "b"}, map[string]string{"s": "a", "c": "b", "later": "z"}
	for i := range short.Components {
		if componentReceipt(short, short.Components[i], "g") != componentReceipt(long, long.Components[i], "g") {
			t.Fatal("receipt of a component changed when later components were listed")
		}
	}
	if wantEvents(short, short.Components[1]) != 400000 {
		t.Fatal("event count")
	}
}
func TestStagingTargetNeverTheServingForward(t *testing.T) {
	if guardTarget("legacy_replay_rehearsal_hub2_full_chain_20261116", 25462) != nil || guardTarget(database, port) != nil {
		t.Fatal("private staging target refused")
	}
	for _, c := range []struct {
		name string
		port int
	}{{"legacy_replay_rehearsal_hub2_full_chain_20261116", 15432}, {"legacy_replay_rehearsal_hub2_full_chain_20261116", 5432}, {"cosmos_tax", 25462}, {"legacy_replay_rehearsal_hub1_x_20261116", 25462}, {"legacy_replay_rehearsal_hub2_x; DROP", 25462}, {"postgres", 25462}} {
		if guardTarget(c.name, c.port) == nil {
			t.Fatal("unsafe staging target admitted")
		}
	}
}

func TestSealOfIsAlwaysAPinnedFile(t *testing.T) {
	m := chainManifest(3)
	m.Components[0].Records, m.Components[1].Records, m.Components[2].Records = "/a/prefix/x/records", "/a/600000/candidate/movements.ndjson", "/a/attempt2/800000/candidate/movements.ndjson"
	m.Pins = map[string]string{"/s/hub2-genesis-prefix-1/final-acceptance.json": strings.Repeat("a", 64), "/a/600000/financial/component-result.json": strings.Repeat("b", 64), "/a/attempt2/800000/final-component-acceptance.json": strings.Repeat("c", 64), "/a/attempt2/800000/financial/component-result.json": strings.Repeat("d", 64), "/a/800000/final-component-acceptance.json": strings.Repeat("e", 64)}
	for i, want := range []string{"/s/hub2-genesis-prefix-1/final-acceptance.json", "/a/600000/financial/component-result.json", "/a/attempt2/800000/final-component-acceptance.json"} {
		if got, e := sealOf(m, m.Components[i]); e != nil || got != want {
			t.Fatal("seal of component", i, got)
		}
	}
	delete(m.Pins, "/a/600000/financial/component-result.json")
	if _, e := sealOf(m, m.Components[1]); e == nil {
		t.Fatal("unpinned seal admitted")
	}
}
