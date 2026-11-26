package taxapi

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var streamFixtureStrings = []string{"", "a", "cosmos1qqq", "<b>&\"q\"</b>", "line\nbreak\ttab", "\u2028\u2029", "caf\u00e9 \U0001F600", "\xff\xfe bad utf8", "\\slash/", strings.Repeat("x", 300)}

// fillRandom sets every exported field reachable from v to random values,
// including nil and empty forms, the way stored evidence can hold them.
func fillRandom(r *rand.Rand, v reflect.Value, depth int) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(streamFixtureStrings[r.Intn(len(streamFixtureStrings))])
	case reflect.Bool:
		v.SetBool(r.Intn(2) == 0)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if r.Intn(3) > 0 {
			v.SetInt(r.Int63n(1<<40) - 1<<39)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(r.Intn(256)))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(r.Float64() * 1e6)
	case reflect.Pointer:
		if r.Intn(3) == 0 || depth > 4 {
			return
		}
		p := reflect.New(v.Type().Elem())
		fillRandom(r, p.Elem(), depth+1)
		v.Set(p)
	case reflect.Slice:
		switch r.Intn(4) {
		case 0:
			return
		case 1:
			v.Set(reflect.MakeSlice(v.Type(), 0, 0))
			return
		}
		n := 1 + r.Intn(6)
		s := reflect.MakeSlice(v.Type(), n, n)
		for i := 0; i < n; i++ {
			fillRandom(r, s.Index(i), depth+1)
		}
		v.Set(s)
	case reflect.Map:
		switch r.Intn(4) {
		case 0:
			return
		case 1:
			v.Set(reflect.MakeMap(v.Type()))
			return
		}
		m := reflect.MakeMap(v.Type())
		for i := 0; i < 1+r.Intn(5); i++ {
			k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
			fillRandom(r, k, depth+1)
			fillRandom(r, e, depth+1)
			m.SetMapIndex(k, e)
		}
		v.Set(m)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			if r.Intn(4) > 0 {
				v.Set(reflect.ValueOf(time.Unix(r.Int63n(2e9), r.Int63n(1e9)).In(time.FixedZone("z", 3600*(r.Intn(5)-2)))))
			}
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fillRandom(r, v.Field(i), depth+1)
			}
		}
	}
}

func streamed(t *testing.T, v any) []byte {
	var b bytes.Buffer
	require.NoError(t, streamStructJSON(&b, v))
	return b.Bytes()
}

func TestStreamEvidenceMatchesMarshal(t *testing.T) {
	r := rand.New(rand.NewSource(847))
	for i := 0; i < 3000; i++ {
		var e CommissionFlowEvidence
		fillRandom(r, reflect.ValueOf(&e).Elem(), 0)
		want, err := json.Marshal(e)
		require.NoError(t, err)
		require.Equal(t, string(want), string(streamed(t, e)), "seed case %d", i)
	}
	want, _ := json.Marshal(CommissionFlowEvidence{})
	require.Equal(t, string(want), string(streamed(t, CommissionFlowEvidence{})))
}

func TestStreamEvidenceFieldsSupported(t *testing.T) {
	rt := reflect.TypeOf(CommissionFlowEvidence{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		require.False(t, f.Anonymous, f.Name)
		_, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		require.Contains(t, []string{"", "omitempty"}, opts, f.Name)
	}
}

func TestStreamRefusesUnsupported(t *testing.T) {
	type quoted struct {
		N int `json:",string"`
	}
	type inner struct{ A int }
	type embedded struct{ inner }
	require.Error(t, streamStructJSON(io.Discard, quoted{}))
	require.Error(t, streamStructJSON(io.Discard, embedded{}))
	require.Error(t, streamStructJSON(io.Discard, []int{1}))
}

type pointerMarshaler struct{ V int }

func (p *pointerMarshaler) MarshalJSON() ([]byte, error) { return []byte(`"p"`), nil }

func TestStreamSliceElementsUseAddress(t *testing.T) {
	v := struct {
		S []pointerMarshaler
		B []byte
		R json.RawMessage `json:",omitempty"`
	}{S: []pointerMarshaler{{1}, {2}}, B: []byte{1, 2, 3}, R: json.RawMessage(`{"a":1}`)}
	want, _ := json.Marshal(v)
	require.Equal(t, string(want), string(streamed(t, v)))
}

func randomHexJSON(r *rand.Rand, n int) string {
	var b strings.Builder
	b.WriteString(`{"v":"`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%016x", r.Uint64())
	}
	b.WriteString(`"}`)
	return b.String()
}

// bigEvidence is a few MiB of evidence, so it is chunked.
func bigEvidence() CommissionFlowEvidence {
	r := rand.New(rand.NewSource(7))
	var e CommissionFlowEvidence
	fillRandom(r, reflect.ValueOf(&e).Elem(), 0)
	e.Archive = nil
	for i := 0; i < 6000; i++ {
		e.Archive = append(e.Archive, FlowArchiveReceipt{Address: "cosmos1" + strings.Repeat(string(rune('a'+i%26)), 38), Height: int64(1e7 + i), Method: "abci_query", QueryPath: "/cosmos.bank.v1beta1.Query/AllBalances", RequestData: "0a2d", SHA256: strings.Repeat("ab", 32), Response: randomHexJSON(r, 20)})
	}
	return e
}

func TestPayloadStreamMatchesWholeEncode(t *testing.T) {
	for _, e := range []CommissionFlowEvidence{bigEvidence(), {Channels: map[string]string{"channel-0": "osmosis-1"}}} {
		raw, err := json.Marshal(e)
		require.NoError(t, err)
		var whole CommissionFlowPayload
		var wholeChunks [][]byte
		inlineWant := len(raw) <= commissionPayloadInlineLimit
		if !inlineWant {
			require.NoError(t, encodeCommissionPayload(raw, &whole, func(i int, b []byte) error {
				require.Equal(t, len(wholeChunks), i)
				wholeChunks = append(wholeChunks, append([]byte(nil), b...))
				return nil
			}))
		}
		var m CommissionFlowPayload
		var chunks [][]byte
		w := newPayloadStreamWriter(&m, func(i int, b []byte) error {
			require.Equal(t, len(chunks), i)
			chunks = append(chunks, append([]byte(nil), b...))
			return nil
		})
		require.NoError(t, streamStructJSON(w, e))
		inline, chunked, err := w.finish()
		require.NoError(t, err)
		require.Equal(t, payloadHash(raw), w.rawSHA256())
		if inlineWant {
			require.False(t, chunked)
			require.Equal(t, string(raw), inline)
			require.Empty(t, chunks)
			continue
		}
		require.True(t, chunked)
		require.Empty(t, inline)
		require.Equal(t, whole, m)
		require.Equal(t, wholeChunks, chunks)
		require.Greater(t, len(chunks), 1, "raw %d encoded %d", len(raw), m.EncodedBytes)
	}
}

func TestPayloadStreamInlineBoundary(t *testing.T) {
	for _, n := range []int{commissionPayloadInlineLimit - 1, commissionPayloadInlineLimit, commissionPayloadInlineLimit + 1} {
		raw := []byte(`"` + strings.Repeat("z", n-2) + `"`)
		var m CommissionFlowPayload
		var count int
		w := newPayloadStreamWriter(&m, func(int, []byte) error { count++; return nil })
		for p := raw; len(p) > 0; {
			k := min(len(p), 4093)
			_, err := w.Write(p[:k])
			require.NoError(t, err)
			p = p[k:]
		}
		inline, chunked, err := w.finish()
		require.NoError(t, err)
		require.Equal(t, n > commissionPayloadInlineLimit, chunked, n)
		if !chunked {
			require.Equal(t, string(raw), inline)
			continue
		}
		var whole CommissionFlowPayload
		require.NoError(t, encodeCommissionPayload(raw, &whole, func(int, []byte) error { return nil }))
		require.Equal(t, whole, m)
		require.Equal(t, 1, count)
	}
}

func TestCaptureStreamMatchesString(t *testing.T) {
	e := bigEvidence()
	raw, _ := json.Marshal(e)
	snap := CommissionFlowVersion{Version: "v", Chain: "c", Validator: "x", Build: "b", RunJSON: `{"r":1}`, RowsJSON: `[]`, EvidenceJSON: string(raw)}
	a, b := t.TempDir(), t.TempDir()
	require.NoError(t, os.Chmod(a, 0700))
	require.NoError(t, os.Chmod(b, 0700))
	want, err := captureCommissionFlowPayloadFields(a, snap, nil)
	require.NoError(t, err)
	snap.EvidenceJSON = ""
	got, err := captureCommissionFlowPayloadFields(b, snap, map[string]payloadSource{"evidence": func(w io.Writer) error { return streamStructJSON(w, e) }})
	require.NoError(t, err)
	require.Equal(t, want, got)
	read := func(root string) []byte {
		d, _ := filepath.Glob(filepath.Join(root, "attempt-*", "evidence.json.gz"))
		require.Len(t, d, 1)
		f, err := os.ReadFile(d[0])
		require.NoError(t, err)
		zr, err := gzip.NewReader(bytes.NewReader(f))
		require.NoError(t, err)
		out, err := io.ReadAll(zr)
		require.NoError(t, err)
		return append(out, f...)
	}
	require.Equal(t, read(a), read(b))
}
