package main

import "testing"

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{fixtureNewRelease, fixtureReviewedRelease, true},
		{fixtureReviewedRelease, fixtureNewRelease, false},
		{fixtureNewRelease, "v29.0.0-rc0", true},
		{"v29.0.0-rc0", fixtureNewRelease, false},
		{"v29.0.0-rc1", fixtureReviewedRelease, true},
		{"v28.10.0", "v28.9.0", true},
		{fixtureReviewedRelease, fixtureReviewedRelease, false},
	}
	for _, c := range cases {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%s,%s)=%v want %v", c.a, c.b, got, c.want)
		}
	}
	if majorMinor("0.53.8") != "0.53" {
		t.Fatal("majorMinor")
	}
}
