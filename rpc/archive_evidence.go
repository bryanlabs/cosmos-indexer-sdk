package rpc

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

const maxArchiveResponseBytes = 64 << 20

type archiveCall struct {
	method string
	height int64
}

func archiveRequestCall(request *http.Request) (archiveCall, error) {
	var call archiveCall
	if request.Method == http.MethodPost && request.GetBody != nil {
		body, err := request.GetBody()
		if err != nil {
			return call, fmt.Errorf("cannot inspect archive request")
		}
		defer body.Close()
		var envelope struct {
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(io.LimitReader(body, 65536)).Decode(&envelope); err != nil {
			return call, fmt.Errorf("invalid archive JSON-RPC request")
		}
		call.method = envelope.Method
		if raw := envelope.Params["height"]; len(raw) != 0 {
			call.height, err = archiveHeight(raw)
			if err != nil {
				return call, err
			}
		}
	} else if request.Method == http.MethodGet {
		call.method = path.Base(request.URL.Path)
		if raw := request.URL.Query().Get("height"); raw != "" {
			var err error
			call.height, err = strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return call, fmt.Errorf("invalid archive request height")
			}
		}
	}
	return call, nil
}

func archiveHeight(raw json.RawMessage) (int64, error) {
	value, err := strconv.ParseInt(strings.Trim(string(raw), `"`), 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid archive response or request height")
	}
	return value, nil
}

// inspectResponse checks RPC-level failures as well as HTTP denials. Captured
// responses contain source data only, never the credential-bearing request URL.
func (g *ArchiveGuard) inspectResponse(request *http.Request, response *http.Response, sequence uint64) error {
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		response.Body.Close()
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	call, err := archiveRequestCall(request)
	if err != nil {
		response.Body.Close()
		return err
	}
	if call.method != "status" && call.method != "block" && call.method != "block_results" {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxArchiveResponseBytes+1))
	response.Body.Close()
	if err != nil || len(body) > maxArchiveResponseBytes {
		return fmt.Errorf("archive response unreadable or exceeds size limit")
	}
	var envelope struct {
		Error  json.RawMessage `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("invalid archive JSON-RPC response")
	}
	if len(envelope.Error) != 0 && string(envelope.Error) != "null" {
		var failure struct{ Code int }
		if err := json.Unmarshal(envelope.Error, &failure); err != nil {
			return fmt.Errorf("archive JSON-RPC error")
		}
		return fmt.Errorf("archive JSON-RPC error %d", failure.Code)
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return fmt.Errorf("archive response has no result")
	}
	var result struct {
		Height json.RawMessage `json:"height"`
		Block  struct {
			Header struct {
				Height json.RawMessage `json:"height"`
			} `json:"header"`
		} `json:"block"`
	}
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		return fmt.Errorf("invalid archive result envelope")
	}
	var filename string
	if call.method == "status" {
		filename = fmt.Sprintf("%020d-status.raw.gz", sequence)
	} else {
		rawHeight := result.Height
		if call.method == "block" {
			rawHeight = result.Block.Header.Height
		}
		height, err := archiveHeight(rawHeight)
		if err != nil || height == 0 || (call.height > 0 && call.height != height) {
			return fmt.Errorf("archive response height mismatch")
		}
		filename = fmt.Sprintf("%d-%s.raw.gz", height, call.method)
	}
	if g.EvidenceDir != "" {
		if err := writeArchiveEvidence(g.EvidenceDir, filename, body); err != nil {
			return fmt.Errorf("archive evidence persistence failed: %w", err)
		}
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return nil
}

func writeArchiveEvidence(directory, filename string, body []byte) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".archive-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	writer := gzip.NewWriter(file)
	_, writeErr := writer.Write(body)
	closeErr := writer.Close()
	syncErr := file.Sync()
	fileErr := file.Close()
	for _, err := range []error{writeErr, closeErr, syncErr, fileErr} {
		if err != nil {
			return err
		}
	}
	// Link publishes atomically and refuses to overwrite a previous response.
	if err := os.Link(file.Name(), filepath.Join(directory, filename)); err != nil {
		return err
	}
	for _, name := range []string{directory, filepath.Dir(directory)} {
		dir, err := os.Open(name)
		if err != nil {
			return err
		}
		syncErr := dir.Sync()
		closeErr := dir.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
