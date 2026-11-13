// Command replay034 runs the Cosmos Hub 2 application of 2019 over the
// cached blocks of the chain, from its genesis file, and proves each step:
// the state hash the application commits after a block must be the one the
// next block's header carries. It only reads the cache and writes its own
// working directory. It never continues past a block whose hash differs.
package main

import (
	"bytes"
	"errors"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"runtime/pprof"
	"syscall"
	"time"

	"github.com/cosmos/cosmos-sdk/baseapp"
	gaia "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
	"github.com/cosmos/cosmos-sdk/store"
	storetypes "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/opt"
	abci "github.com/tendermint/tendermint/abci/types"
	cmn "github.com/tendermint/tendermint/libs/common"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/types"
)

const release = "cosmos-sdk v0.34.10 (gaia), tendermint v0.31.11, iavl v0.12.1"

var address = regexp.MustCompile(`cosmos(valoper|valcons|pub|valoperpub|valconspub)?1[0-9a-z]{38,}`)

// errSignal ends a run that was asked to stop. The state is intact and a
// later run continues from it.
var errSignal = errors.New("stopped by signal")

func plain(v interface{}) string {
	return address.ReplaceAllString(fmt.Sprint(v), "ADDR")
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: replay034 scan|run [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "scan":
		err = scan(os.Args[2:])
	case "run":
		err = run(os.Args[2:])
	default:
		err = fmt.Errorf("unknown mode")
	}
	if err == errSignal {
		os.Exit(3)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "stopped:", plain(err))
		os.Exit(1)
	}
}

type runState struct {
	Release      string  `json:"release"`
	Chain        string  `json:"chain"`
	Started      string  `json:"started"`
	Updated      string  `json:"updated"`
	From         int64   `json:"first_height_this_run"`
	Height       int64   `json:"last_height_committed"`
	HashVerified int64   `json:"state_hash_verified_through"`
	Target       int64   `json:"target"`
	Transactions int64   `json:"transactions_this_run"`
	Rate         float64 `json:"blocks_per_second_last_window"`
	RateRun      float64 `json:"blocks_per_second_this_run"`
	ResultDiffs  int64   `json:"heights_where_results_differ_from_cache"`
	Evidence     int64   `json:"evidence_items_this_run"`
	Stop         string  `json:"stop_reason,omitempty"`
}

// tailState carries the hash of the last height's results across a restart,
// so the first header after it is still checked against them.
type tailState struct {
	Height  int64  `json:"height"`
	Results []byte `json:"results_hash"`
}

func writeJSON(path string, v interface{}) {
	body, _ := json.MarshalIndent(v, "", " ")
	tmp := path + ".tmp"
	if os.WriteFile(tmp, append(body, '\n'), 0o600) == nil {
		os.Rename(tmp, path)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cache := fs.String("cache", "/cache", "cache root")
	chain := fs.String("chain", "cosmoshub-2", "chain id")
	genesis := fs.String("genesis", "", "genesis file")
	home := fs.String("home", "", "working directory, created if absent")
	to := fs.Int64("to", 0, "last height to run")
	readers := fs.Int("readers", 2, "cache readers")
	pruning := fs.String("pruning", "recent", "state versions to keep: recent, syncable or nothing")
	compare := fs.Bool("compare-results", true, "compare each result with the cached one")
	blockCache := fs.Int("db-cache-mb", 8, "database block cache")
	writeBuffer := fs.Int("db-buffer-mb", 4, "database write buffer")
	profile := fs.String("cpuprofile", "", "write a CPU profile of the run to this file")
	fs.Parse(args)
	if *profile != "" {
		f, err := os.Create(*profile)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			return err
		}
		defer pprof.StopCPUProfile()
	}
	if *genesis == "" || *home == "" || *to < 1 {
		return fmt.Errorf("genesis, home and to are required")
	}
	if err := os.MkdirAll(*home, 0o700); err != nil {
		return err
	}
	db, err := dbm.NewGoLevelDBWithOpts("application", *home, &opt.Options{
		BlockCacheCapacity:     *blockCache << 20,
		WriteBuffer:            *writeBuffer << 20,
		OpenFilesCacheCapacity: 2000,
		Filter:                 filter.NewBloomFilter(10),
	})
	if err != nil {
		return err
	}
	defer db.Close()
	// Which old state versions the database keeps does not enter the state
	// hash, which covers the current state only. "recent" keeps the last 100
	// versions, enough to reopen after a crash; "syncable" also keeps every
	// 10,000th.
	var keep sdk.PruningOptions
	switch *pruning {
	case "recent":
		keep = storetypes.NewPruningOptions(100, 0)
	case "syncable", "nothing":
		keep = store.NewPruningOptionsFromString(*pruning)
	default:
		return fmt.Errorf("pruning is recent, syncable or nothing")
	}
	app := gaia.NewGaiaApp(log.NewNopLogger(), db, nil, true, 0, baseapp.SetPruning(keep))
	vals, err := openValidators(filepath.Join(*home, "validator-updates.jsonl"))
	if err != nil {
		return err
	}
	defer vals.close()

	state := runState{Release: release, Chain: *chain, Started: time.Now().UTC().Format(time.RFC3339), Target: *to}
	statusPath, tailPath := filepath.Join(*home, "status.json"), filepath.Join(*home, "tail.json")
	diffs, err := os.OpenFile(filepath.Join(*home, "result-differences.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer diffs.Close()

	height := app.LastBlockHeight()
	var lastHash, lastResults []byte
	appHash := []byte{}
	if height == 0 {
		doc, err := types.GenesisDocFromFile(*genesis)
		if err != nil {
			return fmt.Errorf("genesis file does not load")
		}
		if doc.ChainID != *chain {
			return fmt.Errorf("genesis file is for another chain")
		}
		initial := make([]*types.Validator, len(doc.Validators))
		for i, v := range doc.Validators {
			initial[i] = types.NewValidator(v.PubKey, v.Power)
		}
		response := app.InitChain(abci.RequestInitChain{
			Time:            doc.GenesisTime,
			ChainId:         doc.ChainID,
			ConsensusParams: types.TM2PB.ConsensusParams(doc.ConsensusParams),
			Validators:      types.TM2PB.ValidatorUpdates(types.NewValidatorSet(initial)),
			AppStateBytes:   doc.AppState,
		})
		first := response.Validators
		if len(first) == 0 {
			first = types.TM2PB.ValidatorUpdates(types.NewValidatorSet(initial))
		}
		if len(first) == 0 {
			return fmt.Errorf("no validators after the genesis file was loaded")
		}
		if err := vals.start(first); err != nil {
			return err
		}
		fmt.Printf("genesis loaded: %d validators\n", len(first))
	} else {
		if err := vals.resume(height); err != nil {
			return err
		}
		previous := load(*cache, *chain, height, false)
		if previous.err != nil {
			return fmt.Errorf("height %d: %v", height, previous.err)
		}
		lastHash = previous.metaID
		appHash = app.LastCommitID().Hash
		var tail tailState
		if body, err := os.ReadFile(tailPath); err == nil && json.Unmarshal(body, &tail) == nil && tail.Height == height {
			lastResults = tail.Results
		}
		fmt.Printf("resumed after height %d\n", height)
	}
	state.From, state.Height, state.HashVerified = height+1, height, height-1
	if *to == height {
		fmt.Printf("already at the target height %d\n", height)
		return nil
	}
	if *to < height {
		return fmt.Errorf("the state is at height %d, past the target", height)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	began, window, windowHeight := time.Now(), time.Now(), height
	report := func(reason string) {
		now := time.Now()
		state.Updated = now.UTC().Format(time.RFC3339)
		state.Height = height
		state.Rate = float64(height-windowHeight) / now.Sub(window).Seconds()
		state.RateRun = float64(height-state.From+1) / now.Sub(began).Seconds()
		state.Stop = reason
		writeJSON(statusPath, state)
		if reason != "" {
			writeJSON(tailPath, tailState{Height: height, Results: lastResults})
		}
		fmt.Printf("%s height %d verified %d rate %.1f run %.1f txs %d result-diffs %d evidence %d %s\n", state.Updated, height, state.HashVerified, state.Rate, state.RateRun, state.Transactions, state.ResultDiffs, state.Evidence, reason)
		window, windowHeight = now, height
	}
	fail := func(h int64, format string, a ...interface{}) error {
		err := fmt.Errorf("height %d: %s", h, fmt.Sprintf(format, a...))
		report(plain(err))
		return err
	}

	for c := range feed(*cache, *chain, height+1, *to, *readers, *compare) {
		select {
		case <-stop:
			report("stopped by signal")
			return errSignal
		default:
		}
		h, b := c.height, c.block
		if c.err != nil {
			return fail(h, "%v", c.err)
		}
		if h > 1 && !bytes.Equal(b.LastBlockID.Hash, lastHash) {
			return fail(h, "the block does not name the previous cached block as its parent")
		}
		if !bytes.Equal(b.AppHash, appHash) {
			return fail(h, "STATE HASH DIFFERS: the state after height %d is not the one this header carries", h-1)
		}
		state.HashVerified = h - 1
		if lastResults != nil && !bytes.Equal(b.LastResultsHash, lastResults) {
			return fail(h, "the results of height %d do not hash to the value this header carries", h-1)
		}
		if !bytes.Equal(b.ValidatorsHash, vals.current.Hash()) || !bytes.Equal(b.NextValidatorsHash, vals.next.Hash()) {
			return fail(h, "the validator set does not hash to the value this header carries")
		}
		if _, v := vals.current.GetByAddress(b.ProposerAddress); v == nil {
			return fail(h, "the proposer is not in the validator set")
		}
		votes := []abci.VoteInfo{}
		if h > 1 {
			if len(b.LastCommit.Precommits) != vals.last.Size() {
				return fail(h, "the previous block has %d vote slots for %d validators", len(b.LastCommit.Precommits), vals.last.Size())
			}
			for i, v := range vals.last.Validators {
				vote := b.LastCommit.Precommits[i]
				if vote != nil && !bytes.Equal(vote.ValidatorAddress, v.Address) {
					return fail(h, "a vote sits in another validator's slot")
				}
				votes = append(votes, abci.VoteInfo{Validator: types.TM2PB.Validator(v), SignedLastBlock: vote != nil})
			}
		}
		var byzantine []abci.Evidence
		for _, ev := range b.Evidence.Evidence {
			set, err := vals.at(ev.Height())
			if err != nil {
				return fail(h, "%v", err)
			}
			if now, err := vals.at(h); err != nil || !bytes.Equal(now.Hash(), vals.current.Hash()) {
				return fail(h, "the rebuilt validator history does not match the running set")
			}
			if _, v := set.GetByAddress(ev.Address()); v == nil {
				return fail(h, "evidence names a validator outside the set of its height")
			}
			byzantine = append(byzantine, types.TM2PB.Evidence(ev, set, b.Time))
			state.Evidence++
		}
		round := int32(0)
		if h > 1 {
			round = int32(b.LastCommit.Round())
		}

		var failure error
		var deliver []*abci.ResponseDeliverTx
		var begin abci.ResponseBeginBlock
		var end abci.ResponseEndBlock
		func() {
			defer func() {
				if r := recover(); r != nil {
					os.WriteFile(filepath.Join(*home, "panic.private.txt"), []byte(fmt.Sprintf("%v\n%s", r, debug.Stack())), 0o600)
					failure = fmt.Errorf("the application stopped while running the block: %s", plain(r))
				}
			}()
			begin = app.BeginBlock(abci.RequestBeginBlock{
				Hash:                c.metaID,
				Header:              types.TM2PB.Header(&b.Header),
				LastCommitInfo:      abci.LastCommitInfo{Round: round, Votes: votes},
				ByzantineValidators: byzantine,
			})
			for _, tx := range b.Data.Txs {
				r := app.DeliverTx(tx)
				deliver = append(deliver, &r)
			}
			end = app.EndBlock(abci.RequestEndBlock{Height: h})
			if end.ConsensusParamUpdates != nil {
				failure = fmt.Errorf("the application changed consensus parameters, which this tool does not track")
				return
			}
			if failure = vals.record(h, end.ValidatorUpdates); failure != nil {
				return
			}
			appHash = app.Commit().Data
		}()
		if failure != nil {
			return fail(h, "%v", failure)
		}
		if err := vals.advance(h); err != nil {
			return fail(h, "%v", err)
		}
		lastHash, lastResults = c.metaID, types.NewResults(deliver).Hash()
		height = h
		state.Transactions += int64(len(deliver))
		if c.results != nil {
			if what := differ(c.results, begin, deliver, end); what != "" {
				state.ResultDiffs++
				fmt.Fprintf(diffs, "%d %s\n", h, what)
			}
		}
		if time.Since(window) >= 30*time.Second {
			report("")
		}
	}
	report(fmt.Sprintf("reached the target; the state after height %d is checked by the next header only", height))
	return nil
}

func sameTags(a, b []cmn.KVPair) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i].Key, b[i].Key) || !bytes.Equal(a[i].Value, b[i].Value) {
			return false
		}
	}
	return true
}

// tagDelta says how two tag lists differ by key name only: keys this run
// has more of (+), keys the cache has more of (-), or the keys whose values
// or order differ when the counts agree.
func tagDelta(mine, theirs []cmn.KVPair) string {
	count := map[string]int{}
	var keys []string
	for _, t := range mine {
		if _, ok := count[string(t.Key)]; !ok {
			keys = append(keys, string(t.Key))
		}
		count[string(t.Key)]++
	}
	for _, t := range theirs {
		if _, ok := count[string(t.Key)]; !ok {
			keys = append(keys, string(t.Key))
		}
		count[string(t.Key)]--
	}
	out := ""
	for _, k := range keys {
		if n := count[k]; n > 0 {
			out += fmt.Sprintf("+%d %s;", n, k)
		} else if n < 0 {
			out += fmt.Sprintf("%d %s;", n, k)
		}
	}
	if out != "" {
		return out
	}
	for i := range mine {
		if !bytes.Equal(mine[i].Key, theirs[i].Key) {
			return "order"
		}
		if !bytes.Equal(mine[i].Value, theirs[i].Value) {
			out += "value " + string(mine[i].Key) + ";"
		}
	}
	return out
}

// differ names the parts of this run's results that are not the cached ones.
// The names carry no content.
func differ(c *cachedResults, begin abci.ResponseBeginBlock, deliver []*abci.ResponseDeliverTx, end abci.ResponseEndBlock) string {
	what := ""
	add := func(s string) {
		if what != "" {
			what += ","
		}
		what += s
	}
	if len(c.deliver) != len(deliver) {
		return "transaction-count"
	}
	for i, mine := range deliver {
		theirs := c.deliver[i]
		if theirs == nil {
			add(fmt.Sprintf("tx%d:absent", i))
			continue
		}
		if mine.Code != theirs.Code {
			add(fmt.Sprintf("tx%d:code", i))
		}
		if !bytes.Equal(mine.Data, theirs.Data) {
			add(fmt.Sprintf("tx%d:data", i))
		}
		if mine.GasWanted != theirs.GasWanted || mine.GasUsed != theirs.GasUsed {
			add(fmt.Sprintf("tx%d:gas", i))
		}
		if !sameTags(mine.Tags, theirs.Tags) {
			add(fmt.Sprintf("tx%d:tags(%s)", i, tagDelta(mine.Tags, theirs.Tags)))
		}
		if mine.Log != theirs.Log {
			add(fmt.Sprintf("tx%d:log", i))
		}
	}
	var beginTags, endTags []cmn.KVPair
	var updates []abci.ValidatorUpdate
	if c.begin != nil {
		beginTags = c.begin.Tags
	}
	if c.end != nil {
		endTags, updates = c.end.Tags, c.end.ValidatorUpdates
	}
	if !sameTags(begin.Tags, beginTags) {
		add("begin:tags(" + tagDelta(begin.Tags, beginTags) + ")")
	}
	if !sameTags(end.Tags, endTags) {
		add("end:tags(" + tagDelta(end.Tags, endTags) + ")")
	}
	if len(updates) != len(end.ValidatorUpdates) {
		add("end:validator-updates")
	} else {
		for i := range updates {
			if updates[i].Power != end.ValidatorUpdates[i].Power || !bytes.Equal(updates[i].PubKey.Data, end.ValidatorUpdates[i].PubKey.Data) {
				add("end:validator-updates")
				break
			}
		}
	}
	return what
}

// scan proves that every height of a range is in the cache with what a
// replay needs: the block, hashing to the hash the node gave; the previous
// block's votes, the evidence and the transactions, each hashing to the
// value in the header; the proposer; the link to the previous block; and
// the stored results with one result for each transaction.
func scan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	cache := fs.String("cache", "/cache", "cache root")
	chain := fs.String("chain", "cosmoshub-2", "chain id")
	from := fs.Int64("from", 1, "first height")
	to := fs.Int64("to", 0, "last height")
	readers := fs.Int("readers", 4, "cache readers")
	out := fs.String("out", "", "summary file")
	fs.Parse(args)
	if *to < *from || *out == "" {
		return fmt.Errorf("to and out are required")
	}
	type summary struct {
		Chain              string `json:"chain"`
		From               int64  `json:"from"`
		To                 int64  `json:"to"`
		Updated            string `json:"updated"`
		Done               bool   `json:"done"`
		Heights            int64  `json:"heights_read"`
		Sound              int64  `json:"heights_block_hash_parts_and_proposer_proven"`
		Linked             int64  `json:"heights_naming_the_previous_cached_block"`
		WithVotes          int64  `json:"heights_with_previous_block_votes"`
		VoteSlots          int64  `json:"vote_slots"`
		VotesCast          int64  `json:"votes_cast"`
		WithEvidence       int64  `json:"heights_with_evidence"`
		Evidence           int64  `json:"evidence_items"`
		Transactions       int64  `json:"transactions"`
		ResultsMatchCount  int64  `json:"heights_with_one_stored_result_per_transaction"`
		WithStateHash      int64  `json:"heights_with_a_state_hash_in_the_header"`
		Problems           int64  `json:"heights_with_a_problem"`
		FirstProblemHeight int64  `json:"first_problem_height,omitempty"`
		FirstProblem       string `json:"first_problem,omitempty"`
	}
	s := summary{Chain: *chain, From: *from, To: *to}
	problems, err := os.Create(*out + ".problems")
	if err != nil {
		return err
	}
	defer problems.Close()
	problem := func(h int64, what string) {
		s.Problems++
		if s.FirstProblemHeight == 0 {
			s.FirstProblemHeight, s.FirstProblem = h, what
		}
		fmt.Fprintf(problems, "%d %s\n", h, what)
	}
	var previous []byte
	tick := time.Now()
	for c := range feed(*cache, *chain, *from, *to, *readers, true) {
		s.Heights++
		if c.err != nil {
			problem(c.height, plain(c.err))
			previous = c.metaID
			continue
		}
		b := c.block
		s.Sound++
		if c.height == *from || (previous != nil && bytes.Equal(b.LastBlockID.Hash, previous)) {
			s.Linked++
		} else {
			problem(c.height, "does not name the previous cached block as its parent")
		}
		previous = c.metaID
		if b.LastCommit != nil && len(b.LastCommit.Precommits) > 0 {
			s.WithVotes++
			for _, vote := range b.LastCommit.Precommits {
				s.VoteSlots++
				if vote != nil {
					s.VotesCast++
				}
			}
		}
		if n := len(b.Evidence.Evidence); n > 0 {
			s.WithEvidence++
			s.Evidence += int64(n)
		}
		s.Transactions += int64(len(b.Data.Txs))
		if len(c.results.deliver) == len(b.Data.Txs) {
			s.ResultsMatchCount++
		} else {
			problem(c.height, "stored results do not number the transactions")
		}
		if len(b.AppHash) > 0 {
			s.WithStateHash++
		}
		if time.Since(tick) >= 30*time.Second {
			tick = time.Now()
			s.Updated = tick.UTC().Format(time.RFC3339)
			writeJSON(*out, s)
		}
	}
	s.Updated, s.Done = time.Now().UTC().Format(time.RFC3339), true
	writeJSON(*out, s)
	body, _ := json.MarshalIndent(s, "", " ")
	fmt.Println(string(body))
	if s.Problems > 0 {
		return fmt.Errorf("%d heights have a problem, first at %d", s.Problems, s.FirstProblemHeight)
	}
	return nil
}
