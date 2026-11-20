package taxapi

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"

	"gorm.io/gorm"
)

const (
	commissionPayloadInlineLimit = 1 << 20
	commissionPayloadChunkSize   = 1 << 20
	commissionPayloadCodec       = "gzip-json-bytes-v1"
	commissionPayloadMarker      = "cosmos-commission-payload/v1:"
)

// CommissionFlowPayload is an immutable, complete index of one private JSON
// value. Original JSON bytes, not JSONB's normalized representation, are hashed.
// Its chunks and the owning result are committed in the same transaction.
type CommissionFlowPayload struct {
	Version       string `gorm:"primaryKey"`
	Chain         string `gorm:"primaryKey"`
	Validator     string `gorm:"primaryKey"`
	Field         string `gorm:"primaryKey"`
	Build         string `gorm:"not null"`
	Codec         string `gorm:"not null"`
	RawSHA256     string `gorm:"not null"`
	RawBytes      int64  `gorm:"not null"`
	EncodedSHA256 string `gorm:"not null"`
	EncodedBytes  int64  `gorm:"not null"`
	ChunkCount    int    `gorm:"not null"`
	ChunkBytes    int    `gorm:"not null"`
}

func (CommissionFlowPayload) TableName() string { return "tax_commission_flow_payloads" }

// CommissionFlowPayloadChunk uses bytea, avoiding JSONB's per-container JEntry
// size limit. An INSERT conflict is an error, never an update or replacement.
type CommissionFlowPayloadChunk struct {
	Version   string `gorm:"primaryKey"`
	Chain     string `gorm:"primaryKey"`
	Validator string `gorm:"primaryKey"`
	Field     string `gorm:"primaryKey"`
	Ordinal   int    `gorm:"primaryKey"`
	SHA256    string `gorm:"not null"`
	Data      []byte `gorm:"type:bytea;not null"`
}

func (CommissionFlowPayloadChunk) TableName() string { return "tax_commission_flow_payload_chunks" }

func payloadHash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func payloadField(snapshot *CommissionFlowVersion, field string) (*string, error) {
	switch field {
	case "run":
		return &snapshot.RunJSON, nil
	case "rows":
		return &snapshot.RowsJSON, nil
	case "evidence":
		return &snapshot.EvidenceJSON, nil
	default:
		return nil, fmt.Errorf("unknown private payload field")
	}
}

func payloadMarker(m CommissionFlowPayload) string {
	// JSON string, deliberately NOT an object that an old struct decoder might
	// silently accept as an empty financial evidence record.
	b, _ := json.Marshal(commissionPayloadMarker + m.Field + ":" + m.RawSHA256)
	return string(b)
}

func isPayloadMarker(raw string) bool {
	var value string
	return json.Unmarshal([]byte(raw), &value) == nil && strings.HasPrefix(value, commissionPayloadMarker)
}

type payloadChunkWriter struct {
	pending []byte
	hash    hash.Hash
	bytes   int64
	count   int
	save    func(int, []byte) error
}

func (w *payloadChunkWriter) flush() error {
	if len(w.pending) == 0 {
		return nil
	}
	if err := w.save(w.count, w.pending); err != nil {
		return err
	}
	w.count++
	w.pending = make([]byte, 0, commissionPayloadChunkSize)
	return nil
}

func (w *payloadChunkWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		part := min(commissionPayloadChunkSize-len(w.pending), len(p))
		w.pending = append(w.pending, p[:part]...)
		_, _ = w.hash.Write(p[:part])
		w.bytes += int64(part)
		n += part
		p = p[part:]
		if len(w.pending) == commissionPayloadChunkSize {
			if err := w.flush(); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

func encodeCommissionPayload(raw []byte, m *CommissionFlowPayload, save func(int, []byte) error) error {
	if !json.Valid(raw) {
		return fmt.Errorf("invalid private JSON payload")
	}
	m.Codec, m.RawSHA256, m.RawBytes = commissionPayloadCodec, payloadHash(raw), int64(len(raw))
	m.ChunkBytes = commissionPayloadChunkSize
	chunks := &payloadChunkWriter{hash: sha256.New(), save: save}
	compressor, err := gzip.NewWriterLevel(chunks, gzip.BestSpeed)
	if err != nil {
		return err
	}
	if _, err := compressor.Write(raw); err != nil {
		_ = compressor.Close()
		return err
	}
	if err := compressor.Close(); err != nil {
		return err
	}
	if err := chunks.flush(); err != nil {
		return err
	}
	m.EncodedSHA256, m.EncodedBytes, m.ChunkCount = hex.EncodeToString(chunks.hash.Sum(nil)), chunks.bytes, chunks.count
	return nil
}

// storeCommissionFlowPayloads must run inside the result transaction. No marker
// or complete manifest can become visible independently from all its chunks.
func storeCommissionFlowPayloads(tx *gorm.DB, snapshot *CommissionFlowVersion) error {
	for _, field := range []string{"run", "rows", "evidence"} {
		value, _ := payloadField(snapshot, field)
		if len(*value) <= commissionPayloadInlineLimit {
			continue
		}
		m := CommissionFlowPayload{Version: snapshot.Version, Chain: snapshot.Chain, Validator: snapshot.Validator, Build: snapshot.Build, Field: field}
		if err := encodeCommissionPayload([]byte(*value), &m, func(ordinal int, data []byte) error {
			chunk := CommissionFlowPayloadChunk{Version: m.Version, Chain: m.Chain, Validator: m.Validator, Field: field, Ordinal: ordinal, SHA256: payloadHash(data), Data: data}
			return tx.Create(&chunk).Error
		}); err != nil {
			return err
		}
		if err := tx.Create(&m).Error; err != nil {
			return err
		}
		*value = payloadMarker(m)
	}
	return nil
}

// payloadChunkReader holds at most one compressed chunk at a time. ReadByte
// allows gzip's single-member decoder to avoid reading past the stream end.
type payloadChunkReader struct {
	rows    *sql.Rows
	m       CommissionFlowPayload
	pending []byte
	index   int
	hash    hash.Hash
	bytes   int64
}

func (r *payloadChunkReader) next() error {
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		if r.index != r.m.ChunkCount || r.bytes != r.m.EncodedBytes || hex.EncodeToString(r.hash.Sum(nil)) != r.m.EncodedSHA256 {
			return fmt.Errorf("incomplete or corrupt private payload chunks")
		}
		return io.EOF
	}
	var ordinal int
	var data []byte
	var checksum string
	if err := r.rows.Scan(&ordinal, &checksum, &data); err != nil {
		return err
	}
	if ordinal != r.index || ordinal >= r.m.ChunkCount || len(data) == 0 || len(data) > r.m.ChunkBytes || (ordinal < r.m.ChunkCount-1 && len(data) != r.m.ChunkBytes) || payloadHash(data) != checksum {
		return fmt.Errorf("invalid private payload chunk sequence or checksum")
	}
	r.index++
	r.bytes += int64(len(data))
	if r.bytes > r.m.EncodedBytes {
		return fmt.Errorf("private payload exceeds complete index")
	}
	_, _ = r.hash.Write(data)
	r.pending = data
	return nil
}

func (r *payloadChunkReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 {
		if err := r.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *payloadChunkReader) ReadByte() (byte, error) {
	var b [1]byte
	_, err := r.Read(b[:])
	return b[0], err
}

type payloadRawWriter struct {
	out   io.Writer
	hash  hash.Hash
	bytes int64
	limit int64
}

func (w *payloadRawWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.limit-w.bytes {
		return 0, fmt.Errorf("private payload exceeds declared raw length")
	}
	n, err := w.out.Write(p)
	_, _ = w.hash.Write(p[:n])
	w.bytes += int64(n)
	return n, err
}

// ReadCommissionFlowPayload reconstructs original JSON bytes, checking every
// ordered chunk, gzip checksum, encoded/raw length and SHA. The caller must
// quarantine output until nil is returned; failed reads can have partial bytes.
// Streaming consumers can use a private file or io.Discard without allocating
// another whole financial evidence object. This method never writes the DB.
func (s *Server) ReadCommissionFlowPayload(snapshot CommissionFlowVersion, field string, out io.Writer) error {
	value, err := payloadField(&snapshot, field)
	if err != nil {
		return err
	}
	if !isPayloadMarker(*value) {
		if !json.Valid([]byte(*value)) {
			return fmt.Errorf("invalid inline private payload")
		}
		_, err := io.WriteString(out, *value)
		return err
	}
	var m CommissionFlowPayload
	where := "version = ? AND chain = ? AND validator = ? AND field = ?"
	if err := s.db.Where(where, snapshot.Version, snapshot.Chain, snapshot.Validator, field).Take(&m).Error; err != nil {
		return err
	}
	if m.Build != snapshot.Build || m.Codec != commissionPayloadCodec || m.RawBytes <= commissionPayloadInlineLimit || m.EncodedBytes <= 0 || m.ChunkBytes != commissionPayloadChunkSize || m.ChunkCount <= 0 || int64(m.ChunkCount) != (m.EncodedBytes-1)/int64(m.ChunkBytes)+1 || len(m.RawSHA256) != 64 || len(m.EncodedSHA256) != 64 || payloadMarker(m) != *value {
		return fmt.Errorf("invalid private payload complete index or provenance")
	}
	rows, err := s.db.Model(&CommissionFlowPayloadChunk{}).Select("ordinal, sha256, data").Where(where, m.Version, m.Chain, m.Validator, field).Order("ordinal ASC").Rows()
	if err != nil {
		return err
	}
	defer rows.Close()
	encoded := &payloadChunkReader{rows: rows, m: m, hash: sha256.New()}
	reader, err := gzip.NewReader(encoded)
	if err != nil {
		return fmt.Errorf("private payload gzip header: %w", err)
	}
	reader.Multistream(false)
	raw := &payloadRawWriter{out: out, hash: sha256.New(), limit: m.RawBytes}
	_, copyErr := io.Copy(raw, reader)
	closeErr := reader.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("private payload decode: %w", err)
	}
	// Reject trailing members/bytes and force a complete ordered-index check.
	var extra [1]byte
	n, endErr := encoded.Read(extra[:])
	if n != 0 || !errors.Is(endErr, io.EOF) {
		return fmt.Errorf("trailing or incomplete private payload stream")
	}
	if raw.bytes != m.RawBytes || hex.EncodeToString(raw.hash.Sum(nil)) != m.RawSHA256 {
		return fmt.Errorf("private payload raw length or checksum mismatch")
	}
	return nil
}

// LoadCommissionFlowVersion is the explicit version-aware reader. Old results
// stay readable and untouched; chunked results are returned only when fully
// reassembled and verified. Serving endpoints still use their original tables.
func (s *Server) LoadCommissionFlowVersion(version, chain, validator string) (CommissionFlowVersion, error) {
	var snapshot CommissionFlowVersion
	if err := s.db.Where("version = ? AND chain = ? AND validator = ?", version, chain, validator).Take(&snapshot).Error; err != nil {
		return CommissionFlowVersion{}, err
	}
	for _, field := range []string{"run", "rows", "evidence"} {
		value, _ := payloadField(&snapshot, field)
		if !isPayloadMarker(*value) {
			continue
		}
		var b bytes.Buffer
		if err := s.ReadCommissionFlowPayload(snapshot, field, &b); err != nil {
			return CommissionFlowVersion{}, err
		}
		*value = b.String()
	}
	return snapshot, nil
}
