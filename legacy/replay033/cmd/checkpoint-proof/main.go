// checkpoint-proof verifies an offline checkpoint without executing another
// block or changing the replay's original status, records, or database.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/syndtr/goleveldb/leveldb/opt"
	amino "github.com/tendermint/go-amino"
	dbm "github.com/tendermint/tendermint/libs/db"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	"github.com/tendermint/tendermint/types"
)

const release = "cosmos-sdk v0.33.2 (gaia), tendermint v0.31.0-dev0-fix0, iavl v0.12.1"

var codec = func() *amino.Codec { c := amino.NewCodec(); ctypes.RegisterAmino(c); return c }()

type receipt struct {
	Kind           string `json:"kind"`
	Release        string `json:"release"`
	Chain          string `json:"chain"`
	Height         int64  `json:"checkpoint_height"`
	Verified       int64  `json:"state_hash_verified_through"`
	Next           int64  `json:"next_header_height"`
	AppHash        string `json:"application_hash"`
	CurrentCapture string `json:"current_capture_sha256"`
	NextCapture    string `json:"next_capture_sha256"`
	StatusSHA      string `json:"original_status_sha256"`
	TailSHA        string `json:"original_tail_sha256"`
	PrivateCopy    string `json:"private_copy_directory"`
}

func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func loadBlock(root, chain string, height int64) (*types.Block, []byte, string, error) {
	p := filepath.Join(root, chain, strconv.FormatInt(height/10000, 10), strconv.FormatInt(height, 10)+"-block.raw.gz")
	f, e := os.Open(p)
	if e != nil {
		return nil, nil, "", errors.New("cached header unavailable")
	}
	defer f.Close()
	z, e := gzip.NewReader(f)
	if e != nil {
		return nil, nil, "", errors.New("cached gzip invalid")
	}
	defer z.Close()
	raw, e := io.ReadAll(io.LimitReader(z, (512<<20)+1))
	if e != nil || len(raw) > 512<<20 {
		return nil, nil, "", errors.New("cached header unreadable or oversized")
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	var r ctypes.ResultBlock
	if json.Unmarshal(raw, &envelope) != nil || codec.UnmarshalJSON(envelope.Result, &r) != nil || r.Block == nil || r.BlockMeta == nil {
		return nil, nil, "", errors.New("cached block invalid")
	}
	b := r.Block
	if b.ChainID != chain || b.Height != height || len(r.BlockMeta.BlockID.Hash) == 0 || !bytes.Equal(b.Header.Hash(), r.BlockMeta.BlockID.Hash) || b.ValidateBasic() != nil {
		return nil, nil, "", errors.New("cached block identity or hash invalid")
	}
	return b, r.BlockMeta.BlockID.Hash, digest(raw), nil
}
func checkNext(height int64, appHash, currentID, results []byte, current, next *types.Block) error {
	if height < 1 || current == nil || next == nil || current.Height != height || next.Height != height+1 || current.ChainID != next.ChainID {
		return errors.New("checkpoint header identity mismatch")
	}
	if len(appHash) == 0 || !bytes.Equal(next.LastBlockID.Hash, currentID) || !bytes.Equal(next.AppHash, appHash) || !bytes.Equal(next.LastResultsHash, results) || !bytes.Equal(current.NextValidatorsHash, next.ValidatorsHash) {
		return errors.New("checkpoint next-header proof differs")
	}
	return nil
}
func outside(home, out string) bool {
	h, e := filepath.EvalSymlinks(home)
	if e != nil {
		return false
	}
	h, e = filepath.Abs(h)
	if e != nil {
		return false
	}
	p, e := filepath.EvalSymlinks(filepath.Dir(out))
	if e != nil {
		return false
	}
	p, e = filepath.Abs(p)
	if e != nil {
		return false
	}
	rel, e := filepath.Rel(h, filepath.Join(p, filepath.Base(out)))
	return e == nil && (rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
func prove(cache, chain, home string) (receipt, error) {
	var r receipt
	if chain != "cosmoshub-1" {
		return r, errors.New("only the pinned Hub 1 release is supported")
	}
	if st, e := os.Stat(filepath.Join(home, "application.db")); e != nil || !st.IsDir() {
		return r, errors.New("existing offline checkpoint required")
	}
	copyHome, e := cloneOffline(home)
	if e != nil {
		return r, e
	}
	db, e := dbm.NewGoLevelDBWithOpts("application", copyHome, &opt.Options{ReadOnly: true})
	if e != nil {
		return r, errors.New("private checkpoint copy cannot open read-only")
	}
	defer db.Close()
	h, appHash, e := loadedRoot(db)
	if e != nil {
		return r, e
	}
	statusRaw, e := os.ReadFile(filepath.Join(home, "status.json"))
	if e != nil {
		return r, errors.New("original replay status unavailable")
	}
	var status struct {
		Release  string `json:"release"`
		Chain    string `json:"chain"`
		Height   int64  `json:"last_height_committed"`
		Verified int64  `json:"state_hash_verified_through"`
	}
	if json.Unmarshal(statusRaw, &status) != nil || status.Release != release || status.Chain != chain || status.Height != h || status.Verified < h-1 || status.Verified > h {
		return r, errors.New("original replay status identity invalid")
	}
	tailRaw, e := os.ReadFile(filepath.Join(home, "tail.json"))
	if e != nil {
		return r, errors.New("original replay tail unavailable")
	}
	var tail struct {
		Height  int64  `json:"height"`
		Results []byte `json:"results_hash"`
	}
	if json.Unmarshal(tailRaw, &tail) != nil || tail.Height != h {
		return r, errors.New("original replay tail invalid")
	}
	current, currentID, currentSHA, e := loadBlock(cache, chain, h)
	if e != nil {
		return r, e
	}
	next, _, nextSHA, e := loadBlock(cache, chain, h+1)
	if e != nil {
		return r, e
	}
	if e = checkNext(h, appHash, currentID, tail.Results, current, next); e != nil {
		return r, e
	}
	return receipt{"checkpoint_next_header_only", release, chain, h, h, h + 1, hex.EncodeToString(appHash), currentSHA, nextSHA, digest(statusRaw), digest(tailRaw), copyHome}, nil
}
func run(args []string) error {
	fs := flag.NewFlagSet("checkpoint-proof", flag.ContinueOnError)
	cache := fs.String("cache", "", "private cached block root")
	chain := fs.String("chain", "", "chain")
	home := fs.String("home", "", "existing offline checkpoint")
	out := fs.String("out", "", "fresh private proof outside checkpoint")
	if fs.Parse(args) != nil || *cache == "" || *home == "" || *out == "" || !outside(*home, *out) {
		return errors.New("cache, chain, home and separate fresh output required")
	}
	r, e := prove(*cache, *chain, *home)
	if e != nil {
		return e
	}
	f, e := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return errors.New("fresh private proof cannot be created")
	}
	defer f.Close()
	if json.NewEncoder(f).Encode(r) != nil || f.Sync() != nil {
		return errors.New("private checkpoint proof write failed")
	}
	fmt.Printf("checkpoint_height=%d state_hash_verified_through=%d next_header=%d source_opened_read_only=true\n", r.Height, r.Verified, r.Next)
	return nil
}
func main() {
	defer func() {
		if recover() != nil {
			fmt.Fprintln(os.Stderr, "checkpoint proof stopped inside pinned application")
			os.Exit(1)
		}
	}()
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, "checkpoint proof stopped:", e)
		os.Exit(1)
	}
}
