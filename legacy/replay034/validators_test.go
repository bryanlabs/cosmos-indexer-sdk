package main

import (
	"bytes"
	"math/rand"
	"path/filepath"
	"testing"

	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/ed25519"
	"github.com/tendermint/tendermint/types"
)

func TestValidatorHistoryRebuildsTheRunningSets(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	var keys []abci.ValidatorUpdate
	for i := 0; i < 12; i++ {
		keys = append(keys, types.TM2PB.NewValidatorUpdate(ed25519.GenPrivKey().PubKey(), int64(10+i)))
	}
	path := filepath.Join(t.TempDir(), "changes.jsonl")
	v, err := openValidators(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.start(keys[:5]); err != nil {
		t.Fatal(err)
	}
	power := map[int]int64{0: 10, 1: 11, 2: 12, 3: 13, 4: 14}
	hashes := map[int64][]byte{1: v.current.Hash()}
	const last = 400
	for h := int64(1); h <= last; h++ {
		var updates []abci.ValidatorUpdate
		if random.Intn(3) == 0 {
			i := random.Intn(len(keys))
			next := int64(random.Intn(40))
			if _, in := power[i]; !in && next == 0 {
				next = 5
			}
			if next == 0 && len(power) == 1 {
				next = 9
			}
			u := keys[i]
			u.Power = next
			updates = append(updates, u)
			if next == 0 {
				delete(power, i)
			} else {
				power[i] = next
			}
		}
		if err := v.record(h, updates); err != nil {
			t.Fatal(err)
		}
		if err := v.advance(h); err != nil {
			t.Fatal(err)
		}
		hashes[h+1] = v.current.Hash()
	}
	for h := int64(1); h <= last+1; h++ {
		set, err := v.at(h)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(set.Hash(), hashes[h]) {
			t.Fatalf("set rebuilt for height %d is not the one that was in force", h)
		}
	}
	want := [][]byte{v.last.Hash(), v.current.Hash(), v.next.Hash()}
	// A change written for a height the application never committed is dropped.
	if err := v.write(last+1, keys[:1]); err != nil {
		t.Fatal(err)
	}
	v.close()
	again, err := openValidators(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.close()
	if err := again.resume(last); err != nil {
		t.Fatal(err)
	}
	got := [][]byte{again.last.Hash(), again.current.Hash(), again.next.Hash()}
	for i := range want {
		if !bytes.Equal(want[i], got[i]) {
			t.Fatalf("set %d after a restart is not the running one", i)
		}
	}
	if len(again.changes) != len(v.changes) {
		t.Fatalf("restart kept %d changes, want %d", len(again.changes), len(v.changes))
	}
	if _, err := again.at(0); err == nil {
		t.Fatal("height 0 accepted")
	}
}
