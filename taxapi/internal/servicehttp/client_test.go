package servicehttp

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestEndpointPinsOriginAndEncodesQuery(t *testing.T) {
	hits := 0
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++; _, _ = w.Write([]byte(`{}`)) }))
	defer attacker.Close()
	expected := "token&chain=other#fragment"
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("denom") != expected {
			t.Errorf("query injection: %v", r.URL.Query())
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, attacker.URL, http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer service.Close()
	endpoint, err := New(&http.Client{Timeout: time.Second}, service.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/price", attacker.URL, "//" + attacker.Listener.Addr().String(), "/../price"} {
		var out map[string]bool
		if err := endpoint.GetJSON(path, url.Values{"denom": {expected}}, &out); err != nil || !out["ok"] {
			t.Fatalf("path %q: %v %v", path, out, err)
		}
	}
	if err := endpoint.GetJSON("/redirect", url.Values{"denom": {expected}}, &struct{}{}); err == nil {
		t.Fatal("redirect accepted")
	}
	if hits != 0 {
		t.Fatalf("request escaped configured origin: %d", hits)
	}
}

func TestEndpointRejectsInvalidConfiguration(t *testing.T) {
	for _, origin := range []string{"file:///etc/passwd", "https://user:secret@example.com", "https://example.com?target=other", "https://example.com#fragment", "https:opaque", "//example.com"} {
		if _, err := New(&http.Client{Timeout: time.Second}, origin); err == nil {
			t.Fatalf("accepted %q", origin)
		}
	}
}
