package legacy

import (
	"bytes"
	"testing"

	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/stretchr/testify/require"
)

type fixtureUnjail struct{ Validator []byte }

func (fixtureUnjail) marker() {}

type fixtureBits struct {
	ExtraBitsStored byte
	Elems           []byte
}
type fixtureThreshold struct {
	K       uint
	PubKeys []cryptotypes.PubKey
}
type fixtureMultisignature struct {
	BitArray *fixtureBits
	Sigs     [][]byte
}

func TestSignDocIsTheSortedAminoJSONOfEachEra(t *testing.T) {
	a, b := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20)
	account, operator := bech(t, "cosmos", a), bech(t, "cosmosvaloper", b)
	fee := `"fee":{"amount":[{"amount":"37","denom":"uatom"}],"gas":"250000"},"memo":"synthetic\u0000memo"`
	for _, chain := range allHubs {
		tx, err := Decode(chain, fixture(t, chain, []fixtureMsg{
			fixtureSend{a, b, []fixtureCoin{{"uatom", "123"}}}, fixtureDelegate{a, b, fixtureCoin{"uatom", "9"}}, fixtureWithdraw{a, b}, fixtureUnjail{b}}, nil))
		require.NoError(t, err)
		doc, err := tx.SignDoc(chain)
		require.NoError(t, err)
		parties := `"delegator_address":"` + account + `","validator_address":"` + operator + `"`
		bonded, unjail := `"amount":{"amount":"9","denom":"uatom"},`+parties, `{"type":"cosmos-sdk/MsgUnjail","value":{"address":"`+operator+`"}}`
		if chain == "cosmoshub-1" {
			bonded = parties + `,"value":{"amount":"9","denom":"uatom"}`
		}
		if chain != "cosmoshub-3" {
			unjail = `{"address":"` + operator + `"}`
		}
		require.Equal(t, `{"account_number":"7","chain_id":"`+chain+`",`+fee+`,"msgs":[`+
			`{"type":"cosmos-sdk/MsgSend","value":{"amount":[{"amount":"123","denom":"uatom"}],"from_address":"`+account+`","to_address":"`+bech(t, "cosmos", b)+`"}},`+
			`{"type":"cosmos-sdk/MsgDelegate","value":{`+bonded+`}},`+
			`{"type":"cosmos-sdk/MsgWithdrawDelegationReward","value":{"delegator_address":"`+account+`","validator_address":"`+operator+`"}},`+
			unjail+`],"sequence":"42"}`, string(doc.Bytes(7, 42)), chain)
	}
}

func TestSignDocOfValidatorAndGovernanceMessages(t *testing.T) {
	a := bytes.Repeat([]byte{1}, 20)
	account, operator := bech(t, "cosmos", a), bech(t, "cosmosvaloper", a)
	value := func(chain string, msg fixtureMsg) string {
		tx, _ := one(t, chain, msg)
		doc, err := tx.SignDoc(chain)
		require.NoError(t, err)
		text := string(doc.Bytes(0, 0))
		start, end := bytes.Index([]byte(text), []byte(`"msgs":[`))+8, bytes.LastIndex([]byte(text), []byte(`],"sequence"`))
		return text[start:end]
	}
	rate := "50000000000000000"
	require.Equal(t, `{"type":"cosmos-sdk/MsgEditValidator","value":{"Description":{"details":"d","identity":"","moniker":"m \u0026 co","website":""},"address":"`+operator+`","commission_rate":"0.050000000000000000","min_self_delegation":null}}`,
		value("cosmoshub-2", fixtureEditValidator{FixtureDescription: FixtureDescription{Moniker: "m & co", Details: "d"}, Validator: a, CommissionRate: &rate}))
	require.Equal(t, `{"type":"cosmos-sdk/MsgVote","value":{"option":"NoWithVeto","proposal_id":"12","voter":"`+account+`"}}`,
		value("cosmoshub-3", fixtureVote{ProposalID: 12, Voter: a, Option: 4}))
	require.Equal(t, `{"type":"cosmos-sdk/MsgSubmitProposal","value":{"content":{"type":"cosmos-sdk/TextProposal","value":{"description":"why","title":"what"}},"initial_deposit":[{"amount":"5","denom":"uatom"}],"proposer":"`+account+`"}}`,
		value("cosmoshub-3", fixtureProposalByContent{Content: fixtureTextProposal{"what", "why"}, Deposit: []fixtureCoin{{"uatom", "5"}}, Proposer: a}))
	require.Equal(t, `{"type":"cosmos-sdk/MsgSubmitProposal","value":{"description":"why","initial_deposit":null,"proposal_type":"Text","proposer":"`+account+`","title":"what"}}`,
		value("cosmoshub-1", fixtureProposalByKind{Title: "what", Description: "why", Kind: 1, Proposer: a}))
	require.Equal(t, `{"type":"cosmos-sdk/MsgUndelegate","value":{"delegator_address":"`+account+`","shares_amount":"1.500000000000000000","validator_address":"`+operator+`"}}`,
		value("cosmoshub-1", fixtureUndelegateShares{Delegator: a, Validator: a, Shares: "1500000000000000000"}))
}

func TestSignDocRefusesWhatItCannotReproduce(t *testing.T) {
	a := bytes.Repeat([]byte{1}, 20)
	tx, _ := one(t, "cosmoshub-3", fixtureUnknown{Payload: a})
	_, err := tx.SignDoc("cosmoshub-3")
	require.Error(t, err)
}

func TestVerifySignatureAcceptsOnlyTheSignedAccountNumberAndSequence(t *testing.T) {
	a, b := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20)
	msgs := []fixtureMsg{fixtureSend{a, b, []fixtureCoin{{"uatom", "123"}}}}
	first, second := secp256k1.GenPrivKey(), secp256k1.GenPrivKey()
	for _, chain := range allHubs {
		tx, err := Decode(chain, fixture(t, chain, msgs, first.PubKey()))
		require.NoError(t, err)
		doc, err := tx.SignDoc(chain)
		require.NoError(t, err)
		signature, err := first.Sign(doc.Bytes(7, 42))
		require.NoError(t, err)
		key := tx.Signatures[0].PublicKeyRaw
		require.True(t, VerifySignature(key, signature, doc.Bytes(7, 42)))
		require.False(t, VerifySignature(key, signature, doc.Bytes(7, 43)))
		require.False(t, VerifySignature(key, signature, doc.Bytes(8, 42)))
		require.False(t, VerifySignature(key, signature[:63], doc.Bytes(7, 42)))

		other, err := second.Sign(doc.Bytes(7, 42))
		require.NoError(t, err)
		keys := codec.NewLegacyAmino()
		keys.RegisterInterface((*cryptotypes.PubKey)(nil), nil)
		keys.RegisterConcrete(&secp256k1.PubKey{}, "tendermint/PubKeySecp256k1", nil)
		keys.RegisterConcrete(fixtureThreshold{}, "tendermint/PubKeyMultisigThreshold", nil)
		key, err = keys.Amino.MarshalBinaryBare(fixtureThreshold{K: 2, PubKeys: []cryptotypes.PubKey{first.PubKey(), secp256k1.GenPrivKey().PubKey(), second.PubKey()}})
		require.NoError(t, err)
		_, err = publicKeyAddress(key, 0)
		require.NoError(t, err)
		pack := func(bits byte, sigs ...[]byte) []byte {
			raw, err := fixtureCodec(chain).Amino.MarshalBinaryBare(fixtureMultisignature{BitArray: &fixtureBits{ExtraBitsStored: 3, Elems: []byte{bits}}, Sigs: sigs})
			require.NoError(t, err)
			return raw
		}
		require.True(t, VerifySignature(key, pack(0b10100000, signature, other), doc.Bytes(7, 42)))
		require.False(t, VerifySignature(key, pack(0b10100000, signature, other), doc.Bytes(7, 43)))
		require.False(t, VerifySignature(key, pack(0b11000000, signature, other), doc.Bytes(7, 42)), "second signature is not the second member's")
		require.False(t, VerifySignature(key, pack(0b10000000, signature), doc.Bytes(7, 42)), "below the threshold")
	}
}
