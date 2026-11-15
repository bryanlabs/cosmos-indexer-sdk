package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigOnlyExistingPrivateFixture(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:${PRESERVATION_REDACTED}@127.0.0.1:15432/test?sslmode=disable",
		"postgres://u:${PRESERVATION_REDACTED}@example.com:25671/test?sslmode=disable",
		"postgres://u:${PRESERVATION_REDACTED}@127.0.0.1:25671/cosmostax?sslmode=disable",
		"postgres://u:${PRESERVATION_REDACTED}@127.0.0.1:25672/test?sslmode=disable",
		"",
	} {
		t.Run(dsn, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.env")
			if e := os.WriteFile(path, []byte("LEGACY_STORE_TEST_ADMIN_DSN='"+dsn+"'\n"), 0600); e != nil {
				t.Fatal(e)
			}
			if _, e := config(path); e == nil {
				t.Fatal("unsafe fixture accepted")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "fixture.env")
	if e := os.WriteFile(path, []byte("export LEGACY_STORE_TEST_ADMIN_DSN='postgres://u:${PRESERVATION_REDACTED}@127.0.0.1:25671/test?sslmode=disable'\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := config(path); e != nil {
		t.Fatal("fixed private fixture refused")
	}
}
