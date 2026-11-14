package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func cacheWindowFixture(t *testing.T) (string, map[string]any, []byte) {
	t.Helper()
	dir := t.TempDir()
	members := map[string]string{}
	boundaries := map[string]any{}
	var remote bytes.Buffer
	for h := 1; h <= 4; h++ {
		for _, kind := range []string{"block", "block_results"} {
			name := "cosmoshub-2/0/" + strconv.Itoa(h) + "-" + kind + ".raw.gz"
			raw := []byte(fmt.Sprintf(`{"result":{"height":"%d"}}`, h))
			if kind == "block" {
				raw = []byte(fmt.Sprintf(`{"result":{"block_meta":{"block_id":{"hash":"%064d"}},"block":{"header":{"height":"%d","chain_id":"cosmoshub-2","app_hash":"%s","last_block_id":{"hash":"%064d"}}}}}`, h, h, strings.Repeat("a", 64), h-1))
				boundaries[strconv.Itoa(h)] = map[string]any{"raw_block_sha256": windowHash(raw), "block_hash": fmt.Sprintf("%064d", h), "header_app_hash": strings.Repeat("a", 64)}
			}
			var compressed bytes.Buffer
			z := gzip.NewWriter(&compressed)
			z.Write(raw)
			z.Close()
			raw = compressed.Bytes()
			path := filepath.Join(dir, "cache", name)
			os.MkdirAll(filepath.Dir(path), 0700)
			os.WriteFile(path, raw, 0600)
			members[name] = windowHash(raw)
			remote.WriteString(windowHash(raw) + "  " + name + "\n")
		}
	}
	binding := map[string]any{"all_raw_members": members, "boundaries": boundaries, "native_state_completion": "source-attested by retained successful original status/report; no retained per-height native AppHash or full response bytes", "original_job_executed_482091": true, "bounded_component_does_not_execute_any_native_block": true}
	return dir, binding, remote.Bytes()
}
func TestSealedCacheAllRawSourceBindings(t *testing.T) {
	dir, binding, remote := cacheWindowFixture(t)
	manifest, _ := json.Marshal(binding)
	if verifyWindowCache(dir, manifest, remote, 1, 4) != nil {
		t.Fatal("baseline refused")
	}
	for _, name := range []string{"missing", "changed-byte", "changed-hash", "reordered-remote", "duplicate-remote", "partial-remote", "extra-remote", "fabricated-native-proof", "next-unexecuted", "executes-native", "missing-bound", "wrong-bound-apphash", "wrong-bound-raw", "wrong-bound-block", "public-file", "duplicate-json"} {
		t.Run(name, func(t *testing.T) {
			dir, binding, remote := cacheWindowFixture(t)
			members := binding["all_raw_members"].(map[string]string)
			switch name {
			case "missing":
				delete(members, "cosmoshub-2/0/1-block.raw.gz")
			case "changed-byte":
				os.WriteFile(filepath.Join(dir, "cache/cosmoshub-2/0/1-block.raw.gz"), []byte("changed"), 0600)
			case "changed-hash":
				members["cosmoshub-2/0/1-block.raw.gz"] = "wrong"
			case "reordered-remote":
				lines := bytes.Split(remote, []byte{'\n'})
				lines[0], lines[1] = lines[1], lines[0]
				remote = bytes.Join(lines, []byte{'\n'})
			case "duplicate-remote":
				lines := bytes.Split(remote, []byte{'\n'})
				lines[1] = lines[0]
				remote = bytes.Join(lines, []byte{'\n'})
			case "partial-remote":
				remote = remote[:len(remote)-1]
			case "extra-remote":
				remote = append(remote, []byte("extra\n")...)
			case "fabricated-native-proof":
				binding["native_state_completion"] = "independent all native roots proved"
			case "next-unexecuted":
				binding["original_job_executed_482091"] = false
			case "executes-native":
				binding["bounded_component_does_not_execute_any_native_block"] = false
			case "missing-bound":
				delete(binding["boundaries"].(map[string]any), "1")
			case "wrong-bound-apphash":
				binding["boundaries"].(map[string]any)["1"].(map[string]any)["header_app_hash"] = strings.Repeat("b", 64)
			case "wrong-bound-raw":
				binding["boundaries"].(map[string]any)["1"].(map[string]any)["raw_block_sha256"] = strings.Repeat("b", 64)
			case "wrong-bound-block":
				binding["boundaries"].(map[string]any)["1"].(map[string]any)["block_hash"] = strings.Repeat("b", 64)
			case "public-file":
				os.Chmod(filepath.Join(dir, "cache/cosmoshub-2/0/1-block.raw.gz"), 0644)
			}
			manifest, _ := json.Marshal(binding)
			if name == "duplicate-json" {
				manifest = append(bytes.Clone(manifest[:len(manifest)-1]), []byte(`,"all_raw_members":{}}`)...)
			}
			if verifyWindowCache(dir, manifest, remote, 1, 4) == nil {
				t.Fatal("mutated/missing cache evidence admitted")
			}
		})
	}
}
