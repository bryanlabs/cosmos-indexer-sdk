package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

// Supplemental evidence headers require an explicit immutable capture pin as
// well as the per-member hash, never just a matching arbitrary local file.
func evidenceSourceCache(cache, pin string, height int64) string {
	need(cache != "" && len(pin) == 64, "supplemental evidence capture pin required")
	capturePath := filepath.Join(filepath.Dir(cache), "capture.private.json")
	info, e := os.Stat(capturePath)
	must(e)
	need(info.Size() <= 128<<20, "unbounded supplemental capture refused")
	body, e := os.ReadFile(capturePath)
	must(e)
	sum := sha256.Sum256(body)
	need(hex.EncodeToString(sum[:]) == pin, "supplemental evidence capture differs")
	var capture struct {
		Members map[string]string `json:"cache_members_sha256"`
	}
	must(json.Unmarshal(body, &capture))
	member := filepath.Join("cosmoshub-1", strconv.FormatInt(height/10000, 10), strconv.FormatInt(height, 10)+"-block.raw.gz")
	expected, ok := capture.Members[member]
	need(ok && len(expected) == 64, "historical evidence header absent from sealed capture")
	memberPath := filepath.Join(cache, member)
	info, e = os.Stat(memberPath)
	must(e)
	need(info.Size() <= 16<<20, "unbounded evidence header refused")
	file, e := os.ReadFile(memberPath)
	must(e)
	sum = sha256.Sum256(file)
	need(hex.EncodeToString(sum[:]) == expected, "historical evidence header source bytes differ")
	return cache
}
