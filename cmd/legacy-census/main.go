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

	"github.com/DefiantLabs/cosmos-indexer/legacy"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
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
	safeKey     = regexp.MustCompile(`^[a-z_.-]{1,40}$`)
	digits      = regexp.MustCompile(`\d+`)
)

// expectedAction is the old sdk.Msg Type() each Amino message emitted as its
// action tag or message.action attribute. Agreement with the stored execution
// result is independent evidence that the message decode is correct.
var expectedAction = map[string]string{
	"cosmos-sdk/MsgSend":                        "send",
	"cosmos-sdk/MsgMultiSend":                   "multisend",
	"cosmos-sdk/MsgDelegate":                    "delegate",
	"cosmos-sdk/MsgUndelegate":                  "begin_unbonding",
	"cosmos-sdk/MsgBeginRedelegate":             "begin_redelegate",
	"cosmos-sdk/MsgWithdrawDelegationReward":    "withdraw_delegator_reward",
	"cosmos-sdk/MsgWithdrawValidatorCommission": "withdraw_validator_commission",
	"cosmos-sdk/MsgModifyWithdrawAddress":       "set_withdraw_address",
	"cosmos-sdk/MsgDeposit":                     "deposit",
	"cosmos-sdk/MsgVote":                        "vote",
	"cosmos-sdk/MsgSubmitProposal":              "submit_proposal",
	"cosmos-sdk/MsgCreateValidator":             "create_validator",
	"cosmos-sdk/MsgEditValidator":               "edit_validator",
	"cosmos-sdk/MsgUnjail":                      "unjail",
	"cosmos-sdk/MsgVerifyInvariant":             "verify_invariant",
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
	FailedEvidence                                       counter
	Dirs                                                 map[int64]*dirStat
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
		&s.FeeDenoms, &s.FeeShape, &s.ActionCheck, &s.SignerCheck, &s.FailedEvidence}
}

func (s *stats) merge(o *stats) {
	a, b := s.counters(), o.counters()
	for i := range a {
		a[i].merge(*b[i])
	}
	s.Heights += o.Heights
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
// chain number, height, index and result code. Files are removed on exit.
type spill struct {
	mu      sync.Mutex
	dir     string
	files   [spillBuckets]*os.File
	writers [spillBuckets]*bufio.Writer
}

func newSpill(dir string) (*spill, error) {
	sp := &spill{dir: dir}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	for i := range sp.files {
		f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("bucket-%02d", i)), os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
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

type inclusion struct {
	chain  uint8
	height int64
	index  int
	code   uint32
}

func (sp *spill) analyze(chains []string) (map[string]counter, error) {
	out := map[string]counter{}
	for _, c := range chains {
		out[c] = counter{}
	}
	out["cross-chain"] = counter{}
	for i := range sp.files {
		if err := sp.writers[i].Flush(); err != nil {
			return nil, err
		}
		if _, err := sp.files[i].Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		seen := map[[16]byte][]inclusion{}
		reader := bufio.NewReaderSize(sp.files[i], 64<<10)
		var rec [recordSize]byte
		for {
			if _, err := io.ReadFull(reader, rec[:]); err != nil {
				if err == io.EOF {
					break
				}
				return nil, err
			}
			key := [16]byte(rec[:16])
			seen[key] = append(seen[key], inclusion{chain: rec[16], height: int64(binary.BigEndian.Uint64(rec[17:25])), index: int(binary.BigEndian.Uint16(rec[25:27])), code: binary.BigEndian.Uint32(rec[27:31])})
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
				c := out[chains[chain]]
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

func (sp *spill) close() {
	for _, f := range sp.files {
		if f != nil {
			_ = f.Close()
		}
	}
	_ = os.RemoveAll(sp.dir)
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
			case m.Derived == nil:
				status = "unsupported (known name, no projection)"
			}
			if status != "projected" {
				projected = false
				s.MsgShape.add(name+" | "+shape(m.Raw), 1)
			}
			s.MsgStatus.add(name+" | "+status, 1)
			s.MsgOutcome.add(name+" | in "+outcome(result.Code)+" transaction", 1)
			expected = append(expected, expectedAction[m.AminoName])
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

func main() {
	root := flag.String("root", "/cache", "cache root holding <chain>/<height/10000>/ captures")
	chainList := flag.String("chains", "cosmoshub-3", "comma separated chain ids")
	perDir := flag.Int("per-dir", 100, "contiguous heights sampled from each 10000 height directory; 0 reads all")
	workers := flag.Int("workers", 1, "parallel readers")
	maxFile := flag.Int64("max-file-bytes", 2<<20, "skip and count heights whose compressed capture is larger")
	maxBody := flag.Int64("max-body-bytes", 24<<20, "decompressed size limit per capture")
	spillDir := flag.String("spill-dir", "", "private scratch directory for repeat detection, removed on exit")
	progress := flag.Int("progress-dirs", 25, "report progress to stderr every N directories")
	flag.Parse()
	if *spillDir == "" {
		fmt.Fprintln(os.Stderr, "spill-dir is required")
		os.Exit(2)
	}
	// A chain may carry its own sample size, for example cosmoshub-2:0.
	var chains []string
	sample := map[string]int{}
	for _, item := range strings.Split(*chainList, ",") {
		name, size, found := strings.Cut(item, ":")
		chains = append(chains, name)
		sample[name] = *perDir
		if found {
			n, err := strconv.Atoi(size)
			if err != nil || n < 0 {
				fmt.Fprintln(os.Stderr, "invalid per chain sample size")
				os.Exit(2)
			}
			sample[name] = n
		}
	}
	sp, err := newSpill(*spillDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot create spill directory")
		os.Exit(1)
	}
	defer sp.close()
	started := time.Now()
	opt := options{maxFile: *maxFile, maxBody: *maxBody}
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	fmt.Fprintf(out, "legacy census, %s\nworkers %d, size guard %d bytes compressed\n", time.Now().UTC().Format(time.RFC3339), *workers, *maxFile)
	totals := map[string]*stats{}
	for number, chain := range chains {
		entries, err := os.ReadDir(filepath.Join(*root, chain))
		if err != nil {
			fmt.Fprintf(out, "\n== %s: no cache directory\n", chain)
			continue
		}
		var dirs []int64
		for _, e := range entries {
			if n, err := strconv.ParseInt(e.Name(), 10, 64); err == nil && e.IsDir() {
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
		var cached, incomplete int64
		for n, dir := range dirs {
			path := filepath.Join(*root, chain, strconv.FormatInt(dir, 10))
			heights, err := listDir(path)
			if err != nil {
				incomplete++
				continue
			}
			cached += int64(len(heights))
			if take := sample[chain]; take > 0 && len(heights) > take {
				start := (len(heights) - take) * int(dir*7919%1000) / 1000
				heights = heights[start : start+take]
			}
			for _, h := range heights {
				base := filepath.Join(path, strconv.FormatInt(h, 10))
				jobs <- job{chain: chain, number: uint8(number), dir: dir, height: h, block: base + "-block.raw.gz", result: base + "-block_results.raw.gz"}
			}
			if *progress > 0 && (n+1)%*progress == 0 {
				fmt.Fprintf(os.Stderr, "%s: %d of %d directories queued, %s elapsed\n", chain, n+1, len(dirs), time.Since(started).Round(time.Second))
			}
		}
		close(jobs)
		wg.Wait()
		total := newStats()
		for _, s := range results {
			total.merge(s)
		}
		totals[chain] = total
		fmt.Fprintf(out, "\n== %s\n", chain)
		fmt.Fprintf(out, "  sample: %d contiguous heights per 10000 height directory (0 means every cached height)\n", sample[chain])
		fmt.Fprintf(out, "  directories with captures: %d (unreadable %d), complete cached heights seen: %d\n", len(dirs), incomplete, cached)
		fmt.Fprintf(out, "  heights read: %d, lowest %d (%s), highest %d (%s)\n", total.Heights, total.FirstHeight, total.FirstTime.UTC().Format("2006-01-02"), total.LastHeight, total.LastTime.UTC().Format("2006-01-02"))
		fmt.Fprintf(out, "  heights with transactions: %d, transactions: %d, transaction bytes: %d, most transactions in one height: %d\n", total.HeightsWithTxs, total.Txs, total.TxBytes, total.MaxTxsInBlock)
		printCounter(out, "heights skipped", total.Skipped)
		printCounter(out, "block capture errors", total.BlockErrors)
		printCounter(out, "result capture errors", total.ResultErrors)
		printCounter(out, "transaction decode", total.Decode)
		printCounter(out, "transaction issues from the decoder", total.TxIssues)
		printCounter(out, "messages by type and decoder status", total.MsgStatus)
		printCounter(out, "field shapes of messages without a projection", total.MsgShape)
		printCounter(out, "messages by type and transaction outcome", total.MsgOutcome)
		printCounter(out, "messages per decoded transaction (upper bound of bucket)", total.MsgsPerTx)
		printCounter(out, "decoded messages against recorded actions (successful transactions)", total.ActionCheck)
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
		out.Flush()
	}
	repeats, err := sp.analyze(chains)
	if err != nil {
		fmt.Fprintln(out, "\nrepeat analysis failed")
	} else {
		fmt.Fprintf(out, "\n== repeated transaction hashes (counts only)\n")
		for _, chain := range append(append([]string{}, chains...), "cross-chain") {
			printCounter(out, chain, repeats[chain])
		}
	}
	fmt.Fprintf(out, "\nelapsed %s\n", time.Since(started).Round(time.Second))
}
