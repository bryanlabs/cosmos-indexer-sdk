// hub2-migrate036 applies the official SDK v0.34-to-v0.36 genesis migration.
// It never opens a native database or connects to SQL. Original exports remain
// unchanged; verification recomputes the same official transformation.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/cosmos/cosmos-sdk/x/genutil"
	v036 "github.com/cosmos/cosmos-sdk/x/genutil/legacy/v036"
)

func pinned(path, hash string) ([]byte, error) {
	st, e := os.Lstat(path)
	if e != nil || !st.Mode().IsRegular() {
		return nil, errors.New("regular pinned export required")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	opened, e := f.Stat()
	if e != nil || !os.SameFile(st, opened) {
		return nil, errors.New("export changed")
	}
	b, e := io.ReadAll(io.LimitReader(f, 256<<20+1))
	if e != nil || len(b) > 256<<20 {
		return nil, errors.New("invalid export size")
	}
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != hash {
		return nil, errors.New("export hash mismatch")
	}
	return b, nil
}
func migrate(b []byte) (out []byte, err error) {
	defer func() {
		if recover() != nil {
			out = nil
			err = errors.New("official SDK migration refused source invariants")
		}
	}()
	var root map[string]json.RawMessage
	if json.Unmarshal(b, &root) != nil {
		return nil, errors.New("invalid native export")
	}
	var height int64
	var zero bool
	if json.Unmarshal(root["source_height"], &height) != nil || height < 1 || json.Unmarshal(root["for_zero_height"], &zero) != nil || !zero {
		return nil, errors.New("genuine zero-height export required")
	}
	var state genutil.AppMap
	if json.Unmarshal(root["app_state"], &state) != nil || state["accounts"] == nil || state["distr"] == nil {
		return nil, errors.New("complete SDK034 export required")
	}
	migrated := v036.Migrate(state)
	root["app_state"], err = json.Marshal(migrated)
	if err != nil {
		return nil, errors.New("migration serialization failed")
	}
	h := sha256.Sum256(b)
	root["migration_input_sha256"], _ = json.Marshal(hex.EncodeToString(h[:]))
	root["migration_sdk"], _ = json.Marshal("cosmos-sdk-v0.37.4/official-v036")
	out, err = json.Marshal(root)
	if err != nil {
		return nil, errors.New("migration serialization failed")
	}
	return append(out, '\n'), nil
}
func run() error {
	input := flag.String("input", "", "original native export")
	sha := flag.String("sha256", "", "original SHA256")
	output := flag.String("output", "", "NEW retained output path")
	check := flag.String("check-output", "", "existing output to verify, no writes")
	checkSHA := flag.String("check-sha256", "", "existing output SHA256")
	flag.Parse()
	if flag.NArg() != 0 || (*output == "") == (*check == "") {
		return errors.New("choose exactly output or check-output")
	}
	b, e := pinned(*input, *sha)
	if e != nil {
		return e
	}
	out, e := migrate(b)
	if e != nil {
		return e
	}
	if *check != "" {
		existing, e := pinned(*check, *checkSHA)
		if e != nil {
			return e
		}
		if !bytes.Equal(existing, out) {
			return errors.New("official migration output differs")
		}
		return nil
	}
	f, e := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	if _, e = f.Write(out); e != nil {
		return e
	}
	return f.Sync()
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
