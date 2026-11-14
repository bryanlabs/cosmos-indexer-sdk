package db

import "testing"

func TestRedactURLPathsKeepsHostOnly(t *testing.T) {
	got := redactURLPaths(`Post "https://rpc.example.com/secret-key/": EOF; retry http://10.0.0.1:26657/block?height=5 failed`)
	want := `Post "https://rpc.example.com": EOF; retry http://10.0.0.1:26657 failed`
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
