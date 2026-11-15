package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	abci "github.com/tendermint/tendermint/abci/types"
	tm "github.com/tendermint/tendermint/types"
	"io"
	"os"
)

// A supplemental history is read-only and explicitly hash-pinned. Its sets
// still require independent authentication against each retained header used.
func historicalValidators(path, pin string, height int64) *tm.ValidatorSet {
	need(path != "" && len(pin) == 64 && height > 0, "authenticated validator history required")
	f, e := os.Open(path)
	must(e)
	defer f.Close()
	info, e := f.Stat()
	must(e)
	need(info.Size() > 0 && info.Size() <= 16<<20, "unbounded validator history refused")
	digest := sha256.New()
	reader := io.TeeReader(f, digest)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 65536), 1<<20)
	set := map[string]*tm.Validator{}
	last := int64(-1)
	count := 0
	for scanner.Scan() {
		var row struct {
			H int64                  `json:"h"`
			U []abci.ValidatorUpdate `json:"u"`
		}
		must(json.Unmarshal(scanner.Bytes(), &row))
		need(row.H > last && (count > 0 || row.H == 0), "validator history gap/order identity differs")
		last = row.H
		count++
		updates, e := tm.PB2TM.ValidatorUpdates(row.U)
		must(e)
		if row.H != 0 && row.H+2 > height {
			continue
		}
		for _, v := range updates {
			if v.VotingPower == 0 {
				delete(set, string(v.Address))
			} else {
				set[string(v.Address)] = v
			}
		}
	}
	must(scanner.Err())
	need(count > 0 && hex.EncodeToString(digest.Sum(nil)) == pin, "validator history source pin differs")
	vals := []*tm.Validator{}
	for _, v := range set {
		vals = append(vals, tm.NewValidator(v.PubKey, v.VotingPower))
	}
	return tm.NewValidatorSet(vals)
}
