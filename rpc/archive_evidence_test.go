package rpc

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func archiveTestRequest(t *testing.T, client *http.Client, endpoint string) (*http.Response, error) {
	t.Helper()
	return client.Post(endpoint, "application/json", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"block","params":{"height":"7"}}`))
}

func TestArchiveEvidencePreservesResponseNotCredentialURL(t *testing.T) {
	for _, concurrent := range []int{1, 2} {
		t.Run(fmt.Sprint(concurrent), func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":1,"result":{"block":{"header":{"height":"7"},"data":{"txs":[]}}}}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			defer server.Close()
			directory := t.TempDir()
			guard := &ArchiveGuard{Interval: time.Millisecond, Budget: 3, MaxConcurrent: concurrent, EvidenceDir: directory}
			client := &http.Client{Transport: guard}
			response, err := archiveTestRequest(t, client, server.URL+"/credential-bearing-path")
			require.NoError(t, err)
			observed, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, body, string(observed))
			filename := filepath.Join(directory, "7-block.raw.gz")
			file, err := os.Open(filename)
			require.NoError(t, err)
			defer file.Close()
			reader, err := gzip.NewReader(file)
			require.NoError(t, err)
			captured, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.NoError(t, reader.Close())
			require.Equal(t, body, string(captured))
			require.NotContains(t, string(captured), "credential-bearing-path")
			stat, err := file.Stat()
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o600), stat.Mode().Perm())
			_, err = archiveTestRequest(t, client, server.URL)
			require.ErrorContains(t, err, "evidence persistence failed", "a fresh directory is required for each run")
		})
	}
}

func TestArchiveRPCFailureIsStickyAndRedacted(t *testing.T) {
	for _, concurrency := range []int{1, 2} {
		for _, status := range []int{200, 401, 403, 429, 502} {
			t.Run(fmt.Sprintf("%d/%d", concurrency, status), func(t *testing.T) {
				var calls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"denied /credential-bearing-path"}}`)
				}))
				defer server.Close()
				guard := &ArchiveGuard{Interval: time.Millisecond, Budget: 5, MaxConcurrent: concurrency}
				client := &http.Client{Transport: guard}
				for i := 0; i < 2; i++ {
					_, err := archiveTestRequest(t, client, server.URL)
					require.ErrorContains(t, err, "circuit open")
					require.NotContains(t, err.Error(), "credential-bearing-path")
				}
				require.Equal(t, int64(1), calls.Load())
			})
		}
	}
}

func TestArchiveWrongHeightAndMissingResultFailClosed(t *testing.T) {
	for _, body := range []string{
		`{"result":{"block":{"header":{"height":"8"}}}}`,
		`{"result":{"block":{"header":{"height":"-1"}}}}`,
		`{"result":null}`,
		`{broken`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			defer server.Close()
			guard := &ArchiveGuard{Budget: 2, Interval: time.Millisecond, EvidenceDir: t.TempDir()}
			_, err := archiveTestRequest(t, &http.Client{Transport: guard}, server.URL)
			require.Error(t, err)
			require.True(t, strings.Contains(err.Error(), "circuit open"))
			files, err := os.ReadDir(guard.EvidenceDir)
			require.NoError(t, err)
			require.Empty(t, files)
		})
	}
}
