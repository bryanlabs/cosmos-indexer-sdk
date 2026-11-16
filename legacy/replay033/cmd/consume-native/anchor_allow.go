//go:build !splitproof

package main

// Saved native checkpoints of the original Hub 1 replay that a range may open from.
func allowedAnchor(height int64) bool {
	return height == 100000 || height == 200000 || height == 300000 || height == 400000
}
