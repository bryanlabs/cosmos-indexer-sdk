package taxapi

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
	"reflect"
	"time"
)

// This is intentionally not enabled by environment variables or production
// entrypoints. A read journal is not a database snapshot. Its owner must prove
// the same immutable source epoch before replay AND before every new read.
// Deterministic replay reconstructs heap sequence, cursor, seen wallets and
// pools using the existing engine, without serializing its growing graph.
const flowCheckpointMaxRecord = 64 << 20
const flowCheckpointMaxRecords = 100000

type flowCheckpointIdentity struct {
	Schema                                                                       int
	Root, Chain, Build, SourceEpoch, SourceSeal, SettingsSHA256, ReceiversSHA256 string
	From, AsOf                                                                   time.Time
}

type flowCheckpointHooks struct {
	VerifySource func() error
	// Capture and Restore preserve private archive/correction/label/channel
	// provenance. They must be idempotent and must not perform financial writes.
	Capture func() (json.RawMessage, error)
	Restore func(json.RawMessage) error
}

type flowCheckpointRequest struct {
	Kind, Address string
	Start, End    time.Time
	Event         *flowEvent `json:",omitempty"`
}

type flowCheckpointResult struct {
	Holdings    flowHoldings
	Known       bool
	Events      []flowEvent
	Stop        flowStop
	Destination string
	Provenance  json.RawMessage
}

type flowCheckpointRecord struct {
	Identity flowCheckpointIdentity
	Sequence int
	Previous string
	Request  flowCheckpointRequest
	Result   flowCheckpointResult
}

type flowCheckpointEnvelope struct {
	SHA256 string
	Record json.RawMessage
}

type checkpointFlowLoader struct {
	base          flowLoader
	dir           string
	identity      flowCheckpointIdentity
	hooks         flowCheckpointHooks
	count, cursor int
	previous      string
	fault         error
}

func openFlowCheckpoint(dir string, id flowCheckpointIdentity, base flowLoader, hooks flowCheckpointHooks) (*checkpointFlowLoader, error) {
	if id.Schema != 1 || id.Root == "" || id.Chain == "" || id.Build == "" || id.SourceEpoch == "" || id.SourceSeal == "" || id.SettingsSHA256 == "" || id.ReceiversSHA256 == "" || id.From.IsZero() || !id.AsOf.After(id.From) || base == nil || hooks.VerifySource == nil || hooks.Capture == nil || hooks.Restore == nil {
		return nil, errors.New("checkpoint requires complete source, code, root, window, settings and provenance contract")
	}
	id.From, id.AsOf = id.From.UTC(), id.AsOf.UTC()
	info, err := os.Lstat(dir)
	if err != nil || !filepath.IsAbs(dir) || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("checkpoint requires existing absolute private mode-0700 directory")
	}
	if err := hooks.VerifySource(); err != nil {
		return nil, errors.New("checkpoint source epoch not verified")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	if len(entries) > flowCheckpointMaxRecords {
		return nil, errors.New("checkpoint record limit exceeded")
	}
	l := &checkpointFlowLoader{base: base, dir: dir, identity: id, hooks: hooks, count: len(entries)}
	// Validate the ENTIRE prefix before returning any cached financial facts.
	// A torn/unexpected file, gap, mismatch or corrupt record rejects the attempt;
	// callers may start a separate empty attempt, never silently mix this prefix.
	for i, entry := range entries {
		if entry.Name() != checkpointName(i) {
			return nil, errors.New("checkpoint incomplete or unexpected file")
		}
		_, digest, err := l.read(i, l.previous)
		if err != nil {
			return nil, err
		}
		l.previous = digest
	}
	l.previous = ""
	return l, nil
}

func checkpointName(i int) string { return fmt.Sprintf("%08d.json", i) }

func (l *checkpointFlowLoader) read(i int, previous string) (flowCheckpointRecord, string, error) {
	var record flowCheckpointRecord
	path := filepath.Join(l.dir, checkpointName(i))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > flowCheckpointMaxRecord {
		return record, "", errors.New("checkpoint invalid private record")
	}
	file, err := os.Open(path)
	if err != nil {
		return record, "", err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, flowCheckpointMaxRecord+1))
	err = errors.Join(readErr, file.Close())
	if err != nil || len(raw) > flowCheckpointMaxRecord {
		return record, "", errors.New("checkpoint read failed or exceeded bound")
	}
	var envelope flowCheckpointEnvelope
	if json.Unmarshal(raw, &envelope) != nil {
		return record, "", errors.New("checkpoint corrupt envelope")
	}
	digest := sha256.Sum256(envelope.Record)
	if hex.EncodeToString(digest[:]) != envelope.SHA256 || json.Unmarshal(envelope.Record, &record) != nil || !reflect.DeepEqual(record.Identity, l.identity) || record.Sequence != i || record.Previous != previous {
		return record, "", errors.New("checkpoint identity, sequence or digest mismatch")
	}
	return record, envelope.SHA256, nil
}

func syncCheckpointDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func (l *checkpointFlowLoader) call(req flowCheckpointRequest, read func() (flowCheckpointResult, error)) (result flowCheckpointResult, err error) {
	req.Start, req.End = req.Start.UTC(), req.End.UTC()
	if req.Event != nil {
		event := *req.Event
		event.Key.Time = event.Key.Time.UTC()
		req.Event = &event
	}
	if l.fault != nil {
		return result, l.fault
	}
	defer func() {
		if err != nil {
			l.fault = err
		}
	}()
	if l.hooks.VerifySource() != nil {
		return result, errors.New("checkpoint source epoch changed")
	}
	if l.cursor < l.count {
		record, digest, err := l.read(l.cursor, l.previous)
		if err != nil {
			return result, err
		}
		a, _ := json.Marshal(req)
		b, _ := json.Marshal(record.Request)
		if !bytes.Equal(a, b) {
			return result, errors.New("checkpoint traversal diverged")
		}
		if l.hooks.Restore(record.Result.Provenance) != nil {
			return result, errors.New("checkpoint provenance restoration failed")
		}
		if l.hooks.VerifySource() != nil {
			return result, errors.New("checkpoint source epoch changed during replay")
		}
		if base, ok := l.base.(interface {
			restoreCheckpointResult(flowCheckpointRequest, flowCheckpointResult)
		}); ok {
			base.restoreCheckpointResult(req, record.Result)
		}
		l.cursor++
		l.previous = digest
		return record.Result, nil
	}
	if l.cursor >= flowCheckpointMaxRecords {
		return result, errors.New("checkpoint record limit exceeded")
	}
	result, err = read()
	if err != nil {
		return result, err
	}
	if l.hooks.VerifySource() != nil {
		return result, errors.New("checkpoint source epoch changed during read")
	}
	result.Provenance, err = l.hooks.Capture()
	if err != nil || !json.Valid(result.Provenance) {
		return result, errors.New("checkpoint provenance capture failed")
	}
	record := flowCheckpointRecord{Identity: l.identity, Sequence: l.cursor, Previous: l.previous, Request: req, Result: result}
	raw, err := json.Marshal(record)
	if err != nil {
		return result, err
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	encoded, err := json.Marshal(flowCheckpointEnvelope{SHA256: digest, Record: raw})
	if err != nil || len(encoded) > flowCheckpointMaxRecord {
		return result, errors.New("checkpoint record exceeds bound")
	}
	// One bounded successful source-read unit per immutable file. Never overwrite
	// a committed record. The temp file is deliberately retained on failure so
	// the next open refuses an incomplete attempt rather than guessing.
	temp := filepath.Join(l.dir, checkpointName(l.cursor)+".pending")
	f, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return result, errors.New("checkpoint create failed")
	}
	_, writeErr := f.Write(encoded)
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return result, errors.New("checkpoint durable write failed")
	}
	target := filepath.Join(l.dir, checkpointName(l.cursor))
	// Link is create-if-absent, unlike rename which could replace committed data.
	if os.Link(temp, target) != nil {
		return result, errors.New("checkpoint immutable commit failed")
	}
	if syncCheckpointDir(l.dir) != nil {
		return result, errors.New("checkpoint directory sync failed")
	}
	if os.Remove(temp) != nil || syncCheckpointDir(l.dir) != nil {
		return result, errors.New("checkpoint final commit failed")
	}
	l.previous = digest
	l.cursor++
	l.count++
	return result, nil
}

// check MUST succeed before closing evidence/capture/save. ibcDestination has
// no error return in flowLoader, so it latches failures here. A journal prefix
// never means a complete root, and this API never writes a completion marker.
func (l *checkpointFlowLoader) check() error {
	if l.fault != nil {
		return l.fault
	}
	if l.cursor != l.count {
		return errors.New("checkpoint replay did not consume complete prefix")
	}
	if l.hooks.VerifySource() != nil {
		return errors.New("checkpoint final source epoch changed")
	}
	return nil
}

func (l *checkpointFlowLoader) opening(a string, day time.Time) (flowHoldings, bool, error) {
	r, err := l.call(flowCheckpointRequest{Kind: "opening", Address: a, Start: day}, func() (flowCheckpointResult, error) {
		h, k, e := l.base.opening(a, day)
		return flowCheckpointResult{Holdings: h, Known: k}, e
	})
	return r.Holdings, r.Known, err
}
func (l *checkpointFlowLoader) holdingsAt(a string, day time.Time) (flowHoldings, bool, error) {
	base, ok := l.base.(interface {
		holdingsAt(string, time.Time) (flowHoldings, bool, error)
	})
	if !ok {
		return flowHoldings{}, false, errors.New("checkpoint closing loader unavailable")
	}
	r, err := l.call(flowCheckpointRequest{Kind: "closing", Address: a, Start: day}, func() (flowCheckpointResult, error) {
		h, k, e := base.holdingsAt(a, day)
		return flowCheckpointResult{Holdings: h, Known: k}, e
	})
	return r.Holdings, r.Known, err
}
func (l *checkpointFlowLoader) events(a string, start, end time.Time) ([]flowEvent, error) {
	r, err := l.call(flowCheckpointRequest{Kind: "events", Address: a, Start: start, End: end}, func() (flowCheckpointResult, error) {
		events, e := l.base.events(a, start, end)
		return flowCheckpointResult{Events: events}, e
	})
	return r.Events, err
}
func (l *checkpointFlowLoader) stop(a string) (flowStop, error) {
	r, err := l.call(flowCheckpointRequest{Kind: "stop", Address: a}, func() (flowCheckpointResult, error) {
		stop, e := l.base.stop(a)
		return flowCheckpointResult{Stop: stop}, e
	})
	return r.Stop, err
}
func (l *checkpointFlowLoader) ibcDestination(e flowEvent) string {
	r, _ := l.call(flowCheckpointRequest{Kind: "ibc", Event: &e}, func() (flowCheckpointResult, error) {
		return flowCheckpointResult{Destination: l.base.ibcDestination(e)}, nil
	})
	return r.Destination
}
