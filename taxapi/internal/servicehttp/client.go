// Package servicehttp binds JSON reads to an operator-configured service origin.
package servicehttp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// StatusError is a response other than 200. Message is the service's own
// JSON error message, when it sent one; the error text leaves it out, since
// it can echo request values.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string { return fmt.Sprintf("service HTTP status %d", e.Code) }

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
	return e.GetJSONWithHeaders(path, query, nil, out)
}

// GetJSONWithHeaders is GetJSON with request headers, such as the block height
// a Cosmos REST query is answered at.
func (e *Endpoint) GetJSONWithHeaders(path string, query url.Values, headers map[string]string, out any) error {
	target := e.base
	target.Path = strings.TrimRight(target.Path, "/") + "/" + strings.TrimLeft(path, "/")
	target.RawPath = ""
	target.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var body struct {
			Message string `json:"message"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = json.Unmarshal(raw, &body)
		return &StatusError{Code: resp.StatusCode, Message: body.Message}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
