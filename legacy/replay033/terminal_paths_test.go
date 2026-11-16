package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClosedTerminalEachIndependentOptionGuard(t *testing.T) {
	type options struct {
		chain                     string
		to                        int64
		compare, next             bool
		reference, genesis, proof string
	}
	good := options{"cosmoshub-1", 500015, true, false, "reference", "genesis", "proof"}
	check := func(o options) error {
		return validateTerminalRun(o.chain, o.to, o.compare, o.next, o.reference, o.genesis, o.proof)
	}
	if check(good) != nil {
		t.Fatal("positive exact terminal options refused")
	}
	for _, name := range []string{"chain", "height", "comparison", "next_header", "proof", "genesis"} {
		t.Run(name, func(t *testing.T) {
			o := good
			switch name {
			case "chain":
				o.chain = "cosmoshub-2"
			case "height":
				o.to = 500014
			case "comparison":
				o.compare = false
			case "next_header":
				o.next = true
			case "proof":
				o.proof = ""
			case "genesis":
				o.genesis = ""
			}
			if check(o) == nil {
				t.Fatal("single invalid option accepted")
			}
		})
	}
}

func TestClosedTerminalPathsRejectAllAliasesBeforeWritableOpen(t *testing.T) {
	root := t.TempDir()
	ref := filepath.Join(root, "reference")
	if os.Mkdir(ref, 0700) != nil {
		t.Fatal("fixture")
	}
	alias := filepath.Join(root, "alias")
	if os.Symlink(ref, alias) != nil {
		t.Fatal("fixture")
	}
	home, copy := filepath.Join(root, "home"), filepath.Join(root, "copy")
	output := filepath.Join(root, "proof")
	if validateTerminalPaths(ref, home, copy, output) != nil {
		t.Fatal("disjoint positive refused")
	}
	for _, name := range []string{"same_home", "nested_home", "reference_inside_home", "symlink_home", "same_copy", "nested_copy", "copy_inside_home", "output_inside_reference", "output_through_symlink", "output_inside_copy"} {
		t.Run(name, func(t *testing.T) {
			r, h, c, o := ref, home, copy, output
			switch name {
			case "same_home":
				h = ref
			case "nested_home":
				h = filepath.Join(ref, "future")
			case "reference_inside_home":
				h = root
			case "symlink_home":
				h = alias
			case "same_copy":
				c = ref
			case "nested_copy":
				c = filepath.Join(ref, "copy")
			case "copy_inside_home":
				c = filepath.Join(home, "copy")
			case "output_inside_reference":
				o = filepath.Join(ref, "evidence")
			case "output_through_symlink":
				o = filepath.Join(alias, "future", "evidence")
			case "output_inside_copy":
				o = filepath.Join(copy, "evidence")
			}
			if validateTerminalPaths(r, h, c, o) == nil {
				t.Fatal("overlap accepted")
			}
		})
	}
	// A valid complete CLI shape with only an overlapping home must reject before
	// creating recording marker, DB files, result-differences, or reference copy.
	before, _ := terminalManifest(ref)
	err := run([]string{"--chain", "cosmoshub-1", "--to", "500015", "--home", alias, "--closed-terminal-reference", ref, "--terminal-reference-copy", copy, "--genesis", filepath.Join(root, "genesis"), "--terminal-proof", output, "--coin-records", filepath.Join(root, "movements")})
	after, _ := terminalManifest(ref)
	if err == nil || !strings.Contains(err.Error(), "disjoint") || before != after {
		t.Fatal("CLI did not reject alias at the path guard before all opens")
	}
	if _, e := os.Stat(copy); !os.IsNotExist(e) {
		t.Fatal("invalid CLI created reference copy")
	}
}

func TestTerminalExportRequiresReferenceBeforeAnyDatabaseOpen(t *testing.T) {
	home := fixtureCheckpoint(t)
	before := checkpointHashes(t, home)
	if exportZero([]string{"--home", home, "--out", filepath.Join(t.TempDir(), "export"), "--require-height", "500015"}) == nil {
		t.Fatal("unbound terminal export accepted")
	}
	after := checkpointHashes(t, home)
	if len(before) != len(after) {
		t.Fatal("source changed")
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatal("source changed")
		}
	}
}
