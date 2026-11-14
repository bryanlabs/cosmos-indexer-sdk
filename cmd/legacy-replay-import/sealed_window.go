package main

// Sealed windows are validation-only components, not a shortened successful
// replay Job. The original status and original Job target are retained verbatim.
import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
)

const windowJob = "cosmos-tax-legacy-record-hub2-3e087ab-long1"
const windowUID = "abef1dde-ab1f-48d7-8b30-518dbc139b5a"

type sealedFile struct {
	Name         string `json:"name"`
	SHA          string `json:"sha256"`
	Bytes        int64  `json:"bytes"`
	Inode        uint64 `json:"source_inode"`
	Before       int64  `json:"source_size_before"`
	After        int64  `json:"source_size_after"`
	RemoteBefore string `json:"remote_prefix_sha256_before"`
	RemoteAfter  string `json:"remote_prefix_sha256_after"`
}
type windowReceipt struct {
	Version     string                `json:"version"`
	Job         string                `json:"source_job"`
	UID         string                `json:"source_job_uid"`
	PodUID      string                `json:"source_pod_uid"`
	Image       string                `json:"source_image"`
	Source      string                `json:"source_commit"`
	Target      int64                 `json:"original_target"`
	From        int64                 `json:"from"`
	Through     int64                 `json:"through"`
	JobComplete bool                  `json:"whole_job_complete"`
	Component   string                `json:"component"`
	Files       map[string]sealedFile `json:"files"`
	Artifacts   map[string]string     `json:"artifact_sha256"`
}

func validateWindowScope(w windowReceipt, p replayrecords.Proof) error {
	if w.Version != "hub2-sealed-window-v1" || w.Job != windowJob || w.UID != windowUID || w.PodUID == "" || w.Image != replayrecords.GasFinancialImage || w.Source != replayrecords.GasFinancialSource || w.Target != 2902000 || w.From != 400001 || w.Through != 482090 || w.JobComplete || w.Component != "per-complete-height-financial-reconciliation-not-chain-books-acceptance" || len(w.Files) != 5 || p.From != w.From || p.Through != w.Through || p.VerifiedThrough != w.Through || p.Export {
		return refused
	}
	return nil
}

func validateOriginalWindow(raw, log []byte) error {
	var original struct {
		Release      string      `json:"release"`
		Chain        string      `json:"chain"`
		Started      string      `json:"started"`
		Updated      string      `json:"updated"`
		From         int64       `json:"first_height_this_run"`
		Committed    int64       `json:"last_height_committed"`
		Verified     int64       `json:"state_hash_verified_through"`
		Target       int64       `json:"target"`
		Transactions int64       `json:"transactions_this_run"`
		Rate         json.Number `json:"blocks_per_second_last_window"`
		RateRun      json.Number `json:"blocks_per_second_this_run"`
		Diffs        int64       `json:"heights_where_results_differ_from_cache"`
		Evidence     int64       `json:"evidence_items_this_run"`
		Stop         string      `json:"stop_reason"`
	}
	if strictWindowJSON(raw, &original) != nil || original.Release != replayrecords.Release034 || original.Chain != "cosmoshub-2" || original.From != 1 || original.Target != 2902000 || original.Committed < 482091 || original.Committed > original.Target || original.Verified < 482090 || original.Verified != original.Committed-1 || original.Stop != "" || original.Transactions < 0 || original.Diffs < 0 || original.Diffs > original.Committed || original.Evidence < 0 {
		return refused
	}
	started, e := time.Parse(time.RFC3339, original.Started)
	if e != nil {
		return refused
	}
	updated, e := time.Parse(time.RFC3339, original.Updated)
	if e != nil || updated.Before(started) {
		return refused
	}
	if !bytes.Contains(log, []byte(original.Updated+originalReport(original.Committed, original.Verified))) || !bytes.HasSuffix(log, []byte{'\n'}) {
		return refused
	}
	return nil
}

// Only exclusive private regular local artifacts are admitted. File names are
// fixed, not receipt-controlled paths to arbitrary files.
func privateRaw(path string, limit int64) ([]byte, error) {
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > limit {
		return nil, refused
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, refused
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, refused
	}
	return b, nil
}
func windowHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func boundFile(dir, kind string, x sealedFile) (string, error) {
	names := map[string]string{"movements": "movements.prefix.ndjson", "validators": "validators.prefix.ndjson", "differences": "differences.prefix.txt", "log": "run.prefix.log", "status": "status.original.json"}
	if names[kind] == "" || x.Name != names[kind] || len(x.SHA) != 64 || x.Bytes <= 0 || x.Before < x.Bytes || x.After < x.Before || x.Inode == 0 || x.RemoteBefore != x.SHA || x.RemoteAfter != x.SHA {
		return "", refused
	}
	path := filepath.Join(dir, x.Name)
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != x.Bytes {
		return "", refused
	}
	f, e := os.Open(path)
	if e != nil {
		return "", refused
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil || hex.EncodeToString(h.Sum(nil)) != x.SHA {
		return "", refused
	}
	return path, nil
}

// loadSealedWindow returns an explicitly derived admission scope. It never
// rewrites or mislabels the original source status as a completed bounded run.
func loadSealedWindow(path, records, status, differences string, p replayrecords.Proof) (replayStatus, error) {
	var w windowReceipt
	raw, e := privateRaw(path, 1<<20)
	if e != nil {
		return replayStatus{}, refused
	}
	if strictWindowJSON(raw, &w) != nil || validateWindowScope(w, p) != nil {
		return replayStatus{}, refused
	}
	dir := filepath.Dir(path)
	paths := map[string]string{}
	for _, kind := range []string{"movements", "validators", "differences", "log", "status"} {
		x, ok := w.Files[kind]
		if !ok {
			return replayStatus{}, refused
		}
		paths[kind], e = boundFile(dir, kind, x)
		if e != nil {
			return replayStatus{}, refused
		}
	}
	if verifyWindowArtifacts(dir, w) != nil {
		return replayStatus{}, refused
	}
	if filepath.Clean(status) != paths["status"] {
		return replayStatus{}, refused
	}
	raw, e = privateRaw(paths["status"], 1<<20)
	if e != nil {
		return replayStatus{}, refused
	}
	// The durable successful report was emitted after native state/results/parent/
	// validator checks and the complete-height movement sync. No direct retained
	// per-height native AppHash/raw response proof is claimed by this component.
	log, e := privateRaw(paths["log"], 16<<20)
	if e != nil {
		return replayStatus{}, refused
	}
	if validateOriginalWindow(raw, log) != nil {
		return replayStatus{}, refused
	}
	count, err := validateWindowStreams(paths, records, differences, p, w.Files["movements"].SHA)
	if err != nil {
		return replayStatus{}, refused
	}
	return replayStatus{Release: replayrecords.Release034, Chain: "cosmoshub-2", From: w.From, Committed: w.Through, Verified: w.Through, Diffs: count}, nil
}

// The public loader pins all bounds. This helper permits small exhaustive test
// fixtures without a synthetic half-million-height file.
func validateWindowStreams(paths map[string]string, records, differences string, p replayrecords.Proof, prefixSHA string) (int64, error) {
	// Preserve genesis..400000 context in the original prefix. The core mapper
	// below is per height and does not seed a ledger at a fictitious opening state.
	f, e := os.Open(paths["movements"])
	if e != nil {
		return 0, refused
	}
	defer f.Close()
	prefixProof := replayrecords.Proof{Chain: p.Chain, Release: p.Release, From: 1, Through: p.Through, VerifiedThrough: p.Through, SHA256: prefixSHA}
	selected := sha256.New()
	// Copy the exact raw line interval, preserving all fields, zeros and aborted
	// scopes. The independent Stream pass authenticates every original height.
	if replayrecords.Stream(f, prefixProof, func(replayrecords.Height) error { return nil }) != nil {
		return 0, refused
	}
	if _, e = f.Seek(0, 0); e != nil {
		return 0, refused
	}
	br := bufio.NewReader(f)
	var first, last int64
	for {
		line, e := br.ReadBytes('\n')
		if e == io.EOF {
			break
		}
		if e != nil {
			return 0, refused
		}
		var r replayrecords.Record
		if strictWindowJSON(line, &r) != nil {
			return 0, refused
		}
		if r.Height >= p.From {
			if first == 0 {
				first = r.Height
			}
			last = r.Height
			selected.Write(line)
		}
	}
	if first != p.From || last != p.Through || hex.EncodeToString(selected.Sum(nil)) != p.SHA256 {
		return 0, refused
	}
	actual, e := os.Open(records)
	if e != nil {
		return 0, refused
	}
	defer actual.Close()
	if replayrecords.Stream(actual, p, func(replayrecords.Height) error { return nil }) != nil {
		return 0, refused
	}
	// Validator changes are sparse. Genesis is retained; no invented empty-height
	// updates. Final prefix includes all changes up to the same sealed bound.
	vf, e := os.Open(paths["validators"])
	if e != nil {
		return 0, refused
	}
	defer vf.Close()
	info, e := vf.Stat()
	if e != nil || info.Size() < 1 {
		return 0, refused
	}
	var tail [1]byte
	if _, e = vf.ReadAt(tail[:], info.Size()-1); e != nil || tail[0] != '\n' {
		return 0, refused
	}
	sc := bufio.NewScanner(vf)
	sc.Buffer(make([]byte, 4096), 16<<20)
	prev := int64(-1)
	for sc.Scan() {
		var v struct {
			H int64             `json:"h"`
			U []json.RawMessage `json:"u"`
		}
		if strictWindowJSON(sc.Bytes(), &v) != nil || v.H <= prev || v.H > p.Through || len(v.U) == 0 {
			return 0, refused
		}
		if prev == -1 && v.H != 0 {
			return 0, refused
		}
		prev = v.H
	}
	if sc.Err() != nil || prev < 0 {
		return 0, refused
	}
	// The entire original discrepancy prefix is retained. Only exact raw lines
	// in the selected window may reach the existing exact-profile gate.
	df, e := os.Open(paths["differences"])
	if e != nil {
		return 0, refused
	}
	defer df.Close()
	dbr := bufio.NewReaderSize(df, replayrecords.MaxDifferenceLine+1)
	var bounded bytes.Buffer
	prev = 0
	count := int64(0)
	for {
		line, e := dbr.ReadSlice('\n')
		if e == io.EOF && len(line) == 0 {
			break
		}
		if e != nil {
			return 0, refused
		}
		token := strings.SplitN(string(line), " ", 2)[0]
		h, e := strconv.ParseInt(token, 10, 64)
		if e != nil || strconv.FormatInt(h, 10) != token || h <= prev || h > p.Through {
			return 0, refused
		}
		prev = h
		if h >= p.From {
			bounded.Write(line)
			count++
		}
	}
	raw, e := privateRaw(differences, 16<<20)
	if e != nil || !bytes.Equal(raw, bounded.Bytes()) || windowHash(raw) != p.ResultDifferencesSHA256 {
		return 0, refused
	}
	return count, nil
}
func originalReport(committed, verified int64) string {
	return " height " + strconv.FormatInt(committed, 10) + " verified " + strconv.FormatInt(verified, 10) + " rate "
}

// Canonicalize exactly Unicode simple-fold equivalence, including long-s and
// Kelvin aliases. This is linear in total key bytes, including large manifests.
func windowFieldFold(s string) string {
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

// Reject duplicate object keys, including case-fold aliases as encoding/json
// would otherwise accept them. Unknown receipt fields are also refused.
func strictWindowJSON(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func() error
	walk = func() error {
		t, e := d.Token()
		if e != nil {
			return refused
		}
		switch t {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				s, ok := k.(string)
				if e != nil || !ok {
					return refused
				}
				folded := windowFieldFold(s)
				if seen[folded] {
					return refused
				}
				seen[folded] = true
				if walk() != nil {
					return refused
				}
			}
			t, e = d.Token()
			if e != nil || t != json.Delim('}') {
				return refused
			}
		case json.Delim('['):
			for d.More() {
				if walk() != nil {
					return refused
				}
			}
			t, e = d.Token()
			if e != nil || t != json.Delim(']') {
				return refused
			}
		}
		return nil
	}
	if walk() != nil {
		return refused
	}
	if _, e := d.Token(); e != io.EOF {
		return refused
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return refused
	}
	return nil
}
