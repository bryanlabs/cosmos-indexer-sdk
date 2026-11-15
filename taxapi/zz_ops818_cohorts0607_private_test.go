//go:build ops818harness && ops818cohorts0607

package taxapi

import (
	"bufio"
	"compress/gzip"
	"container/heap"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
)

const xcSource = "6754a0d7d7986e48e3e3467b2448aae84d2c1b92"
const xcBase = "/Users/danb/.cache/ops818-handoff/route-acceptance-20261114T0454"

var xcCohort = os.Getenv("H_XC_COHORT")

var xcRoot = "/Users/danb/.cache/ops818-handoff/current-lot-continuation-6754-20261114T1358"

type xcRule struct {
	Rule      string                `json:"rule"`
	Liquid    []flowPart            `json:"liquid"`
	Staked    map[string][]flowPart `json:"staked"`
	Unbonding map[string][]flowPart `json:"unbonding"`
	Locked    []flowPart            `json:"locked"`
	Bids      map[string][]flowPart `json:"bids"`
	Total     decimal.Decimal       `json:"total"`
	Overdraw  decimal.Decimal       `json:"overdraw"`
}
type xcState struct {
	Address string                      `json:"address"`
	Since   time.Time                   `json:"since"`
	Hop     int                         `json:"hop"`
	Known   bool                        `json:"opening_known"`
	Source  CommissionFlowHoldingSource `json:"opening_source"`
	Rules   []xcRule                    `json:"rules"`
}

func xcSnapshot(w *flowWallet) xcState {
	s := xcState{Address: w.Addr, Since: w.Since, Hop: w.Hop, Known: w.Known, Source: w.OpeningSource}
	for r, p := range w.pools {
		x := xcRule{Rule: flowRuleNames[r], Liquid: append([]flowPart(nil), p.liquid.parts...), Locked: append([]flowPart(nil), p.locked.parts...), Staked: map[string][]flowPart{}, Unbonding: map[string][]flowPart{}, Bids: map[string][]flowPart{}, Total: p.total(), Overdraw: w.Overdraw[r]}
		for _, pair := range []struct {
			from map[string]*flowPool
			to   map[string][]flowPart
		}{{p.staked, x.Staked}, {p.unbonding, x.Unbonding}, {p.bids, x.Bids}} {
			for k, v := range pair.from {
				pair.to[k] = append([]flowPart(nil), v.parts...)
			}
		}
		s.Rules = append(s.Rules, x)
	}
	return s
}
func xcWallet(s xcState) (*flowWallet, error) {
	if len(s.Rules) != flowRules {
		return nil, errors.New("state rule count")
	}
	w := &flowWallet{Addr: s.Address, Since: s.Since, Hop: s.Hop, Known: s.Known, OpeningSource: s.Source}
	for r, x := range s.Rules {
		if x.Rule != flowRuleNames[r] {
			return nil, errors.New("state rule order")
		}
		groups := [][]flowPart{x.Liquid, x.Locked}
		for _, buckets := range []map[string][]flowPart{x.Staked, x.Unbonding, x.Bids} {
			for _, parts := range buckets {
				groups = append(groups, parts)
			}
		}
		if x.Total.IsNegative() || x.Overdraw.IsNegative() {
			return nil, errors.New("negative state total")
		}
		for _, parts := range groups {
			for _, part := range parts {
				if part.Amount.IsNegative() {
					return nil, errors.New("negative pool part")
				}
			}
		}
		p := newFlowPools(r)
		p.liquid.parts = append([]flowPart(nil), x.Liquid...)
		p.locked.parts = append([]flowPart(nil), x.Locked...)
		for _, pair := range []struct {
			from map[string][]flowPart
			to   map[string]*flowPool
		}{{x.Staked, p.staked}, {x.Unbonding, p.unbonding}, {x.Bids, p.bids}} {
			for k, v := range pair.from {
				pair.to[k] = &flowPool{rule: r, parts: append([]flowPart(nil), v...)}
			}
		}
		if !p.total().Equal(x.Total) {
			return nil, errors.New("state pool total mismatch")
		}
		w.pools[r] = p
		w.Overdraw[r] = x.Overdraw
	}
	return w, nil
}
func xcEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func xcHash(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	_, e = io.Copy(h, f)
	return fmt.Sprintf("%x", h.Sum(nil)), e
}
func xcRead(path string, v any) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func xcSave(path string, v any) error {
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	e = json.NewEncoder(f).Encode(v)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	return ce
}
func xcLines(path string, fn func([]byte) error) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	var reader io.Reader = f
	if filepath.Ext(path) == ".gz" {
		z, e := gzip.NewReader(f)
		if e != nil {
			return e
		}
		defer z.Close()
		reader = z
	}
	s := bufio.NewScanner(reader)
	s.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for s.Scan() {
		if e = fn(s.Bytes()); e != nil {
			return e
		}
	}
	return s.Err()
}
func xcID(a string, s, e time.Time) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(a+"|"+s.UTC().Format(time.RFC3339Nano)+"|"+e.UTC().Format(time.RFC3339Nano))))
}

type xcWindow struct {
	Key        string    `json:"key"`
	Address    string    `json:"address"`
	Since      time.Time `json:"since"`
	End        time.Time `json:"end"`
	Input      string    `json:"input"`
	InputSHA   string    `json:"input_sha256"`
	Receipt    string    `json:"receipt"`
	ReceiptSHA string    `json:"receipt_sha256"`
}
type xcManifest struct {
	Source  string `json:"source"`
	Old     string `json:"old"`
	Capture string `json:"old_capture"`
	Files   map[string]struct {
		SHA   string `json:"sha256"`
		Bytes int64  `json:"bytes"`
	} `json:"files"`
	Windows  []xcWindow        `json:"windows"`
	Statuses []json.RawMessage `json:"statuses"`
}
type xcOpening struct {
	Address                                  string
	Day                                      time.Time
	Holdings                                 flowHoldings
	Known                                    bool
	Error                                    string
	SourcePath, SourceSHA256, OriginalSource string
	IndependentlyDecoded                     bool
}
type xcLoader struct {
	manifest       xcManifest
	windows        map[string]xcWindow
	openings       map[string]xcOpening
	stops          map[string]flowStop
	loaded         map[string]bool
	used           map[string]string
	pendingKind    string
	pendingDetails any
	requested      map[string]bool
	cursor         *flowKey
	priorApplied   int
	settled        bool
}

func (l *xcLoader) opening(a string, day time.Time) (flowHoldings, bool, error) {
	o, ok := l.openings[xcID(a, day, day)]
	if !ok {
		return flowHoldings{}, false, l.need("retained-opening", map[string]any{"address": a, "day": day})
	}
	if !o.Known || o.Error != "<nil>" {
		return flowHoldings{}, false, errors.New("invalid exact opening")
	}
	if e := currentAppend(filepath.Join(xcRoot, "continuation-openings.private.jsonl"), map[string]any{"address": a, "day": day, "retained_opening_evidence": o}); e != nil {
		return flowHoldings{}, false, e
	}
	return o.Holdings, true, nil
}
func (l *xcLoader) need(kind string, details any) error {
	l.pendingKind = kind
	l.pendingDetails = details
	b, _ := json.Marshal(details)
	key := kind + string(b)
	if l.requested == nil {
		l.requested = map[string]bool{}
	}
	if !l.requested[key] {
		if e := currentAppend(filepath.Join(xcRequestRoot, "extra-bank-request.private.jsonl"), map[string]any{"kind": kind, "request": details, "source": xcSource, "phase": "blocked-awaiting-parent", "partial": true, "closing_reconciled": false, "financially_accepted": false}); e != nil {
			return e
		}
		l.requested[key] = true
	}
	return fmt.Errorf("blocked-awaiting-parent:%s", kind)
}
func (l *xcLoader) stop(a string) (flowStop, error) {
	if s, ok := l.stops[a]; ok {
		return s, nil
	}
	return flowStop{}, l.need("source-bound-stop", map[string]any{"address": a})
}
func (l *xcLoader) ibcDestination(e flowEvent) string {
	// No network inference. Every new channel must have the original source-bound destination.
	key := detailValue(e.Detail, "packet_src_port") + "/" + detailValue(e.Detail, "packet_src_channel")
	if d, ok := xcChannels[key]; ok {
		if e.Trigger == flowSkipSwap {
			return d + " (swap)"
		}
		return d
	}
	if detailValue(e.Detail, "packet_src_port") == "" || detailValue(e.Detail, "packet_src_channel") == "" {
		dest := "unknown channel"
		if c := detailValue(e.Detail, "packet_destination_client"); c != "" {
			dest = "client " + c
		}
		if e.Trigger == flowSkipSwap {
			dest += " (swap)"
		}
		return dest
	}
	// xcRun and events preflight prevent any unresolved source-bound mapping here.
	panic("unbound private IBC destination")
}

var xcChannels = map[string]string{}
var xcRequestRoot = "/Users/danb/.cache/ops818-handoff/current-lot-continuation-6754-20261114T1358"

func (l *xcLoader) supplement() error {
	path := filepath.Join(xcRequestRoot, "source-evidence.private.json")
	var binding struct {
		Source  string `json:"source"`
		RawPath string `json:"raw_path"`
		RawSHA  string `json:"raw_sha256"`
	}
	if e := xcRead(path, &binding); os.IsNotExist(e) {
		return nil
	} else if e != nil {
		return e
	}
	h, e := xcHash(binding.RawPath)
	if e != nil || h != binding.RawSHA || binding.Source != xcSource {
		return errors.New("supplement source hash mismatch")
	}
	var raw struct {
		Source      string          `json:"source"`
		Readonly    string          `json:"readonly"`
		AssignedXID json.RawMessage `json:"assigned_xid"`
		Stops       []struct {
			Address string   `json:"address"`
			Stop    flowStop `json:"stop"`
		} `json:"stops"`
		Channels []IBCChannel `json:"channels"`
		Openings []xcOpening  `json:"openings"`
	}
	if e = xcRead(binding.RawPath, &raw); e != nil {
		return e
	}
	if raw.Source != xcSource || raw.Readonly != "on" || string(raw.AssignedXID) != "null" {
		return errors.New("supplement read-only/source proof mismatch")
	}
	for _, x := range raw.Stops {
		if old, ok := l.stops[x.Address]; ok && !xcEqual(old, x.Stop) {
			return errors.New("supplement attempted to relabel retained stop")
		}
		l.stops[x.Address] = x.Stop
	}
	for _, x := range raw.Channels {
		if x.Chain != apiMainnet || x.Port == "" || x.Channel == "" || x.ChainID == "" {
			return errors.New("supplement channel identity missing")
		}
		key := x.Port + "/" + x.Channel
		if old, ok := xcChannels[key]; ok && old != x.ChainID {
			return errors.New("supplement channel conflict")
		}
		xcChannels[key] = x.ChainID
	}
	for _, x := range raw.Openings {
		h, e := xcHash(x.SourcePath)
		if e != nil || h != x.SourceSHA256 || !x.Known || x.Error != "<nil>" {
			return errors.New("supplement opening proof mismatch")
		}
		key := xcID(x.Address, x.Day, x.Day)
		if old, ok := l.openings[key]; ok && !currentHoldingsMoneyEqual(old.Holdings, x.Holdings) {
			return errors.New("supplement opening conflict")
		}
		l.openings[key] = x
	}
	if prior, ok := l.used[binding.RawPath]; ok && prior != h {
		return errors.New("immutable supplement changed")
	}
	l.used[binding.RawPath] = h
	return nil
}

func (l *xcLoader) cached(a string, start, end time.Time) ([]flowEvent, error) {
	key := xcID(a, start, end)
	path := filepath.Join(l.manifest.Capture, "windows", key+".events.private.json")
	var binding struct {
		Address  string    `json:"address"`
		Since    time.Time `json:"since"`
		End      time.Time `json:"asof_exclusive"`
		Source   string    `json:"source"`
		Input    string    `json:"source_input"`
		InputSHA string    `json:"source_input_sha256"`
		Count    int       `json:"converted_native_flow_events"`
	}
	bindingPath := filepath.Join(l.manifest.Capture, "windows", key+".binding.private.json")
	var be error
	for attempt := 0; attempt < 20; attempt++ {
		be = xcRead(bindingPath, &binding)
		if be == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if be != nil {
		return nil, errors.New("converted binding unavailable or invalid after local write grace")
	}
	w, ok := l.windows[key]
	if !ok {
		expected := filepath.Join(l.manifest.Capture, "windows", key+".input.private.json")
		if binding.Input != expected {
			return nil, errors.New("unscoped converted input provenance invalid")
		}
		w = xcWindow{Address: a, Since: start, End: end, Input: expected, InputSHA: binding.InputSHA}
	}
	if binding.Address != a || !binding.Since.Equal(start) || !binding.End.Equal(end) || binding.Source != xcSource || binding.Input != w.Input || binding.InputSHA != w.InputSHA {
		return nil, errors.New("converted input/window/source binding mismatch")
	}
	if ok {
		rh, re := xcHash(w.Receipt)
		if re != nil || rh != w.ReceiptSHA {
			return nil, errors.New("converted native receipt changed")
		}
	}
	h, e := xcHash(binding.Input)
	if e != nil || h != binding.InputSHA {
		return nil, errors.New("converted source bytes mismatch")
	}
	var events []flowEvent
	if e = xcRead(path, &events); e != nil {
		return nil, e
	}
	if len(events) != binding.Count {
		return nil, errors.New("converted count mismatch")
	}
	for i, event := range events {
		if event.Key.Time.Before(start) || !event.Key.Time.Before(end) || (i > 0 && event.Key.before(events[i-1].Key)) {
			return nil, errors.New("converted bounds/order mismatch")
		}
	}
	eh, e := xcHash(path)
	if e != nil {
		return nil, e
	}
	bh, e := xcHash(filepath.Join(l.manifest.Capture, "windows", key+".binding.private.json"))
	if e != nil {
		return nil, e
	}
	if prior, ok := l.used[path]; ok && prior != eh {
		return nil, errors.New("converted immutable bytes changed")
	}
	l.used[path] = eh
	l.used[filepath.Join(l.manifest.Capture, "windows", key+".binding.private.json")] = bh
	return events, nil
}
func (l *xcLoader) events(a string, start, end time.Time) ([]flowEvent, error) {
	key := xcID(a, start, end)
	path := filepath.Join(l.manifest.Capture, "windows", key+".events.private.json")
	if _, e := os.Stat(path); e == nil {
		events, e := l.cached(a, start, end)
		if e == nil {
			for _, event := range events {
				if !xcChannelKnown(event) && (l.cursor == nil || event.Key.before(*l.cursor)) {
					return nil, l.need("source-bound-channel", map[string]any{"address": a, "since": start, "asof_exclusive": end, "canonical_window": key, "converted_binding": filepath.Join(l.manifest.Capture, "windows", key+".binding.private.json"), "event": event, "port": detailValue(event.Detail, "packet_src_port"), "channel": detailValue(event.Detail, "packet_src_channel")})
				}
			}
		}
		if e == nil {
			e = currentAppend(filepath.Join(xcRoot, "continuation-loaded.private.jsonl"), map[string]any{"address": a, "since": start, "asof_exclusive": end, "canonical_window": key, "converted_cache_reused": true})
		}
		return events, e
	}
	w, ok := l.windows[key]
	if !ok {
		return nil, l.need("new-exact-native-input-and-bank", map[string]any{"address": a, "since": start, "asof_exclusive": end, "canonical_window": key, "native_read_not_duplicated": true})
	}
	verified := filepath.Join(xcRoot, "windows", key+".verified-credits.private.json")
	if _, e := os.Stat(verified); os.IsNotExist(e) {
		cmd := exec.Command("python3", filepath.Join(xcBase, "prepare_cohorts0607_private.py"), w.Key, verified)
		cmd.Env = append(os.Environ(), "H_XC_OUTPUT_ROOT="+xcRoot)
		// The verifier emits phases only. Its private log is not sent to the terminal.
		b, e := cmd.CombinedOutput()
		if e != nil {
			if x, ok := e.(*exec.ExitError); ok && x.ExitCode() == 3 {
				return nil, l.need("native-bank-adapter", map[string]any{"address": a, "since": start, "asof_exclusive": end, "window": w.Key, "canonical_window": key, "current_input": w.Input, "current_input_sha256": w.InputSHA})
			}
			_ = b
			return nil, errors.New("actual bank credit verification rejected")
		}
	}
	var admitted struct {
		Credits struct {
			Address  string    `json:"address"`
			Since    time.Time `json:"since"`
			End      time.Time `json:"asof_exclusive"`
			InputSHA string    `json:"current_input_sha256"`
			Records  []struct {
				Message uint   `json:"message"`
				Coin    string `json:"coin"`
			} `json:"credits"`
		} `json:"credits"`
		Verified bool `json:"verified_against_actual_bank_attributes"`
	}
	if e := xcRead(verified, &admitted); e != nil {
		return nil, e
	}
	if !admitted.Verified || admitted.Credits.Address != a || !admitted.Credits.Since.Equal(start) || !admitted.Credits.End.Equal(end) || admitted.Credits.InputSHA != w.InputSHA {
		return nil, errors.New("verified credit window mismatch")
	}
	h, e := xcHash(w.Input)
	if e != nil || h != w.InputSHA {
		return nil, errors.New("native input changed")
	}
	var input reportInput
	if e = xcRead(w.Input, &input); e != nil {
		return nil, e
	}
	if len(input.Wallets) != 1 {
		return nil, errors.New("native wallet count mismatch")
	}
	credited := map[flowCredit]bool{}
	for _, c := range admitted.Credits.Records {
		credited[flowCredit{c.Message, c.Coin}] = true
	}
	events, e := xcConvert(input, a, start, end, credited)
	if e != nil {
		return nil, e
	}
	for _, event := range events {
		if !xcChannelKnown(event) && (l.cursor == nil || event.Key.before(*l.cursor)) {
			return nil, l.need("source-bound-channel", map[string]any{"address": a, "since": start, "asof_exclusive": end, "canonical_window": key, "source_input": w.Input, "source_input_sha256": w.InputSHA, "event": event, "port": detailValue(event.Detail, "packet_src_port"), "channel": detailValue(event.Detail, "packet_src_channel")})
		}
	}
	if e = xcSave(filepath.Join(xcRoot, "windows", key+".events.private.json"), events); e != nil {
		return nil, e
	}
	if e = xcSave(filepath.Join(xcRoot, "windows", key+".binding.private.json"), map[string]any{"address": a, "since": start, "asof_exclusive": end, "source": xcSource, "source_input": w.Input, "source_input_sha256": w.InputSHA, "source_receipt": w.Receipt, "source_receipt_sha256": w.ReceiptSHA, "verified_credits": verified, "converted_native_flow_events": len(events), "projection": "unchanged production flowEventFromRow with actual native credit map and current fee rows, not bank-cash financial acceptance", "financially_accepted": false}); e != nil {
		return nil, e
	}
	for _, path := range []string{filepath.Join(xcRoot, "windows", key+".events.private.json"), filepath.Join(xcRoot, "windows", key+".binding.private.json"), verified} {
		h, x := xcHash(path)
		if x != nil {
			return nil, x
		}
		l.used[path] = h
	}
	if e = currentAppend(filepath.Join(xcRoot, "continuation-loaded.private.jsonl"), map[string]any{"address": a, "since": start, "asof_exclusive": end, "canonical_window": key, "converted_cache_reused": false, "source_input_sha256": w.InputSHA}); e != nil {
		return nil, e
	}
	return events, nil
}
func xcConvert(input reportInput, a string, start, end time.Time, credited map[flowCredit]bool) ([]flowEvent, error) {
	rows := input.Wallets[0]
	allowed := map[uint]bool{}
	for _, row := range rows.Events {
		if flowSingleHopNative(row, DefaultNativeAsset.Denom) && row.ToAddr == a {
			allowed[row.MessageID] = true
		}
	}
	events := make([]flowEvent, 0, len(rows.Events)+len(rows.Fees))
	for _, row := range rows.Events {
		if row.Timestamp.Before(start) || !row.Timestamp.Before(end) {
			return nil, errors.New("native event bounds mismatch")
		}
		if flowSingleHopNative(row, DefaultNativeAsset.Denom) && (!allowed[row.MessageID] || !credited[flowCredit{row.MessageID, row.Amount + DefaultNativeAsset.Denom}]) {
			continue
		}
		if event, ok := flowEventFromRow(row, a, DefaultNativeAsset.Denom); ok {
			events = append(events, event)
		}
	}
	for _, fee := range rows.Fees {
		if fee.Ts.Before(start) || !fee.Ts.Before(end) {
			return nil, errors.New("native fee bounds mismatch")
		}
		if fee.Denom != DefaultNativeAsset.Denom {
			continue
		}
		amount, e := decimal.NewFromString(fee.Amount)
		if e != nil {
			return nil, errors.New("native fee invalid")
		}
		events = append(events, flowEvent{Key: flowKey{Time: fee.Ts.UTC()}, Category: apiFee, Dir: apiOut, Amount: amount, From: a})
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Key.before(events[j].Key) })
	return events, nil
}

type xcApplied struct {
	Ordinal int                `json:"ordinal"`
	From    string             `json:"from_wallet"`
	Event   flowEvent          `json:"event"`
	Before  map[string]xcState `json:"before"`
	After   map[string]xcState `json:"after"`
	New     []xcState          `json:"new_discovery_prefix_states"`
	Error   string             `json:"error"`
}
type xcTerminal struct {
	Error      string                     `json:"error"`
	Wallets    int                        `json:"wallets"`
	Pending    int                        `json:"pending_events"`
	Attempted  map[string]bool            `json:"attempted"`
	Commission map[string]decimal.Decimal `json:"commission"`
	Source     string                     `json:"current_source"`
}

func xcEnqueue(tr *flowTrace, w *flowWallet, events []flowEvent, cursor *flowKey) {
	for _, e := range events {
		if cursor != nil && e.Key.before(*cursor) {
			continue
		}
		tr.seq++
		heap.Push(&tr.queue, flowQueued{key: e.Key, seq: tr.seq, wallet: w, event: e})
	}
}

// xcRestoreRecord never reapplies a committed event. It consumes one exact
// original queue occurrence, verifies state continuity, and adopts recorded
// post-route state. New-discovery states are post-route, not pre-route prefixes.
func xcRestoreRecord(tr *flowTrace, rec xcApplied, load func(*flowWallet) ([]flowEvent, error), failed *flowQueued) error {
	if tr.queue.Len() == 0 {
		return errors.New("restore queue underflow")
	}
	q := heap.Pop(&tr.queue).(flowQueued)
	if q.wallet.Addr != rec.From || !xcEqual(q.event, rec.Event) {
		return errors.New("restore queue event/duplicate/order mismatch")
	}
	for a, s := range rec.Before {
		w := tr.Wallets[a]
		if w == nil || !xcEqual(xcSnapshot(w), s) {
			return errors.New("restore before-state discontinuity")
		}
	}
	if rec.Error != "<nil>" {
		if len(rec.New) != 0 || tr.Wallets[rec.Event.To] != nil || len(rec.Before) != 1 || len(rec.After) != 1 || rec.Event.Category != apiTransfer || rec.Event.Dir != apiOut || rec.Event.From == rec.Event.To {
			return errors.New("unsupported failed apply phase")
		}
		*failed = q
	}
	for a, s := range rec.After {
		if a != s.Address {
			return errors.New("restore after address mismatch")
		}
		old := tr.Wallets[a]
		if old == nil {
			found := false
			for _, n := range rec.New {
				if n.Address == a && xcEqual(n, s) {
					found = true
				}
			}
			if !found {
				return errors.New("unproven new state")
			}
			continue
		}
		w, e := xcWallet(s)
		if e != nil {
			return e
		}
		*old = *w
	}
	for _, s := range rec.New {
		if tr.Wallets[s.Address] != nil {
			return errors.New("duplicate discovery")
		}
		w, e := xcWallet(s)
		if e != nil {
			return e
		}
		tr.Wallets[w.Addr] = w
		tr.order = append(tr.order, w)
		events, e := load(w)
		if e != nil {
			return e
		}
		xcEnqueue(tr, w, events, &rec.Event.Key)
	}
	return nil
}
func xcRestore(l *xcLoader) (*flowTrace, xcApplied, int, error) {
	var ident struct {
		Validator string    `json:"validator"`
		From      time.Time `json:"from"`
		End       time.Time `json:"asof_exclusive"`
		Source    string    `json:"source"`
		Roots     []string  `json:"roots"`
	}
	dir := filepath.Join(l.manifest.Capture, xcCohort)
	if e := xcRead(filepath.Join(dir, "identity.private.json"), &ident); e != nil {
		return nil, xcApplied{}, 0, e
	}
	if ident.Source != xcSource {
		return nil, xcApplied{}, 0, errors.New("identity source mismatch")
	}
	tr := newFlowTrace(ident.Validator, ident.From, ident.End, l)
	tr.strict = true
	e := xcLines(filepath.Join(dir, "initial-states.private.jsonl"), func(b []byte) error {
		var s xcState
		if e := json.Unmarshal(b, &s); e != nil {
			return e
		}
		w, e := xcWallet(s)
		if e != nil {
			return e
		}
		tr.Wallets[w.Addr] = w
		tr.order = append(tr.order, w)
		events, e := l.cached(w.Addr, w.Since, tr.AsOf)
		if e != nil {
			return e
		}
		xcEnqueue(tr, w, events, nil)
		return nil
	})
	if e != nil {
		return nil, xcApplied{}, 0, e
	}
	if len(tr.order) != len(ident.Roots) {
		return nil, xcApplied{}, 0, errors.New("initial root count mismatch")
	}
	for i, a := range ident.Roots {
		if tr.order[i].Addr != a {
			return nil, xcApplied{}, 0, errors.New("initial root order mismatch")
		}
	}
	var last xcApplied
	var failed flowQueued
	n := 0
	e = xcLines(filepath.Join(dir, "applied.private.jsonl"), func(b []byte) error {
		if failed.wallet != nil {
			return errors.New("records after failure")
		}
		var rec xcApplied
		if e := json.Unmarshal(b, &rec); e != nil {
			return e
		}
		n++
		if rec.Ordinal != n {
			return errors.New("applied ordinal mismatch")
		}
		e := xcRestoreRecord(tr, rec, func(w *flowWallet) ([]flowEvent, error) { return l.cached(w.Addr, w.Since, tr.AsOf) }, &failed)
		last = rec
		if n%32768 == 0 {
			fmt.Printf("phase reconstruct count %d pending %d\n", n, tr.queue.Len())
		}
		return e
	})
	if e != nil {
		return nil, last, n, e
	}
	var terminal xcTerminal
	if e = xcRead(filepath.Join(dir, "trace-result.private.json"), &terminal); e != nil {
		return nil, last, n, e
	}
	if failed.wallet == nil || last.Error != terminal.Error || terminal.Source != xcSource || tr.queue.Len() != terminal.Pending || len(tr.order) != terminal.Wallets {
		return nil, last, n, errors.New("terminal queue/state/source mismatch")
	}
	i := 0
	e = xcLines(filepath.Join(dir, "final-pools.private.jsonl"), func(b []byte) error {
		var s xcState
		if e := json.Unmarshal(b, &s); e != nil {
			return e
		}
		if i >= len(tr.order) || !xcEqual(s, xcSnapshot(tr.order[i])) {
			return errors.New("final state/discovery order mismatch")
		}
		i++
		return nil
	})
	if e != nil || i != len(tr.order) {
		return nil, last, n, errors.New("final pools mismatch")
	}
	// Verify the receiver opening succeeded and no receiver events/registration/prefix ran.
	openingCount, loadedCount := 0, 0
	receiverOpening := false
	e = xcLines(filepath.Join(dir, "openings.private.jsonl"), func(b []byte) error {
		var x struct {
			Address  string       `json:"address"`
			Day      time.Time    `json:"day"`
			Holdings flowHoldings `json:"holdings"`
			Evidence xcOpening    `json:"retained_opening_evidence"`
			Known    bool         `json:"known"`
			Error    string       `json:"error"`
			Reused   bool         `json:"retained_opening_reused"`
		}
		if e := json.Unmarshal(b, &x); e != nil {
			return e
		}
		openingCount++
		if !x.Known || x.Error != "<nil>" || !x.Reused {
			// 06 failed in opening before receiver registration. No empty
			// opening is admitted. Keep its exact missing-source dependency.
			if x.Address != last.Event.To || x.Known || x.Reused || x.Error != terminal.Error {
				return errors.New("original opening incomplete outside failed occurrence")
			}
			if !x.Day.Equal(flowDay(last.Event.Key.Time).AddDate(0, 0, -1)) {
				return errors.New("failed opening day mismatch")
			}
			receiverOpening = true
			return nil
		}
		evidence, ok := l.openings[xcID(x.Address, x.Day, x.Day)]
		if !ok || !xcEqual(evidence, x.Evidence) || !xcEqual(x.Holdings, evidence.Holdings) {
			return errors.New("original opening evidence mismatch")
		}
		expected := flowDay(last.Event.Key.Time).AddDate(0, 0, -1)
		if w := tr.Wallets[x.Address]; w != nil {
			expected = w.Since.AddDate(0, 0, -1)
		} else if x.Address != last.Event.To {
			return errors.New("unrelated original opening")
		}
		if !x.Day.Equal(expected) {
			return errors.New("original opening day mismatch")
		}
		if x.Address == last.Event.To {
			receiverOpening = true
		}
		return nil
	})
	if e != nil {
		return nil, last, n, e
	}
	e = xcLines(filepath.Join(dir, "loaded.private.jsonl"), func(b []byte) error {
		var x struct {
			Address string    `json:"address"`
			Since   time.Time `json:"since"`
			End     time.Time `json:"asof_exclusive"`
			Window  string    `json:"window"`
		}
		if e := json.Unmarshal(b, &x); e != nil {
			return e
		}
		loadedCount++
		w := tr.Wallets[x.Address]
		if x.Address == last.Event.To || w == nil || !x.Since.Equal(w.Since) || !x.End.Equal(tr.AsOf) || x.Window != xcID(x.Address, x.Since, x.End) {
			return errors.New("failed receiver loaded or unrelated load")
		}
		l.loaded[x.Address] = true
		return nil
	})
	if e != nil {
		return nil, last, n, e
	}
	if openingCount != terminal.Wallets+1 || loadedCount != terminal.Wallets || len(l.loaded) != terminal.Wallets || !receiverOpening || !terminal.Attempted[last.Event.To] || len(terminal.Attempted) != terminal.Wallets+1 {
		return nil, last, n, errors.New("failed discovery phase proof mismatch")
	}
	for a := range tr.Wallets {
		if !terminal.Attempted[a] {
			return nil, last, n, errors.New("registered wallet missing from original attempted set")
		}
	}
	tr.attempted = terminal.Attempted
	tr.Commission = terminal.Commission
	var rows []CommissionFlow
	if e = xcRead(filepath.Join(dir, "rows.private.json"), &rows); e != nil {
		return nil, last, n, e
	}
	for _, row := range rows {
		rule := -1
		for i, name := range flowRuleNames {
			if name == row.Rule {
				rule = i
			}
		}
		if rule < 0 || row.Validator != tr.Validator {
			return nil, last, n, errors.New("partial fate identity mismatch")
		}
		a, e := decimal.NewFromString(row.AmountBase)
		if e != nil {
			return nil, last, n, e
		}
		k := FlowFateKey{rule, row.Month, row.Class, row.Detail, row.ViaUnbonding}
		if _, ok := tr.Fates[k]; ok {
			return nil, last, n, errors.New("duplicate partial fate")
		}
		tr.Fates[k] = a
	}
	e = xcLines(filepath.Join(dir, "tagged-routes.private.jsonl"), func(b []byte) error {
		var x struct {
			Event  flowEvent `json:"event"`
			Stop   flowStop  `json:"stop"`
			Loaded bool      `json:"destination_loaded"`
		}
		if e := json.Unmarshal(b, &x); e != nil {
			return e
		}
		a := x.Event.To
		if a == "" {
			return nil
		}
		if old, ok := tr.stops[a]; ok && !xcEqual(old, x.Stop) {
			return errors.New("cached stop inconsistency")
		}
		tr.stops[a] = x.Stop
		return nil
	})
	if e != nil {
		return nil, last, n, e
	}
	for a := range tr.Wallets {
		if s, ok := tr.stops[a]; ok && s.Class != "" {
			return nil, last, n, errors.New("loaded destination has nonempty cached stop")
		}
		tr.stops[a] = flowStop{}
	}
	l.stops = tr.stops
	// The failing transfer removed only parent liquid pools before events() failed.
	// Restore its exact BEFORE state and put that same original seq back, once.
	for a, s := range last.Before {
		w, e := xcWallet(s)
		if e != nil {
			return nil, last, n, e
		}
		*tr.Wallets[a] = *w
	}
	heap.Push(&tr.queue, failed)
	if tr.queue.Len() != terminal.Pending+1 {
		return nil, last, n, errors.New("failed queue reinsertion mismatch")
	}
	return tr, last, n, nil
}

type xcQueueRecord struct {
	Wallet string    `json:"source_wallet"`
	Event  flowEvent `json:"event"`
	Seq    int       `json:"seq"`
}

func xcFates(tr *flowTrace) []CommissionFlow {
	rows := []CommissionFlow{}
	for k, a := range tr.Fates {
		rows = append(rows, CommissionFlow{Chain: apiMainnet, Validator: tr.Validator, Rule: flowRuleNames[k.Rule], Month: k.Month, Class: k.Class, Detail: k.Detail, ViaUnbonding: k.ViaUnbonding, AmountBase: a.String()})
	}
	sort.Slice(rows, func(i, j int) bool { return fmt.Sprint(rows[i]) < fmt.Sprint(rows[j]) })
	return rows
}
func xcChannelKnown(e flowEvent) bool {
	if e.Category != flowCatIBCOut || e.Dir != apiOut || !e.Amount.IsPositive() {
		return true
	}
	p, c := detailValue(e.Detail, "packet_src_port"), detailValue(e.Detail, "packet_src_channel")
	if p == "" || c == "" {
		return true
	}
	_, ok := xcChannels[p+"/"+c]
	return ok
}
func xcMarkedFates(tr *flowTrace) []map[string]any {
	rows := []map[string]any{}
	for _, row := range xcFates(tr) {
		b, _ := json.Marshal(row)
		var v map[string]any
		_ = json.Unmarshal(b, &v)
		v["partial"] = true
		v["closing_reconciled"] = false
		v["financially_accepted"] = false
		rows = append(rows, v)
	}
	return rows
}
func xcExistingJSONL(dir, name string) string {
	p := filepath.Join(dir, name)
	if _, e := os.Stat(p); e == nil {
		return p
	}
	return p + ".gz"
}
func xcCheckpointWriter(path string) (*json.Encoder, func() error, error) {
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return nil, nil, e
	}
	z, e := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if e != nil {
		f.Close()
		return nil, nil, e
	}
	close := func() error {
		ze := z.Close()
		se := f.Sync()
		fe := f.Close()
		if ze != nil {
			return ze
		}
		if se != nil {
			return se
		}
		return fe
	}
	return json.NewEncoder(z), close, nil
}
func xcCheckpoint(tr *flowTrace, name, phase string, settled bool, applied int) error {
	dir := filepath.Join(xcRoot, name)
	if e := os.Mkdir(dir, 0700); e != nil {
		return e
	}
	enc, close, e := xcCheckpointWriter(filepath.Join(dir, "full-queue.private.jsonl.gz"))
	if e != nil {
		return e
	}
	// Sorting a copy preserves both the exact event order and original enqueue seq.
	q := append(flowQueue(nil), tr.queue...)
	sort.Sort(q)
	for _, x := range q {
		if e = enc.Encode(xcQueueRecord{x.wallet.Addr, x.event, x.seq}); e != nil {
			_ = close()
			return e
		}
	}
	if e = close(); e != nil {
		return e
	}
	enc, close, e = xcCheckpointWriter(filepath.Join(dir, "states.private.jsonl.gz"))
	if e != nil {
		return e
	}
	for _, w := range tr.order {
		if e = enc.Encode(xcSnapshot(w)); e != nil {
			_ = close()
			return e
		}
	}
	if e = close(); e != nil {
		return e
	}
	var statuses []json.RawMessage
	if l, ok := tr.load.(*xcLoader); ok {
		statuses = l.manifest.Statuses
		if e = xcSave(filepath.Join(dir, "loader-provenance.private.json"), map[string]any{"source": xcSource, "used_file_sha256": l.used, "native_source_windows": l.manifest.Windows, "pending_kind": l.pendingKind, "pending_request": l.pendingDetails, "partial": true, "closing_reconciled": false, "financially_accepted": false}); e != nil {
			return e
		}
	}
	if e = xcSave(filepath.Join(dir, "original-cohort-statuses.private.json"), map[string]any{"statuses": statuses, "snapshot_not_assumed_current": true, "partial": true, "closing_reconciled": false, "financially_accepted": false}); e != nil {
		return e
	}
	// Partial fate rows are inline marked. They are never presented as settled statements.
	if e = xcSave(filepath.Join(dir, "partial-rows.private.json"), map[string]any{"rows": xcMarkedFates(tr), "partial": true, "settled_once": settled, "closing_reconciled": false, "financially_accepted": false}); e != nil {
		return e
	}
	return xcSave(filepath.Join(dir, "trace-state.private.json"), map[string]any{"source": xcSource, "phase": phase, "seq": tr.seq, "pending_events": tr.queue.Len(), "applied_new": applied, "wallets": len(tr.order), "validator": tr.Validator, "from": tr.From, "asof_exclusive": tr.AsOf, "attempted": tr.attempted, "stops": tr.stops, "commission": tr.Commission, "unresolved": tr.Unresolved, "partial": true, "settled_once": settled, "closing_reconciled": false, "financially_accepted": false})
}
func xcWait(l *xcLoader) error {
	for {
		if e := l.supplement(); e != nil {
			return e
		}
		if d, ok := l.pendingDetails.(map[string]any); ok {
			if l.pendingKind == "source-bound-channel" {
				p, _ := d["port"].(string)
				c, _ := d["channel"].(string)
				if _, ok := xcChannels[p+"/"+c]; ok {
					return nil
				}
			}
			if l.pendingKind == "source-bound-stop" {
				a, _ := d["address"].(string)
				if _, ok := l.stops[a]; ok {
					return nil
				}
			}
			if l.pendingKind == "retained-opening" {
				a, _ := d["address"].(string)
				day, ok := d["day"].(time.Time)
				if ok {
					if _, ok := l.openings[xcID(a, day, day)]; ok {
						return nil
					}
				}
			}
		}
		var disk syscall.Statfs_t
		if syscall.Statfs(xcRoot, &disk) != nil || uint64(disk.Bavail)*uint64(disk.Bsize) < 4*1024*1024*1024 {
			return errors.New("persistent disk below safety reserve while waiting")
		}
		if l.pendingKind == "native-bank-adapter" {
			d, ok := l.pendingDetails.(map[string]any)
			if !ok {
				return errors.New("pending source request malformed")
			}
			key, _ := d["window"].(string)
			canonical, _ := d["canonical_window"].(string)
			_, e := os.Stat(filepath.Join(xcBase, "native-cash-semantic-adapter-20261114T1355", "windows", key, "complete.private.json"))
			_, ce := os.Stat(filepath.Join(l.manifest.Capture, "windows", canonical+".binding.private.json"))
			if e == nil || ce == nil {
				return nil
			}
		}
		// Retain the live request and literal queue until the parent supplies exact evidence.
		// No duplicate input, bank, metadata or opening reads are issued here.
		time.Sleep(time.Second)
	}
}
func xcRun(tr *flowTrace, dir string) (count int, phase string, runError error) {
	if l, ok := tr.load.(*xcLoader); ok && l.settled {
		return l.priorApplied, "already-settled-rejected", errors.New("private trace already settled")
	}
	f, e := os.OpenFile(filepath.Join(dir, "new-applied.private.jsonl.gz"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return 0, "logger-failed", e
	}
	z, e := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if e != nil {
		f.Close()
		return 0, "logger-failed", e
	}
	closed := false
	closeMember := func() error {
		if closed {
			return nil
		}
		ze := z.Close()
		se := f.Sync()
		closed = true
		if ze != nil {
			return ze
		}
		return se
	}
	defer func() {
		ze := closeMember()
		fe := f.Close()
		if ze != nil || fe != nil {
			phase = "logger-finalization-failed"
			runError = errors.New("gzip proof finalization failed")
		}
	}()
	enc := json.NewEncoder(z)
	restartMember := func() error {
		var e error
		z, e = gzip.NewWriterLevel(f, gzip.BestSpeed)
		if e != nil {
			return e
		}
		enc = json.NewEncoder(z)
		closed = false
		return nil
	}
	n := 0
	if l, ok := tr.load.(*xcLoader); ok {
		n = l.priorApplied
	}
	attempts := 0
	blocks := 0
	began := time.Now()
	for tr.queue.Len() > 0 {
		var disk syscall.Statfs_t
		if syscall.Statfs(dir, &disk) != nil || uint64(disk.Bavail)*uint64(disk.Bsize) < 4*1024*1024*1024 {
			return n, "capacity-safety-stop", errors.New("persistent disk below safety reserve")
		}
		if l, ok := tr.load.(*xcLoader); ok && !xcChannelKnown(tr.queue[0].event) {
			event := tr.queue[0].event
			_ = l.need("source-bound-channel", map[string]any{"event": event, "source_wallet": tr.queue[0].wallet.Addr, "original_enqueue_seq": tr.queue[0].seq, "port": detailValue(event.Detail, "packet_src_port"), "channel": detailValue(event.Detail, "packet_src_channel")})
			if e = closeMember(); e != nil {
				return n, "logger-finalization-failed", e
			}
			blocks++
			if e = xcCheckpoint(tr, fmt.Sprintf("blocked-channel-%06d", blocks), "blocked-awaiting-parent-channel", false, n); e != nil {
				return n, "blocked-checkpoint-failed", e
			}
			fmt.Printf("phase blocked-awaiting-parent-channel count %d pending %d\n", n, tr.queue.Len())
			if e = xcWait(l); e != nil {
				return n, "parent-source-wait-stopped", e
			}
			if e = restartMember(); e != nil {
				return n, "logger-failed", e
			}
			l.pendingKind = ""
			l.pendingDetails = nil
			continue
		}
		q := heap.Pop(&tr.queue).(flowQueued)
		w, event := q.wallet, q.event
		before := map[string]xcState{w.Addr: xcSnapshot(w)}
		if event.Category == apiTransfer && event.Dir == apiOut {
			if target := tr.Wallets[event.To]; target != nil {
				before[target.Addr] = xcSnapshot(target)
			}
		}
		prior := len(tr.order)
		if l, ok := tr.load.(*xcLoader); ok {
			key := event.Key
			l.cursor = &key
		}
		err := tr.apply(w, event, true)
		if l, ok := tr.load.(*xcLoader); ok {
			l.cursor = nil
		}
		after := map[string]xcState{w.Addr: xcSnapshot(w)}
		if target := tr.Wallets[event.To]; target != nil && event.Category == apiTransfer && event.Dir == apiOut {
			after[target.Addr] = xcSnapshot(target)
		}
		added := []xcState{}
		for _, v := range tr.order[prior:] {
			added = append(added, xcSnapshot(v))
		}
		attempts++
		rec := map[string]any{"attempt_ordinal": attempts, "ordinal": n + 1, "original_enqueue_seq": q.seq, "from_wallet": w.Addr, "event": event, "before": before, "after": after, "new_discovery_post_route_states": added, "error": fmt.Sprint(err), "partial": true, "closing_reconciled": false, "financially_accepted": false}
		if e = enc.Encode(rec); e != nil {
			return n, "logger-failed", e
		}
		if err != nil {
			// Blocked events are rewound only for the proven pre-registration discovery phase.
			if len(added) != 0 || tr.Wallets[event.To] != nil || len(before) != 1 || event.Category != apiTransfer || event.Dir != apiOut {
				return n, "failed-unrewound-inflight", err
			}
			for a, s := range before {
				restored, x := xcWallet(s)
				if x != nil {
					return n, "rewind-rejected", x
				}
				*tr.Wallets[a] = *restored
			}
			heap.Push(&tr.queue, q)
			if e = closeMember(); e != nil {
				return n, "logger-finalization-failed", e
			}
			l, ok := tr.load.(*xcLoader)
			if !ok || l.pendingKind == "" {
				return n, "failed-rewound-exact", err
			}
			blocks++
			if e = xcCheckpoint(tr, fmt.Sprintf("blocked-%06d", blocks), "blocked-awaiting-parent", false, n); e != nil {
				return n, "blocked-checkpoint-failed", e
			}
			if e = currentAppend(filepath.Join(dir, "phase.private.jsonl"), map[string]any{"phase": "blocked-awaiting-parent", "kind": l.pendingKind, "applied_new": n, "pending_events": tr.queue.Len(), "at": time.Now().UTC()}); e != nil {
				return n, "phase-save-failed", e
			}
			fmt.Printf("phase blocked-awaiting-parent count %d pending %d\n", n, tr.queue.Len())
			if e = xcWait(l); e != nil {
				return n, "parent-source-wait-stopped", e
			}
			if e = restartMember(); e != nil {
				return n, "logger-failed", e
			}
			l.pendingKind = ""
			l.pendingDetails = nil
			fmt.Printf("phase parent-source-returned count %d pending %d\n", n, tr.queue.Len())
			continue
		}
		n++
		if n%1024 == 0 {
			if e = z.Flush(); e != nil {
				return n, "logger-failed", e
			}
			if e = f.Sync(); e != nil {
				return n, "logger-failed", e
			}
			if e = currentAppend(filepath.Join(dir, "progress.private.jsonl"), map[string]any{"applied_new": n, "pending_events": tr.queue.Len(), "wallets": len(tr.order), "elapsed_seconds": time.Since(began).Seconds(), "at": time.Now().UTC()}); e != nil {
				return n, "progress-failed", e
			}
			fmt.Printf("phase continuation count %d pending %d\n", n, tr.queue.Len())
		}
	}
	if e = closeMember(); e != nil {
		return n, "logger-finalization-failed", e
	}
	tr.settle()
	if l, ok := tr.load.(*xcLoader); ok {
		l.settled = true
	}
	return n, "trace-complete-not-financially-accepted", nil
}
func xcRestoreCheckpoint(l *xcLoader, dir string) (*flowTrace, error) {
	var s struct {
		Source     string                     `json:"source"`
		Seq        int                        `json:"seq"`
		Validator  string                     `json:"validator"`
		From       time.Time                  `json:"from"`
		End        time.Time                  `json:"asof_exclusive"`
		Pending    int                        `json:"pending_events"`
		Wallets    int                        `json:"wallets"`
		Applied    int                        `json:"applied_new"`
		Attempted  map[string]bool            `json:"attempted"`
		Stops      map[string]flowStop        `json:"stops"`
		Commission map[string]decimal.Decimal `json:"commission"`
		Unresolved []string                   `json:"unresolved"`
		Settled    bool                       `json:"settled_once"`
	}
	if e := xcRead(filepath.Join(dir, "trace-state.private.json"), &s); e != nil {
		return nil, e
	}
	if s.Source != xcSource || s.Settled {
		return nil, errors.New("checkpoint source or settled mismatch")
	}
	tr := newFlowTrace(s.Validator, s.From, s.End, l)
	tr.strict = true
	tr.seq = s.Seq
	tr.attempted = s.Attempted
	tr.stops = s.Stops
	tr.Commission = s.Commission
	tr.Unresolved = s.Unresolved
	e := xcLines(xcExistingJSONL(dir, "states.private.jsonl"), func(b []byte) error {
		var x xcState
		if e := json.Unmarshal(b, &x); e != nil {
			return e
		}
		if tr.Wallets[x.Address] != nil {
			return errors.New("checkpoint duplicate wallet")
		}
		w, e := xcWallet(x)
		if e != nil {
			return e
		}
		tr.Wallets[w.Addr] = w
		tr.order = append(tr.order, w)
		return nil
	})
	if e != nil {
		return nil, e
	}
	seqs := map[int]bool{}
	var prev *xcQueueRecord
	e = xcLines(xcExistingJSONL(dir, "full-queue.private.jsonl"), func(b []byte) error {
		var x xcQueueRecord
		if e := json.Unmarshal(b, &x); e != nil {
			return e
		}
		w := tr.Wallets[x.Wallet]
		if w == nil || x.Seq <= 0 || x.Seq > tr.seq || seqs[x.Seq] {
			return errors.New("checkpoint queue identity/seq mismatch")
		}
		seqs[x.Seq] = true
		if prev != nil && (x.Event.Key.before(prev.Event.Key) || (x.Event.Key == prev.Event.Key && x.Seq <= prev.Seq)) {
			return errors.New("checkpoint queue order mismatch")
		}
		copy := x
		prev = &copy
		heap.Push(&tr.queue, flowQueued{key: x.Event.Key, seq: x.Seq, wallet: w, event: x.Event})
		return nil
	})
	if e != nil {
		return nil, e
	}
	if len(tr.order) != s.Wallets || tr.queue.Len() != s.Pending {
		return nil, errors.New("checkpoint queue/state counts mismatch")
	}
	var rows struct {
		Rows []CommissionFlow `json:"rows"`
	}
	if e = xcRead(filepath.Join(dir, "partial-rows.private.json"), &rows); e != nil {
		return nil, e
	}
	for _, row := range rows.Rows {
		r := -1
		for i, name := range flowRuleNames {
			if name == row.Rule {
				r = i
			}
		}
		if r < 0 || row.Validator != tr.Validator {
			return nil, errors.New("checkpoint fate identity mismatch")
		}
		a, e := decimal.NewFromString(row.AmountBase)
		if e != nil {
			return nil, e
		}
		k := FlowFateKey{r, row.Month, row.Class, row.Detail, row.ViaUnbonding}
		if _, ok := tr.Fates[k]; ok {
			return nil, errors.New("checkpoint duplicate fate")
		}
		tr.Fates[k] = a
	}
	l.stops = tr.stops
	l.priorApplied = s.Applied
	provenance := filepath.Join(dir, "loader-provenance.private.json")
	if _, e := os.Stat(provenance); e == nil {
		var p struct {
			Used map[string]string `json:"used_file_sha256"`
		}
		if e = xcRead(provenance, &p); e != nil {
			return nil, e
		}
		for path, want := range p.Used {
			h, e := xcHash(path)
			if e != nil || h != want {
				return nil, errors.New("checkpoint loader provenance mismatch")
			}
			if l.used == nil {
				l.used = map[string]string{}
			}
			l.used[path] = want
		}
	}
	return tr, nil
}
func TestOPS818CohortsPrivateContinuation(t *testing.T) {
	syscall.Umask(0077)
	xcRoot = os.Getenv("H_XC_OUTPUT_ROOT")
	xcRequestRoot = xcRoot
	if xcRoot == "" || (xcCohort != "validator-06" && xcCohort != "validator-07") {
		t.Fatal("exclusive cohort/root required")
	}
	var m xcManifest
	if e := xcRead(filepath.Join(xcRoot, "restore-manifest.private.json"), &m); e != nil || m.Source != xcSource {
		t.Fatal("manifest invalid")
	}
	for path, binding := range m.Files {
		h, e := xcHash(path)
		if e != nil || h != binding.SHA {
			t.Fatal("immutable restore artifact mismatch")
		}
	}
	l := &xcLoader{manifest: m, windows: map[string]xcWindow{}, openings: map[string]xcOpening{}, loaded: map[string]bool{}, used: map[string]string{}, requested: map[string]bool{}}
	for _, w := range m.Windows {
		l.windows[xcID(w.Address, w.Since, w.End)] = w
	}
	var cohort struct{ OpeningEvidence []xcOpening }
	if e := xcRead(filepath.Join(m.Old, "cohort.private.json"), &cohort); e != nil {
		t.Fatal("opening evidence invalid")
	}
	verified := map[string]string{}
	for _, o := range cohort.OpeningEvidence {
		h, ok := verified[o.SourcePath]
		if !ok {
			var e error
			h, e = xcHash(o.SourcePath)
			if e != nil {
				t.Fatal("opening source missing")
			}
			verified[o.SourcePath] = h
		}
		if h != o.SourceSHA256 {
			t.Fatal("opening source mismatch")
		}
		key := xcID(o.Address, o.Day, o.Day)
		if old, ok := l.openings[key]; ok && !currentHoldingsMoneyEqual(old.Holdings, o.Holdings) {
			t.Fatal("opening conflict")
		}
		l.openings[key] = o
	}
	tr, last, n, e := xcRestore(l)
	if e != nil {
		_ = xcSave(filepath.Join(xcRoot, "restore-failure-safe.json"), map[string]any{"phase": e.Error(), "records": n})
		t.Fatal("exact restore rejected; private failure phase retained")
	}
	var terminal xcTerminal
	if e = xcRead(filepath.Join(m.Capture, xcCohort, "trace-result.private.json"), &terminal); e != nil {
		t.Fatal("terminal unavailable")
	}
	if e = xcSave(filepath.Join(xcRoot, "restore-proof.private.json"), map[string]any{"source": xcSource, "cohort": xcCohort, "applied_records": n, "committed_records": n - 1, "terminal_pending_excluding_failed_inflight": terminal.Pending, "original_restored_pending_including_failed": terminal.Pending + 1, "continuation_restored_pending": tr.queue.Len(), "seq": tr.seq, "wallets": len(tr.order), "failed_record": last, "converted_source_bindings": l.used, "committed_events_reapplied": 0, "failed_occurrence_reinserted_with_original_seq": true, "partial": true, "closing_reconciled": false, "financially_accepted": false}); e != nil {
		t.Fatal("restore proof save failed")
	}
	if e = xcCheckpoint(tr, "restored", "exact-restored-before-failed-event", false, 0); e != nil {
		t.Fatal("literal restored queue save failed")
	}
	fmt.Printf("phase exact_restored count %d pending %d\n", n, tr.queue.Len())
	if e = os.Mkdir(filepath.Join(xcRoot, "windows"), 0700); e != nil {
		t.Fatal("exclusive windows required")
	}
	if e = os.Mkdir(filepath.Join(xcRoot, "run"), 0700); e != nil {
		t.Fatal("exclusive run required")
	}
	if e = l.supplement(); e != nil {
		t.Fatal("supplement rejected")
	}
	applied, phase, runErr := xcRun(tr, filepath.Join(xcRoot, "run"))
	if e = xcCheckpoint(tr, "terminal", phase, runErr == nil || l.settled, applied); e != nil {
		t.Fatal("terminal queue save failed")
	}
	if e = xcSave(filepath.Join(xcRoot, "runtime-result-safe.json"), map[string]any{"source": xcSource, "phase": phase, "pid": os.Getpid(), "applied_new": applied, "pending_events": tr.queue.Len(), "wallets": len(tr.order), "financially_accepted": false}); e != nil {
		t.Fatal("runtime result save failed")
	}
	fmt.Printf("phase %s count %d pending %d\n", phase, applied, tr.queue.Len())
}

// Synthetic tests exercise the same restoration helper against an uninterrupted
// production trace. No database, archive, bank capture or process memory is used.
type xcFailLedger struct {
	*fakeFlowLedger
	fail string
}

func (l *xcFailLedger) events(a string, s, e time.Time) ([]flowEvent, error) {
	if a == l.fail {
		return nil, errors.New("synthetic credit join failure")
	}
	return l.fakeFlowLedger.events(a, s, e)
}
func xcSynthetic(t *testing.T, duplicate bool) {
	t.Helper()
	l := newFakeFlowLedger()
	a, b, c := "synthetic-a", "synthetic-b", "synthetic-c"
	l.openings[a] = flowHoldings{Liquid: amt(10)}
	l.openings[b] = flowHoldings{Liquid: amt(15)}
	l.openings[c] = flowHoldings{Liquid: amt(3)}
	l.commission(flowAt(0, 1), a, 100)
	l.commission(flowAt(0, 2), a, 20)
	l.rows[b] = append(l.rows[b], flowEvent{Key: flowAt(0, 0), Category: apiFee, Dir: apiOut, Amount: amt(2), From: b})
	l.send(flowAt(0, 3), a, b, 30)
	l.send(flowAt(0, 4), b, c, 18)
	// Equal keys, identical occurrences and same-origin merging must retain seq.
	fee := flowEvent{Key: flowAt(0, 4), Category: apiFee, Dir: apiOut, Amount: amt(1), From: a}
	l.rows[a] = append(l.rows[a], fee)
	if duplicate {
		l.rows[a] = append(l.rows[a], fee)
	}
	for addr, events := range l.rows {
		sort.SliceStable(events, func(i, j int) bool { return events[i].Key.before(events[j].Key) })
		l.rows[addr] = events
	}
	full := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 1, 0), l)
	full.strict = true
	if e := full.run([]string{a}); e != nil {
		t.Fatal("synthetic baseline")
	}
	failing := &xcFailLedger{fakeFlowLedger: l, fail: c}
	tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 1, 0), failing)
	tr.strict = true
	if e := tr.discover(a, 0, tr.From, nil); e != nil {
		t.Fatal(e)
	}
	initial := xcSnapshot(tr.order[0])
	records := []xcApplied{}
	var failed flowQueued
	for tr.queue.Len() > 0 {
		q := heap.Pop(&tr.queue).(flowQueued)
		before := map[string]xcState{q.wallet.Addr: xcSnapshot(q.wallet)}
		if dst := tr.Wallets[q.event.To]; dst != nil && q.event.Category == apiTransfer && q.event.Dir == apiOut {
			before[dst.Addr] = xcSnapshot(dst)
		}
		prior := len(tr.order)
		e := tr.apply(q.wallet, q.event, true)
		after := map[string]xcState{q.wallet.Addr: xcSnapshot(q.wallet)}
		if dst := tr.Wallets[q.event.To]; dst != nil && q.event.Category == apiTransfer && q.event.Dir == apiOut {
			after[dst.Addr] = xcSnapshot(dst)
		}
		added := []xcState{}
		for _, w := range tr.order[prior:] {
			added = append(added, xcSnapshot(w))
		}
		records = append(records, xcApplied{Ordinal: len(records) + 1, From: q.wallet.Addr, Event: q.event, Before: before, After: after, New: added, Error: fmt.Sprint(e)})
		if e != nil {
			failed = q
			break
		}
	}
	if failed.wallet == nil {
		t.Fatal("synthetic must fail after debit")
	}
	restored := newFlowTrace(tr.Validator, tr.From, tr.AsOf, l)
	restored.strict = true
	w, e := xcWallet(initial)
	if e != nil {
		t.Fatal(e)
	}
	restored.Wallets[w.Addr] = w
	restored.order = append(restored.order, w)
	xcEnqueue(restored, w, l.rows[w.Addr], nil)
	var popped flowQueued
	for _, rec := range records {
		if e = xcRestoreRecord(restored, rec, func(w *flowWallet) ([]flowEvent, error) { return l.rows[w.Addr], nil }, &popped); e != nil {
			t.Fatal(e)
		}
	}
	if restored.queue.Len() != tr.queue.Len() || popped.seq != failed.seq || restored.seq != tr.seq {
		t.Fatal("synthetic exact queue/seq mismatch")
	}
	for i, w := range tr.order {
		if !xcEqual(xcSnapshot(w), xcSnapshot(restored.order[i])) {
			t.Fatal("synthetic terminal pools mismatch")
		}
	}
	for a, s := range records[len(records)-1].Before {
		w, e := xcWallet(s)
		if e != nil {
			t.Fatal(e)
		}
		*restored.Wallets[a] = *w
	}
	heap.Push(&restored.queue, popped)
	restored.Fates = tr.Fates
	restored.Commission = tr.Commission
	restored.stops = tr.stops
	restored.attempted = tr.attempted
	for restored.queue.Len() > 0 {
		q := heap.Pop(&restored.queue).(flowQueued)
		if e = restored.apply(q.wallet, q.event, true); e != nil {
			t.Fatal(e)
		}
	}
	restored.settle()
	if !xcEqual(xcFates(full), xcFates(restored)) || !xcEqual(full.Commission, restored.Commission) || full.seq != restored.seq {
		t.Fatal("synthetic Fates/Commission/sequence mismatch")
	}
	if len(full.order) != len(restored.order) {
		t.Fatal("synthetic discovery count mismatch")
	}
	for i, w := range full.order {
		if !xcEqual(xcSnapshot(w), xcSnapshot(restored.order[i])) {
			t.Fatal("synthetic uninterrupted state mismatch")
		}
	}
	if !reflect.DeepEqual(full.attempted, restored.attempted) {
		t.Fatal("synthetic attempted mismatch")
	}
}
func TestOPS818CohortsContinuationSynthetic(t *testing.T) {
	t.Run("logger_failed_apply_rewind_then_retry", func(t *testing.T) {
		previous := xcRoot
		var e error
		xcRoot, e = os.MkdirTemp(os.Getenv("TMPDIR"), "continuation-retry-")
		if e != nil {
			t.Fatal(e)
		}
		defer func() { xcRoot = previous }()
		l := newFakeFlowLedger()
		l.commission(flowAt(0, 1), "retry-root", 30)
		l.send(flowAt(0, 2), "retry-root", "retry-destination", 20)
		baseline := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 1, 0), l)
		baseline.strict = true
		if e = baseline.run([]string{"retry-root"}); e != nil {
			t.Fatal(e)
		}
		failing := &xcFailLedger{fakeFlowLedger: l, fail: "retry-destination"}
		tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 1, 0), failing)
		tr.strict = true
		if e = tr.discover("retry-root", 0, tr.From, nil); e != nil {
			t.Fatal(e)
		}
		first := filepath.Join(xcRoot, "first")
		second := filepath.Join(xcRoot, "second")
		if e = os.Mkdir(first, 0700); e != nil {
			t.Fatal(e)
		}
		if e = os.Mkdir(second, 0700); e != nil {
			t.Fatal(e)
		}
		n, phase, e := xcRun(tr, first)
		if e == nil || phase != "failed-rewound-exact" || n != 1 || tr.queue.Len() != 1 {
			t.Fatal("failed debit not rewound and retained")
		}
		records := 0
		if e = xcLines(filepath.Join(first, "new-applied.private.jsonl.gz"), func(b []byte) error {
			var row map[string]json.RawMessage
			if e := json.Unmarshal(b, &row); e != nil {
				return e
			}
			if row["before"] == nil || row["after"] == nil {
				return errors.New("lossless snapshots absent")
			}
			records++
			return nil
		}); e != nil || records != 2 {
			t.Fatal("gzip proof failed to finalize losslessly")
		}
		tr.load = l
		if _, phase, e = xcRun(tr, second); e != nil || phase != "trace-complete-not-financially-accepted" {
			t.Fatal("retry failed")
		}
		if !xcEqual(xcFates(tr), xcFates(baseline)) || !xcEqual(tr.Commission, baseline.Commission) || tr.seq != baseline.seq {
			t.Fatal("retry Fates/Commission/seq differ from production")
		}
		for i, w := range baseline.order {
			if !xcEqual(xcSnapshot(w), xcSnapshot(tr.order[i])) {
				t.Fatal("retry state differs from uninterrupted production")
			}
		}
	})
	t.Run("production_credit_message_scope", func(t *testing.T) {
		start, end := flowTestStart, flowTestStart.AddDate(0, 1, 0)
		row := tax.TaxableEvent{Category: "ibc_in", Denom: "transfer/channel-0/uatom", Amount: "10", ToAddr: "other-receiver", Timestamp: flowAt(0, 1).Time, MessageID: 9}
		input := reportInput{Wallets: []walletRows{{Events: []tax.TaxableEvent{row}}}}
		events, e := xcConvert(input, "synthetic-receiver", start, end, map[flowCredit]bool{{9, "10uatom"}: true})
		if e != nil || len(events) != 0 {
			t.Fatal("credit outside production allowed message scope accepted")
		}
	})
	t.Run("IBC_client_swap_suffix", func(t *testing.T) {
		l := &xcLoader{}
		event := flowEvent{Trigger: flowSkipSwap}
		if l.ibcDestination(event) != "unknown channel (swap)" {
			t.Fatal("production swap suffix missing")
		}
	})
	t.Run("equal_keys_prefix_failed_debit_merge", func(t *testing.T) { xcSynthetic(t, false) })
	t.Run("duplicate_occurrences", func(t *testing.T) { xcSynthetic(t, true) })
	t.Run("checkpoint_roundtrip_and_tamper_rejection", func(t *testing.T) {
		previous := xcRoot
		var e error
		xcRoot, e = os.MkdirTemp(os.Getenv("TMPDIR"), "continuation-proof-")
		if e != nil {
			t.Fatal(e)
		}
		defer func() { xcRoot = previous }()
		l := newFakeFlowLedger()
		l.commission(flowAt(0, 1), "synthetic-root", 10)
		tr := newFlowTrace(flowTestValidator, flowTestStart, flowTestStart.AddDate(0, 1, 0), l)
		tr.strict = true
		if e = tr.discover("synthetic-root", 0, tr.From, nil); e != nil {
			t.Fatal(e)
		}
		if e = xcCheckpoint(tr, "proof", "synthetic", false, 0); e != nil {
			t.Fatal(e)
		}
		restored, e := xcRestoreCheckpoint(&xcLoader{}, filepath.Join(xcRoot, "proof"))
		if e != nil {
			t.Fatal(e)
		}
		if !xcEqual(xcSnapshot(tr.order[0]), xcSnapshot(restored.order[0])) || tr.queue.Len() != restored.queue.Len() || tr.seq != restored.seq || !xcEqual(tr.queue[0].event, restored.queue[0].event) || tr.queue[0].seq != restored.queue[0].seq {
			t.Fatal("checkpoint exact roundtrip mismatch")
		}
		q := heap.Pop(&restored.queue).(flowQueued)
		bad := xcApplied{From: q.wallet.Addr, Event: q.event, Before: map[string]xcState{q.wallet.Addr: xcSnapshot(q.wallet)}, Error: "<nil>"}
		heap.Push(&restored.queue, q)
		bad.Event.Amount = amt(11)
		var failed flowQueued
		if e = xcRestoreRecord(restored, bad, func(*flowWallet) ([]flowEvent, error) { return nil, nil }, &failed); e == nil {
			t.Fatal("tampered event accepted")
		}
		path := filepath.Join(xcRoot, "proof", "full-queue.private.jsonl.gz")
		f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal(e)
		}
		z := gzip.NewWriter(f)
		e = json.NewEncoder(z).Encode(xcQueueRecord{q.wallet.Addr, q.event, q.seq})
		ze := z.Close()
		f.Close()
		if ze != nil {
			t.Fatal(ze)
		}
		if e != nil {
			t.Fatal(e)
		}
		if _, e = xcRestoreCheckpoint(&xcLoader{}, filepath.Join(xcRoot, "proof")); e == nil {
			t.Fatal("duplicate enqueue seq accepted")
		}
	})
}
