package main

import (
	"compress/gzip"
	"fmt"
	"os"
)

// Optional framing changes storage bytes only. Decompression returns the exact
// original record bytes, including order, types, empty values and aborted rows.
// Every committed height flushes gzip before syncing the underlying private file.
// A truncated stream or missing completion marker remains inadmissible.
type evidenceWriter struct {
	file   *os.File
	gzip   *gzip.Writer
	closed bool
}

func createRecordEvidence(path string, compressed bool) (*evidenceWriter, error) {
	f, err := createPrivateEvidence(path)
	if err != nil {
		return nil, err
	}
	w := &evidenceWriter{file: f}
	if compressed {
		w.gzip, err = gzip.NewWriterLevel(f, gzip.BestSpeed)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("private compression setup failed")
		}
	}
	return w, nil
}
func (w *evidenceWriter) Write(b []byte) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("private evidence already closed")
	}
	if w.gzip != nil {
		return w.gzip.Write(b)
	}
	return w.file.Write(b)
}
func (w *evidenceWriter) Sync() error {
	if w.closed {
		return fmt.Errorf("private evidence already closed")
	}
	if w.gzip != nil {
		if err := w.gzip.Flush(); err != nil {
			return err
		}
	}
	return w.file.Sync()
}
func (w *evidenceWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	var first error
	if w.gzip != nil {
		first = w.gzip.Close()
	}
	if err := w.file.Sync(); first == nil {
		first = err
	}
	if err := w.file.Close(); first == nil {
		first = err
	}
	return first
}
