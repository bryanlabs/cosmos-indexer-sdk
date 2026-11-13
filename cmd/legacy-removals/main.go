// Command legacy-removals asks archive nodes for the chain state around
// Cosmos Hub 3 messages that took stake away from a validator, and writes a
// removal verdict for each. Hub 3 removed an unbonded validator inside the
// message that took its last stake and paid its commission with a transfer
// no event tells from a reward, so the importer reads such a payment as
// commission only on a verdict from this tool.
//
// A candidate is chain/height/transaction/message. A heights file adds every
// executed message of those heights that leaves a validator, which is how
// the messages of heights the importer held become candidates. For each
// candidate the tool
// reads the message from the retained captures, then asks every archive for
// the validator's record after the block. A record that is still there is
// "not removed". A record that is gone must have been there after the block
// before, and the commission the validator had accumulated then, in whole
// coins, is what its removal paid. An archive counts only when it also
// returns a key every height has, so an archive without the height cannot
// pass for a missing record. Archives that both answer must agree.
//
// The state before and after a block speaks for one message only: the last
// executed message of the block that leaves the validator, in a block that
// does not also create that validator. An earlier leave of the same
// validator in the block, and any leave in a block that creates it, gets no
// line, and the importer treats a message without a line as unproven.
//
// The archive addresses come from the environment and are never printed.
// Every answer is kept in the evidence directory as received. Output is
// counts only; the verdict file holds no address and no amount other than
// the commission.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/core"
	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	sdk "github.com/cosmos/cosmos-sdk/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

func fail(text string) {
	fmt.Fprintln(os.Stderr, text)
	os.Exit(2)
}

// archive is one node and its pacing. name is its position, never its address.
type archive struct {
	name     string
	url      string
	client   *http.Client
	interval time.Duration
	last     time.Time
	evidence string
	requests int
}

// errNotServed means the archive answered and does not have the height.
var errNotServed = fmt.Errorf("height not served")

// state returns the value stored under key in a module's store after height.
// An empty value is a key that is not there. A transport failure is retried
// and then fatal, so a verdict never depends on a dropped connection.
func (a *archive) state(ctx context.Context, label, store string, key []byte, height int64) ([]byte, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "abci_query",
		"params": map[string]any{"path": "/store/" + store + "/key", "data": hex.EncodeToString(key), "height": strconv.FormatInt(height, 10), "prove": false}})
	var answer []byte
	for attempt := 1; ; attempt++ {
		if wait := a.interval - time.Since(a.last); wait > 0 {
			time.Sleep(wait)
		}
		a.last = time.Now()
		a.requests++
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, bytes.NewReader(body))
		if err != nil {
			fail("archive " + a.name + ": address not usable")
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := a.client.Do(request)
		if err == nil {
			answer, err = io.ReadAll(io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			if err == nil && response.StatusCode == http.StatusOK {
				break
			}
		}
		if attempt == 6 {
			fail("archive " + a.name + ": no answer after six attempts")
		}
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
	if err := os.WriteFile(filepath.Join(a.evidence, fmt.Sprintf("archive-%s-%s.json", a.name, label)), answer, 0o600); err != nil {
		fail("evidence could not be written")
	}
	var parsed struct {
		Error  json.RawMessage `json:"error"`
		Result *struct {
			Response struct {
				Code   uint32 `json:"code"`
				Value  string `json:"value"`
				Height string `json:"height"`
			} `json:"response"`
		} `json:"result"`
	}
	if err := json.Unmarshal(answer, &parsed); err != nil {
		fail("archive " + a.name + ": answer is not JSON")
	}
	if len(parsed.Error) > 0 || parsed.Result == nil || parsed.Result.Response.Code != 0 ||
		parsed.Result.Response.Height != strconv.FormatInt(height, 10) {
		return nil, errNotServed
	}
	value, err := base64.StdEncoding.DecodeString(parsed.Result.Response.Value)
	if err != nil {
		fail("archive " + a.name + ": value is not base64")
	}
	return value, nil
}

// wholeCommission reads a stored accumulated commission (a length prefixed
// list of denomination and an amount with eighteen decimals) and returns the
// whole uatom the chain pays out of it, zero when there are none.
func wholeCommission(value []byte) (*big.Int, error) {
	bad := fmt.Errorf("stored commission not readable")
	field := func(b []byte, tag byte) ([]byte, []byte, error) {
		if len(b) == 0 || b[0] != tag {
			return nil, nil, bad
		}
		size, n := binary.Uvarint(b[1:])
		if n <= 0 || uint64(len(b)-1-n) < size {
			return nil, nil, bad
		}
		return b[1+n : 1+n+int(size)], b[1+n+int(size):], nil
	}
	whole, coins := new(big.Int), 0
	if len(value) == 0 {
		return whole, nil
	}
	size, n := binary.Uvarint(value)
	if n <= 0 || uint64(len(value)-n) != size {
		return nil, bad
	}
	rest := value[n:]
	for len(rest) > 0 {
		var coin, denom, amount []byte
		var err error
		if coin, rest, err = field(rest, 0x0a); err != nil {
			return nil, err
		}
		if denom, coin, err = field(coin, 0x0a); err != nil {
			return nil, err
		}
		if amount, coin, err = field(coin, 0x12); err != nil || len(coin) != 0 {
			return nil, bad
		}
		parsed, ok := new(big.Int).SetString(string(amount), 10)
		if coins++; !ok || parsed.Sign() < 0 || string(denom) != "uatom" || coins > 1 {
			return nil, bad
		}
		whole.Quo(parsed, new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	}
	return whole, nil
}

// verdict asks one archive about one validator around one height.
func (a *archive) verdict(ctx context.Context, label string, validator sdk.ValAddress, height int64) (string, error) {
	record := append([]byte{0x21}, validator...)
	after, err := a.state(ctx, label+"-record-after", "staking", record, height)
	if err != nil {
		return "", err
	}
	if len(after) > 0 {
		return "not removed", nil
	}
	// Every height has a total power. Without it this archive does not have
	// the height, and the missing record proves nothing.
	if control, err := a.state(ctx, label+"-control-after", "staking", []byte{0x12}, height); err != nil || len(control) == 0 {
		return "", errNotServed
	}
	before, err := a.state(ctx, label+"-record-before", "staking", record, height-1)
	if err != nil {
		return "", err
	}
	if len(before) == 0 {
		return "", fmt.Errorf("the validator has no record before the block either")
	}
	stored, err := a.state(ctx, label+"-commission-before", "distribution", append([]byte{0x07}, validator...), height-1)
	if err != nil {
		return "", err
	}
	whole, err := wholeCommission(stored)
	if err != nil {
		return "", err
	}
	if whole.Sign() == 0 {
		return "removed with no commission", nil
	}
	return "removed with commission " + whole.String() + "uatom", nil
}

// deciding returns the messages of one block that the state before and after
// the block can speak for, by transaction and message position, with the
// validator each leaves: for every validator, the last executed message that
// leaves it. It also returns the validators the block's executed messages
// create; the state around the block says nothing about a validator that was
// both left and created inside it.
func deciding(items []core.LegacyInclusion) (map[[2]int64]string, map[string]bool) {
	last, created := map[string][2]int64{}, map[string]bool{}
	for i, item := range items {
		if item.Tx == nil || item.Result == nil || item.Result.Code != 0 {
			continue
		}
		for j, message := range item.Tx.Messages {
			if left := core.LegacyValidatorLeft(message.Derived); left != "" {
				last[left] = [2]int64{int64(i), int64(j)}
			}
			if made, ok := message.Derived.(*stakingtypes.MsgCreateValidator); ok {
				created[made.ValidatorAddress] = true
			}
		}
	}
	out := map[[2]int64]string{}
	for validator, at := range last {
		if !created[validator] {
			out[at] = validator
		}
	}
	return out, created
}

func main() {
	root := flag.String("cache", "/cache", "block cache root")
	chain := flag.String("chain", "cosmoshub-3", "legacy chain")
	candidates := flag.String("candidates", "", "file with one chain/height/transaction/message per line; anything after a space is ignored")
	heights := flag.String("heights", "", "file with one height per line; every executed message in them that leaves a validator is a candidate")
	out := flag.String("out", "", "verdict file to write")
	evidence := flag.String("evidence", "", "directory that receives every archive answer as received")
	rate := flag.Float64("rate", 4, "requests a second to each archive, 5 at most")
	maxBody := flag.Int64("max-body", 64<<20, "largest decompressed capture accepted")
	flag.Parse()
	if *chain != "cosmoshub-3" {
		fail("removal verdicts are only built for cosmoshub-3")
	}
	if (*candidates == "" && *heights == "") || *out == "" || *evidence == "" || *rate <= 0 || *rate > 5 {
		fail("a candidates file, an output file, an evidence directory and a rate of 5 or less are required")
	}
	if err := os.MkdirAll(*evidence, 0o700); err != nil {
		fail("evidence directory unavailable")
	}
	var archives []*archive
	for _, name := range []string{"1", "2"} {
		if url := os.Getenv("LEGACY_ARCHIVE_" + name); url != "" {
			archives = append(archives, &archive{name: name, url: url, client: &http.Client{Timeout: 60 * time.Second},
				interval: time.Duration(float64(time.Second) / *rate), evidence: *evidence})
		}
	}
	if len(archives) == 0 {
		fail("LEGACY_ARCHIVE_1 and LEGACY_ARCHIVE_2 name the archives; at least one is required")
	}
	type candidate struct{ height, tx, msg int64 }
	seen := map[candidate]bool{}
	var list []candidate
	var wanted []string
	if *heights != "" {
		body, err := os.ReadFile(*heights)
		if err != nil {
			fail("a candidates or heights file is unavailable")
		}
		wanted = strings.Fields(string(body))
	}
	var err error
	for _, word := range wanted {
		height, err := strconv.ParseInt(word, 10, 64)
		if err != nil || height < 2 {
			fail("the heights file holds something that is not a height with a block before it")
		}
		h, err := capture.LoadHeight(*root, *chain, height, *maxBody)
		if err != nil {
			fail(fmt.Sprintf("height %d: capture not readable", height))
		}
		items, err := core.DecodeLegacyBlock(h.Block, h.Results)
		if err != nil {
			fail(fmt.Sprintf("height %d: capture not decodable", height))
		}
		for i, item := range items {
			if item.Tx == nil || item.Result == nil || item.Result.Code != 0 {
				continue
			}
			for j, message := range item.Tx.Messages {
				if c := (candidate{height, int64(i), int64(j)}); core.LegacyValidatorLeft(message.Derived) != "" && !seen[c] {
					seen[c] = true
					list = append(list, c)
				}
			}
		}
	}
	text := ""
	if *candidates != "" {
		body, err := os.ReadFile(*candidates)
		if err != nil {
			fail("a candidates or heights file is unavailable")
		}
		text = strings.TrimSpace(string(body))
	}
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		key, _, _ := strings.Cut(line, " ")
		parts := strings.Split(key, "/")
		if len(parts) != 4 || parts[0] != *chain {
			fail("a candidate is not chain/height/transaction/message of this chain")
		}
		var c candidate
		for i, into := range []*int64{&c.height, &c.tx, &c.msg} {
			if *into, err = strconv.ParseInt(parts[i+1], 10, 64); err != nil || *into < 0 {
				fail("a candidate is not chain/height/transaction/message of this chain")
			}
		}
		if c.height < 2 {
			fail("a candidate has no block before it")
		}
		if !seen[c] {
			seen[c] = true
			list = append(list, c)
		}
	}
	sort.Slice(list, func(a, b int) bool {
		if list[a].height != list[b].height {
			return list[a].height < list[b].height
		}
		if list[a].tx != list[b].tx {
			return list[a].tx < list[b].tx
		}
		return list[a].msg < list[b].msg
	})

	ctx := context.Background()
	counts := map[string]int{}
	var lines []string
	for _, c := range list {
		h, err := capture.LoadHeight(*root, *chain, c.height, *maxBody)
		if err != nil {
			fail(fmt.Sprintf("height %d: capture not readable", c.height))
		}
		items, err := core.DecodeLegacyBlock(h.Block, h.Results)
		if err != nil || c.tx >= int64(len(items)) || items[c.tx].Tx == nil || items[c.tx].Result == nil || c.msg >= int64(len(items[c.tx].Tx.Messages)) {
			fail(fmt.Sprintf("height %d: the candidate names no message", c.height))
		}
		left := core.LegacyValidatorLeft(items[c.tx].Tx.Messages[c.msg].Derived)
		validator, err := sdk.ValAddressFromBech32(left)
		if items[c.tx].Result.Code != 0 || left == "" || err != nil {
			fail(fmt.Sprintf("height %d: the candidate is not an executed message that leaves a validator", c.height))
		}
		speaks, created := deciding(items)
		if created[left] {
			counts["no line: the block also creates the validator"]++
			continue
		}
		if speaks[[2]int64{c.tx, c.msg}] != left {
			counts["no line: a later message of the block leaves the same validator"]++
			continue
		}
		label := fmt.Sprintf("%d-%d-%d", c.height, c.tx, c.msg)
		var answers []string
		for _, a := range archives {
			answer, err := a.verdict(ctx, label, validator, c.height)
			if err == errNotServed {
				counts["archive "+a.name+" did not serve the height"]++
				continue
			}
			if err != nil {
				fail(fmt.Sprintf("height %d: archive %s: %s", c.height, a.name, err.Error()))
			}
			answers = append(answers, answer)
		}
		if len(answers) == 0 {
			fail(fmt.Sprintf("height %d: no archive served the height", c.height))
		}
		served := "one archive"
		if len(answers) == 2 {
			if answers[0] != answers[1] {
				fail(fmt.Sprintf("height %d: the two archives disagree", c.height))
			}
			served = "two archives"
		}
		class := answers[0]
		if strings.HasPrefix(class, "removed with commission") {
			class = "removed with commission"
		}
		counts[class+", "+served]++
		lines = append(lines, fmt.Sprintf("%s/%d/%d/%d %s, %s", *chain, c.height, c.tx, c.msg, answers[0], served))
	}
	body := []byte(strings.Join(lines, "\n") + "\n")
	if err := os.WriteFile(*out, body, 0o600); err != nil {
		fail("verdict file could not be written")
	}
	fmt.Printf("candidates %d, verdict lines %d, verdict file sha256 %x\n", len(list), len(lines), sha256.Sum256(body))
	for _, a := range archives {
		fmt.Printf("archive %s requests %d\n", a.name, a.requests)
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-55s %d\n", k, counts[k])
	}
}
