package main

// Closed-reference proof is deliberately not next-header consensus proof. Only
// this pinned Hub1 tail may use it. No caller supplied root or waiver is trusted.
import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"syscall"

	gaia "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
	"github.com/cosmos/cosmos-sdk/store"
	st "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/syndtr/goleveldb/leveldb/opt"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
	tmtypes "github.com/tendermint/tendermint/types"
)

const terminalHeight int64 = 500015
const terminalManifestPin = "417424c8075ea3dcfd166eddaad8f5012c2510797abeca7f4f0864d399f0fd86"
const terminalGenesisPin = "73a866b21723ecbc28b6d15951b2eb3aa2f2443650ff6df489bf55ac5edceefa"
const terminalBlockPin = "a050aed356f1138980ac066b3abf67ab5ffc43c53875a775f3ef2ec31e1f394c"
const terminalResultsPin = "8e1fe3fe0221de321d961039c342fb8d21fc21425b52b9993292d7e9f62ff776"

func terminalFileHash(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", fmt.Errorf("terminal source file unavailable")
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", fmt.Errorf("terminal source read failed")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Relative names, NUL, lowercase SHA256, newline. Reject links and special files,
// including nested directory links. This binds ALL source bytes, not DB roots.
func terminalManifest(home string) (string, error) {
	hashes := map[string]string{}
	e := filepath.WalkDir(home, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fs.ErrInvalid
		}
		rel, e := filepath.Rel(home, p)
		if e != nil {
			return e
		}
		h, e := terminalFileHash(p)
		if e != nil {
			return e
		}
		hashes[rel] = h
		return nil
	})
	if e != nil {
		return "", fmt.Errorf("terminal complete source manifest refused")
	}
	names := make([]string, 0, len(hashes))
	for n := range hashes {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%s\x00%s\n", n, hashes[n])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Never DB-open the supplied reference. Keep an exclusive private byte copy,
// then open ONLY that copy read-only. Retain every failed copy for diagnosis.
func cloneTerminalReference(home, dest string) (string, error) {
	lock := filepath.Join(home, "application.db", "LOCK")
	s, e := os.Lstat(lock)
	if e != nil || !s.Mode().IsRegular() {
		return "", fmt.Errorf("terminal reference lock invalid")
	}
	f, e := os.Open(lock)
	if e != nil {
		return "", fmt.Errorf("terminal reference lock unavailable")
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil {
		return "", fmt.Errorf("terminal reference has active writer")
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	before, e := terminalManifest(home)
	if e != nil || before != terminalManifestPin {
		return "", fmt.Errorf("terminal reference original bytes differ from pinned closed source")
	}
	if e := os.Mkdir(dest, 0700); e != nil {
		return "", fmt.Errorf("terminal private copy unavailable")
	}
	e = filepath.WalkDir(home, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(home, p)
		if e != nil {
			return e
		}
		if rel == "." {
			return nil
		}
		to := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.Mkdir(to, 0700)
		}
		if !d.Type().IsRegular() {
			return fs.ErrInvalid
		}
		in, e := os.Open(p)
		if e != nil {
			return e
		}
		defer in.Close()
		out, e := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, e = io.Copy(out, in)
		ce := out.Close()
		if e != nil {
			return e
		}
		return ce
	})
	if e != nil {
		return "", fmt.Errorf("terminal private byte copy failed")
	}
	after, e := terminalManifest(home)
	copied, ce := terminalManifest(dest)
	if e != nil || ce != nil || before != after || copied != before {
		return "", fmt.Errorf("terminal source changed during copy")
	}
	return dest, nil
}

// Independently load every native IAVL tree and compare its actual commit ID
// against the persisted commit metadata before using a Gaia query as reference.
func terminalLoadedRoot(db dbm.DB) (sdk.CommitID, error) {
	var h int64
	var info historyCommitInfo
	if cdc.UnmarshalBinaryLengthPrefixed(db.Get([]byte("s/latest")), &h) != nil || h != terminalHeight || cdc.UnmarshalBinaryLengthPrefixed(db.Get([]byte(fmt.Sprintf("s/%d", h))), &info) != nil || info.Version != h {
		return sdk.CommitID{}, fmt.Errorf("terminal committed metadata invalid")
	}
	names := []string{"main", "acc", "staking", "slashing", "mint", "distr", "gov", "fee", "params"}
	if len(info.StoreInfos) != len(names) {
		return sdk.CommitID{}, fmt.Errorf("terminal persistent store set invalid")
	}
	expected := map[string]sdk.CommitID{}
	for _, s := range info.StoreInfos {
		if _, ok := expected[s.Name]; ok {
			return sdk.CommitID{}, fmt.Errorf("terminal duplicate store")
		}
		expected[s.Name] = s.Core.CommitID
	}
	cms := store.NewCommitMultiStore(db)
	keys := make([]*st.KVStoreKey, 0, len(names))
	for _, n := range names {
		if _, ok := expected[n]; !ok {
			return sdk.CommitID{}, fmt.Errorf("terminal store absent")
		}
		k := st.NewKVStoreKey(n)
		keys = append(keys, k)
		cms.MountStoreWithDB(k, st.StoreTypeIAVL, nil)
	}
	if cms.LoadVersion(h) != nil {
		return sdk.CommitID{}, fmt.Errorf("terminal trees cannot load")
	}
	for _, k := range keys {
		got, want := cms.GetCommitStore(k).LastCommitID(), expected[k.Name()]
		if got.Version != h || want.Version != h || !bytes.Equal(got.Hash, want.Hash) {
			return sdk.CommitID{}, fmt.Errorf("terminal loaded tree root differs")
		}
	}
	return cms.LastCommitID(), nil
}

type closedTerminalReference struct {
	Commit                 sdk.CommitID          `json:"commit"`
	Stores                 []nativeStoreEvidence `json:"all_persistent_stores"`
	Results                []byte                `json:"results_hash"`
	BlockHash              []byte                `json:"block_hash"`
	ValidatorHistorySHA256 string                `json:"validator_history_sha256"`
	OriginalStatus         json.RawMessage       `json:"original_status"`
	OriginalTail           json.RawMessage       `json:"original_tail"`
	SourceBlock            *tmtypes.Block        `json:"source_block"`
	Begin                  interface{}           `json:"begin"`
	Deliver                interface{}           `json:"deliver"`
	End                    interface{}           `json:"end"`
}

func loadClosedTerminalReference(home, cache, genesis, copyHome string) (*closedTerminalReference, error) {
	for _, p := range []struct{ path, pin string }{{genesis, terminalGenesisPin}, {basePath(cache, "cosmoshub-1", terminalHeight) + "-block.raw.gz", terminalBlockPin}, {basePath(cache, "cosmoshub-1", terminalHeight) + "-block_results.raw.gz", terminalResultsPin}} {
		h, e := terminalFileHash(p.path)
		if e != nil || h != p.pin {
			return nil, fmt.Errorf("terminal immutable genesis/header/result source differs")
		}
	}
	copyHome, e := cloneTerminalReference(home, copyHome)
	if e != nil {
		return nil, e
	}
	raw, e := os.ReadFile(filepath.Join(copyHome, "status.json"))
	if e != nil {
		return nil, fmt.Errorf("terminal closed status unavailable")
	}
	var status runState
	if json.Unmarshal(raw, &status) != nil || status.Release != release || status.Chain != "cosmoshub-1" || status.Height != terminalHeight || status.Target != terminalHeight || status.HashVerified != terminalHeight-1 || status.ResultDiffs != 0 || status.Stop != "reached the target; the state after height 500015 is checked by the next header only" {
		return nil, fmt.Errorf("terminal original closed status invalid")
	}
	marker, e := os.ReadFile(filepath.Join(copyHome, "complete"))
	if e != nil || len(marker) != 0 {
		return nil, fmt.Errorf("terminal closed completion marker invalid")
	}
	tailRaw, e := os.ReadFile(filepath.Join(copyHome, "tail.json"))
	var tail tailState
	if e != nil || json.Unmarshal(tailRaw, &tail) != nil || tail.Height != terminalHeight {
		return nil, fmt.Errorf("terminal original result tail invalid")
	}
	c := load(cache, "cosmoshub-1", terminalHeight, true)
	if c.err != nil || c.results == nil || c.results.begin == nil || c.results.end == nil || !bytes.Equal(tmtypes.NewResults(c.results.deliver).Hash(), tail.Results) {
		return nil, fmt.Errorf("terminal raw ABCI result binding differs")
	}
	db, e := dbm.NewGoLevelDBWithOpts("application", copyHome, &opt.Options{ReadOnly: true})
	if e != nil {
		return nil, fmt.Errorf("terminal private checkpoint cannot open read-only")
	}
	defer db.Close()
	root, e := terminalLoadedRoot(db)
	if e != nil {
		return nil, e
	}
	app := gaia.NewGaiaApp(log.NewNopLogger(), db, nil, true)
	if app.LastBlockHeight() != terminalHeight || !reflect.DeepEqual(root, app.LastCommitID()) {
		return nil, fmt.Errorf("terminal native app commit differs")
	}
	if _, e = historyRoots(db, app); e != nil {
		return nil, e
	}
	stores, e := auditNativeStores(app, terminalHeight)
	if e != nil {
		return nil, e
	}
	vh, e := terminalFileHash(filepath.Join(copyHome, "validator-updates.jsonl"))
	if e != nil {
		return nil, e
	}
	// Read-only DB opening must not modify even the private original-byte copy.
	after, e := terminalManifest(copyHome)
	if e != nil || after != terminalManifestPin {
		return nil, fmt.Errorf("terminal private reference bytes changed")
	}
	return &closedTerminalReference{root, stores, tail.Results, c.metaID, vh, raw, tailRaw, c.block, c.results.begin, c.results.deliver, c.results.end}, nil
}

func verifyClosedTerminal(ref *closedTerminalReference, app replayApp, height int64, blockHash, results []byte, validatorHistory string) error {
	if ref == nil || height != terminalHeight || app.LastBlockHeight() != terminalHeight || !reflect.DeepEqual(app.LastCommitID(), ref.Commit) || !bytes.Equal(blockHash, ref.BlockHash) || !bytes.Equal(results, ref.Results) {
		return fmt.Errorf("terminal height/root/header/result differs from closed reference")
	}
	vh, e := terminalFileHash(validatorHistory)
	if e != nil || vh != ref.ValidatorHistorySHA256 {
		return fmt.Errorf("terminal complete validator history differs")
	}
	stores, e := auditNativeStores(app, height)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(stores, ref.Stores) {
		return fmt.Errorf("terminal complete all-nine raw native state differs")
	}
	return nil
}

func completeClosedTerminal(f *evidenceWriter, ref *closedTerminalReference, first int64) error {
	row := struct {
		Schema             string                   `json:"schema"`
		Kind               string                   `json:"kind"`
		Release            string                   `json:"release"`
		Chain              string                   `json:"chain"`
		First              int64                    `json:"first_height"`
		Height             int64                    `json:"height"`
		ProofMode          string                   `json:"proof_mode"`
		SourceManifest     string                   `json:"source_manifest_sha256"`
		Genesis            string                   `json:"genesis_sha256"`
		BlockCapture       string                   `json:"terminal_block_capture_sha256"`
		ResultsCapture     string                   `json:"terminal_results_capture_sha256"`
		NextHeaderProof    bool                     `json:"next_header_proof"`
		FinancialAdmission bool                     `json:"financial_admission"`
		Reference          *closedTerminalReference `json:"closed_reference"`
	}{"native033-closed-terminal-v1", "closed_terminal_history_completed", release, "cosmoshub-1", first, terminalHeight, "closed_pinned_native_reference", terminalManifestPin, terminalGenesisPin, terminalBlockPin, terminalResultsPin, false, false, ref}
	if json.NewEncoder(f).Encode(row) != nil || f.Sync() != nil {
		return fmt.Errorf("terminal complete evidence write failed")
	}
	return nil
}
