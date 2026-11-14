package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNoProductionLoadOrCredentialSurface(t *testing.T) {
	// Environment credentials, even if set by a caller, are never read.
	t.Setenv("DATABASE_URL", "postgres://must-never-be-opened.invalid/production")
	for _, args := range [][]string{{"--dsn", "postgres://forbidden"}, {"--load"}, {"--enable-production"}, {"--migrate"}, {"--repair"}, {"--delete"}, {"--publish"}, {"load"}, nil} {
		var out bytes.Buffer
		require.Error(t, plan(args, &out))
		require.Empty(t, out.String())
	}
}
func TestPinnedManifestAndFixtureDefaultRefusals(t *testing.T) {
	root := os.Getenv("HUB2_PROMOTION_ARTIFACT_ROOT")
	require.NotEmpty(t, root)
	dir, e := os.MkdirTemp(root, "plan-unit-")
	require.NoError(t, e)
	p := filepath.Join(dir, "incomplete.json")
	data := []byte(`{"fixture":true,"profile":"SYNTHETIC-ONLY-hub2-atomic-v1"}`)
	require.NoError(t, os.WriteFile(p, data, 0600))
	h := sha256.Sum256(data)
	pin := hex.EncodeToString(h[:])
	var out bytes.Buffer
	require.Error(t, plan([]string{"--manifest", p, "--manifest-sha256", "wrong"}, &out))
	require.Error(t, plan([]string{"--manifest", p, "--manifest-sha256", pin}, &out))
	require.Error(t, plan([]string{"--manifest", p, "--manifest-sha256", pin, "--fixture-only"}, &out))
	require.Empty(t, out.String())
}
