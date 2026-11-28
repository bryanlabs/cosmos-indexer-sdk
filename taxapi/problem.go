package taxapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// Every refusal the API sends is an RFC 9457 problem (application/problem+json),
// the same envelope the website's /api/tax/v1 routes send. Handlers keep
// answering with http.Error; withProblems turns a plain-text error body into a
// problem on the way out. An error body that is already a JSON object (an
// asset-review refusal with its pending count, for example) passes unchanged.

const (
	problemContentType = "application/problem+json"
	problemTypeBase    = "https://bryanlabs.net/api-docs/conventions#"
)

var problemTitles = map[int]string{
	400: "Bad Request",
	401: "Unauthorized",
	403: "Forbidden",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	413: "Content Too Large",
	422: "Unprocessable Content",
	429: "Too Many Requests",
	500: "Internal Server Error",
	502: "Bad Gateway",
	503: "Service Unavailable",
	504: "Gateway Timeout",
}

// problemCodes match the website's shared codes (lib/api/problem.ts), so a
// refusal keeps its code when the website passes it on.
var problemCodes = map[int]string{
	400: "bad_request",
	401: "unauthorized",
	403: "forbidden",
	404: "not_found",
	405: "method_not_allowed",
	409: "conflict",
	413: "payload_too_large",
	422: "validation_failed",
	429: "rate_limited",
	500: "internal_error",
	502: "upstream_error",
	503: "unavailable",
	504: "upstream_timeout",
}

// Problem is the error body.
type Problem struct {
	Type       string `json:"type"`
	Title      string `json:"title"`
	Status     int    `json:"status"`
	Detail     string `json:"detail"`
	Code       string `json:"code"`
	Instance   string `json:"instance,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	RetryAfter *int   `json:"retry_after,omitempty"`
}

func newProblem(status int, detail string, r *http.Request, retryAfter string) Problem {
	code := problemCodes[status]
	if code == "" {
		code = "request_failed"
		if status >= 500 {
			code = "internal_error"
		}
	}
	title := problemTitles[status]
	if title == "" {
		title = http.StatusText(status)
	}
	if detail == "" {
		detail = title
	}
	p := Problem{Type: problemTypeBase + code, Title: title, Status: status, Detail: detail, Code: code}
	if r != nil {
		p.Instance = r.URL.Path
		p.RequestID = strings.TrimSpace(r.Header.Get("X-Request-Id"))
	}
	if n, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && n >= 0 {
		p.RetryAfter = &n
	}
	return p
}

// withProblems wraps the API so every plain-text error leaves as a problem.
func withProblems(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pw := &problemWriter{w: w, r: r}
		h.ServeHTTP(pw, r)
		pw.finish()
	})
}

// problemWriter holds back the body of an error answer whose content type is
// plain text or not set yet, so finish can send it as a problem. Every other
// answer streams through untouched.
type problemWriter struct {
	w         http.ResponseWriter
	r         *http.Request
	status    int
	wrote     bool
	buffering bool
	buf       bytes.Buffer
}

func (p *problemWriter) Header() http.Header { return p.w.Header() }

// Unwrap lets http.ResponseController reach the connection's writer.
func (p *problemWriter) Unwrap() http.ResponseWriter { return p.w }

func (p *problemWriter) WriteHeader(status int) {
	if p.wrote {
		return
	}
	p.wrote = true
	p.status = status
	ct := strings.ToLower(p.w.Header().Get("Content-Type"))
	if status >= 400 && (ct == "" || strings.HasPrefix(ct, "text/plain")) {
		p.buffering = true
		return
	}
	p.w.WriteHeader(status)
}

func (p *problemWriter) Write(b []byte) (int, error) {
	if !p.wrote {
		p.WriteHeader(http.StatusOK)
	}
	if p.buffering {
		return p.buf.Write(b)
	}
	return p.w.Write(b)
}

func (p *problemWriter) Flush() {
	if p.buffering {
		return
	}
	if f, ok := p.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (p *problemWriter) finish() {
	if !p.buffering {
		return
	}
	h := p.w.Header()
	body := bytes.TrimSpace(p.buf.Bytes())
	if len(body) > 0 && body[0] == '{' && json.Valid(body) {
		if h.Get("Content-Type") == "" {
			h.Set("Content-Type", apiJSONContentType)
		}
		p.w.WriteHeader(p.status)
		_, _ = p.w.Write(p.buf.Bytes())
		return
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(newProblem(p.status, string(body), p.r, h.Get("Retry-After"))); err != nil {
		p.w.WriteHeader(p.status)
		_, _ = p.w.Write(p.buf.Bytes())
		return
	}
	h.Del("Content-Length")
	h.Set("Content-Type", problemContentType)
	h.Set("Cache-Control", "no-store")
	p.w.WriteHeader(p.status)
	_, _ = p.w.Write(out.Bytes())
}
