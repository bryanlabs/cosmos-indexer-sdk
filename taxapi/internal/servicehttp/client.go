// Package servicehttp binds JSON reads to an operator-configured service origin.
package servicehttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Endpoint separates trusted configuration from request-controlled resource
// paths and query values. Private cluster origins are intentional.
type Endpoint struct {
	base   url.URL
	client http.Client
}

func New(client *http.Client, origin string) (*Endpoint, error) {
	base, err := url.Parse(origin)
	if err != nil {
		return nil, fmt.Errorf("invalid service origin: %w", err)
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Opaque != "" {
		return nil, fmt.Errorf("service origin must be an HTTP(S) URL without credentials, query or fragment")
	}
	bounded := *client
	bounded.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Endpoint{base: *base, client: bounded}, nil
}

// GetJSON cannot change the origin, including through // paths, query values,
// path traversal or redirects. Path input is never resolved as another URL.
func (e *Endpoint) GetJSON(path string, query url.Values, out any) error {
	target := e.base
	target.Path = strings.TrimRight(target.Path, "/") + "/" + strings.TrimLeft(path, "/")
	target.RawPath = ""
	target.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("service HTTP status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
