// Command legacy-census measures how much of the retained Hub 1 to 3 block
// cache the offline Amino candidate can decode. It reads captures only, makes
// no network calls and writes no database. Output is aggregate counts: no
// addresses, amounts, memos, hashes or transaction bytes are ever printed.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	indexertx "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	"github.com/DefiantLabs/cosmos-indexer/legacy"
	"github.com/DefiantLabs/cosmos-indexer/legacy/evidence"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	abci "github.com/cometbft/cometbft/abci/types"
	tmjson "github.com/cometbft/cometbft/libs/json"
	amino "github.com/tendermint/go-amino"
)

const (
	spillBuckets = 64
	recordSize   = 32
)

var (
	captureName = regexp.MustCompile(`^(\d+)-(block|block_results)\.raw\.gz$`)
	safeText    = regexp.MustCompile(`^[A-Za-z0-9 _.,:/()'-]{1,90}$`)
	safeAction  = regexp.MustCompile(`^[a-z_]{1,40}$`)
	safeKind    = regexp.MustCompile(`^[a-z0-9_/]{1,40}$`)
	safeKey     = regexp.MustCompile(`^[a-z_.-]{1,40}$`)
	gasWanted   = regexp.MustCompile(`gasWanted: (\d{1,18}),`)
	digits      = regexp.MustCompile(`\d+`)
)

// updateShapes records how each validator update is written, read from the
// raw capture so it does not depend on the decoder under test. Only key names,
// the key type word and whether a power is present are kept.
func updateShapes(result json.RawMessage, s *stats) {
	if !bytes.Contains(result, []byte("alidator")) {
		return
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(result, &top) != nil {
		return
	}
	var end map[string]json.RawMessage
	for _, key := range []string{"results", "end_block", "EndBlock"} {
		if raw, ok := top[key]; ok {
			var next map[string]json.RawMessage
			if json.Unmarshal(raw, &next) == nil && next != nil {
				if key == "results" {
					top = next
					for _, inner := range []string{"end_block", "EndBlock"} {
						if raw, ok := top[inner]; ok {
							_ = json.Unmarshal(raw, &end)
						}
					}
				} else {
					end = next
				}
			}
		}
	}
	var updates []map[string]json.RawMessage
	for _, key := range []string{"validator_updates", "ValidatorUpdates"} {
		if raw, ok := end[key]; ok && json.Unmarshal(raw, &updates) == nil {
			break
		}
	}
	for _, u := range updates {
		keys := make([]string, 0, len(u))
		for k := range u {
			if safeAction.MatchString(strings.ToLower(k)) {
				keys = append(keys, k)
			} else {
				keys = append(keys, "(other)")
			}
		}
		sort.Strings(keys)
		power := "absent"
		if raw, ok := u["power"]; ok {
			switch {
			case len(raw) > 0 && raw[0] == '"':
				power = "text"
			case len(raw) > 0 && (raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9'):
				power = "number"
			default:
				power = "other"
			}
			if string(raw) == `"0"` || string(raw) == "0" {
				power += " zero"
			}
		}
		kind := "absent"
		var key map[string]json.RawMessage
		if json.Unmarshal(u["pub_key"], &key) == nil && key != nil {
			var typ string
			_ = json.Unmarshal(key["type"], &typ)
			kind = "(other)"
			if safeKind.MatchString(strings.ToLower(typ)) {
				kind = typ
			}
			names := make([]string, 0, len(key))
			for k := range key {
				if safeAction.MatchString(k) {
					names = append(names, k)
				}
			}
			sort.Strings(names)
			kind += " with " + strings.Join(names, ",")
		}
		s.UpdateShape.add("fields "+strings.Join(keys, ",")+" | key "+kind+" | power "+power, 1)
	}
}

var keyKinds = func() map[[4]byte]string {
	out := map[[4]byte]string{}
	for _, name := range []string{"tendermint/PubKeySecp256k1", "tendermint/PubKeyEd25519", "tendermint/PubKeyMultisigThreshold"} {
		_, prefix := amino.NameToDisfix(name)
		out[[4]byte(prefix)] = strings.TrimPrefix(name, "tendermint/PubKey")
	}
	return out
}()

type counter map[string]int64

func (c counter) add(key string, n int64) { c[key] += n }
func (c counter) merge(o counter) {
	for k, v := range o {
		c[k] += v
	}
}

type dirStat struct {
	Heights  int64
	Dialects counter
}

type stats struct {
	Heights, HeightsWithTxs, Txs, TxBytes, MaxTxsInBlock int64
	FirstHeight, LastHeight                              int64
	FirstTime, LastTime                                  time.Time
	Skipped, BlockErrors, ResultErrors                   counter
	Decode, TxIssues, SigIssues, KeyKinds                counter
	MsgStatus, MsgShape, MsgOutcome, MsgsPerTx           counter
	Codes, CodeEvidence, LogShape                        counter
	TxEventTypes, TxTagKeys, Actions                     counter
	BeginEventTypes, EndEventTypes, BeginTagKeys         counter
	EndTagKeys, BlockExtras                              counter
	FeeDenoms, FeeShape, ActionCheck, SignerCheck        counter
	FailedEvidence, UpdateShape, MessageEvidence         counter
	EvidenceShape, Classified, ClassifiedRecorded        counter
	TransferCheck                                        counter
	Dirs                                                 map[int64]*dirStat
	Sample                                               int
	DirsSeen, DirsUnreadable, CachedHeights              int64
}

func newStats() *stats {
	s := &stats{Dirs: map[int64]*dirStat{}}
	for _, c := range s.counters() {
		*c = counter{}
	}
	return s
}

func (s *stats) counters() []*counter {
	return []*counter{&s.Skipped, &s.BlockErrors, &s.ResultErrors, &s.Decode, &s.TxIssues, &s.SigIssues, &s.KeyKinds,
		&s.MsgStatus, &s.MsgShape, &s.MsgOutcome, &s.MsgsPerTx, &s.Codes, &s.CodeEvidence, &s.LogShape, &s.TxEventTypes,
		&s.TxTagKeys, &s.Actions, &s.BeginEventTypes, &s.EndEventTypes, &s.BeginTagKeys, &s.EndTagKeys, &s.BlockExtras,
		&s.FeeDenoms, &s.FeeShape, &s.ActionCheck, &s.SignerCheck, &s.FailedEvidence, &s.UpdateShape, &s.MessageEvidence, &s.EvidenceShape, &s.Classified, &s.ClassifiedRecorded, &s.TransferCheck}
}

func (s *stats) merge(o *stats) {
	a, b := s.counters(), o.counters()
	for i := range a {
		a[i].merge(*b[i])
	}
	s.Heights += o.Heights
	s.DirsSeen += o.DirsSeen
	s.DirsUnreadable += o.DirsUnreadable
	s.CachedHeights += o.CachedHeights
	s.Sample = o.Sample
	s.HeightsWithTxs += o.HeightsWithTxs
	s.Txs += o.Txs
	s.TxBytes += o.TxBytes
	if o.MaxTxsInBlock > s.MaxTxsInBlock {
		s.MaxTxsInBlock = o.MaxTxsInBlock
	}
	if o.FirstHeight != 0 && (s.FirstHeight == 0 || o.FirstHeight < s.FirstHeight) {
		s.FirstHeight, s.FirstTime = o.FirstHeight, o.FirstTime
	}
	if o.LastHeight > s.LastHeight {
		s.LastHeight, s.LastTime = o.LastHeight, o.LastTime
	}
	for dir, d := range o.Dirs {
		mine := s.Dirs[dir]
		if mine == nil {
			mine = &dirStat{Dialects: counter{}}
			s.Dirs[dir] = mine
		}
		mine.Heights += d.Heights
		mine.Dialects.merge(d.Dialects)
	}
}

// spill keeps repeat detection off the heap. A record is a truncated hash, the
// hub number, height, index and result code. Each run writes its own
// subdirectory so an interrupted run can be repeated without double counting;
// analyze reads one run or every run under the root.
type spill struct {
	mu      sync.Mutex
	dir     string
	files   [spillBuckets]*os.File
	writers [spillBuckets]*bufio.Writer
}

var hubNumber = map[string]uint8{"cosmoshub-1": 1, "cosmoshub-2": 2, "cosmoshub-3": 3}

func newSpill(root, run string) (*spill, error) {
	sp := &spill{dir: filepath.Join(root, run)}
	if err := os.RemoveAll(sp.dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(sp.dir, 0o700); err != nil {
		return nil, err
	}
	for i := range sp.files {
		f, err := os.OpenFile(filepath.Join(sp.dir, fmt.Sprintf("bucket-%02d", i)), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		sp.files[i] = f
		sp.writers[i] = bufio.NewWriterSize(f, 8<<10)
	}
	return sp, nil
}

func (sp *spill) add(hash [32]byte, chain uint8, height int64, index int, code uint32) {
	var rec [recordSize]byte
	copy(rec[:16], hash[:16])
	rec[16] = chain
	binary.BigEndian.PutUint64(rec[17:25], uint64(height))
	binary.BigEndian.PutUint16(rec[25:27], uint16(index))
	binary.BigEndian.PutUint32(rec[27:31], code)
	sp.mu.Lock()
	_, _ = sp.writers[hash[0]%spillBuckets].Write(rec[:])
	sp.mu.Unlock()
}

func (sp *spill) finish() error {
	for i, f := range sp.files {
		if err := sp.writers[i].Flush(); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	// The marker lets a whole-set analysis ignore a run that was cut short.
	return os.WriteFile(filepath.Join(sp.dir, "complete"), nil, 0o600)
}

type inclusion struct {
	chain  uint8
	height int64
	index  int
	code   uint32
}

// analyzeRuns counts repeats across the given complete run directories.
func analyzeRuns(dirs []string) (map[string]counter, error) {
	out := map[string]counter{"cross-chain": {}}
	name := func(n uint8) string { return "cosmoshub-" + strconv.Itoa(int(n)) }
	for bucketNumber := 0; bucketNumber < spillBuckets; bucketNumber++ {
		seen := map[[16]byte][]inclusion{}
		for _, dir := range dirs {
			f, err := os.Open(filepath.Join(dir, fmt.Sprintf("bucket-%02d", bucketNumber)))
			if err != nil {
				return nil, err
			}
			reader := bufio.NewReaderSize(f, 64<<10)
			var rec [recordSize]byte
			for {
				if _, err := io.ReadFull(reader, rec[:]); err != nil {
					if err == io.EOF {
						break
					}
					f.Close()
					return nil, err
				}
				key := [16]byte(rec[:16])
				seen[key] = append(seen[key], inclusion{chain: rec[16], height: int64(binary.BigEndian.Uint64(rec[17:25])), index: int(binary.BigEndian.Uint16(rec[25:27])), code: binary.BigEndian.Uint32(rec[27:31])})
			}
			f.Close()
		}
		for _, list := range seen {
			byChain := map[uint8][]inclusion{}
			for _, item := range list {
				byChain[item.chain] = append(byChain[item.chain], item)
			}
			if len(byChain) > 1 {
				out["cross-chain"].add("hashes included on more than one chain", 1)
			}
			for chain, items := range byChain {
				c := out[name(chain)]
				if c == nil {
					c = counter{}
					out[name(chain)] = c
				}
				c.add("distinct hashes", 1)
				if len(items) < 2 {
					continue
				}
				sort.Slice(items, func(a, b int) bool {
					if items[a].height != items[b].height {
						return items[a].height < items[b].height
					}
					return items[a].index < items[b].index
				})
				c.add("hashes included more than once", 1)
				c.add("extra inclusions beyond the first", int64(len(items)-1))
				successes := 0
				for _, item := range items {
					if item.code == 0 {
						successes++
					}
				}
				c.add(fmt.Sprintf("repeated hash with %s successful inclusions", bucket(int64(successes), 0, 1)), 1)
				for j := 1; j < len(items); j++ {
					prev, cur := items[j-1], items[j]
					gap := cur.height - prev.height
					switch {
					case gap == 0:
						c.add("repeat gap: same height", 1)
					case gap <= 3:
						c.add(fmt.Sprintf("repeat gap: %d heights", gap), 1)
					case gap <= 100:
						c.add("repeat gap: 4 to 100 heights", 1)
					default:
						c.add("repeat gap: over 100 heights", 1)
					}
					c.add(fmt.Sprintf("repeat sequence: %s then %s", outcome(prev.code), outcome(cur.code)), 1)
				}
			}
		}
	}
	return out, nil
}

func completeRuns(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "complete")); e.IsDir() && err == nil {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

func outcome(code uint32) string {
	if code == 0 {
		return "success"
	}
	return "failure"
}

func bucket(n int64, limits ...int64) string {
	for _, l := range limits {
		if n <= l {
			return strconv.FormatInt(l, 10)
		}
	}
	return "more than " + strconv.FormatInt(limits[len(limits)-1], 10)
}

func safe(err error) string {
	text := digits.ReplaceAllString(err.Error(), "N")
	if !safeText.MatchString(text) {
		return "error text withheld"
	}
	return text
}

func readCapture(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("capture unreadable")
	}
	defer f.Close()
	zr, err := gzip.NewReader(bufio.NewReaderSize(f, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("capture is not gzip")
	}
	body, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, fmt.Errorf("capture gzip stream damaged")
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("capture exceeds decompressed limit")
	}
	return body, nil
}

// shape describes Amino field numbers and wire classes only. b20 is a 20 byte
// value, bnum an all-digit string (an sdk.Int or sdk.Dec), b any other bytes.
func shape(raw []byte) string {
	if len(raw) < 4 {
		return "truncated"
	}
	body := raw[4:]
	if raw[0] == 0 {
		if len(raw) < 8 {
			return "truncated"
		}
		body = raw[8:]
	}
	var order []uint64
	class := map[uint64]string{}
	repeated := map[uint64]bool{}
	for len(body) > 0 {
		key, n := binary.Uvarint(body)
		if n <= 0 {
			return "unscannable"
		}
		body = body[n:]
		num, wire := key>>3, key&7
		var kind string
		switch wire {
		case 0:
			_, n := binary.Uvarint(body)
			if n <= 0 {
				return "unscannable"
			}
			body, kind = body[n:], "v"
		case 1:
			if len(body) < 8 {
				return "unscannable"
			}
			body, kind = body[8:], "f64"
		case 5:
			if len(body) < 4 {
				return "unscannable"
			}
			body, kind = body[4:], "f32"
		case 2:
			l, n := binary.Uvarint(body)
			if n <= 0 || l > uint64(len(body)-n) {
				return "unscannable"
			}
			value := body[n : n+int(l)]
			body = body[n+int(l):]
			kind = "b"
			if len(value) == 20 {
				kind = "b20"
			} else if len(value) > 0 && bytes.Trim(value, "0123456789") == nil {
				kind = "bnum"
			}
		default:
			return "unscannable"
		}
		if previous, ok := class[num]; ok {
			repeated[num] = true
			if previous != kind {
				class[num] = "mixed"
			}
			continue
		}
		class[num] = kind
		order = append(order, num)
	}
	var parts []string
	for _, num := range order {
		part := fmt.Sprintf("%d:%s", num, class[num])
		if repeated[num] {
			part += "*"
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "empty"
	}
	return strings.Join(parts, " ")
}

type job struct {
	chain  string
	number uint8
	dir    int64
	height int64
	block  string
	result string
}

type options struct {
	maxFile, maxBody int64
	// emptyStride above zero reads the result of a height with no
	// transactions only when the height is a multiple of it.
	emptyStride int64
}

func process(j job, opt options, s *stats, sp *spill) {
	for _, path := range []string{j.block, j.result} {
		info, err := os.Stat(path)
		if err != nil {
			s.Skipped.add("capture vanished", 1)
			return
		}
		if info.Size() > opt.maxFile {
			s.Skipped.add("compressed capture over size guard", 1)
			return
		}
	}
	raw, err := readCapture(j.block, opt.maxBody)
	if err != nil {
		s.BlockErrors.add(safe(err), 1)
		return
	}
	var block struct {
		Result struct {
			Block struct {
				Header struct {
					ChainID string    `json:"chain_id"`
					Height  int64     `json:"height,string"`
					Time    time.Time `json:"time"`
				} `json:"header"`
				Data struct {
					Txs []string `json:"txs"`
				} `json:"data"`
			} `json:"block"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &block) != nil {
		s.BlockErrors.add("block JSON invalid", 1)
		return
	}
	header := block.Result.Block.Header
	if header.ChainID != j.chain || header.Height != j.height {
		s.BlockErrors.add("block identity does not match its file", 1)
		return
	}
	if opt.emptyStride > 0 && len(block.Result.Block.Data.Txs) == 0 && j.height%opt.emptyStride != 0 {
		d := s.Dirs[j.dir]
		if d == nil {
			d = &dirStat{Dialects: counter{}}
			s.Dirs[j.dir] = d
		}
		d.Heights++
		d.Dialects.add("(no transactions, result not read)", 1)
		s.Heights++
		if s.FirstHeight == 0 || j.height < s.FirstHeight {
			s.FirstHeight, s.FirstTime = j.height, header.Time
		}
		if j.height > s.LastHeight {
			s.LastHeight, s.LastTime = j.height, header.Time
		}
		s.BlockExtras.add("heights with no transactions whose result was not read", 1)
		return
	}
	raw, err = readCapture(j.result, opt.maxBody)
	if err != nil {
		s.ResultErrors.add(safe(err), 1)
		return
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &envelope) != nil || len(envelope.Result) == 0 {
		s.ResultErrors.add("result envelope invalid", 1)
		return
	}
	raw = nil
	updateShapes(envelope.Result, s)
	var results rpc.CustomBlockResults
	if err := tmjson.Unmarshal(envelope.Result, &results); err != nil {
		s.ResultErrors.add(safe(err), 1)
		return
	}
	dialect := "modern flat (not a legacy envelope)"
	if results.Legacy != nil {
		dialect = results.Legacy.Dialect
	}
	d := s.Dirs[j.dir]
	if d == nil {
		d = &dirStat{Dialects: counter{}}
		s.Dirs[j.dir] = d
	}
	d.Heights++
	d.Dialects.add(dialect, 1)
	s.Heights++
	if s.FirstHeight == 0 || j.height < s.FirstHeight {
		s.FirstHeight, s.FirstTime = j.height, header.Time
	}
	if j.height > s.LastHeight {
		s.LastHeight, s.LastTime = j.height, header.Time
	}
	if results.Height != j.height {
		s.ResultErrors.add("result height does not match its file", 1)
	}
	for _, e := range results.BeginBlockEvents {
		s.BeginEventTypes.add(e.Type, 1)
	}
	for _, e := range results.EndBlockEvents {
		s.EndEventTypes.add(e.Type, 1)
	}
	if results.Legacy != nil {
		for _, t := range results.Legacy.BeginTags {
			s.BeginTagKeys.add(label(t.Key), 1)
		}
		for _, t := range results.Legacy.EndTags {
			s.EndTagKeys.add(label(t.Key), 1)
		}
	}
	if len(results.ValidatorUpdates) > 0 {
		s.BlockExtras.add("heights with validator updates", 1)
	}
	if results.ConsensusParamUpdates != nil {
		s.BlockExtras.add("heights with consensus parameter updates", 1)
	}
	txs := block.Result.Block.Data.Txs
	if len(txs) != len(results.TxsResults) {
		s.ResultErrors.add("transaction count differs from result count", 1)
		return
	}
	if len(txs) == 0 {
		return
	}
	s.HeightsWithTxs++
	if int64(len(txs)) > s.MaxTxsInBlock {
		s.MaxTxsInBlock = int64(len(txs))
	}
	for i, encoded := range txs {
		s.Txs++
		result := results.TxsResults[i]
		code := fmt.Sprintf("code %d", result.Code)
		if result.Codespace != "" && safeAction.MatchString(result.Codespace) {
			code += " codespace " + result.Codespace
		}
		s.Codes.add(code, 1)
		var tags []rpc.LegacyTag
		if results.Legacy != nil {
			s.CodeEvidence.add(results.Legacy.CodeEvidence[i], 1)
			tags = results.Legacy.TxTags[i]
		}
		var actions []string
		for _, e := range result.Events {
			s.TxEventTypes.add(e.Type, 1)
			if e.Type == "message" {
				for _, a := range e.Attributes {
					if a.Key == "action" {
						actions = append(actions, a.Value)
					}
				}
			}
		}
		for _, t := range tags {
			s.TxTagKeys.add(label(t.Key), 1)
			if string(t.Key) == "action" {
				actions = append(actions, string(t.Value))
			}
		}
		for _, a := range actions {
			if safeAction.MatchString(a) {
				s.Actions.add(a, 1)
			} else {
				s.Actions.add("(non-identifier action value)", 1)
			}
		}
		log := strings.TrimSpace(result.Log)
		switch {
		case log == "":
			s.LogShape.add(outcome(result.Code)+": empty log", 1)
		case strings.HasPrefix(log, "["):
			s.LogShape.add(outcome(result.Code)+": structured array log", 1)
		case strings.HasPrefix(log, "{"):
			s.LogShape.add(outcome(result.Code)+": object log", 1)
		default:
			s.LogShape.add(outcome(result.Code)+": plain text log", 1)
		}
		if result.Code != 0 {
			evidence := "failed with no events or tags"
			if len(result.Events) > 0 || len(tags) > 0 {
				evidence = "failed with events or tags"
			}
			if result.GasUsed == 0 {
				evidence += ", gas used zero"
			} else {
				evidence += ", gas used nonzero"
			}
			s.FailedEvidence.add(evidence, 1)
		}
		txRaw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			s.Decode.add("failed: transaction base64 invalid", 1)
			continue
		}
		s.TxBytes += int64(len(txRaw))
		sp.add(sha256.Sum256(txRaw), j.number, j.height, i, result.Code)
		tx, err := legacy.Decode(j.chain, txRaw)
		if err != nil {
			s.Decode.add("failed: "+safe(err), 1)
			continue
		}
		s.Decode.add("ok", 1)
		for _, issue := range tx.Issues {
			s.TxIssues.add(issue, 1)
		}
		s.MsgsPerTx.add(bucket(int64(len(tx.Messages)), 0, 1, 2, 5, 20), 1)
		var expected []string
		projected := true
		for _, m := range tx.Messages {
			name, status := m.AminoName, "projected"
			switch {
			case m.AminoName == "":
				name, status = "prefix "+m.Prefix, "opaque (unknown prefix)"
			case m.Derived == nil && len(m.Signers) > 0:
				status = "unprojected (share-valued amount, signer known)"
			case m.Derived == nil:
				status = "unsupported (known name, no projection)"
			}
			if status != "projected" {
				projected = false
				s.MsgShape.add(name+" | "+shape(m.Raw), 1)
			}
			s.MsgStatus.add(name+" | "+status, 1)
			s.MsgOutcome.add(name+" | in "+outcome(result.Code)+" transaction", 1)
			expected = append(expected, legacy.ActionName(j.chain, m.AminoName))
		}
		if projected {
			s.Decode.add("ok and every message projected", 1)
		}
		if result.Code == 0 {
			switch {
			case len(actions) == 0:
				s.ActionCheck.add("success without action evidence", 1)
			case strings.Join(actions, ",") == strings.Join(expected, ","):
				s.ActionCheck.add("decoded message sequence equals recorded actions", 1)
			case len(actions) != len(expected):
				s.ActionCheck.add("recorded action count differs from decoded messages", 1)
			default:
				s.ActionCheck.add("recorded actions differ from decoded message types", 1)
			}
		}
		messages, err := evidence.For(j.chain, tx, result, tags)
		switch {
		case err != nil:
			s.MessageEvidence.add(outcome(result.Code)+": unattributed, "+safe(err), 1)
		case result.Code == 0:
			s.MessageEvidence.add("success: every message has its own evidence", 1)
		case len(messages) > 0:
			s.MessageEvidence.add("failed: a message began executing, fee charged, "+code, 1)
			s.MessageEvidence.add(fmt.Sprintf("failed: stopped at message %s of %s", bucket(int64(len(messages)), 1, 2, 5, 20), bucket(int64(len(tx.Messages)), 1, 2, 5, 20)), 1)
		case evidence.AnteOnlyLocation(evidence.OutOfGasLocation(result)):
			s.MessageEvidence.add("failed: out of gas in the ante handler at "+evidence.OutOfGasLocation(result)+", fee not charged", 1)
		case evidence.OutOfGasLocation(result) != "":
			s.MessageEvidence.add("failed: no execution marker, out of gas at "+evidence.OutOfGasLocation(result)+", fee charge unknown", 1)
			wanted := int64(-1)
			if m := gasWanted.FindStringSubmatch(result.Log); m != nil {
				wanted, _ = strconv.ParseInt(m[1], 10, 64)
			}
			s.MessageEvidence.add(fmt.Sprintf("fee charge unknown: gas wanted up to %s, transaction bytes up to %s, signers up to %s",
				bucket(wanted, 0, 20000, 50000, 100000, 200000), bucket(int64(len(encoded)*3/4), 300, 600, 1200), bucket(int64(len(tx.Signers)), 1, 2)), 1)
		default:
			s.MessageEvidence.add("failed: no execution marker, fee charge unknown, "+code, 1)
		}
		if err == nil {
			for _, m := range messages {
				if m.Success {
					evidenceShape(tx.Messages[m.Index], m, s)
				}
			}
			if j.chain == "cosmoshub-3" && result.Code == 0 {
				transferCheck(tx, messages, result.Events, s)
			}
		}
		switch {
		case len(tx.Fee) == 0:
			s.FeeShape.add(outcome(result.Code)+": no fee coins declared", 1)
		case len(tx.Fee) == 1:
			s.FeeShape.add(outcome(result.Code)+": one fee coin declared", 1)
		default:
			s.FeeShape.add(outcome(result.Code)+": several fee coins declared", 1)
		}
		for _, c := range tx.Fee {
			if safeAction.MatchString(c.Denom) {
				s.FeeDenoms.add(c.Denom, 1)
			} else {
				s.FeeDenoms.add("(other denom)", 1)
			}
		}
		for _, sig := range tx.Signatures {
			kind := "absent"
			if len(sig.PublicKeyRaw) >= 4 {
				kind = keyKinds[[4]byte(sig.PublicKeyRaw[:4])]
				if kind == "" {
					kind = "unrecognized"
				}
			}
			s.KeyKinds.add(kind, 1)
			if sig.Issue != "" {
				s.SigIssues.add(sig.Issue, 1)
			}
		}
		switch {
		case !tx.SignersComplete:
			s.SignerCheck.add("signers unresolved (unsupported message present)", 1)
		case len(tx.Signers) != len(tx.Signatures):
			s.SignerCheck.add("signature count differs from message signers", 1)
		default:
			match, absent := true, false
			for k, sig := range tx.Signatures {
				if len(sig.Address) == 0 {
					absent = true
				} else if !bytes.Equal(sig.Address, tx.Signers[k]) {
					match = false
				}
			}
			switch {
			case !match:
				s.SignerCheck.add("a public key address differs from its message signer", 1)
			case absent:
				s.SignerCheck.add("signers resolved, some public keys absent", 1)
			default:
				s.SignerCheck.add("every public key address equals its message signer", 1)
			}
		}
	}
}

var taxParser = &tax.Parser{ID: "legacy-census"}

// evidenceShape records, for one executed message, which tag keys or event
// types the chain left for it, and what the serving tax classifier makes of
// the projection when it is given those events unchanged. Hub 1 and Hub 2
// tags are not events, so only their keys are counted. Only message names,
// key names, event type names and category names are kept.
func evidenceShape(msg legacy.Message, m evidence.Message, s *stats) {
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, t := range m.Tags {
		add(label(t.Key))
	}
	for _, e := range m.Events {
		if safeAction.MatchString(e.Type) {
			add(e.Type)
		} else {
			add("(non-identifier event type)")
		}
	}
	sort.Strings(names)
	kind := "tag keys"
	if m.Events != nil {
		kind = "event types"
	}
	s.EvidenceShape.add(msg.AminoName+" | "+kind+": "+strings.Join(names, ","), 1)
	if m.Events == nil {
		return
	}
	if msg.Derived == nil {
		s.Classified.add(msg.AminoName+" | not classified, no projection", 1)
		return
	}
	recorded := classifyRows(msg, m.Index, m.Events)
	s.ClassifiedRecorded.add(msg.AminoName+" | "+recorded, 1)
	paid, err := evidence.TransferSenders(msg.AminoName, m.Events)
	if err != nil {
		s.Classified.add(msg.AminoName+" | not classified, "+safe(err), 1)
		return
	}
	s.Classified.add(msg.AminoName+" | "+classifyRows(msg, m.Index, paid), 1)
}

// classifyRows gives the serving tax classifier one projection and its
// events, and describes the rows only by category and a count bucket.
func classifyRows(msg legacy.Message, index int, events []abci.Event) (description string) {
	defer func() {
		if recover() != nil {
			description = "classifier panic"
		}
	}()
	log := &indexertx.LogMessage{MessageIndex: index}
	for _, e := range events {
		event := indexertx.LogMessageEvent{Type: e.Type}
		for _, a := range e.Attributes {
			event.Attributes = append(event.Attributes, indexertx.Attribute{Key: a.Key, Value: a.Value})
		}
		log.Events = append(log.Events, event)
	}
	out, err := taxParser.ParseMessage(msg.Derived, log, config.IndexConfig{})
	var incomplete *tax.ClassificationIncompleteError
	switch {
	case errors.As(err, &incomplete):
		return "classifier reports incomplete"
	case err != nil || out == nil:
		return "classifier error"
	}
	rows, _ := (*out).([]tax.TaxableEvent)
	categories := map[string]int{}
	for _, row := range rows {
		if safeAction.MatchString(row.Category) {
			categories[row.Category]++
		} else {
			categories["(other category)"]++
		}
	}
	var parts []string
	for category, n := range categories {
		parts = append(parts, category+" x"+bucket(int64(n), 1, 2, 5, 20))
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return "no rows"
	}
	return strings.Join(parts, ", ")
}

// transferCheck compares the payer given to each Hub 3 transfer from the
// per message log against the flat event list of the same result, where the
// bank wrote each transfer immediately before its payer.
func transferCheck(tx *legacy.Tx, messages []evidence.Message, flat []abci.Event, s *stats) {
	var fromLog, fromFlat []string
	for _, m := range messages {
		name := tx.Messages[m.Index].AminoName
		if name == "cosmos-sdk/MsgMultiSend" {
			s.TransferCheck.add("not compared: transaction holds a multisend", 1)
			return
		}
		paid, err := evidence.TransferSenders(name, m.Events)
		if err != nil {
			s.TransferCheck.add("not compared: "+safe(err), 1)
			return
		}
		for _, e := range paid {
			if e.Type != "transfer" {
				continue
			}
			for i := 0; i+2 < len(e.Attributes); i += 3 {
				fromLog = append(fromLog, e.Attributes[i].Value+"|"+e.Attributes[i+1].Value+"|"+e.Attributes[i+2].Value)
			}
		}
	}
	for i, e := range flat {
		if e.Type != "transfer" {
			continue
		}
		if len(e.Attributes) != 2 || e.Attributes[0].Key != "recipient" || e.Attributes[1].Key != "amount" ||
			i+1 >= len(flat) || flat[i+1].Type != "message" || len(flat[i+1].Attributes) != 1 || flat[i+1].Attributes[0].Key != "sender" {
			s.TransferCheck.add("flat transfer is not followed by its payer", 1)
			return
		}
		fromFlat = append(fromFlat, e.Attributes[0].Value+"|"+flat[i+1].Attributes[0].Value+"|"+e.Attributes[1].Value)
	}
	switch {
	case len(fromLog) == 0 && len(fromFlat) == 0:
		s.TransferCheck.add("no transfers", 1)
	case strings.Join(fromLog, ",") == strings.Join(fromFlat, ","):
		s.TransferCheck.add("payers from the log equal payers from the flat events", 1)
		s.TransferCheck.add("transfers given a payer", int64(len(fromLog)))
	default:
		s.TransferCheck.add("payers from the log differ from the flat events", 1)
	}
}

func label(key []byte) string {
	if safeKey.Match(key) {
		return string(key)
	}
	return "(non-identifier key)"
}

func listDir(path string) ([]int64, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	seen := map[int64]uint8{}
	for _, e := range entries {
		m := captureName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		h, _ := strconv.ParseInt(m[1], 10, 64)
		if m[2] == "block" {
			seen[h] |= 1
		} else {
			seen[h] |= 2
		}
	}
	var heights []int64
	for h, bits := range seen {
		if bits == 3 {
			heights = append(heights, h)
		}
	}
	sort.Slice(heights, func(a, b int) bool { return heights[a] < heights[b] })
	return heights, nil
}

func printCounter(w io.Writer, title string, c counter) {
	if len(c) == 0 {
		return
	}
	fmt.Fprintf(w, "\n  %s\n", title)
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool {
		if c[keys[a]] != c[keys[b]] {
			return c[keys[a]] > c[keys[b]]
		}
		return keys[a] < keys[b]
	})
	for _, k := range keys {
		fmt.Fprintf(w, "    %12d  %s\n", c[k], k)
	}
}

// result is what one run emits: aggregate counters only.
type result struct {
	Started  time.Time
	Elapsed  string
	Workers  int
	MaxFile  int64
	Dirs     string
	Chains   map[string]*stats
	Repeats  map[string]counter
	RunCount int
}

func report(out io.Writer, r *result, repeatScope string) {
	fmt.Fprintf(out, "legacy census, started %s, %d run(s) merged\nworkers %d, size guard %d bytes compressed\n", r.Started.UTC().Format(time.RFC3339), r.RunCount, r.Workers, r.MaxFile)
	var chains []string
	for chain := range r.Chains {
		chains = append(chains, chain)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(chains)))
	for _, chain := range chains {
		total := r.Chains[chain]
		fmt.Fprintf(out, "\n== %s\n", chain)
		fmt.Fprintf(out, "  sample: %d contiguous heights per 10000 height directory (0 means every cached height)\n", total.Sample)
		fmt.Fprintf(out, "  directories with captures: %d (unreadable %d), complete cached heights seen: %d\n", total.DirsSeen, total.DirsUnreadable, total.CachedHeights)
		fmt.Fprintf(out, "  heights read: %d, lowest %d (%s), highest %d (%s)\n", total.Heights, total.FirstHeight, total.FirstTime.UTC().Format("2006-01-02"), total.LastHeight, total.LastTime.UTC().Format("2006-01-02"))
		fmt.Fprintf(out, "  heights with transactions: %d, transactions: %d, transaction bytes: %d, most transactions in one height: %d\n", total.HeightsWithTxs, total.Txs, total.TxBytes, total.MaxTxsInBlock)
		printCounter(out, "heights skipped", total.Skipped)
		printCounter(out, "block capture errors", total.BlockErrors)
		printCounter(out, "result capture errors", total.ResultErrors)
		printCounter(out, "validator update shapes", total.UpdateShape)
		printCounter(out, "transaction decode", total.Decode)
		printCounter(out, "transaction issues from the decoder", total.TxIssues)
		printCounter(out, "messages by type and decoder status", total.MsgStatus)
		printCounter(out, "field shapes of messages without a projection", total.MsgShape)
		printCounter(out, "messages by type and transaction outcome", total.MsgOutcome)
		printCounter(out, "messages per decoded transaction (upper bound of bucket)", total.MsgsPerTx)
		printCounter(out, "decoded messages against recorded actions (successful transactions)", total.ActionCheck)
		printCounter(out, "evidence attributed to each message, and fee charge of failed transactions", total.MessageEvidence)
		printCounter(out, "evidence kept per executed message, by message type", total.EvidenceShape)
		printCounter(out, "serving tax classifier on Hub 3 projections, events as recorded (rows by category, count bucket)", total.ClassifiedRecorded)
		printCounter(out, "serving tax classifier on Hub 3 projections, transfers given their payer", total.Classified)
		printCounter(out, "Hub 3 transfer payers, per message log against flat events (successful transactions)", total.TransferCheck)
		printCounter(out, "recorded action values", total.Actions)
		printCounter(out, "signer cross-check", total.SignerCheck)
		printCounter(out, "public key kinds", total.KeyKinds)
		printCounter(out, "signature issues", total.SigIssues)
		printCounter(out, "result codes", total.Codes)
		printCounter(out, "result code evidence", total.CodeEvidence)
		printCounter(out, "failed transaction evidence", total.FailedEvidence)
		printCounter(out, "result log shape", total.LogShape)
		printCounter(out, "declared fee shape", total.FeeShape)
		printCounter(out, "declared fee denoms", total.FeeDenoms)
		printCounter(out, "transaction event types", total.TxEventTypes)
		printCounter(out, "transaction tag keys", total.TxTagKeys)
		printCounter(out, "begin block event types", total.BeginEventTypes)
		printCounter(out, "end block event types", total.EndEventTypes)
		printCounter(out, "begin block tag keys", total.BeginTagKeys)
		printCounter(out, "end block tag keys", total.EndTagKeys)
		printCounter(out, "block level extras", total.BlockExtras)
		fmt.Fprintf(out, "\n  result dialect by height range\n")
		var numbers []int64
		for dir := range total.Dirs {
			numbers = append(numbers, dir)
		}
		sort.Slice(numbers, func(a, b int) bool { return numbers[a] < numbers[b] })
		signature := func(d *dirStat) string {
			var names []string
			for name := range d.Dialects {
				names = append(names, name)
			}
			sort.Strings(names)
			return strings.Join(names, " + ")
		}
		for i := 0; i < len(numbers); {
			k, heights := i, int64(0)
			for k < len(numbers) && signature(total.Dirs[numbers[k]]) == signature(total.Dirs[numbers[i]]) && (k == i || numbers[k] == numbers[k-1]+1) {
				heights += total.Dirs[numbers[k]].Heights
				k++
			}
			fmt.Fprintf(out, "    heights %d to %d: %s (%d heights read)\n", numbers[i]*10000, numbers[k-1]*10000+9999, signature(total.Dirs[numbers[i]]), heights)
			i = k
		}
	}
	fmt.Fprintf(out, "\n== repeated transaction hashes (counts only; %s)\n", repeatScope)
	var names []string
	for name := range r.Repeats {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		printCounter(out, name, r.Repeats[name])
	}
	fmt.Fprintf(out, "\nelapsed %s\n", r.Elapsed)
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}

func main() {
	root := flag.String("root", "/cache", "cache root holding <chain>/<height/10000>/ captures")
	chainList := flag.String("chains", "cosmoshub-3", "comma separated chain ids, each optionally with :<sample size>")
	perDir := flag.Int("per-dir", 100, "contiguous heights sampled from each 10000 height directory; 0 reads all")
	dirRange := flag.String("dirs", "", "only directories in this inclusive range, for example 0-49")
	workers := flag.Int("workers", 1, "parallel readers")
	maxFile := flag.Int64("max-file-bytes", 2<<20, "skip and count heights whose compressed capture is larger")
	maxBody := flag.Int64("max-body-bytes", 24<<20, "decompressed size limit per capture")
	emptyStride := flag.Int64("empty-stride", 0, "read the result of a height with no transactions only at multiples of this height; 0 reads every result")
	spillRoot := flag.String("spill-dir", "", "private scratch directory for repeat detection")
	run := flag.String("run", "run", "name of this run inside the spill directory")
	asJSON := flag.Bool("json", false, "emit this run's aggregate counters as JSON for a later merge")
	merge := flag.String("merge", "", "comma separated JSON run files to merge into one report; reads no cache")
	repeatsFile := flag.String("repeats", "", "with merge: JSON from analyze-all to use for the repeat section")
	analyzeAll := flag.Bool("analyze-all", false, "count repeats across every complete run in the spill directory and emit JSON; reads no cache")
	progress := flag.Int("progress-dirs", 25, "report progress to stderr every N directories")
	flag.Parse()
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	if *merge != "" {
		total := &result{Chains: map[string]*stats{}, Repeats: map[string]counter{}}
		var elapsed time.Duration
		for _, path := range strings.Split(*merge, ",") {
			raw, err := os.ReadFile(path)
			if err != nil {
				fail("cannot read a run file")
			}
			var r result
			if json.Unmarshal(raw, &r) != nil {
				fail("run file is not census JSON")
			}
			if total.Started.IsZero() || r.Started.Before(total.Started) {
				total.Started = r.Started
			}
			total.Workers, total.MaxFile = r.Workers, r.MaxFile
			total.RunCount++
			d, _ := time.ParseDuration(r.Elapsed)
			elapsed += d
			for chain, s := range r.Chains {
				if total.Chains[chain] == nil {
					total.Chains[chain] = newStats()
				}
				total.Chains[chain].merge(s)
			}
			for name, c := range r.Repeats {
				if total.Repeats[name] == nil {
					total.Repeats[name] = counter{}
				}
				total.Repeats[name].merge(c)
			}
		}
		total.Elapsed = elapsed.String()
		scope := "summed per run, so a repeat that spans two runs is not counted"
		if *repeatsFile != "" {
			raw, err := os.ReadFile(*repeatsFile)
			var repeats map[string]counter
			if err != nil || json.Unmarshal(raw, &repeats) != nil {
				fail("cannot read the repeats file")
			}
			total.Repeats = repeats
			scope = "counted across every run together"
		}
		report(out, total, scope)
		return
	}
	if *spillRoot == "" {
		fail("spill-dir is required")
	}
	if *analyzeAll {
		dirs, err := completeRuns(*spillRoot)
		if err != nil {
			fail("cannot list the spill directory")
		}
		repeats, err := analyzeRuns(dirs)
		if err != nil {
			fail("repeat analysis failed")
		}
		repeats["runs"] = counter{"complete runs analyzed": int64(len(dirs))}
		_ = json.NewEncoder(out).Encode(repeats)
		return
	}

	first, last := int64(0), int64(1<<62)
	if *dirRange != "" {
		lo, hi, ok := strings.Cut(*dirRange, "-")
		var errLo, errHi error
		first, errLo = strconv.ParseInt(lo, 10, 64)
		last, errHi = strconv.ParseInt(hi, 10, 64)
		if !ok || errLo != nil || errHi != nil || last < first {
			fail("invalid directory range")
		}
	}
	// A chain may carry its own sample size, for example cosmoshub-2:0.
	var chains []string
	sample := map[string]int{}
	for _, item := range strings.Split(*chainList, ",") {
		name, size, found := strings.Cut(item, ":")
		if hubNumber[name] == 0 {
			fail("unsupported chain")
		}
		chains = append(chains, name)
		sample[name] = *perDir
		if found {
			n, err := strconv.Atoi(size)
			if err != nil || n < 0 {
				fail("invalid per chain sample size")
			}
			sample[name] = n
		}
	}
	sp, err := newSpill(*spillRoot, *run)
	if err != nil {
		fail("cannot create spill directory")
	}
	started := time.Now()
	opt := options{maxFile: *maxFile, maxBody: *maxBody, emptyStride: *emptyStride}
	total := &result{Started: started, Workers: *workers, MaxFile: *maxFile, Dirs: *dirRange, Chains: map[string]*stats{}, RunCount: 1}
	for _, chain := range chains {
		entries, err := os.ReadDir(filepath.Join(*root, chain))
		if err != nil {
			continue
		}
		var dirs []int64
		for _, e := range entries {
			if n, err := strconv.ParseInt(e.Name(), 10, 64); err == nil && e.IsDir() && n >= first && n <= last {
				dirs = append(dirs, n)
			}
		}
		sort.Slice(dirs, func(a, b int) bool { return dirs[a] < dirs[b] })
		jobs := make(chan job, 256)
		results := make([]*stats, *workers)
		var wg sync.WaitGroup
		for w := 0; w < *workers; w++ {
			results[w] = newStats()
			wg.Add(1)
			go func(s *stats) {
				defer wg.Done()
				for j := range jobs {
					process(j, opt, s, sp)
				}
			}(results[w])
		}
		sum := newStats()
		sum.Sample = sample[chain]
		for n, dir := range dirs {
			path := filepath.Join(*root, chain, strconv.FormatInt(dir, 10))
			heights, err := listDir(path)
			sum.DirsSeen++
			if err != nil {
				sum.DirsUnreadable++
				continue
			}
			sum.CachedHeights += int64(len(heights))
			if take := sample[chain]; take > 0 && len(heights) > take {
				start := (len(heights) - take) * int(dir*7919%1000) / 1000
				heights = heights[start : start+take]
			}
			for _, h := range heights {
				base := filepath.Join(path, strconv.FormatInt(h, 10))
				jobs <- job{chain: chain, number: hubNumber[chain], dir: dir, height: h, block: base + "-block.raw.gz", result: base + "-block_results.raw.gz"}
			}
			if *progress > 0 && (n+1)%*progress == 0 {
				fmt.Fprintf(os.Stderr, "%s: %d of %d directories queued, %s elapsed\n", chain, n+1, len(dirs), time.Since(started).Round(time.Second))
			}
		}
		close(jobs)
		wg.Wait()
		for _, s := range results {
			sum.merge(s)
		}
		sum.Sample = sample[chain]
		total.Chains[chain] = sum
	}
	if err := sp.finish(); err != nil {
		fail("cannot finish the spill")
	}
	total.Repeats, err = analyzeRuns([]string{sp.dir})
	if err != nil {
		fail("repeat analysis failed")
	}
	total.Elapsed = time.Since(started).Round(time.Second).String()
	if *asJSON {
		_ = json.NewEncoder(out).Encode(total)
		return
	}
	report(out, total, "this run only")
}
