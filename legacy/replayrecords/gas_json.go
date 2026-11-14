package replayrecords

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strconv"
	"unicode"
)

var canonicalInt64 = regexp.MustCompile(`^(0|-[1-9][0-9]*|[1-9][0-9]*)$`)

// GasInt preserves number lexemes until canonical decimal and int64 validation.
// Amino's quoted integers and JSON integers are accepted, never floats or -0.
type GasInt int64

func (n *GasInt) UnmarshalJSON(raw []byte) error {
	s := string(raw)
	if len(raw) > 0 && raw[0] == '"' {
		if json.Unmarshal(raw, &s) != nil {
			return invalid
		}
	}
	if !canonicalInt64.MatchString(s) {
		return invalid
	}
	v, e := strconv.ParseInt(s, 10, 64)
	if e != nil {
		return invalid
	}
	*n = GasInt(v)
	return nil
}

// strictGasJSON rejects duplicate members, unknown typed fields, trailing values
// and noncanonical numeric lexemes before any typed conversion can lose a sign.
func strictGasJSON(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if gasJSONValue(d, 0) != nil {
		return invalid
	}
	if _, e := d.Token(); e != io.EOF {
		return invalid
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return invalid
	}
	return nil
}

// Match encoding/json's Unicode simple-fold equivalence, including long-s
// and Kelvin-sign aliases, not merely ASCII lowercasing.
func gasJSONFieldFold(s string) string {
	runes := []rune(s)
	for i, r := range runes {
		if r >= 'a' && r <= 'z' {
			runes[i] = r - ('a' - 'A')
			continue
		}
		if r < 128 {
			continue
		}
		for {
			next := unicode.SimpleFold(r)
			if next <= r {
				runes[i] = next
				break
			}
			r = next
		}
	}
	return string(runes)
}
func gasJSONValue(d *json.Decoder, depth int) error {
	if depth > 64 {
		return invalid
	}
	t, e := d.Token()
	if e != nil {
		return invalid
	}
	switch v := t.(type) {
	case json.Number:
		var n GasInt
		return n.UnmarshalJSON([]byte(v))
	case json.Delim:
		switch v {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				s, ok := k.(string)
				folded := gasJSONFieldFold(s)
				if e != nil || !ok || seen[folded] {
					return invalid
				}
				seen[folded] = true
				if gasJSONValue(d, depth+1) != nil {
					return invalid
				}
			}
			t, e = d.Token()
			if e != nil || t != json.Delim('}') {
				return invalid
			}
		case '[':
			for d.More() {
				if gasJSONValue(d, depth+1) != nil {
					return invalid
				}
			}
			t, e = d.Token()
			if e != nil || t != json.Delim(']') {
				return invalid
			}
		default:
			return invalid
		}
	}
	return nil
}
