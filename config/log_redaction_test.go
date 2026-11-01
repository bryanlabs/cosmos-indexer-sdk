package config

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEndpointLogsRedactCredentials(t *testing.T) {
	ProtectEndpointLogs("https://archive.example/private-test-key?api_key=query-test-secret")
	ProtectEndpointLogs("https://user:basic-test-secret@archive.example")
	var output bytes.Buffer
	input := []byte("Post https://archive.example/private-test-key/block_results?api_key=query-test-secret: timeout, basic-test-secret")
	n, err := (redactingLogWriter{Writer: &output}).Write(input)
	require.NoError(t, err)
	require.Equal(t, len(input), n)
	require.NotContains(t, output.String(), "private-test-key")
	require.NotContains(t, output.String(), "query-test-secret")
	require.NotContains(t, output.String(), "basic-test-secret")
	require.Contains(t, output.String(), "timeout")
}
