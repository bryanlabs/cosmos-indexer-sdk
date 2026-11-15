//go:build ops818dynamic0607v1 && ops818harness && ops818cohorts0607

package taxapi

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type dyAdmission struct {
	Source           string   `json:"source"`
	Window           dyWindow `json:"window"`
	Events           string   `json:"events"`
	EventsSHA        string   `json:"events_sha256"`
	CompleteProof    string   `json:"complete_proof"`
	CompleteProofSHA string   `json:"complete_proof_sha256"`
}

type dySourceProof struct {
	Source         string          `json:"source"`
	Address        string          `json:"address"`
	Since          time.Time       `json:"since"`
	End            time.Time       `json:"asof_exclusive"`
	Key            string          `json:"canonical_window"`
	Input          string          `json:"input_path"`
	InputSHA       string          `json:"input_sha256"`
	Events         string          `json:"events_path"`
	EventsSHA      string          `json:"events_sha256"`
	Receipt        string          `json:"receipt_path"`
	ReceiptSHA     string          `json:"receipt_sha256"`
	Raw            string          `json:"raw_path"`
	RawSHA         string          `json:"compressed_sha256"`
	Credits        string          `json:"verified_credits_path"`
	CreditsSHA     string          `json:"verified_credits_sha256"`
	Build          string          `json:"source_build_binding_path"`
	BuildSHA       string          `json:"source_build_binding_sha256"`
	Uniqueness     string          `json:"uniqueness_path"`
	UniquenessSHA  string          `json:"uniqueness_sha256"`
	Finalizer      string          `json:"finalizer_path"`
	FinalizerSHA   string          `json:"finalizer_sha256"`
	InputRows      int             `json:"input_tax_rows"`
	FlowCount      int             `json:"flow_event_count"`
	NativeComplete bool            `json:"native_complete"`
	Readonly       string          `json:"readonly"`
	XID            json.RawMessage `json:"assigned_xid"`
}

// An immutable per-window admission is not an old nonfee equality receipt or a
// claim that multiple source snapshots were one. It binds the actual new proof.
func dyAdmissionEvents(l *dyLoader, a string, start, end time.Time) ([]flowEvent, bool, error) {
	root := os.Getenv("OPS818_DYNAMIC_ADMISSIONS")
	if root == "" {
		return nil, false, nil
	}
	key := dyID(a, start, end)
	path := filepath.Join(root, key+".private.json")
	var adm dyAdmission
	if e := dyRead(path, &adm); os.IsNotExist(e) {
		return nil, false, nil
	} else if e != nil {
		return nil, false, e
	}
	w := adm.Window
	if adm.Source != dySource || w.Key != key || w.Address != a || !w.Since.Equal(start) || !w.End.Equal(end) || !start.Before(end) {
		return nil, false, errors.New("dynamic admission scope mismatch")
	}
	for p, want := range map[string]string{w.Input: w.InputSHA, w.Receipt: w.ReceiptSHA, adm.Events: adm.EventsSHA, adm.CompleteProof: adm.CompleteProofSHA} {
		got, e := dyHash(p)
		if want == "" || e != nil || got != want {
			return nil, false, errors.New("dynamic admission immutable bytes mismatch")
		}
	}
	var proof dySourceProof
	if e := dyRead(adm.CompleteProof, &proof); e != nil {
		return nil, false, e
	}
	if proof.Source != dySource || proof.Address != a || !proof.Since.Equal(start) || !proof.End.Equal(end) || proof.Key != key || proof.Input != w.Input || proof.InputSHA != w.InputSHA || proof.Events != adm.Events || proof.EventsSHA != adm.EventsSHA || proof.Receipt != w.Receipt || proof.ReceiptSHA != w.ReceiptSHA || !proof.NativeComplete || proof.Readonly != "on" || string(proof.XID) != "null" {
		return nil, false, errors.New("dynamic new source proof not complete or bound")
	}
	dependencies := map[string]string{proof.Raw: proof.RawSHA, proof.Credits: proof.CreditsSHA, proof.Build: proof.BuildSHA, proof.Uniqueness: proof.UniquenessSHA, proof.Finalizer: proof.FinalizerSHA}
	for p, want := range dependencies {
		got, e := dyHash(p)
		if want == "" || e != nil || got != want {
			return nil, false, errors.New("dynamic raw/credit/build source dependency mismatch")
		}
	}
	var input reportInput
	if e := dyRead(w.Input, &input); e != nil || len(input.Wallets) != 1 || input.Fingerprint != "" {
		return nil, false, errors.New("dynamic full typed input contract rejected")
	}
	var events []flowEvent
	if e := dyRead(adm.Events, &events); e != nil {
		return nil, false, e
	}
	if len(input.Wallets[0].Events) != proof.InputRows || len(events) != proof.FlowCount {
		return nil, false, errors.New("dynamic full input/event count mismatch")
	}
	for i, event := range events {
		if event.Key.Time.Before(start) || !event.Key.Time.Before(end) || (i > 0 && event.Key.before(events[i-1].Key)) {
			return nil, false, errors.New("dynamic events bounds/order rejected")
		}
	}
	if old, ok := l.windows[key]; ok {
		if !dyEqual(old, w) {
			return nil, false, errors.New("dynamic window conflicts with retained source")
		}
	} else {
		l.windows[key] = w
		l.manifest.Windows = append(l.manifest.Windows, w)
	}
	if l.used == nil {
		l.used = map[string]string{}
	}
	for _, p := range []string{path, w.Input, w.Receipt, adm.Events, adm.CompleteProof} {
		got, e := dyHash(p)
		if e != nil {
			return nil, false, e
		}
		if old, ok := l.used[p]; ok && old != got {
			return nil, false, errors.New("dynamic admitted source changed")
		}
		l.used[p] = got
	}
	for p, got := range dependencies {
		if prior, ok := l.used[p]; ok && prior != got {
			return nil, false, errors.New("dynamic raw source dependency changed")
		}
		l.used[p] = got
	}
	return events, true, nil
}
