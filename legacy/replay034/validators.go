package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/types"
)

// validators follows the validator set the way the consensus engine did:
// the changes the application returns at the end of height H take effect at
// height H+2. Every change is written to a file before the application
// commits the height, so a stopped run can rebuild the sets.
type validators struct {
	file                *os.File
	initial             []*types.Validator
	changes             []change
	last, current, next *types.ValidatorSet
}

type change struct {
	height  int64
	updates []*types.Validator
}

type line struct {
	H int64                  `json:"h"`
	U []abci.ValidatorUpdate `json:"u"`
}

func openValidators(path string) (*validators, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &validators{file: f}, nil
}

func (v *validators) close() { v.file.Close() }

func (v *validators) write(h int64, updates []abci.ValidatorUpdate) error {
	body, err := json.Marshal(line{H: h, U: updates})
	if err != nil {
		return err
	}
	if _, err := v.file.Write(append(body, '\n')); err != nil {
		return err
	}
	return v.file.Sync()
}

func copies(in []*types.Validator) []*types.Validator {
	out := make([]*types.Validator, len(in))
	for i, x := range in {
		out[i] = types.NewValidator(x.PubKey, x.VotingPower)
	}
	return out
}

func (v *validators) start(first []abci.ValidatorUpdate) error {
	if info, err := v.file.Stat(); err != nil || info.Size() != 0 {
		return fmt.Errorf("validator change file is not empty at genesis")
	}
	initial, err := types.PB2TM.ValidatorUpdates(first)
	if err != nil {
		return err
	}
	if err := v.write(0, first); err != nil {
		return err
	}
	v.initial = initial
	v.last = types.NewValidatorSet(nil)
	v.current = types.NewValidatorSet(copies(initial))
	v.next = types.NewValidatorSet(copies(initial))
	return nil
}

// record keeps the changes of one height. It must be called before the
// application commits that height.
func (v *validators) record(h int64, updates []abci.ValidatorUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	converted, err := types.PB2TM.ValidatorUpdates(updates)
	if err != nil {
		return err
	}
	if err := v.write(h, updates); err != nil {
		return err
	}
	v.changes = append(v.changes, change{height: h, updates: converted})
	return nil
}

// advance moves the sets one height on, after record.
func (v *validators) advance(h int64) error {
	following := v.next.Copy()
	if n := len(v.changes); n > 0 && v.changes[n-1].height == h {
		if err := following.UpdateWithChangeSet(copies(v.changes[n-1].updates)); err != nil {
			return fmt.Errorf("validator changes do not apply: %v", err)
		}
	}
	following.IncrementProposerPriority(1)
	v.last, v.current, v.next = v.current, v.next, following
	return nil
}

// resume rebuilds the sets as they stand after the given committed height
// and drops any change recorded for a height the application did not commit.
func (v *validators) resume(height int64) error {
	if _, err := v.file.Seek(0, 0); err != nil {
		return err
	}
	scanner := bufio.NewScanner(v.file)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	var kept int64
	seen := false
	for scanner.Scan() {
		var l line
		if err := json.Unmarshal(scanner.Bytes(), &l); err != nil {
			break
		}
		if l.H > height {
			break
		}
		converted, err := types.PB2TM.ValidatorUpdates(l.U)
		if err != nil {
			return err
		}
		if !seen {
			if l.H != 0 {
				return fmt.Errorf("validator change file does not start at genesis")
			}
			v.initial, seen = converted, true
		} else {
			v.changes = append(v.changes, change{height: l.H, updates: converted})
		}
		kept += int64(len(scanner.Bytes())) + 1
	}
	if !seen {
		return fmt.Errorf("validator change file is empty")
	}
	if err := v.file.Truncate(kept); err != nil {
		return err
	}
	if _, err := v.file.Seek(0, 2); err != nil {
		return err
	}
	all := v.changes
	v.changes = nil
	v.last = types.NewValidatorSet(nil)
	v.current = types.NewValidatorSet(copies(v.initial))
	v.next = types.NewValidatorSet(copies(v.initial))
	i := 0
	for h := int64(1); h <= height; h++ {
		if i < len(all) && all[i].height == h {
			v.changes = append(v.changes, all[i])
			i++
		}
		if err := v.advance(h); err != nil {
			return err
		}
	}
	if i != len(all) {
		return fmt.Errorf("validator change file is out of order")
	}
	return nil
}

// at rebuilds the validator set that was in force at an earlier height, for
// evidence of misbehaviour at that height.
func (v *validators) at(height int64) (*types.ValidatorSet, error) {
	if height < 1 {
		return nil, fmt.Errorf("evidence names height %d", height)
	}
	set := map[string]*types.Validator{}
	for _, x := range v.initial {
		set[string(x.Address)] = x
	}
	for _, c := range v.changes {
		if c.height+2 > height {
			break
		}
		for _, x := range c.updates {
			if x.VotingPower == 0 {
				delete(set, string(x.Address))
			} else {
				set[string(x.Address)] = x
			}
		}
	}
	list := make([]*types.Validator, 0, len(set))
	for _, x := range set {
		list = append(list, types.NewValidator(x.PubKey, x.VotingPower))
	}
	return types.NewValidatorSet(list), nil
}
