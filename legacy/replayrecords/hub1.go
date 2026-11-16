package replayrecords

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const Release033 = "cosmos-sdk v0.33.2 (gaia), tendermint v0.31.0-dev0-fix0, iavl v0.12.1"

// Hub 1 evidence kinds that ride in Height.Records beside the shared kinds.
// They explain a message but never become a row on their own.
var hub1Explaining = map[string]string{"principal_request": "native_bond_tokens", "share_request": "delegation_shares", "staking_shares": "delegation_shares"}

// Hub 1 native store and execution evidence. It stays in the pinned source
// stream, bound by the stream digest, and is counted but not copied.
var hub1Referenced = map[string]bool{"native_execution": true, "native_state": true, "native_state_hash": true, "native_state_checkpoint": true, "native_write": true, "native_cache_write": true}

// Hub1Counts says what one admitted Hub 1 stream held.
type Hub1Counts struct {
	Records, RawBytes, Heights int64
	Kinds                      map[string]int64
}

type hub1Record struct {
	Record
	NativeView      hub1Unread `json:"native_view"`
	NativeParent    hub1Unread `json:"native_parent"`
	WriteOperation  hub1Unread `json:"write_operation"`
	Classification  string     `json:"classification"`
	EvidenceQuality string     `json:"evidence_quality"`
	Before          hub1Unread `json:"before"`
	After           hub1Unread `json:"after"`
	Unit            string     `json:"unit"`
}

// hub1Unread is a value the decoder checks is JSON and then leaves in the
// source stream.
type hub1Unread struct{}

func (*hub1Unread) UnmarshalJSON([]byte) error { return nil }

// Hub1Error names the record a Hub 1 stream was refused on. It carries no
// address and no amount.
type Hub1Error struct {
	Line, Height int64
	Kind, Label  string
	Reason       string
}

func (e *Hub1Error) Error() string {
	return fmt.Sprintf("Hub 1 stream refused at line %d height %d kind %q label %q: %s", e.Line, e.Height, e.Kind, e.Label, e.Reason)
}
func (e *Hub1Error) Unwrap() error { return invalid }

func hub1Label(label string) string {
	if strings.Contains(label, "cosmos") || len(label) > 96 {
		return "withheld"
	}
	return label
}

// StreamHub1 admits one closed Hub 1 observation stream, already
// decompressed. Like Stream it checks the whole stream against its pin
// before the first callback, so open is called twice and must return the
// same immutable bytes both times. A record kind or unit it does not know
// refuses the stream by name; nothing is skipped silently.
func StreamHub1(open func() (io.ReadCloser, error), p Proof, visit func(Height) error) (Hub1Counts, error) {
	pass := func(v func(Height) error) (Hub1Counts, error) {
		r, err := open()
		if err != nil {
			return Hub1Counts{}, err
		}
		defer r.Close()
		return ScanHub1(r, p, v)
	}
	counts, err := pass(nil)
	if err != nil || visit == nil {
		return counts, err
	}
	return pass(visit)
}

// ScanHub1 is one pass of StreamHub1. With a callback it hands heights over
// before the digest is known, so a caller reading several streams must
// first run it over every one of them with no callback.
func ScanHub1(r io.Reader, p Proof, visit func(Height) error) (Hub1Counts, error) {
	counts := Hub1Counts{Kinds: map[string]int64{}}
	if p.Chain != "cosmoshub-1" || p.Release != Release033 || p.Records < 1 || p.RawBytes < 1 || !p.bounded() {
		return counts, invalid
	}
	hash := sha256.New()
	br := bufio.NewReaderSize(io.TeeReader(r, hash), 1<<20)
	current := Height{Number: p.From}
	begin, end := 0, 0
	closed := false
	var x hub1Record
	refuse := func(reason string) (Hub1Counts, error) {
		return counts, &Hub1Error{Line: counts.Records, Height: x.Height, Kind: x.Kind, Label: hub1Label(x.Label), Reason: reason}
	}
	// step applies the stream rules to one decoded record, in stream order.
	step := func() (string, error) {
		counts.Kinds[x.Kind]++
		switch {
		case hub1Referenced[x.Kind]:
			status := (!p.Export && (x.Status == "committed" || x.Status == "aborted")) || (p.Export && x.Status == "exported")
			inside := x.Height == current.Number && x.Height <= p.Through
			// The closing store checkpoints follow the last height.
			final := !p.Export && x.Kind == "native_state_checkpoint" && strings.HasPrefix(x.Label, "segment_final/") && x.Height == p.Through &&
				current.Number == p.Through+1 && len(current.Records) == 0 && x.Phase == "segment" && x.Tx == -1 && x.Message == -1
			if strings.HasPrefix(x.Label, "segment_final/") {
				inside = false
			}
			if !status || (!inside && !final) {
				return "native evidence outside its height or status", nil
			}
			return "", nil
		case hub1Explaining[x.Kind] != "":
			if p.Export || x.Height != current.Number || x.Phase != "message" || x.Tx < 0 || x.Message < 0 || x.Unit != hub1Explaining[x.Kind] || (x.Status != "committed" && x.Status != "aborted") || x.Validator == "" || x.Delegator == "" {
				return "explaining evidence outside a message or with another unit", nil
			}
		case x.Kind == "observation_completed":
			if p.Export || x.Height != p.Through || current.Number != p.Through+1 || len(current.Records) != 0 || x.Phase != "segment" || x.Status != "committed" || x.Code != 0 || x.Tx != -1 || x.Message != -1 {
				return "stream closed away from its last height", nil
			}
			closed = true
			return "", nil
		case x.Kind == "movement" || x.Kind == "principal" || x.Kind == "accounting" || x.Kind == "height_completed" || x.Kind == "export_completed":
			if x.Kind == "movement" && x.Unit != "native_bank_tokens" || x.Kind == "principal" && x.Unit != "native_bond_tokens" {
				return "amount is in a unit this converter has no rule for", nil
			}
			if !admit(x.Record, p, current.Number, &begin, &end) {
				return "record breaks the shared replay record rules", nil
			}
		default:
			return "record kind is not admitted", nil
		}
		current.Records = append(current.Records, x.Record)
		if x.Kind == "height_completed" || x.Kind == "export_completed" {
			counts.Heights++
			if visit != nil {
				if err := visit(current); err != nil {
					return "", err
				}
			}
			current = Height{Number: current.Number + 1}
			begin, end = 0, 0
			closed = p.Export
		}
		return "", nil
	}
	// One goroutine reads the lines, several decode them, and this one
	// applies the rules to the decoded records in stream order.
	stop := make(chan struct{})
	defer close(stop)
	work := make(chan *hub1Batch, hub1Decoders)
	ordered := make(chan *hub1Batch, 2*hub1Decoders)
	for i := 0; i < hub1Decoders; i++ {
		go func() {
			for b := range work {
				b.decode()
			}
		}()
	}
	go hub1Cut(br, work, ordered, stop)
	for b := range ordered {
		<-b.done
		for i := range b.records {
			x = b.records[i]
			counts.Records++
			counts.RawBytes += b.sizes[i]
			if b.reasons[i] != "" {
				return refuse(b.reasons[i])
			}
			if closed {
				return refuse("record after the stream closed")
			}
			reason, err := step()
			if err != nil {
				return counts, err
			}
			if reason != "" {
				return refuse(reason)
			}
		}
		if b.err != nil {
			return counts, b.err
		}
	}
	if !closed || len(current.Records) != 0 || current.Number != p.Through+1 {
		return counts, &Hub1Error{Line: counts.Records, Height: current.Number, Reason: "stream ended before its completion record"}
	}
	if counts.Records != p.Records || counts.RawBytes != p.RawBytes || hex.EncodeToString(hash.Sum(nil)) != p.SHA256 {
		return counts, &Hub1Error{Line: counts.Records, Height: p.Through, Reason: "stream does not match its pinned digest, record count and size"}
	}
	return counts, nil
}

const hub1Decoders = 12

// hub1Batch is a run of consecutive lines and what decoding made of them.
type hub1Batch struct {
	lines   [][]byte
	records []hub1Record
	sizes   []int64
	reasons []string
	// err is a read failure after the last line of the batch.
	err  error
	done chan struct{}
}

func (b *hub1Batch) decode() {
	b.records, b.sizes, b.reasons = make([]hub1Record, len(b.lines)), make([]int64, len(b.lines)), make([]string, len(b.lines))
	for i, line := range b.lines {
		b.sizes[i] = int64(len(line))
		if line[len(line)-1] != '\n' {
			b.reasons[i] = "unterminated line"
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if dec.Decode(&b.records[i]) != nil {
			b.reasons[i] = "not one record of the known shape"
		} else if dec.More() {
			b.reasons[i] = "more than one value on a line"
		}
	}
	b.lines = nil
	close(b.done)
}

// hub1Cut cuts the stream into batches of whole lines, in order.
func hub1Cut(br *bufio.Reader, work, ordered chan<- *hub1Batch, stop <-chan struct{}) {
	defer close(work)
	defer close(ordered)
	send := func(b *hub1Batch) bool {
		select {
		case ordered <- b:
		case <-stop:
			return false
		}
		select {
		case work <- b:
		case <-stop:
			return false
		}
		return true
	}
	for {
		b := &hub1Batch{done: make(chan struct{})}
		size := 0
		for len(b.lines) < 4096 && size < 16<<20 && b.err == nil {
			var line []byte
			for {
				part, e := br.ReadSlice('\n')
				line = append(line, part...)
				if e == bufio.ErrBufferFull && len(line) < 1<<31 {
					continue
				}
				if e != nil && e != io.EOF {
					b.err = invalid
				}
				break
			}
			if len(line) == 0 {
				if b.err == nil {
					b.err = io.EOF
				}
				break
			}
			b.lines = append(b.lines, line)
			size += len(line)
		}
		last := b.err != nil
		if b.err == io.EOF {
			b.err = nil
		}
		if !send(b) || last {
			return
		}
	}
}
