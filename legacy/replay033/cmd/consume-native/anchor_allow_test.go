//go:build !splitproof

package main

import "testing"

func TestOnlySavedCheckpointHeightsOpenARange(t *testing.T) {
	for height, want := range map[int64]bool{0: false, 1: false, 99999: false, 100000: true, 150000: false, 200000: true, 300000: true, 400000: true, 400300: false, 500000: false} {
		if allowedAnchor(height) != want {
			t.Fatalf("height %d allowed=%v", height, !want)
		}
	}
}
