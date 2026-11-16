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
	var line []byte
	for {
		line = line[:0]
		for {
			part, e := br.ReadSlice('\n')
			if len(line)+len(part) > 1<<31 {
				return counts, &Hub1Error{Line: counts.Records + 1, Height: current.Number, Reason: "line exceeds 2 GiB"}
			}
			line = append(line, part...)
			if e == bufio.ErrBufferFull {
				continue
			}
			if e != nil && e != io.EOF {
				return counts, invalid
			}
			break
		}
		if len(line) == 0 {
			break
		}
		counts.Records++
		counts.RawBytes += int64(len(line))
		refuse := func(x hub1Record, reason string) (Hub1Counts, error) {
			return counts, &Hub1Error{Line: counts.Records, Height: x.Height, Kind: x.Kind, Label: hub1Label(x.Label), Reason: reason}
		}
		var x hub1Record
		if line[len(line)-1] != '\n' {
			return refuse(x, "unterminated line")
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if dec.Decode(&x) != nil {
			return refuse(x, "not one record of the known shape")
		}
		if dec.More() {
			return refuse(x, "more than one value on a line")
		}
		if closed {
			return refuse(x, "record after the stream closed")
		}
		counts.Kinds[x.Kind]++
		switch {
		case hub1Referenced[x.Kind]:
			inside := x.Height == current.Number && x.Height <= p.Through
			if !inside || (!p.Export && x.Status != "committed" && x.Status != "aborted") || (p.Export && x.Status != "exported") {
				return refuse(x, "native evidence outside its height or status")
			}
			continue
		case hub1Explaining[x.Kind] != "":
			if p.Export || x.Height != current.Number || x.Phase != "message" || x.Tx < 0 || x.Message < 0 || x.Unit != hub1Explaining[x.Kind] || (x.Status != "committed" && x.Status != "aborted") || x.Validator == "" || x.Delegator == "" {
				return refuse(x, "explaining evidence outside a message or with another unit")
			}
		case x.Kind == "observation_completed":
			if p.Export || x.Height != p.Through || current.Number != p.Through+1 || len(current.Records) != 0 || x.Phase != "segment" || x.Status != "committed" || x.Code != 0 || x.Tx != -1 || x.Message != -1 {
				return refuse(x, "stream closed away from its last height")
			}
			closed = true
			continue
		case x.Kind == "movement" || x.Kind == "principal" || x.Kind == "accounting" || x.Kind == "height_completed" || x.Kind == "export_completed":
			if x.Kind == "movement" && x.Unit != "native_bank_tokens" || x.Kind == "principal" && x.Unit != "native_bond_tokens" {
				return refuse(x, "amount is in a unit this converter has no rule for")
			}
			if !admit(x.Record, p, current.Number, &begin, &end) {
				return refuse(x, "record breaks the shared replay record rules")
			}
		default:
			return refuse(x, "record kind is not admitted")
		}
		current.Records = append(current.Records, x.Record)
		if x.Kind == "height_completed" || x.Kind == "export_completed" {
			counts.Heights++
			if visit != nil {
				if err := visit(current); err != nil {
					return counts, err
				}
			}
			current = Height{Number: current.Number + 1}
			begin, end = 0, 0
			closed = p.Export
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
