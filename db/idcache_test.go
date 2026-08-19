package db

import (
	"sync"
	"testing"
)

func TestIDCacheRoundTrip(t *testing.T) {
	c := newIDCache()
	if _, ok := c.get("/cosmos.bank.v1beta1.MsgSend"); ok {
		t.Fatal("empty cache returned a hit")
	}
	c.put("/cosmos.bank.v1beta1.MsgSend", 42)
	id, ok := c.get("/cosmos.bank.v1beta1.MsgSend")
	if !ok || id != 42 {
		t.Fatalf("got %d/%v, want 42/true", id, ok)
	}
}

// A zero id means the upsert did not return one. Caching that would hand out 0
// as a foreign key forever, so it must be refused.
func TestIDCacheRefusesZero(t *testing.T) {
	c := newIDCache()
	c.put("transfer", 0)
	if _, ok := c.get("transfer"); ok {
		t.Fatal("cached a zero id")
	}
	c.put("transfer", 7)
	if id, _ := c.get("transfer"); id != 7 {
		t.Fatalf("real id did not replace the refused zero, got %d", id)
	}
}

// The cache is read by every RPC worker and written by the DB stage.
func TestIDCacheConcurrent(t *testing.T) {
	c := newIDCache()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(n int) { defer wg.Done(); c.put("k", uint(n%7+1)) }(i)
		go func() { defer wg.Done(); c.get("k") }()
	}
	wg.Wait()
	if id, ok := c.get("k"); !ok || id == 0 {
		t.Fatalf("expected a non-zero id after concurrent writes, got %d/%v", id, ok)
	}
}
