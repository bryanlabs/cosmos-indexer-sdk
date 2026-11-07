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
		{"no origin header sent", "", apiTestValidatorOrigin, true},
		{"exact match", apiTestValidatorOrigin, apiTestValidatorOrigin, true},
		{"trailing slash on origin ignored", "https://myvalidator.example/", apiTestValidatorOrigin, true},
		{"case-insensitive", "https://MyValidator.example", apiTestValidatorOrigin, true},
		{"mismatch rejected", "https://evil.example", apiTestValidatorOrigin, false},
		{"subdomain mismatch rejected", "https://sub.myvalidator.example", apiTestValidatorOrigin, false},
	}
	for _, c := range cases {
		if got := originAllowed(c.origin, c.allowed); got != c.want {
			t.Errorf("%s: originAllowed(%q, %q) = %v, want %v", c.name, c.origin, c.allowed, got, c.want)
		}
	}
}
