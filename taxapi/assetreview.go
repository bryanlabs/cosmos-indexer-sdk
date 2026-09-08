package taxapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	// MaxAssetDecisionsJSONBytes bounds a user-supplied asset-review payload.
	MaxAssetDecisionsJSONBytes = 64 << 10
	MaxAssetDecisions          = 200
	MaxAssetDecisionRecordID   = 256
	MaxAssetDecisionToken      = 32
)

const (
	AssetDecisionExclude  = "exclude"
	AssetDecisionOverride = "override"
)

// AssetDecision is an explicit treatment for one opaque tax-record ID. An
// omitted map entry is unreviewed, never an implicit exclusion or override.
type AssetDecision struct {
	Mode         string `json:"mode"`
	Token        string `json:"token,omitempty"`
	Quantity     string `json:"quantity,omitempty"`
	ValueUSD     string `json:"value_usd,omitempty"`
	CostBasisUSD string `json:"cost_basis_usd,omitempty"`
	AcquiredDate string `json:"acquired_date,omitempty"`
}

// AssetDecisions is keyed by the opaque RecordID supplied by the report
// builder. This package deliberately does not infer record IDs or asset values.
type AssetDecisions map[string]AssetDecision

// ParseAssetDecisions parses and normalizes a bounded user-supplied JSON map.
// It rejects unknown fields, malformed treatments, and ambiguous exclusions.
func ParseAssetDecisions(input string) (AssetDecisions, error) {
	if len(input) > MaxAssetDecisionsJSONBytes {
		return nil, fmt.Errorf("asset decisions JSON exceeds %d bytes", MaxAssetDecisionsJSONBytes)
	}

	if err := checkDecisionJSON(json.NewDecoder(strings.NewReader(input)), 0); err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode asset decisions: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode asset decisions: %w", err)
	}
	if raw == nil {
		return nil, fmt.Errorf("decode asset decisions: expected JSON object")
	}
	if len(raw) > MaxAssetDecisions {
		return nil, fmt.Errorf("asset decisions exceed %d entries", MaxAssetDecisions)
	}

	out := make(AssetDecisions, len(raw))
	for recordID, value := range raw {
		if err := validateRecordID(recordID); err != nil {
			return nil, err
		}
		var decision AssetDecision
		valueDecoder := json.NewDecoder(bytes.NewReader(value))
		valueDecoder.DisallowUnknownFields()
		if err := valueDecoder.Decode(&decision); err != nil {
			return nil, fmt.Errorf("asset decision %q: %w", recordID, err)
		}
		if err := requireJSONEOF(valueDecoder); err != nil {
			return nil, fmt.Errorf("asset decision %q: %w", recordID, err)
		}
		normalized, err := NormalizeAssetDecision(decision)
		if err != nil {
			return nil, fmt.Errorf("asset decision %q: %w", recordID, err)
		}
		out[recordID] = normalized
	}
	return out, nil
}

// NormalizeAssetDecision validates a decision and returns its canonical form.
// Tokens are trimmed and uppercased. Decimal strings remain exactly as supplied
// after validation so manually entered totals retain their explicit precision.
func NormalizeAssetDecision(decision AssetDecision) (AssetDecision, error) {
	switch decision.Mode {
	case AssetDecisionExclude:
		if decision.Token != "" || decision.Quantity != "" || decision.ValueUSD != "" || decision.CostBasisUSD != "" || decision.AcquiredDate != "" {
			return AssetDecision{}, fmt.Errorf("exclude must not include override fields")
		}
		return AssetDecision{Mode: AssetDecisionExclude}, nil
	case AssetDecisionOverride:
		decision.Token = strings.ToUpper(strings.TrimSpace(decision.Token))
		if err := validateToken(decision.Token); err != nil {
			return AssetDecision{}, err
		}
		if err := validateDecimal(decision.Quantity, true); err != nil {
			return AssetDecision{}, fmt.Errorf("quantity: %w", err)
		}
		if err := validateDecimal(decision.ValueUSD, false); err != nil {
			return AssetDecision{}, fmt.Errorf("value_usd: %w", err)
		}
		if err := validateDecimal(decision.CostBasisUSD, false); err != nil {
			return AssetDecision{}, fmt.Errorf("cost_basis_usd: %w", err)
		}
		if err := validateDate(decision.AcquiredDate); err != nil {
			return AssetDecision{}, fmt.Errorf("acquired_date: %w", err)
		}
		return decision, nil
	default:
		return AssetDecision{}, fmt.Errorf("mode must be %q or %q", AssetDecisionExclude, AssetDecisionOverride)
	}
}

// ValidateAssetDecisions validates an in-memory decision map. It does not
// mutate it; callers that need normalized tokens should use ParseAssetDecisions
// or NormalizeAssetDecision.
func ValidateAssetDecisions(decisions AssetDecisions) error {
	if len(decisions) > MaxAssetDecisions {
		return fmt.Errorf("asset decisions exceed %d entries", MaxAssetDecisions)
	}
	for recordID, decision := range decisions {
		if err := validateRecordID(recordID); err != nil {
			return err
		}
		if _, err := NormalizeAssetDecision(decision); err != nil {
			return fmt.Errorf("asset decision %q: %w", recordID, err)
		}
	}
	return nil
}

// CanonicalAssetDecisionsJSON validates, normalizes, and serializes decisions
// with sorted RecordID keys. Use its output in report-cache keys.
func CanonicalAssetDecisionsJSON(decisions AssetDecisions) (string, error) {
	if err := ValidateAssetDecisions(decisions); err != nil {
		return "", err
	}
	keys := make([]string, 0, len(decisions))
	for key := range decisions {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var out bytes.Buffer
	out.WriteByte('{')
	for i, key := range keys {
		if i != 0 {
			out.WriteByte(',')
		}
		keyJSON, _ := json.Marshal(key)
		normalized, _ := NormalizeAssetDecision(decisions[key])
		decisionJSON, _ := json.Marshal(normalized)
		out.Write(keyJSON)
		out.WriteByte(':')
		out.Write(decisionJSON)
	}
	out.WriteByte('}')
	return out.String(), nil
}

// Duplicate keys are ambiguous financial instructions, not last-key-wins data.
func checkDecisionJSON(decoder *json.Decoder, depth int) error {
	if depth > 16 {
		return fmt.Errorf("asset decisions JSON is too deeply nested")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return fmt.Errorf("invalid decision key")
			}
			if seen[name] {
				return fmt.Errorf("duplicate asset decision field or record: %s", name)
			}
			seen[name] = true
			if err := checkDecisionJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := checkDecisionJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("invalid asset decision JSON")
	}
	_, err = decoder.Token()
	return err
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON data")
		}
		return err
	}
	return nil
}

func validateRecordID(recordID string) error {
	if recordID == "" || len(recordID) > MaxAssetDecisionRecordID {
		return fmt.Errorf("record ID must be 1 through %d bytes", MaxAssetDecisionRecordID)
	}
	return nil
}

func validateToken(token string) error {
	if token == "" || len(token) > MaxAssetDecisionToken {
		return fmt.Errorf("token must be 1 through %d bytes", MaxAssetDecisionToken)
	}
	if !(token[0] >= 'A' && token[0] <= 'Z' || token[0] >= '0' && token[0] <= '9') {
		return fmt.Errorf("token must start with a letter or digit")
	}
	for _, r := range token {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return fmt.Errorf("token contains unsafe ticker characters")
		}
	}
	return nil
}

// validateDecimal accepts only finite, nonnegative plain decimal notation. It
// deliberately rejects signs and exponents, preventing NaN/Infinity and values
// whose displayed magnitude differs from their submitted text.
func validateDecimal(value string, positive bool) error {
	if value == "" {
		return fmt.Errorf("must be a decimal")
	}
	integerDigits, fractionalDigits := 0, 0
	seenDot := false
	for _, r := range value {
		if r == '.' && !seenDot {
			seenDot = true
			continue
		}
		if !unicode.IsDigit(r) || r > '9' {
			return fmt.Errorf("must be a finite plain decimal")
		}
		if seenDot {
			fractionalDigits++
		} else {
			integerDigits++
		}
	}
	if integerDigits == 0 || (seenDot && fractionalDigits == 0) {
		return fmt.Errorf("must be a finite plain decimal")
	}
	if integerDigits+fractionalDigits > 38 || fractionalDigits > 18 {
		return fmt.Errorf("must have at most 38 digits and 18 decimal places")
	}
	if positive {
		for _, r := range value {
			if r >= '1' && r <= '9' {
				return nil
			}
		}
		return fmt.Errorf("must be positive")
	}
	return nil
}

func validateDate(value string) error {
	if len(value) != len("2006-01-02") {
		return fmt.Errorf("must use YYYY-MM-DD")
	}
	for i, r := range value {
		if i == 4 || i == 7 {
			if r != '-' {
				return fmt.Errorf("must use YYYY-MM-DD")
			}
		} else if r < '0' || r > '9' {
			return fmt.Errorf("must use YYYY-MM-DD")
		}
	}
	if _, err := time.Parse("2006-01-02", value); err != nil {
		return fmt.Errorf("must be a valid calendar date")
	}
	return nil
}
