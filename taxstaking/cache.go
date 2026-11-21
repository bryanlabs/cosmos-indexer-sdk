package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// cacheSource reads raw block results the extraction cached as
// <root>/<h/10000>/<h>-block_results.raw.gz (proof runs over heights our
// node no longer holds).
type cacheSource struct{ root string }

func (c cacheSource) blockResults(_ context.Context, h int64) ([]byte, error) {
	p := filepath.Join(c.root, fmt.Sprint(h/10000), fmt.Sprintf("%d-block_results.raw.gz", h))
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errMissing
	}
	if err != nil {
		return nil, err
	}
	z, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(z)
}
