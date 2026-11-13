package legacy

import (
	"bytes"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"
	amino "github.com/tendermint/go-amino"
)

// These mirror the field order of the Hub 3 account records. Synthetic
// addresses and amounts only.
type fixtureAccount struct {
	Address       []byte
	Coins         []fixtureCoin
	PubKey        []byte
	AccountNumber uint64
	Sequence      uint64
}
type fixtureBaseVesting struct {
	Account          *fixtureAccount
	OriginalVesting  []fixtureCoin
	DelegatedFree    []fixtureCoin
	DelegatedVesting []fixtureCoin
	EndTime          int64
}
type fixtureContinuousVesting struct {
	Base      *fixtureBaseVesting
	StartTime int64
}
type fixtureDelayedVesting struct {
	Base *fixtureBaseVesting
}
type fixtureModuleAccount struct {
	Account *fixtureAccount
	Name    string
}

func TestStoredAccountSequenceReadsPlainAndVestingRecords(t *testing.T) {
	cdc := amino.NewCodec()
	cdc.RegisterConcrete(fixtureAccount{}, "cosmos-sdk/Account", nil)
	cdc.RegisterConcrete(fixtureContinuousVesting{}, "cosmos-sdk/ContinuousVestingAccount", nil)
	cdc.RegisterConcrete(fixtureDelayedVesting{}, "cosmos-sdk/DelayedVestingAccount", nil)
	cdc.RegisterConcrete(fixtureModuleAccount{}, "cosmos-sdk/ModuleAccount", nil)
	address := sdk.AccAddress(bytes.Repeat([]byte{7}, 20))
	account := func(sequence uint64) *fixtureAccount {
		return &fixtureAccount{Address: address, Coins: []fixtureCoin{{"uatom", "5"}, {"other", "6"}}, PubKey: []byte{1, 2, 3}, AccountNumber: 300, Sequence: sequence}
	}
	for _, sequence := range []uint64{0, 1, 19, 20, 70000} {
		plain := cdc.MustMarshalBinaryBare(*account(sequence))
		got, err := StoredAccountSequence(plain, address)
		require.NoError(t, err)
		require.Equal(t, sequence, got)
		base := &fixtureBaseVesting{Account: account(sequence), OriginalVesting: []fixtureCoin{{"uatom", "9"}}, EndTime: 1700000000}
		for _, vesting := range []any{fixtureContinuousVesting{Base: base, StartTime: 1600000000}, fixtureDelayedVesting{Base: base}} {
			got, err = StoredAccountSequence(cdc.MustMarshalBinaryBare(vesting), address)
			require.NoError(t, err)
			require.Equal(t, sequence, got)
		}
	}
	// A new account with nothing set but its address.
	got, err := StoredAccountSequence(cdc.MustMarshalBinaryBare(fixtureAccount{Address: address}), address)
	require.NoError(t, err)
	require.Zero(t, got)

	plain := cdc.MustMarshalBinaryBare(*account(19))
	_, err = StoredAccountSequence(plain, sdk.AccAddress(bytes.Repeat([]byte{8}, 20)))
	require.ErrorContains(t, err, "not the one asked for")
	_, err = StoredAccountSequence(cdc.MustMarshalBinaryBare(fixtureModuleAccount{Account: account(19), Name: "pool"}), address)
	require.ErrorContains(t, err, "kind this reader does not know")
	for _, bad := range [][]byte{nil, plain[:3], plain[:len(plain)-1], append(bytes.Clone(plain), 0x30, 0x01), append(bytes.Clone(plain), 0x28, 0x05)} {
		_, err = StoredAccountSequence(bad, address)
		require.Error(t, err)
	}
	_, err = StoredAccountSequence(plain, nil)
	require.Error(t, err)
}
