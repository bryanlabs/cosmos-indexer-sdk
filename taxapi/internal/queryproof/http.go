package queryproof

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

type HTTPRequest struct {
	Endpoint, Method, Path, Query string
	Height, ContentType           string
	Body                          []byte
}
type HTTPResponse struct {
	Status int
	Header http.Header
	Body   []byte
	Error  string
}

// Transport accepts only registered endpoint origins. Offline mode never
// invokes Live, even if an endpoint or request is absent from the bundle.
type Transport struct {
	Tape    *Tape
	Origins map[string]string
	Live    http.RoundTripper
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	var endpoint, path string
	origins := make([]string, 0, len(t.Origins))
	for origin := range t.Origins {
		origins = append(origins, origin)
	}
	sort.Slice(origins, func(i, j int) bool {
		if len(origins[i]) == len(origins[j]) {
			return origins[i] < origins[j]
		}
		return len(origins[i]) > len(origins[j])
	})
	for _, origin := range origins {
		name := t.Origins[origin]
		u, e := url.Parse(origin)
		if e != nil {
			continue
		}
		if !t.Tape.replay && t.Tape.Source().Kind == "scratch" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1" {
			return nil, t.Tape.Refuse(errors.New("scratch HTTP capture requires loopback fixture origin"))
		}
		base := strings.TrimRight(u.Path, "/")
		if req.URL.Scheme == u.Scheme && req.URL.Host == u.Host && (req.URL.Path == base || strings.HasPrefix(req.URL.Path, base+"/")) {
			endpoint = name
			path = strings.TrimPrefix(req.URL.Path, base)
			if path == "" {
				path = "/"
			}
			break
		}
	}
	if endpoint == "" {
		return nil, t.Tape.Refuse(errors.New("proof HTTP origin is not registered"))
	}
	var body []byte
	var err error
	if req.Body != nil {
		body, err = io.ReadAll(io.LimitReader(req.Body, MaxResponseBytes+1))
		_ = req.Body.Close()
		if err != nil || len(body) > MaxResponseBytes {
			return nil, t.Tape.Refuse(errors.New("proof HTTP request bound exceeded"))
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	request := HTTPRequest{Endpoint: endpoint, Method: req.Method, Path: path, Query: req.URL.Query().Encode(), Height: req.Header.Get("x-cosmos-block-height"), ContentType: req.Header.Get("Content-Type"), Body: body}
	var response HTTPResponse
	if t.Tape.replay {
		b, e := t.Tape.Take("http", encoded(request))
		if e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &response); e != nil {
			return nil, t.Tape.Refuse(e)
		}
	} else {
		if t.Live == nil {
			return nil, t.Tape.Refuse(errors.New("capture HTTP transport missing"))
		}
		live, e := t.Live.RoundTrip(req)
		if e != nil {
			response.Error = "captured endpoint request failed"
		} else {
			response.Status = live.StatusCode
			response.Header = live.Header.Clone()
			response.Header.Del("Set-Cookie")
			response.Body, e = io.ReadAll(io.LimitReader(live.Body, MaxResponseBytes+1))
			closeErr := live.Body.Close()
			if e != nil || closeErr != nil || len(response.Body) > MaxResponseBytes {
				return nil, t.Tape.Refuse(errors.New("proof HTTP response bound/read failure"))
			}
		}
		if e = t.Tape.Record("http", encoded(request), encoded(response)); e != nil {
			return nil, e
		}
	}
	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	return &http.Response{StatusCode: response.Status, Status: http.StatusText(response.Status), Header: response.Header, Body: io.NopCloser(bytes.NewReader(response.Body)), ContentLength: int64(len(response.Body)), Request: req}, nil
}
