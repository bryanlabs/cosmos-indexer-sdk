package legacy

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/cosmos/cosmos-sdk/codec"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/cosmos-sdk/crypto/keys/ed25519"
	"github.com/cosmos/cosmos-sdk/crypto/keys/multisig"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/stretchr/testify/require"
)

// These mirror the Amino field-order schemas, not protobuf structs. Fresh
// synthetic addresses and keys make the regression data safe to commit.
type fixtureMsg interface{ marker() }
type fixtureCoin struct{ Denom, Amount string }
type fixtureSend struct {
	From, To []byte
	Amount   []fixtureCoin
}

func (fixtureSend) marker() {}

type fixtureDelegate struct {
	Delegator, Validator []byte
	Amount               fixtureCoin
}

func (fixtureDelegate) marker() {}

type fixtureWithdraw struct{ Delegator, Validator []byte }

func (fixtureWithdraw) marker() {}

type fixtureIO struct {
	Address []byte
	Coins   []fixtureCoin
}
type fixtureMulti struct{ Inputs, Outputs []fixtureIO }

func (fixtureMulti) marker() {}

type fixtureUnknown struct{ Payload []byte }

func (fixtureUnknown) marker() {}

type fixtureFee struct {
	Amount []fixtureCoin
	Gas    uint64
}
type fixtureSignature struct {
	PubKey    cryptotypes.PubKey
	Signature []byte
}
type fixtureTx struct {
	Msgs       []fixtureMsg
	Fee        fixtureFee
	Signatures []fixtureSignature
	Memo       string
}

func fixtureCodec(chain string) *codec.LegacyAmino {
	c := codec.NewLegacyAmino()
	cryptocodec.RegisterCrypto(c)
	c.RegisterInterface((*fixtureMsg)(nil), nil)
	c.RegisterConcrete(fixtureSend{}, "cosmos-sdk/MsgSend", nil)
	c.RegisterConcrete(fixtureDelegate{}, "cosmos-sdk/MsgDelegate", nil)
	c.RegisterConcrete(fixtureWithdraw{}, "cosmos-sdk/MsgWithdrawDelegationReward", nil)
	c.RegisterConcrete(fixtureMulti{}, "cosmos-sdk/MsgMultiSend", nil)
	c.RegisterConcrete(fixtureUnknown{}, "future/Unknown", nil)
	registerProjectionFixtures(c, chain)
	name := "auth/StdTx"
	if chain == "cosmoshub-3" {
		name = "cosmos-sdk/StdTx"
	}
	c.RegisterConcrete(fixtureTx{}, name, nil)
	return c
}
func fixture(t *testing.T, chain string, msgs []fixtureMsg, key cryptotypes.PubKey) []byte {
	t.Helper()
	raw, err := fixtureCodec(chain).Amino.MarshalBinaryLengthPrefixed(fixtureTx{Msgs: msgs, Fee: fixtureFee{Amount: []fixtureCoin{{"uatom", "37"}}, Gas: 250000}, Signatures: []fixtureSignature{{key, bytes.Repeat([]byte{3}, 64)}}, Memo: "synthetic\x00memo"})
	require.NoError(t, err)
	return raw
}
func TestAminoStdTxAllHubEras(t *testing.T) {
	a, b := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20)
	for _, chain := range []string{"cosmoshub-1", "cosmoshub-2", "cosmoshub-3"} {
		t.Run(chain, func(t *testing.T) {
			key := &secp256k1.PubKey{Key: bytes.Repeat([]byte{2}, 33)}
			raw := fixture(t, chain, []fixtureMsg{fixtureSend{a, b, []fixtureCoin{{"uatom", "123"}}}, fixtureDelegate{a, b, fixtureCoin{"uatom", "9"}}, fixtureWithdraw{a, b}}, key)
			original := bytes.Clone(raw)
			tx, err := Decode(chain, raw)
			require.NoError(t, err)
			require.Equal(t, original, tx.Raw)
			require.Len(t, tx.Messages, 3)
			require.Empty(t, tx.Issues)
			require.True(t, tx.SignersComplete)
			require.Len(t, tx.Signers, 1)
			require.Equal(t, a, []byte(tx.Signers[0]))
			require.Equal(t, "37", tx.Fee[0].Amount.String())
			require.Equal(t, uint64(250000), tx.Gas)
			require.Equal(t, []byte("synthetic\x00memo"), tx.Memo)
			require.IsType(t, &bank.MsgSend{}, tx.Messages[0].Derived)
			require.IsType(t, &staking.MsgDelegate{}, tx.Messages[1].Derived)
			require.Equal(t, "cosmos-sdk/MsgWithdrawDelegationReward", tx.Messages[2].AminoName)
			require.Equal(t, "/cosmos.distribution.v1beta1.MsgWithdrawDelegatorReward", tx.Messages[2].DerivedTypeURL)
			require.Empty(t, tx.Signatures[0].Issue)
			require.Equal(t, []byte(key.Address()), []byte(tx.Signatures[0].Address))
			var msgRaw [][]byte
			for _, f := range tx.Fields {
				if f.Number == 1 {
					msgRaw = append(msgRaw, f.Value)
				}
			}
			for i, m := range tx.Messages {
				require.Equal(t, msgRaw[i], m.Raw)
			}
			raw[0] ^= 0xff
			require.Equal(t, original, tx.Raw)
		})
	}
}
func TestAminoMultisigAccountAndAbsentKey(t *testing.T) {
	member := &ed25519.PubKey{Key: bytes.Repeat([]byte{4}, 32)}
	key := multisig.NewLegacyAminoPubKey(1, []cryptotypes.PubKey{member})
	a, b := bytes.Clone(key.Address()), bytes.Repeat([]byte{2}, 20)
	for _, pk := range []cryptotypes.PubKey{key, nil} {
		tx, err := Decode("cosmoshub-3", fixture(t, "cosmoshub-3", []fixtureMsg{fixtureSend{a, b, []fixtureCoin{{"uatom", "7"}}}}, pk))
		require.NoError(t, err)
		require.Equal(t, a, []byte(tx.Signers[0]))
		require.Len(t, tx.Signers, 1)
		if pk != nil {
			require.Empty(t, tx.Signatures[0].Issue)
			require.Equal(t, a, []byte(tx.Signatures[0].Address))
			require.NotEqual(t, []byte(member.Address()), []byte(tx.Signatures[0].Address))
		} else {
			require.NotEmpty(t, tx.Signatures[0].Issue)
			require.Empty(t, tx.Signatures[0].Address)
		}
	}
}
func TestAminoMultiSendSignerOrderAndUnknown(t *testing.T) {
	a, b := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20)
	m := fixtureMulti{Inputs: []fixtureIO{{a, []fixtureCoin{{"uatom", "7"}}}, {b, []fixtureCoin{{"uatom", "8"}}}, {a, []fixtureCoin{{"uatom", "1"}}}}, Outputs: []fixtureIO{{a, []fixtureCoin{{"uatom", "16"}}}}}
	tx, err := Decode("cosmoshub-3", fixture(t, "cosmoshub-3", []fixtureMsg{m, fixtureUnknown{[]byte{0, 1, 2}}}, nil))
	require.NoError(t, err)
	require.Len(t, tx.Messages, 2)
	require.IsType(t, &bank.MsgMultiSend{}, tx.Messages[0].Derived)
	require.Nil(t, tx.Messages[1].Derived)
	require.NotEmpty(t, tx.Messages[1].Issue)
	require.NotEmpty(t, tx.Messages[1].Raw)
	require.False(t, tx.SignersComplete)
	require.Len(t, tx.Signers, 2)
	require.Equal(t, a, []byte(tx.Signers[0]))
	require.Equal(t, b, []byte(tx.Signers[1]))
}
func lengthWrap(body []byte) []byte {
	return append(binary.AppendUvarint(nil, uint64(len(body))), body...)
}
func TestAminoUnknownFieldsAndCorruption(t *testing.T) {
	a := bytes.Repeat([]byte{1}, 20)
	raw := fixture(t, "cosmoshub-3", []fixtureMsg{fixtureSend{a, a, []fixtureCoin{{"uatom", "7"}}}}, nil)
	_, n := binary.Uvarint(raw)
	body := bytes.Clone(raw[n:])
	unknown := []byte{0x32, 1, 99}
	body = append(body, unknown...)
	tx, err := Decode("cosmoshub-3", lengthWrap(body))
	require.NoError(t, err)
	require.NotEmpty(t, tx.Issues)
	require.Equal(t, unknown, tx.Fields[len(tx.Fields)-1].Raw)
	require.False(t, tx.SignersComplete)
	for i := 0; i < len(raw); i++ {
		_, err := Decode("cosmoshub-3", raw[:i])
		require.Error(t, err)
	}
	for _, input := range [][]byte{append(bytes.Clone(raw), 1), lengthWrap(append(bytes.Clone(body), 0x80)), lengthWrap(append(bytes.Clone(body), 0x10, 0)), {1, 1}, {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}} {
		_, err := Decode("cosmoshub-3", input)
		require.Error(t, err)
	}
	_, err = Decode("cosmoshub-1", raw)
	require.Error(t, err)
	// A known message with a future field stays opaque, with its entire bytes.
	fields, _ := scan(raw[n+4:])
	msg := append(bytes.Clone(value(fields, 1)), unknown...)
	decoded, err := decodeMessage("cosmoshub-3", msg)
	require.NoError(t, err)
	require.Nil(t, decoded.Derived)
	require.NotEmpty(t, decoded.Issue)
	require.Equal(t, msg, decoded.Raw)
}
func FuzzLegacyAminoNoPanic(f *testing.F) {
	f.Add([]byte{0})
	f.Add([]byte{4, 0x28, 0x28, 0x16, 0xa9})
	cdc := fixtureCodec("cosmoshub-3")
	a := bytes.Repeat([]byte{1}, 20)
	raw, err := cdc.Amino.MarshalBinaryLengthPrefixed(fixtureTx{Msgs: []fixtureMsg{fixtureSend{a, a, []fixtureCoin{{"uatom", "1"}}}}, Fee: fixtureFee{Gas: 90000}})
	if err != nil {
		f.Fatal("synthetic fuzz seed failed")
	}
	f.Add(raw)
	f.Fuzz(func(t *testing.T, raw []byte) {
		tx, err := Decode("cosmoshub-3", raw)
		if err == nil && !bytes.Equal(raw, tx.Raw) {
			t.Fatal("successful decoder changed source bytes")
		}
	})
}

func TestLegacyInclusionIdentity(t *testing.T) {
	a, err := InclusionKey("cosmoshub-1", 1, 0)
	require.NoError(t, err)
	b, _ := InclusionKey("cosmoshub-2", 1, 0)
	c, _ := InclusionKey("cosmoshub-1", 1, 1)
	require.NotEqual(t, a, b)
	require.NotEqual(t, a, c)
	for _, input := range []struct {
		chain  string
		height int64
		index  int
	}{{"cosmoshub-4", 1, 0}, {"cosmoshub-1", 0, 0}, {"cosmoshub-1", 1, -1}} {
		_, err := InclusionKey(input.chain, input.height, input.index)
		require.Error(t, err)
	}
}
