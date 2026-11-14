package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Resolve existing symlink ancestors even when an exclusive future output does
// not exist yet. Path isolation must be established BEFORE any writable open.
func terminalResolvedPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("terminal path required")
	}
	p, e := filepath.Abs(path)
	if e != nil {
		return "", e
	}
	suffix := []string{}
	for {
		real, e := filepath.EvalSymlinks(p)
		if e == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				real = filepath.Join(real, suffix[i])
			}
			return real, nil
		}
		if !os.IsNotExist(e) || filepath.Dir(p) == p {
			return "", fmt.Errorf("terminal path cannot resolve")
		}
		suffix = append(suffix, filepath.Base(p))
		p = filepath.Dir(p)
	}
}
func terminalContains(root, path string) bool {
	rel, e := filepath.Rel(root, path)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func terminalDisjoint(a, b string) bool { return !terminalContains(a, b) && !terminalContains(b, a) }

func validateTerminalPaths(reference, home, copyHome string, outputs ...string) error {
	r, e := terminalResolvedPath(reference)
	if e != nil {
		return e
	}
	h, e := terminalResolvedPath(home)
	if e != nil {
		return e
	}
	c, e := terminalResolvedPath(copyHome)
	if e != nil {
		return e
	}
	if !terminalDisjoint(r, h) || !terminalDisjoint(r, c) || !terminalDisjoint(h, c) {
		return fmt.Errorf("terminal reference, new replay and explicit new copy must be disjoint")
	}
	if _, e = os.Lstat(c); !os.IsNotExist(e) {
		return fmt.Errorf("terminal reference copy must be an exclusive new directory")
	}
	for _, out := range outputs {
		if out == "" {
			continue
		}
		p, e := terminalResolvedPath(out)
		if e != nil {
			return e
		}
		if terminalContains(r, p) || terminalContains(c, p) {
			return fmt.Errorf("terminal output must not modify any reference bytes")
		}
	}
	return nil
}

func validateTerminalRun(chain string, to int64, compare, next bool, reference, genesis, proof string) error {
	if reference == "" {
		if proof != "" {
			return fmt.Errorf("terminal proof requires pinned closed reference")
		}
		return nil
	}
	if chain != "cosmoshub-1" || to != terminalHeight || !compare || next || proof == "" || genesis == "" {
		return fmt.Errorf("closed terminal requires exact Hub1 final height, result checks, genesis and separate proof; next-header mode is exclusive")
	}
	return nil
}
