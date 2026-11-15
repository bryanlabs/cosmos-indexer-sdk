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

	"gorm.io/gorm"
)

type commissionFlowSpool struct {
	dir       string
	temporary bool
	fields    []CommissionFlowPayload
}

func (s *commissionFlowSpool) close() error {
	if s != nil && s.temporary {
		return os.RemoveAll(s.dir)
	}
	return nil
}

// The graph already exists in memory. Packaging adds bounded encoder/gzip
// buffers and disk spools, not another whole graph's worth of JSON buffers.
// Configured captures retain failed attempts; only a completely fsynced set
// receives a manifest. No SQL write occurs until spooling succeeds.
func spoolCommissionFlow(root string, snapshot CommissionFlowVersion, run CommissionFlowRun, rows []CommissionFlow, evidence CommissionFlowEvidence) (spool *commissionFlowSpool, err error) {
	if root != "" {
		info, e := os.Lstat(root)
		if e != nil || !filepath.IsAbs(root) || !info.IsDir() || info.Mode().Perm() != 0700 {
			return nil, errors.New("private commission spool requires absolute mode-0700 root")
		}
	}
	dir, err := os.MkdirTemp(root, "attempt-")
	if err != nil {
		return nil, errors.New("private commission spool creation failed")
	}
	spool = &commissionFlowSpool{dir: dir, temporary: root == ""}
	defer func() {
		if err != nil {
			spool.close()
		}
	}()
	values := []any{run, rows, evidence}
	for i, field := range []string{"run", "rows", "evidence"} {
		meta := CommissionFlowPayload{Version: snapshot.Version, Chain: snapshot.Chain, Validator: snapshot.Validator, Build: snapshot.Build, Field: field, Codec: commissionPayloadCodec, ChunkBytes: commissionPayloadChunkSize}
		file, e := os.OpenFile(filepath.Join(dir, field+".json.gz"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return spool, errors.New("private commission spool open failed")
		}
		rawHash, encodedHash := sha256.New(), sha256.New()
		encoded := &captureCounter{out: io.MultiWriter(file, encodedHash)}
		compressor, e := gzip.NewWriterLevel(encoded, gzip.BestSpeed)
		if e != nil {
			file.Close()
			return spool, e
		}
		raw := &captureCounter{out: io.MultiWriter(compressor, rawHash)}
		writeErr := streamCommissionJSON(raw, values[i])
		if e := errors.Join(writeErr, compressor.Close(), file.Sync(), file.Close()); e != nil {
			return spool, errors.New("private commission spool serialization or durable write failed")
		}
		meta.RawBytes, meta.EncodedBytes = raw.bytes, encoded.bytes
		meta.RawSHA256, meta.EncodedSHA256 = hex.EncodeToString(rawHash.Sum(nil)), hex.EncodeToString(encodedHash.Sum(nil))
		meta.ChunkCount = int((meta.EncodedBytes + commissionPayloadChunkSize - 1) / commissionPayloadChunkSize)
		spool.fields = append(spool.fields, meta)
	}
	manifest := struct {
		Version, Chain, Validator, Build string
		Fields                           []CommissionFlowPayload
	}{snapshot.Version, snapshot.Chain, snapshot.Validator, snapshot.Build, spool.fields}
	file, e := os.OpenFile(filepath.Join(dir, "manifest.json.pending"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return spool, e
	}
	if e := errors.Join(json.NewEncoder(file).Encode(manifest), file.Sync(), file.Close()); e != nil {
		return spool, e
	}
	if e := syncCheckpointDir(dir); e != nil {
		return spool, e
	}
	if e := os.Link(filepath.Join(dir, "manifest.json.pending"), filepath.Join(dir, "manifest.json")); e != nil {
		return spool, e
	}
	if e := syncCheckpointDir(dir); e != nil {
		return spool, e
	}
	if e := os.Remove(filepath.Join(dir, "manifest.json.pending")); e != nil {
		return spool, e
	}
	if e := syncCheckpointDir(dir); e != nil {
		return spool, e
	}
	if root != "" {
		if e := syncCheckpointDir(root); e != nil {
			return spool, e
		}
	}
	return spool, nil
}

func openCommissionSpool(s *commissionFlowSpool, m CommissionFlowPayload) (*os.File, error) {
	path := filepath.Join(s.dir, m.Field+".json.gz")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != m.EncodedBytes {
		return nil, errors.New("private commission spool invalid or incomplete")
	}
	return os.Open(path)
}

// Verify streamed bytes again before they enter SQL. A damaged spool cannot
// produce a complete metadata row or result marker. At most one SQL chunk and
// one inline value are resident; a late failure rolls back the result transaction.
func storeCommissionSpools(tx *gorm.DB, snapshot *CommissionFlowVersion, spool *commissionFlowSpool) error {
	for _, m := range spool.fields {
		file, err := openCommissionSpool(spool, m)
		if err != nil {
			return err
		}
		rawHash := sha256.New()
		gz, err := gzip.NewReader(file)
		if err != nil {
			file.Close()
			return errors.New("private commission spool decompression failed")
		}
		var inline []byte
		var n int64
		if m.RawBytes <= commissionPayloadInlineLimit {
			inline, err = io.ReadAll(io.LimitReader(io.TeeReader(gz, rawHash), commissionPayloadInlineLimit+1))
			n = int64(len(inline))
		} else {
			n, err = io.Copy(rawHash, gz)
		}
		err = errors.Join(err, gz.Close(), file.Close())
		if err != nil || n != m.RawBytes || hex.EncodeToString(rawHash.Sum(nil)) != m.RawSHA256 {
			return errors.New("private commission spool raw integrity failure")
		}
		file, err = openCommissionSpool(spool, m)
		if err != nil {
			return err
		}
		encodedHash := sha256.New()
		reader := io.TeeReader(file, encodedHash)
		buffer := make([]byte, commissionPayloadChunkSize)
		ordinal := 0
		var encodedBytes int64
		for {
			size, readErr := io.ReadFull(reader, buffer)
			if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
				file.Close()
				return errors.New("private commission spool chunk read failed")
			}
			if size > 0 {
				if m.RawBytes > commissionPayloadInlineLimit {
					chunk := CommissionFlowPayloadChunk{Version: m.Version, Chain: m.Chain, Validator: m.Validator, Field: m.Field, Ordinal: ordinal, SHA256: payloadHash(buffer[:size]), Data: buffer[:size]}
					if err := tx.Create(&chunk).Error; err != nil {
						file.Close()
						return err
					}
				}
				encodedBytes += int64(size)
				ordinal++
			}
			if readErr != nil {
				break
			}
		}
		closeErr := file.Close()
		if closeErr != nil || encodedBytes != m.EncodedBytes || ordinal != m.ChunkCount || hex.EncodeToString(encodedHash.Sum(nil)) != m.EncodedSHA256 {
			return errors.New("private commission spool encoded integrity failure")
		}
		value, err := payloadField(snapshot, m.Field)
		if err != nil {
			return err
		}
		if m.RawBytes <= commissionPayloadInlineLimit {
			*value = string(inline)
		} else {
			if err := tx.Create(&m).Error; err != nil {
				return err
			}
			*value = payloadMarker(m)
		}
	}
	if len(spool.fields) != 3 {
		return fmt.Errorf("private commission spool field set incomplete")
	}
	return nil
}
