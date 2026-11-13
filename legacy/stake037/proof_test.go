package main

import (
	"bytes"
	"os"
	"testing"
)

func TestPreservedRootProbe(t *testing.T) {
	a, b := os.Getenv("STAKING_ROOT_790"), os.Getenv("STAKING_ROOT_791")
	if a == "" || b == "" {
		t.Skip("private archive evidence not configured")
	}
	x, e := os.ReadFile(a)
	if e != nil {
		t.Fatal("first proof unavailable")
	}
	y, e := os.ReadFile(b)
	if e != nil {
		t.Fatal("second proof unavailable")
	}
	p, e := stakingRoot(x)
	if e != nil {
		t.Fatal(e)
	}
	q, e := stakingRoot(y)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(p, q) {
		t.Fatal("staking root changes between export and latest archive height")
	}
}
