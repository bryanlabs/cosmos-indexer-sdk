package taxapi

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// captureCommissionFlowPayload retains exact pre-save bytes outside the result
// transaction. Incomplete attempts remain private and never get a manifest.
// Empty root preserves the prior writer behavior. A configured capture fails
// closed before any database write if durable evidence cannot be retained.
func captureCommissionFlowPayload(root string, snapshot CommissionFlowVersion) error {
	if root == "" {
		return nil
	}
	info, err := os.Lstat(root)
	if err != nil || !filepath.IsAbs(root) || !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("private commission capture requires an existing absolute mode-0700 directory")
	}
	dir, err := os.MkdirTemp(root, "attempt-")
	if err != nil {
		return fmt.Errorf("create private commission capture: %w", err)
	}
	manifest := struct {
		Version   string
		Chain     string
		Validator string
		Build     string
		Fields    []CommissionFlowPayload
	}{Version: snapshot.Version, Chain: snapshot.Chain, Validator: snapshot.Validator, Build: snapshot.Build}
	for _, field := range []string{"run", "rows", "evidence"} {
		value, _ := payloadField(&snapshot, field)
		file, err := os.OpenFile(filepath.Join(dir, field+".json.gz"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fmt.Errorf("open private commission capture: %w", err)
		}
		rawHash, encodedHash := sha256.New(), sha256.New()
		sink := &captureCounter{out: io.MultiWriter(file, encodedHash)}
		compressor, err := gzip.NewWriterLevel(sink, gzip.BestSpeed)
		if err != nil {
			_ = file.Close()
			return err
		}
		rawBytes, copyErr := io.Copy(compressor, io.TeeReader(strings.NewReader(*value), rawHash))
		err = errors.Join(copyErr, compressor.Close(), file.Sync(), file.Close())
		if err != nil {
			return fmt.Errorf("retain private commission capture: %w", err)
		}
		manifest.Fields = append(manifest.Fields, CommissionFlowPayload{Version: snapshot.Version, Chain: snapshot.Chain, Validator: snapshot.Validator, Build: snapshot.Build, Field: field, Codec: commissionPayloadCodec, RawSHA256: hex.EncodeToString(rawHash.Sum(nil)), RawBytes: rawBytes, EncodedSHA256: hex.EncodeToString(encodedHash.Sum(nil)), EncodedBytes: sink.bytes})
	}
	file, err := os.OpenFile(filepath.Join(dir, "manifest.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = errors.Join(json.NewEncoder(file).Encode(manifest), file.Sync(), file.Close())
	if err != nil {
		return err
	}
	for _, path := range []string{dir, root} {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			return err
		}
	}
	return nil
}

type captureCounter struct {
	out   io.Writer
	bytes int64
}

func (c *captureCounter) Write(p []byte) (int, error) {
	n, err := c.out.Write(p)
	c.bytes += int64(n)
	return n, err
}
