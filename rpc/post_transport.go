package rpc

import (
	"fmt"
	"net/http"
)

// NoReplayPOSTTransport permits connection reuse without replaying request
// bodies. It is only for explicitly selected JSON-RPC POST archive gateways.
// URI GET clients retain fresh-connection behavior instead.
type NoReplayPOSTTransport struct {
	Transport http.RoundTripper
}

func (t NoReplayPOSTTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodPost || request.Body == nil || request.Body == http.NoBody {
		return nil, fmt.Errorf("archive JSON-RPC transport requires a POST body")
	}
	cloned := request.Clone(request.Context())
	cloned.GetBody = nil
	cloned.Header.Del("Idempotency-Key")
	cloned.Header.Del("X-Idempotency-Key")
	return t.Transport.RoundTrip(cloned)
}
