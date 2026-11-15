package main

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T, raw string, suffix []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "input.gz")
	f, e := os.Create(p)
	if e != nil {
		t.Fatal(e)
	}
	g := gzip.NewWriter(f)
	g.Write([]byte(raw))
	g.Close()
	f.Write(suffix)
	f.Close()
	return p
}
func TestExactClosure(t *testing.T) {
	v, e := scan(fixture(t, "{\"kind\":\"history_completed\",\"height\":42}\n", nil))
	if e != nil || v["records"] != int64(1) {
		t.Fatalf("scan failed: %v", e)
	}
}
func TestRefusals(t *testing.T) {
	for _, s := range []string{"not-json\n", "{\"height\":42}", ""} {
		if _, e := scan(fixture(t, s, nil)); e == nil {
			t.Fatal("accepted invalid stream")
		}
	}
	for _, s := range [][]byte{[]byte("extra"), {0x1f, 0x8b}} {
		if _, e := scan(fixture(t, "{}\n", s)); e == nil {
			t.Fatal("accepted suffix")
		}
	}
	p := fixture(t, "{}\n", nil)
	f, _ := os.OpenFile(p, os.O_RDWR, 0600)
	s, _ := f.Stat()
	f.Truncate(s.Size() - 3)
	f.Close()
	if _, e := scan(p); e == nil {
		t.Fatal("accepted truncated footer")
	}
}
