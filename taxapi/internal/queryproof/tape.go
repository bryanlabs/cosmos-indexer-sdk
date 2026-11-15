// Package queryproof is an opt-in test harness. Production binaries do not
// import it. A replay has no network or database fallback.
package queryproof

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const MaxBundleBytes = 64 << 20
const MaxResponseBytes = 8 << 20

func Digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func encoded(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

type Source struct {
	Kind                   string
	ExportedSnapshot       string
	PGSnapshot             string
	DatabaseIdentitySHA256 string
	ReadOnly               bool
	Isolation              string
	AdmissionSHA256        string
	AdmittedRootSHA256     string
}
type Identity struct {
	SourceCommit            string
	ReferenceNativeSHA256   string
	HarnessExecutableSHA256 string
	HarnessSourceSHA256     string
	Version                 string
	RootSHA256              string
	ProfileSHA256           string
	CorrectionsSHA256       string
	From                    string
	AsOf                    string
}
type Entry struct {
	Kind     string
	Key      string
	Request  json.RawMessage
	Response json.RawMessage
	SHA256   string
	Snapshot string
}
type Bundle struct {
	Schema        string
	Source        Source
	Identity      Identity
	Entries       []Entry
	Complete      bool
	FailureSHA256 string
}
type Tape struct {
	mu     sync.Mutex
	bundle Bundle
	replay bool
	byKey  map[string][]int
	used   map[string]int
	bytes  int
	failed error
	sealed bool
}

func NewCapture(source Source, identity Identity) (*Tape, error) {
	if !source.ReadOnly || source.Isolation != "repeatable read" || source.ExportedSnapshot == "" || source.PGSnapshot == "" || len(source.DatabaseIdentitySHA256) != 64 {
		return nil, errors.New("proof source must be an identified exported read-only repeatable-read snapshot")
	}
	if source.Kind != "scratch" && source.Kind != "admitted-source" {
		return nil, errors.New("proof source kind required")
	}
	if len(identity.RootSHA256) != 64 || identity.SourceCommit == "" || len(identity.HarnessExecutableSHA256) != 64 || len(identity.HarnessSourceSHA256) != 64 {
		return nil, errors.New("proof root, implementation and actual harness identity required")
	}
	if source.Kind == "admitted-source" && (len(source.AdmissionSHA256) != 64 || source.AdmittedRootSHA256 != identity.RootSHA256 || len(identity.ProfileSHA256) != 64 || len(identity.CorrectionsSHA256) != 64) {
		return nil, errors.New("source admission/root/profile binding required")
	}
	bundle := Bundle{Schema: "query-proof-v1", Source: source, Identity: identity}
	return &Tape{bundle: bundle, used: map[string]int{}, bytes: len(encoded(bundle)) + 1024}, nil
}
func (t *Tape) Source() Source     { return t.bundle.Source }
func (t *Tape) Identity() Identity { return t.bundle.Identity }
func key(kind string, request []byte) string {
	return Digest(append(append([]byte(kind), 0), request...))
}
func (t *Tape) Refuse(err error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failed == nil {
		t.failed = err
	}
	return err
}
func (t *Tape) Record(kind string, request, response []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.replay || t.sealed {
		return errors.New("cannot record into replay or sealed capture")
	}
	if len(response) > MaxResponseBytes || t.bytes+len(request)+len(response) > MaxBundleBytes {
		t.failed = errors.New("proof input bound exceeded")
		return t.failed
	}
	if !json.Valid(request) || !json.Valid(response) {
		t.failed = errors.New("proof records require complete JSON")
		return t.failed
	}
	e := Entry{Kind: kind, Key: key(kind, request), Request: append([]byte(nil), request...), Response: append([]byte(nil), response...), Snapshot: t.bundle.Source.ExportedSnapshot}
	e.SHA256 = Digest(encoded(struct {
		Kind, Key, Snapshot string
		Request, Response   json.RawMessage
	}{e.Kind, e.Key, e.Snapshot, e.Request, e.Response}))
	entryBytes := len(encoded(e)) + 1
	if t.bytes+entryBytes > MaxBundleBytes {
		t.failed = errors.New("encoded proof input bound exceeded")
		return t.failed
	}
	t.bundle.Entries = append(t.bundle.Entries, e)
	t.bytes += entryBytes
	return nil
}
func (t *Tape) Take(kind string, request []byte) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := key(kind, request)
	list := t.byKey[k]
	n := t.used[k]
	if !t.replay || n >= len(list) {
		t.failed = errors.New("required frozen input is missing or exhausted; no fallback")
		return nil, t.failed
	}
	t.used[k]++
	return append([]byte(nil), t.bundle.Entries[list[n]].Response...), nil
}
func (t *Tape) Save(directory string) (string, error) { return t.save(directory, true) }

// SaveAttempt retains failed or partial inputs but makes them ineligible for replay.
func (t *Tape) SaveAttempt(directory string) (string, error) { return t.save(directory, false) }
func (t *Tape) save(directory string, complete bool) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failed != nil && complete {
		return "", errors.New("failed capture cannot publish complete bundle")
	}
	if t.replay || t.sealed {
		return "", errors.New("replay or sealed capture cannot publish capture")
	}
	t.bundle.Complete = complete
	if t.failed != nil {
		t.bundle.FailureSHA256 = Digest([]byte(t.failed.Error()))
	}
	b := encoded(t.bundle)
	if len(b) > MaxBundleBytes {
		return "", errors.New("encoded proof bundle bound exceeded")
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(filepath.Join(directory, "bundle.private.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	t.sealed = true
	return Digest(b), nil
}
func Load(path, expected string) (*Tape, error) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > MaxBundleBytes {
		return nil, errors.New("bounded regular proof bundle required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(expected) != 64 || Digest(b) != expected {
		return nil, errors.New("proof bundle digest mismatch")
	}
	var bundle Bundle
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&bundle); err != nil {
		return nil, err
	}
	if dec.Decode(new(any)) != io.EOF {
		return nil, errors.New("proof trailing data")
	}
	if !bundle.Complete || bundle.FailureSHA256 != "" {
		return nil, errors.New("partial or failed capture is not replayable")
	}
	if bundle.Schema != "query-proof-v1" {
		return nil, errors.New("proof schema mismatch")
	}
	t, err := NewCapture(bundle.Source, bundle.Identity)
	if err != nil {
		return nil, err
	}
	t.bundle = bundle
	t.replay = true
	t.byKey = map[string][]int{}
	for i, e := range bundle.Entries {
		if !json.Valid(e.Request) || !json.Valid(e.Response) || e.Key != key(e.Kind, e.Request) || e.Snapshot != bundle.Source.ExportedSnapshot || len(e.Response) > MaxResponseBytes {
			return nil, errors.New("proof input binding mismatch")
		}
		digest := Digest(encoded(struct {
			Kind, Key, Snapshot string
			Request, Response   json.RawMessage
		}{e.Kind, e.Key, e.Snapshot, e.Request, e.Response}))
		if e.SHA256 != digest {
			return nil, errors.New("proof entry digest mismatch")
		}
		t.byKey[e.Key] = append(t.byKey[e.Key], i)
	}
	return t, nil
}

// Finish refuses unused input except the exact identical duplicate coverage
// count omitted by b76. There is no general optional-query or cache escape.
func (t *Tape) Finish(allowDuplicateCoverage bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failed != nil {
		return errors.New("proof input stream was refused")
	}
	if !t.replay {
		return nil
	}
	for k, list := range t.byKey {
		n := t.used[k]
		if n == len(list) {
			continue
		}
		if !allowDuplicateCoverage || n == 0 || len(list) != 2*n {
			return errors.New("unused frozen input; proof incomplete")
		}
		var request SQLRequest
		if t.bundle.Entries[list[0]].Kind != "sql-query" || json.Unmarshal(t.bundle.Entries[list[0]].Request, &request) != nil || !coverageSQL(request.Query) {
			return errors.New("unused input is not the reviewed duplicate coverage count")
		}
		for _, i := range list[1:] {
			if !bytes.Equal(t.bundle.Entries[list[0]].Response, t.bundle.Entries[i].Response) {
				return errors.New("duplicate count responses differ")
			}
		}
	}
	return nil
}
func coverageSQL(q string) bool {
	return strings.Join(strings.Fields(q), " ") == `SELECT count(*) FROM "blocks" WHERE chain_id IN ($1) AND height > $2 AND height <= $3 AND tx_indexed AND block_events_indexed`
}

// CanonicalJSON preserves every field except specifically named top-level
// envelope fields. Arrays are sorted as sets only when the caller opts in.
func CanonicalJSON(raw []byte, omit []string, sortArray bool) ([]byte, error) {
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid comparison JSON")
	}
	if len(omit) > 0 {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, errors.New("comparison envelope object required")
		}
		for _, k := range omit {
			delete(m, k)
		}
	}
	if sortArray {
		a, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("comparison rows array required")
		}
		sort.Slice(a, func(i, j int) bool { return bytes.Compare(encoded(a[i]), encoded(a[j])) < 0 })
	}
	return json.Marshal(v)
}
