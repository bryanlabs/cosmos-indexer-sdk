package taxapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestArchiveCallLocalReceiptsDoNotIncludeConcurrentFailure(t *testing.T) {
	started := make(chan struct{})
	failed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/good" {
			close(started)
			<-failed
			_, _ = w.Write([]byte(`{"result":{"ok":true}}`))
			return
		}
		<-started
		_, _ = w.Write([]byte(`{"error":{"code":-1,"message":"height unavailable"}}`))
		close(failed)
	}))
	defer server.Close()
	a, err := newFlowArchive(server.URL)
	require.NoError(t, err)
	var good, bad []FlowArchiveReceipt
	var goodErr, badErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var body any
		goodErr = a.response(flowRowsHolder, 123, "good", url.Values{}, &body, &good)
	}()
	go func() {
		defer wg.Done()
		var body any
		badErr = a.response(flowRowsHolder, 123, "bad", url.Values{}, &body, &bad)
	}()
	wg.Wait()
	require.NoError(t, goodErr)
	require.Error(t, badErr)
	require.Len(t, good, 1)
	require.Len(t, bad, 1)
	require.NotEqual(t, good[0].SHA256, bad[0].SHA256)
	require.Len(t, a.evidence(nil), 2, "all attempts including failures remain retained")
	require.Equal(t, "good", good[0].Method)
}
