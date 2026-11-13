package replayrecords

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Fixed bounds apply to the entire input, including historical resume lines.
const MaxDifferenceBytes int64 = 256 << 20
const MaxDifferenceLine = 1 << 20

// DifferenceAdmission describes all original bytes, not a filtered replacement.
// ClassificationSHA256 hashes the canonical JSON of this struct with that field empty.
type DifferenceAdmission struct {
	Version              string `json:"version"`
	RawSHA256            string `json:"raw_sha256"`
	From                 int64  `json:"status_from"`
	Committed            int64  `json:"status_committed"`
	StatusDiffs          int64  `json:"status_result_diffs"`
	TotalLines           int64  `json:"total_lines"`
	HistoricalLines      int64  `json:"historical_lines"`
	RunLines             int64  `json:"run_lines"`
	ClassificationSHA256 string `json:"classification_sha256"`
}

var positiveDecimal = regexp.MustCompile(`^[1-9][0-9]*$`)
var tagKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_./:-]{0,255}$`)

func positive(s string) (int64, bool) {
	if !positiveDecimal.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0
}

// tagDetail accepts exactly the runner's order, value-key list, or signed
// key-count list grammar. Unknown/ambiguous key bytes fail closed.
func tagDetail(s string) bool {
	if s == "order" {
		return true
	}
	if !strings.HasSuffix(s, ";") {
		return false
	}
	parts := strings.Split(s[:len(s)-1], ";")
	seen := make(map[string]bool)
	values := strings.HasPrefix(s, "value ")
	for _, part := range parts {
		var key string
		if values {
			if !strings.HasPrefix(part, "value ") {
				return false
			}
			key = strings.TrimPrefix(part, "value ")
		} else {
			if len(part) < 4 || (part[0] != '+' && part[0] != '-') {
				return false
			}
			count, k, ok := strings.Cut(part[1:], " ")
			if _, good := positive(count); !ok || !good {
				return false
			}
			key = k
		}
		// The runner emits one value entry per changed tag occurrence, so
		// repeated value keys are valid. Count deltas emit each key once.
		if !tagKey.MatchString(key) || (!values && seen[key]) {
			return false
		}
		seen[key] = true
	}
	return len(parts) > 0
}

func tagLine(s string) bool {
	lastTx := int64(-1)
	stage := 0
	for _, item := range strings.Split(s, ",") {
		name, detail, ok := strings.Cut(item, ":tags(")
		if !ok || !strings.HasSuffix(detail, ")") || !tagDetail(detail[:len(detail)-1]) {
			return false
		}
		switch name {
		case "begin":
			if stage != 0 {
				return false
			}
			stage = 1
		case "end":
			if stage > 1 {
				return false
			}
			stage = 2
		default:
			if stage != 0 || !strings.HasPrefix(name, "tx") {
				return false
			}
			raw := strings.TrimPrefix(name, "tx")
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 0 || strconv.FormatInt(n, 10) != raw || n <= lastTx {
				return false
			}
			lastTx = n
		}
	}
	return s != ""
}

// AdmitTagDifferences streams and hashes the FULL retained file. Every line must
// be canonical tags-only evidence, strictly height ordered, even before From.
// StatusDiffs counts only From..Committed, matching a resumed runner's counter.
func AdmitTagDifferences(r io.Reader, pinnedSHA string, from, committed, statusDiffs int64) (DifferenceAdmission, error) {
	return admitTagDifferences(r, pinnedSHA, from, committed, statusDiffs, MaxDifferenceBytes)
}

func admitTagDifferences(r io.Reader, pinnedSHA string, from, committed, statusDiffs, maxBytes int64) (DifferenceAdmission, error) {
	var a DifferenceAdmission
	if maxBytes < 1 || maxBytes > MaxDifferenceBytes || from < 1 || committed < from || statusDiffs < 0 || len(pinnedSHA) != 64 {
		return a, invalid
	}
	if b, e := hex.DecodeString(pinnedSHA); e != nil || hex.EncodeToString(b) != pinnedSHA {
		return a, invalid
	}
	hash := sha256.New()
	limited := &io.LimitedReader{R: r, N: maxBytes + 1}
	reader := bufio.NewReaderSize(io.TeeReader(limited, hash), MaxDifferenceLine)
	a = DifferenceAdmission{Version: "runner034-tags-only-v1", RawSHA256: pinnedSHA, From: from, Committed: committed, StatusDiffs: statusDiffs}
	var previous int64
	for {
		raw, err := reader.ReadSlice('\n')
		if err == io.EOF && len(raw) == 0 {
			break
		}
		if err != nil || len(raw) > MaxDifferenceLine || limited.N == 0 {
			return DifferenceAdmission{}, invalid
		}
		line := string(raw[:len(raw)-1])
		height, details, ok := strings.Cut(line, " ")
		h, good := positive(height)
		if !ok || !good || h <= previous || h > committed || !tagLine(details) {
			return DifferenceAdmission{}, invalid
		}
		previous = h
		a.TotalLines++
		if h < from {
			a.HistoricalLines++
		} else {
			a.RunLines++
		}
	}
	if a.RunLines != statusDiffs || hex.EncodeToString(hash.Sum(nil)) != pinnedSHA {
		return DifferenceAdmission{}, invalid
	}
	canonical, err := json.Marshal(a)
	if err != nil {
		return DifferenceAdmission{}, invalid
	}
	digest := sha256.Sum256(canonical)
	a.ClassificationSHA256 = hex.EncodeToString(digest[:])
	return a, nil
}
