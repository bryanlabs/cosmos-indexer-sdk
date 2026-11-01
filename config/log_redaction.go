package config

import (
	"io"
	"net/url"
	"strings"
	"sync"
)

var logSecretValues struct {
	sync.RWMutex
	values []string
}

// ProtectEndpointLogs prevents URL credentials from leaking through HTTP errors.
func ProtectEndpointLogs(endpoint string) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return
	}
	values := []string{endpoint}
	if path := strings.Trim(parsed.Path, "/"); path != "" {
		values = append(values, path)
	}
	if parsed.User != nil {
		if password, ok := parsed.User.Password(); ok && password != "" {
			values = append(values, password)
		}
	}
	for _, items := range parsed.Query() {
		for _, value := range items {
			if value != "" {
				values = append(values, value)
			}
		}
	}
	logSecretValues.Lock()
	defer logSecretValues.Unlock()
	logSecretValues.values = append(logSecretValues.values, values...)
}

type redactingLogWriter struct {
	io.Writer
}

func (w redactingLogWriter) Write(input []byte) (int, error) {
	text := string(input)
	logSecretValues.RLock()
	for _, value := range logSecretValues.values {
		text = strings.ReplaceAll(text, value, "[REDACTED]")
	}
	logSecretValues.RUnlock()
	if _, err := io.WriteString(w.Writer, text); err != nil {
		return 0, err
	}
	return len(input), nil
}
