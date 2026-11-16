//go:build splitproof

package main

// Equivalence-proof builds only: a short real slice is split at a height that
// has no saved checkpoint. Every opening is still checked against the recorded
// roots, the raw block headers and the complete raw baseline.
func allowedAnchor(height int64) bool { return height > 0 }
