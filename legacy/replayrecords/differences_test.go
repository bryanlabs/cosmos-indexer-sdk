package replayrecords

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func diffSHA(raw string) string { h := sha256.Sum256([]byte(raw)); return hex.EncodeToString(h[:]) }

func TestAdmitTagDifferencesResumesAndReceipt(t *testing.T) {
	raw := "1 tx0:tags(value action;value sender;value sender;)\n9 tx0:tags(+1 action;-2 sender;),tx2:tags(order),begin:tags(value proposer;),end:tags(-1 reward;)\n10 end:tags(order)\n"
	a, err := AdmitTagDifferences(strings.NewReader(raw), diffSHA(raw), 9, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	if a.TotalLines != 3 || a.HistoricalLines != 1 || a.RunLines != 2 || a.RawSHA256 != diffSHA(raw) || a.Version != "runner034-tags-only-v1" {
		t.Fatal("incorrect classification")
	}
	digest := a.ClassificationSHA256
	a.ClassificationSHA256 = ""
	canonical, _ := json.Marshal(a)
	if digest != diffSHA(string(canonical)) {
		t.Fatal("classification digest not reproducible")
	}
}

func TestAdmitTagDifferencesNonzeroRunCounts(t *testing.T) {
	for _, count := range []int64{50, 2222} {
		var b strings.Builder
		for h := int64(1); h <= count; h++ {
			fmt.Fprintf(&b, "%d end:tags(value action;)\n", h)
		}
		raw := b.String()
		a, err := AdmitTagDifferences(strings.NewReader(raw), diffSHA(raw), 1, count, count)
		if err != nil || a.RunLines != count || a.StatusDiffs != count || a.TotalLines != count {
			t.Fatal("nonzero tags-only run refused")
		}
	}
}

func TestAdmitTagDifferencesStrictFailures(t *testing.T) {
	for name, raw := range map[string]string{
		"code": "9 tx0:code\n", "data": "9 tx0:data\n", "gas": "9 tx0:gas\n", "log": "9 tx0:log\n", "updates": "9 end:validator-updates\n", "absent": "9 tx0:absent\n", "count": "9 transaction-count\n",
		"mixed": "9 tx0:tags(order),tx1:gas\n", "historical unsafe": "1 tx0:code\n9 end:tags(order)\n",
		"duplicate height": "9 end:tags(order)\n9 end:tags(order)\n", "out of order": "10 end:tags(order)\n9 end:tags(order)\n",
		"future": "11 end:tags(order)\n", "zero": "0 end:tags(order)\n", "height overflow": "9223372036854775808 end:tags(order)\n",
		"leading zero": "09 end:tags(order)\n", "blank": "\n9 end:tags(order)\n", "no newline": "9 end:tags(order)", "crlf": "9 end:tags(order)\r\n",
		"empty detail": "9 end:tags()\n", "injected suffix": "9 end:tags(order)garbage\n", "injected key": "9 end:tags(value x);gas;)\n",
		"zero delta": "9 end:tags(+0 action;)\n", "count overflow": "9 end:tags(+9223372036854775808 action;)\n", "noncanonical delta": "9 end:tags(+01 action;)\n",
		"mixed details": "9 end:tags(value action;+1 sender;)\n", "duplicate count key": "9 end:tags(+1 action;-1 action;)\n",
		"duplicate tx": "9 tx0:tags(order),tx0:tags(order)\n", "tx ordering": "9 tx1:tags(order),tx0:tags(order)\n", "tx zero prefix": "9 tx01:tags(order)\n", "negative tx": "9 tx-1:tags(order)\n",
		"phase ordering": "9 end:tags(order),begin:tags(order)\n", "tx after begin": "9 begin:tags(order),tx0:tags(order)\n", "duplicate begin": "9 begin:tags(order),begin:tags(order)\n", "duplicate end": "9 end:tags(order),end:tags(order)\n",
		"unknown key bytes": "9 end:tags(value key space;)\n", "extra space": "9  end:tags(order)\n", "nul": "9 end:tags(value a\x00;)\n",
		"oversized line": "9 end:tags(value " + strings.Repeat("a", MaxDifferenceLine) + ";)\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := AdmitTagDifferences(strings.NewReader(raw), diffSHA(raw), 9, 10, 1); err == nil {
				t.Fatal("unsafe differences admitted")
			}
		})
	}
	raw := "9 end:tags(order)\n"
	for _, n := range []int64{-1, 0, 2} {
		if _, e := AdmitTagDifferences(strings.NewReader(raw), diffSHA(raw), 9, 10, n); e == nil {
			t.Fatal("incorrect status count admitted")
		}
	}
	for _, pin := range []string{"", strings.Repeat("0", 64), strings.ToUpper(diffSHA(raw))} {
		if _, e := AdmitTagDifferences(strings.NewReader(raw), pin, 9, 10, 1); e == nil {
			t.Fatal("unpinned bytes admitted")
		}
	}
}

type failedDifferenceReader struct{}

func (failedDifferenceReader) Read([]byte) (int, error) {
	return 0, errors.New("synthetic read failure")
}
func TestDifferenceBoundsAndIO(t *testing.T) {
	raw := "9 end:tags(order)\n10 end:tags(order)\n"
	for _, limit := range []int64{int64(len(raw) - 1), int64(len(raw))} {
		_, err := admitTagDifferences(strings.NewReader(raw), diffSHA(raw), 9, 10, 2, limit)
		if (err == nil) != (limit == int64(len(raw))) {
			t.Fatal("incorrect whole-file bound")
		}
	}
	if _, e := AdmitTagDifferences(io.MultiReader(strings.NewReader(raw), failedDifferenceReader{}), diffSHA(raw), 9, 10, 2); e == nil {
		t.Fatal("read error skipped")
	}
	if _, e := AdmitTagDifferences(strings.NewReader(""), diffSHA(""), 9, 10, 0); e != nil {
		t.Fatal(e)
	}
}
