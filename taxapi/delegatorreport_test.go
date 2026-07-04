package taxapi

import "testing"

func TestOriginAllowed(t *testing.T) {
	cases := []struct {
		name    string
		origin  string
		allowed string
		want    bool
	}{
		{"no restriction configured", "https://evil.example", "", true},
		{"no origin header sent", "", "https://myvalidator.example", true},
		{"exact match", "https://myvalidator.example", "https://myvalidator.example", true},
		{"trailing slash on origin ignored", "https://myvalidator.example/", "https://myvalidator.example", true},
		{"case-insensitive", "https://MyValidator.example", "https://myvalidator.example", true},
		{"mismatch rejected", "https://evil.example", "https://myvalidator.example", false},
		{"subdomain mismatch rejected", "https://sub.myvalidator.example", "https://myvalidator.example", false},
	}
	for _, c := range cases {
		if got := originAllowed(c.origin, c.allowed); got != c.want {
			t.Errorf("%s: originAllowed(%q, %q) = %v, want %v", c.name, c.origin, c.allowed, got, c.want)
		}
	}
}
