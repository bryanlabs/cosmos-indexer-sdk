package taxapi

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// This deliberately supports only the JSON shapes used by commission outputs.
// Unsupported tags, embedded fields and custom marshalers fail closed. Unlike
// json.Encoder it never buffers an entire container or a large receipt string.
func streamCommissionJSON(out io.Writer, value any) error {
	w := bufio.NewWriterSize(out, 32<<10)
	if err := streamCommissionValue(w, reflect.ValueOf(value), 0); err != nil {
		return err
	}
	return w.Flush()
}
func streamCommissionString(w *bufio.Writer, s string) error {
	if err := w.WriteByte('"'); err != nil {
		return err
	}
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		escape := ""
		n := 1
		switch {
		case c == '"':
			escape = `\"`
		case c == '\\':
			escape = `\\`
		case c == '\n':
			escape = `\n`
		case c == '\r':
			escape = `\r`
		case c == '\t':
			escape = `\t`
		case c == '\b':
			escape = `\b`
		case c == '\f':
			escape = `\f`
		case c < 0x20 || c == '<' || c == '>' || c == '&':
			const hex = "0123456789abcdef"
			escape = string([]byte{'\\', 'u', '0', '0', hex[c>>4], hex[c&15]})
		case c >= utf8.RuneSelf:
			r, size := utf8.DecodeRuneInString(s[i:])
			n = size
			if r == utf8.RuneError && size == 1 {
				escape = "\ufffd"
			} else if r == '\u2028' {
				escape = `\u2028`
			} else if r == '\u2029' {
				escape = `\u2029`
			}
		}
		if escape != "" {
			if _, err := w.WriteString(s[start:i]); err != nil {
				return err
			}
			if _, err := w.WriteString(escape); err != nil {
				return err
			}
			start = i + n
		}
		i += n
	}
	if _, err := w.WriteString(s[start:]); err != nil {
		return err
	}
	return w.WriteByte('"')
}
func commissionJSONEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64, reflect.Interface, reflect.Pointer:
		return v.IsZero()
	}
	return false
}
func streamCommissionValue(w *bufio.Writer, v reflect.Value, depth int) error {
	if depth > 64 {
		return errors.New("commission JSON nesting exceeds bound")
	}
	if !v.IsValid() {
		_, err := w.WriteString("null")
		return err
	}
	if (v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer) && v.IsNil() {
		_, err := w.WriteString("null")
		return err
	}
	if v.Type() == reflect.TypeOf(time.Time{}) {
		b, err := json.Marshal(v.Interface())
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	}
	if v.Type() == reflect.TypeOf(json.RawMessage{}) {
		b := v.Bytes()
		if !json.Valid(b) {
			return errors.New("invalid commission raw JSON")
		}
		_, err := w.Write(b)
		return err
	}
	if v.CanInterface() {
		if _, ok := v.Interface().(json.Marshaler); ok {
			return errors.New("unsupported commission custom JSON marshaler")
		}
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		return streamCommissionValue(w, v.Elem(), depth+1)
	case reflect.String:
		return streamCommissionString(w, v.String())
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		b, err := json.Marshal(v.Interface())
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			_, err := w.WriteString("null")
			return err
		}
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
			if err := w.WriteByte('"'); err != nil {
				return err
			}
			enc := base64.NewEncoder(base64.StdEncoding, w)
			if _, err := enc.Write(v.Bytes()); err != nil {
				return err
			}
			if err := enc.Close(); err != nil {
				return err
			}
			return w.WriteByte('"')
		}
		if err := w.WriteByte('['); err != nil {
			return err
		}
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				if err := w.WriteByte(','); err != nil {
					return err
				}
			}
			if err := streamCommissionValue(w, v.Index(i), depth+1); err != nil {
				return err
			}
		}
		return w.WriteByte(']')
	case reflect.Map:
		if v.IsNil() {
			_, err := w.WriteString("null")
			return err
		}
		if v.Type().Key().Kind() != reflect.String {
			return errors.New("unsupported commission JSON map key")
		}
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		if err := w.WriteByte('{'); err != nil {
			return err
		}
		for i, k := range keys {
			if i > 0 {
				if err := w.WriteByte(','); err != nil {
					return err
				}
			}
			if err := streamCommissionString(w, k.String()); err != nil {
				return err
			}
			if err := w.WriteByte(':'); err != nil {
				return err
			}
			if err := streamCommissionValue(w, v.MapIndex(k), depth+1); err != nil {
				return err
			}
		}
		return w.WriteByte('}')
	case reflect.Struct:
		if err := w.WriteByte('{'); err != nil {
			return err
		}
		written := false
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if f.PkgPath != "" {
				continue
			}
			tag := f.Tag.Get("json")
			name, options, _ := strings.Cut(tag, ",")
			if name == "-" {
				continue
			}
			if f.Anonymous {
				return errors.New("unsupported embedded commission JSON field")
			}
			if options != "" && options != "omitempty" {
				return errors.New("unsupported commission JSON field options")
			}
			if options == "omitempty" && commissionJSONEmpty(v.Field(i)) {
				continue
			}
			if name == "" {
				name = f.Name
			}
			if written {
				if err := w.WriteByte(','); err != nil {
					return err
				}
			}
			written = true
			if err := streamCommissionString(w, name); err != nil {
				return err
			}
			if err := w.WriteByte(':'); err != nil {
				return err
			}
			if err := streamCommissionValue(w, v.Field(i), depth+1); err != nil {
				return err
			}
		}
		return w.WriteByte('}')
	default:
		return errors.New("unsupported commission JSON value")
	}
}
