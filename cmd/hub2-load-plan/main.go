// hub2-load-plan validates an immutable approval plan without opening a database.
// It has no load, migration, DSN, environment-credential or publication option.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/DefiantLabs/cosmos-indexer/legacy/hub2promotion"
)

func fileHash(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func plan(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("hub2-load-plan", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("manifest", "", "absolute immutable manifest path")
	pin := fs.String("manifest-sha256", "", "independently pinned manifest SHA256")
	fixture := fs.Bool("fixture-only", false, "validate a bounded synthetic package, never whole financial evidence")
	if e := fs.Parse(args); e != nil {
		return hub2promotion.ErrProductionDisabled
	}
	if fs.NArg() != 0 || *path == "" || *pin == "" {
		return errors.New("explicit immutable manifest and SHA256 required")
	}
	m, e := hub2promotion.ReadManifestPinned(*path, *pin)
	if e != nil {
		return errors.New("manifest parse refused")
	}
	if *fixture {
		e = m.ValidateFixture()
	} else {
		e = m.ValidateComplete()
	}
	if e != nil {
		return errors.New("plan admission refused; private evidence retained")
	}
	if !*fixture {
		exe, e := os.Executable()
		if e != nil {
			return e
		}
		sha, e := fileHash(exe)
		if e != nil || sha != m.BinarySHA {
			return errors.New("executing binary differs from sealed build")
		}
	}
	return json.NewEncoder(out).Encode(struct {
		ManifestSHA         string `json:"manifest_sha256"`
		Profile             string `json:"profile"`
		Fixture             bool   `json:"fixture_only"`
		ProductionExecution bool   `json:"production_execution"`
		ReadyForApproval    bool   `json:"ready_for_production_approval"`
		Notice              string `json:"notice"`
	}{*pin, m.Profile, *fixture, false, false, "Plan validation only. Actual financial-owner seals, backup/restore, scale proof, refreshed destination and explicit Dan YES remain required."})
}
func main() {
	if e := plan(os.Args[1:], os.Stdout); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
