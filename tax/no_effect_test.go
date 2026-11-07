package tax

import "testing"

func TestNoTaxEffectTypesAreNotAlsoClassified(t *testing.T) {
	classified := map[string]bool{}
	for _, url := range MessageTypeURLs {
		classified[url] = true
	}
	seen := map[string]bool{}
	for _, url := range NoTaxEffectTypeURLs {
		if classified[url] || seen[url] {
			t.Fatalf("%s is listed twice", url)
		}
		seen[url] = true
	}
}
