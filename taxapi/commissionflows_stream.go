package taxapi

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"reflect"
	"strings"
)

// payloadSource writes one private JSON value to w. It lets a large value,
// such as a big validator's evidence, reach the capture file and the payload
// chunks without ever being held whole in memory as JSON.
type payloadSource func(w io.Writer) error

// streamStructJSON writes exactly the bytes json.Marshal(v) returns for a
// struct value v, without building them in one buffer: each top-level field is
// marshaled on its own, and each element of a top-level slice field on its
// own. It supports the plain field forms CommissionFlowEvidence uses (json
// names, omitempty, no embedded fields, no string option) and refuses others.
func streamStructJSON(w io.Writer, v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Struct {
		return fmt.Errorf("stream JSON needs a struct value")
	}
	rt := rv.Type()
	bw := bufio.NewWriterSize(w, 1<<20)
	if err := bw.WriteByte('{'); err != nil {
		return err
	}
	first := true
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if sf.Anonymous {
			return fmt.Errorf("stream JSON does not support embedded fields")
		}
		if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		omitempty := false
		for _, o := range strings.Split(opts, ",") {
			switch o {
			case "omitempty":
				omitempty = true
			case "":
			default:
				return fmt.Errorf("stream JSON does not support the %q option", o)
			}
		}
		if name == "" {
			name = sf.Name
		}
		fv := rv.Field(i)
		if omitempty && streamEmptyValue(fv) {
			continue
		}
		if !first {
			if err := bw.WriteByte(','); err != nil {
				return err
			}
		}
		first = false
		key, err := json.Marshal(name)
		if err != nil {
			return err
		}
		if _, err := bw.Write(key); err != nil {
			return err
		}
		if err := bw.WriteByte(':'); err != nil {
			return err
		}
		if fv.Kind() == reflect.Slice && !fv.IsNil() && fv.Type().Elem().Kind() != reflect.Uint8 {
			if err := streamSliceJSON(bw, fv); err != nil {
				return err
			}
			continue
		}
		b, err := json.Marshal(fv.Interface())
		if err != nil {
			return err
		}
		if _, err := bw.Write(b); err != nil {
			return err
		}
	}
	if err := bw.WriteByte('}'); err != nil {
		return err
	}
	return bw.Flush()
}

// Slice elements are addressable inside json.Marshal, so each is marshaled
// through its address, which keeps any pointer-receiver MarshalJSON in use.
func streamSliceJSON(bw *bufio.Writer, fv reflect.Value) error {
	if err := bw.WriteByte('['); err != nil {
		return err
	}
	for j := 0; j < fv.Len(); j++ {
		if j > 0 {
			if err := bw.WriteByte(','); err != nil {
				return err
			}
		}
		b, err := json.Marshal(fv.Index(j).Addr().Interface())
		if err != nil {
			return err
		}
		if _, err := bw.Write(b); err != nil {
			return err
		}
	}
	return bw.WriteByte(']')
}

// streamEmptyValue is encoding/json's omitempty test.
func streamEmptyValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	}
	return false
}

// payloadStreamWriter stores one streamed private JSON value the way
// storeCommissionFlowPayloads stores a string: a value of at most
// commissionPayloadInlineLimit bytes stays inline; a longer one becomes the
// same gzip chunks, chunk rows and complete index, from the same bytes.
type payloadStreamWriter struct {
	m       *CommissionFlowPayload
	save    func(int, []byte) error
	inline  []byte
	raw     hash.Hash
	bytes   int64
	chunks  *payloadChunkWriter
	gz      *gzip.Writer
	chunked bool
}

func newPayloadStreamWriter(m *CommissionFlowPayload, save func(int, []byte) error) *payloadStreamWriter {
	return &payloadStreamWriter{m: m, save: save, raw: sha256.New()}
}

func (w *payloadStreamWriter) Write(p []byte) (int, error) {
	if !w.chunked && len(w.inline)+len(p) <= commissionPayloadInlineLimit {
		w.inline = append(w.inline, p...)
		_, _ = w.raw.Write(p)
		w.bytes += int64(len(p))
		return len(p), nil
	}
	if !w.chunked {
		w.chunks = &payloadChunkWriter{hash: sha256.New(), save: w.save}
		gz, err := gzip.NewWriterLevel(w.chunks, gzip.BestSpeed)
		if err != nil {
			return 0, err
		}
		w.gz, w.chunked = gz, true
		if _, err := w.gz.Write(w.inline); err != nil {
			return 0, err
		}
		w.inline = nil
	}
	if _, err := w.gz.Write(p); err != nil {
		return 0, err
	}
	_, _ = w.raw.Write(p)
	w.bytes += int64(len(p))
	return len(p), nil
}

// finish returns the inline JSON when the value stayed inline, or "" with the
// complete index filled in m when it was chunked.
func (w *payloadStreamWriter) finish() (string, bool, error) {
	if !w.chunked {
		if !json.Valid(w.inline) {
			return "", false, fmt.Errorf("invalid private JSON payload")
		}
		return string(w.inline), false, nil
	}
	if err := w.gz.Close(); err != nil {
		return "", true, err
	}
	if err := w.chunks.flush(); err != nil {
		return "", true, err
	}
	w.m.Codec, w.m.RawSHA256, w.m.RawBytes = commissionPayloadCodec, hex.EncodeToString(w.raw.Sum(nil)), w.bytes
	w.m.ChunkBytes = commissionPayloadChunkSize
	w.m.EncodedSHA256, w.m.EncodedBytes, w.m.ChunkCount = hex.EncodeToString(w.chunks.hash.Sum(nil)), w.chunks.bytes, w.chunks.count
	return "", true, nil
}

func (w *payloadStreamWriter) rawSHA256() string { return hex.EncodeToString(w.raw.Sum(nil)) }
