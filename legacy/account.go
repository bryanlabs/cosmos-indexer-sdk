package legacy

import (
	"bytes"
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
	amino "github.com/tendermint/go-amino"
)

// StoredAccountSequence reads the sequence number out of an account record
// as Cosmos Hub 3 kept it in its account store: a plain account, or a vesting
// account that wraps one. The record must be the one of address. A sequence
// of zero is not written to the record at all, so a record without one
// reads as zero. Errors never quote the record.
func StoredAccountSequence(record []byte, address sdk.AccAddress) (uint64, error) {
	bad := fmt.Errorf("stored account not readable")
	if len(record) < 4 || len(address) == 0 {
		return 0, bad
	}
	// How many structs wrap the plain account in each kind of record.
	depth := -1
	for name, wrapped := range map[string]int{"cosmos-sdk/Account": 0, "cosmos-sdk/ContinuousVestingAccount": 2, "cosmos-sdk/DelayedVestingAccount": 2} {
		if _, prefix := amino.NameToDisfix(name); bytes.Equal(record[:4], prefix[:]) {
			depth = wrapped
		}
	}
	if depth < 0 {
		return 0, fmt.Errorf("stored account is of a kind this reader does not know")
	}
	body := record[4:]
	for ; depth > 0; depth-- {
		fields, err := scan(body)
		if err != nil {
			return 0, bad
		}
		inner := -1
		for i, f := range fields {
			if f.Number == 1 && f.Wire == 2 && inner < 0 {
				inner = i
			} else if f.Number == 1 {
				return 0, bad
			}
		}
		if inner < 0 {
			return 0, bad
		}
		body = fields[inner].Value
	}
	fields, err := scan(body)
	if err != nil || schema(fields, map[uint64]uint64{1: 2, 2: 2, 3: 2, 4: 0, 5: 0}, 2) != nil {
		return 0, bad
	}
	if !bytes.Equal(value(fields, 1), address) {
		return 0, fmt.Errorf("stored account is not the one asked for")
	}
	return uintValue(fields, 5), nil
}
