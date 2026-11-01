package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAllowedURL(t *testing.T) {
	const nodeEndpoint = "http://node.test:1317"
	t.Setenv("NODE_REST_API", nodeEndpoint)
	t.Setenv("UPGRADES_API_URL", "https://upgrades.test/feed")
	for _, raw := range []string{
		nodeEndpoint + "/cosmos/base/tendermint/v1beta1/node_info",
		"https://api.github.com/repos/cosmos/gaia/releases",
		"https://upgrades.test/feed",
	} {
		if _, err := allowedURL(raw); err != nil {
			t.Errorf("configured URL %q: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"https://unconfigured.test/feed",
		"http://api.github.com/repos/cosmos/gaia/releases",
		"https://api.github.com.attacker.test/feed",
		"https://api.github.com@attacker.test/feed",
		"https://user:password@upgrades.test/feed",
		"https://upgrades.test/feed#fragment",
		"file:///etc/passwd",
		"/relative",
	} {
		if _, err := allowedURL(raw); err == nil {
			t.Errorf("accepted unsafe URL %q", raw)
		}
	}
}

func TestGetJSONConfiguredEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("GitHub token leaked to a configured non-GitHub endpoint")
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"value":7}`)); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	t.Setenv("UPGRADES_API_URL", server.URL)
	t.Setenv("GITHUB_TOKEN", "fixture-token")
	var got struct{ Value int }
	if err := getJSON(server.URL, &got); err != nil {
		t.Fatal(err)
	}
	if got.Value != 7 {
		t.Fatalf("value = %d, want 7", got.Value)
	}
}

func TestGetJSONRejectsCrossOriginRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://unconfigured.test/redirected", http.StatusFound)
	}))
	defer server.Close()
	t.Setenv("UPGRADES_API_URL", server.URL)
	var got any
	if err := getJSON(server.URL, &got); err == nil || !strings.Contains(err.Error(), "cross-origin redirect refused") {
		t.Fatalf("redirect error = %v", err)
	}
}

func TestRedirectLimitAndSameOrigin(t *testing.T) {
	u, err := url.Parse("https://upgrades.test/feed")
	if err != nil {
		t.Fatal(err)
	}
	req := &http.Request{URL: u}
	if err := client.CheckRedirect(req, []*http.Request{req}); err != nil {
		t.Fatalf("same-origin redirect: %v", err)
	}
	if err := client.CheckRedirect(req, make([]*http.Request, 10)); err == nil {
		t.Fatal("redirect limit was not enforced")
	}
}
