package legacy

import (
	"bytes"
	"testing"

	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/crypto/keys/ed25519"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	crisis "github.com/cosmos/cosmos-sdk/x/crisis/types"
	distribution "github.com/cosmos/cosmos-sdk/x/distribution/types"
	gov "github.com/cosmos/cosmos-sdk/x/gov/types/v1beta1"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/stretchr/testify/require"
)

// Field order here follows the historical SDK structs of each era: 0.33 for
// Hub 1, 0.34 for Hub 2 and 0.37 for Hub 3. Decimals and integers were Amino
// text, so a string field produces the same bytes.
type fixtureVote struct {
	ProposalID uint64
	Voter      []byte
	Option     byte
}

func (fixtureVote) marker() {}

type fixtureProposalByKind struct {
	Title, Description string
	Kind               byte
	Proposer           []byte
	Deposit            []fixtureCoin
}

func (fixtureProposalByKind) marker() {}

type fixtureContent interface{ content() }
type fixtureTextProposal struct{ Title, Description string }

func (fixtureTextProposal) content() {}

type fixtureUpgradeProposal struct{ Title, Description string }

func (fixtureUpgradeProposal) content() {}

type fixtureSpendProposal struct {
	Title, Description string
	Recipient          []byte
	Amount             []fixtureCoin
}

func (fixtureSpendProposal) content() {}

type fixtureProposalByContent struct {
	Content  fixtureContent
	Deposit  []fixtureCoin
	Proposer []byte
}

func (fixtureProposalByContent) marker() {}

// FixtureDescription is exported because the historical MsgEditValidator
// embeds its description, and Amino only encodes an exported embedded field.
type FixtureDescription struct{ Moniker, Identity, Website, Details string }
type fixtureRates struct{ Rate, MaxRate, MaxChangeRate string }
type fixtureCreateValidator struct {
	Description          FixtureDescription
	Commission           fixtureRates
	MinSelfDelegation    string
	Delegator, Validator []byte
	PubKey               cryptotypes.PubKey
	Value                fixtureCoin
}

func (fixtureCreateValidator) marker() {}

type fixtureEditValidator struct {
	FixtureDescription
	Validator         []byte
	CommissionRate    *string
	MinSelfDelegation *string
}

func (fixtureEditValidator) marker() {}

type fixtureInvariant struct {
	Sender        []byte
	Module, Route string
}

func (fixtureInvariant) marker() {}

type fixtureUndelegate struct {
	Delegator, Validator []byte
	Amount               fixtureCoin
}

func (fixtureUndelegate) marker() {}

type fixtureRedelegate struct {
	Delegator, Source, Destination []byte
	Amount                         fixtureCoin
}

func (fixtureRedelegate) marker() {}

type fixtureUndelegateShares struct {
	Delegator, Validator []byte
	Shares               string
}

func (fixtureUndelegateShares) marker() {}

type fixtureRedelegateShares struct {
	Delegator, Source, Destination []byte
	Shares                         string
}

func (fixtureRedelegateShares) marker() {}

func registerProjectionFixtures(c *codec.LegacyAmino, chain string) {
	c.RegisterConcrete(fixtureVote{}, "cosmos-sdk/MsgVote", nil)
	c.RegisterConcrete(fixtureCreateValidator{}, "cosmos-sdk/MsgCreateValidator", nil)
	c.RegisterConcrete(fixtureEditValidator{}, "cosmos-sdk/MsgEditValidator", nil)
	c.RegisterConcrete(fixtureInvariant{}, "cosmos-sdk/MsgVerifyInvariant", nil)
	c.RegisterConcrete(fixtureUnjail{}, "cosmos-sdk/MsgUnjail", nil)
	if chain == "cosmoshub-3" {
		c.RegisterInterface((*fixtureContent)(nil), nil)
		c.RegisterConcrete(fixtureTextProposal{}, "cosmos-sdk/TextProposal", nil)
		c.RegisterConcrete(fixtureUpgradeProposal{}, "cosmos-sdk/SoftwareUpgradeProposal", nil)
		c.RegisterConcrete(fixtureSpendProposal{}, "cosmos-sdk/CommunityPoolSpendProposal", nil)
		c.RegisterConcrete(fixtureProposalByContent{}, "cosmos-sdk/MsgSubmitProposal", nil)
	} else {
		c.RegisterConcrete(fixtureProposalByKind{}, "cosmos-sdk/MsgSubmitProposal", nil)
	}
	if chain == "cosmoshub-1" {
		c.RegisterConcrete(fixtureUndelegateShares{}, "cosmos-sdk/MsgUndelegate", nil)
		c.RegisterConcrete(fixtureRedelegateShares{}, "cosmos-sdk/MsgBeginRedelegate", nil)
	} else {
		c.RegisterConcrete(fixtureUndelegate{}, "cosmos-sdk/MsgUndelegate", nil)
		c.RegisterConcrete(fixtureRedelegate{}, "cosmos-sdk/MsgBeginRedelegate", nil)
	}
}

var allHubs = []string{"cosmoshub-1", "cosmoshub-2", "cosmoshub-3"}

func one(t *testing.T, chain string, msg fixtureMsg) (*Tx, Message) {
	t.Helper()
	tx, err := Decode(chain, fixture(t, chain, []fixtureMsg{msg}, nil))
	require.NoError(t, err)
	require.Len(t, tx.Messages, 1)
	return tx, tx.Messages[0]
}

func bech(t *testing.T, prefix string, b []byte) string {
	t.Helper()
	s, err := sdk.Bech32ifyAddressBytes(prefix, b)
	require.NoError(t, err)
	return s
}

func TestAminoVoteProjection(t *testing.T) {
	a := bytes.Repeat([]byte{1}, 20)
	for _, chain := range allHubs {
		tx, msg := one(t, chain, fixtureVote{ProposalID: 23, Voter: a, Option: 4})
		require.Empty(t, tx.Issues)
		require.True(t, tx.SignersComplete)
		require.Equal(t, a, []byte(tx.Signers[0]))
		vote, ok := msg.Derived.(*gov.MsgVote)
		require.True(t, ok)
		require.Equal(t, uint64(23), vote.ProposalId)
		require.Equal(t, gov.OptionNoWithVeto, vote.Option)
		require.Equal(t, bech(t, "cosmos", a), vote.Voter)
		require.Equal(t, "/cosmos.gov.v1beta1.MsgVote", msg.DerivedTypeURL)

		// Option zero is omitted on the wire and is not a vote. Five is out
		// of range. Neither may be projected as if it were a valid choice.
		for _, option := range []byte{0, 5} {
			tx, msg = one(t, chain, fixtureVote{ProposalID: 23, Voter: a, Option: option})
			require.Nil(t, msg.Derived)
			require.NotEmpty(t, msg.Issue)
			require.False(t, tx.SignersComplete)
		}
	}
}

func TestAminoSubmitProposalProjectionByEra(t *testing.T) {
	a, b := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20)
	deposit := []fixtureCoin{{"uatom", "512000000"}}
	for _, chain := range []string{"cosmoshub-1", "cosmoshub-2"} {
		for kind := byte(1); kind <= 3; kind++ {
			tx, msg := one(t, chain, fixtureProposalByKind{"synthetic title", "synthetic text", kind, a, deposit})
			require.Empty(t, tx.Issues)
			require.Equal(t, a, []byte(tx.Signers[0]))
			proposal := msg.Derived.(*gov.MsgSubmitProposal)
			require.Equal(t, bech(t, "cosmos", a), proposal.Proposer)
			require.Equal(t, "512000000uatom", proposal.InitialDeposit.String())
			if kind == 1 {
				require.Equal(t, "synthetic title", proposal.GetContent().GetTitle())
			} else {
				require.Nil(t, proposal.Content)
			}
		}
		_, msg := one(t, chain, fixtureProposalByKind{"synthetic title", "synthetic text", 9, a, deposit})
		require.Nil(t, msg.Derived)
		require.NotEmpty(t, msg.Issue)
	}

	tx, msg := one(t, "cosmoshub-3", fixtureProposalByContent{fixtureTextProposal{"synthetic title", "synthetic text"}, deposit, a})
	require.Empty(t, tx.Issues)
	require.Equal(t, a, []byte(tx.Signers[0]))
	proposal := msg.Derived.(*gov.MsgSubmitProposal)
	require.Equal(t, "512000000uatom", proposal.InitialDeposit.String())
	require.Equal(t, "synthetic text", proposal.GetContent().GetDescription())

	_, msg = one(t, "cosmoshub-3", fixtureProposalByContent{fixtureSpendProposal{"t", "d", b, []fixtureCoin{{"uatom", "77"}}}, deposit, a})
	spend, ok := msg.Derived.(*gov.MsgSubmitProposal).GetContent().(*distribution.CommunityPoolSpendProposal)
	require.True(t, ok)
	require.Equal(t, bech(t, "cosmos", b), spend.Recipient)
	require.Equal(t, "77uatom", spend.Amount.String())

	// A content with no exact modern form keeps the deposit and proposer and
	// leaves the content to the retained bytes.
	tx, msg = one(t, "cosmoshub-3", fixtureProposalByContent{fixtureUpgradeProposal{"t", "d"}, nil, a})
	require.Empty(t, tx.Issues)
	proposal = msg.Derived.(*gov.MsgSubmitProposal)
	require.Nil(t, proposal.Content)
	require.Empty(t, proposal.InitialDeposit)
	require.Equal(t, bech(t, "cosmos", a), proposal.Proposer)
	require.NotEmpty(t, msg.Raw)

	// The two eras share a name but not a layout. Reading one as the other
	// must fail closed, never produce a proposer from a title.
	hubTwo := fixture(t, "cosmoshub-2", []fixtureMsg{fixtureProposalByKind{"synthetic title", "synthetic text", 1, a, deposit}}, nil)
	hubThree := fixture(t, "cosmoshub-3", []fixtureMsg{fixtureProposalByContent{fixtureTextProposal{"t", "d"}, deposit, a}}, nil)
	_, n, _ := lengthAndBody(hubTwo)
	fields, err := scan(hubTwo[n+4:])
	require.NoError(t, err)
	crossed, err := decodeMessage("cosmoshub-3", value(fields, 1))
	require.NoError(t, err)
	require.Nil(t, crossed.Derived)
	_, n, _ = lengthAndBody(hubThree)
	fields, err = scan(hubThree[n+4:])
	require.NoError(t, err)
	crossed, err = decodeMessage("cosmoshub-2", value(fields, 1))
	require.NoError(t, err)
	require.Nil(t, crossed.Derived)
}

func lengthAndBody(raw []byte) (uint64, int, []byte) {
	size, n := uvarint(raw)
	return size, n, raw[n:]
}

func uvarint(raw []byte) (uint64, int) {
	var x uint64
	for i, b := range raw {
		x |= uint64(b&0x7f) << (7 * uint(i))
		if b < 0x80 {
			return x, i + 1
		}
	}
	return 0, 0
}

func TestAminoCreateValidatorProjection(t *testing.T) {
	a, b := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20)
	key := &ed25519.PubKey{Key: bytes.Repeat([]byte{7}, 32)}
	base := fixtureCreateValidator{
		Description:       FixtureDescription{"synthetic moniker", "", "https://example.invalid", "synthetic details"},
		Commission:        fixtureRates{"100000000000000000", "200000000000000000", "10000000000000000"},
		MinSelfDelegation: "1", Delegator: a, Validator: a, PubKey: key, Value: fixtureCoin{"uatom", "5000000"},
	}
	for _, chain := range allHubs {
		tx, msg := one(t, chain, base)
		require.Empty(t, tx.Issues)
		require.True(t, tx.SignersComplete)
		require.Len(t, tx.Signers, 1)
		created := msg.Derived.(*staking.MsgCreateValidator)
		require.Equal(t, "synthetic moniker", created.Description.Moniker)
		require.Equal(t, "https://example.invalid", created.Description.Website)
		require.Equal(t, "synthetic details", created.Description.Details)
		require.Empty(t, created.Description.SecurityContact)
		require.Equal(t, "0.100000000000000000", created.Commission.Rate.String())
		require.Equal(t, "0.200000000000000000", created.Commission.MaxRate.String())
		require.Equal(t, "0.010000000000000000", created.Commission.MaxChangeRate.String())
		require.Equal(t, "1", created.MinSelfDelegation.String())
		require.Equal(t, bech(t, "cosmos", a), created.DelegatorAddress)
		require.Equal(t, bech(t, "cosmosvaloper", a), created.ValidatorAddress)
		require.Equal(t, "5000000uatom", created.Value.String())
		stored, ok := created.Pubkey.GetCachedValue().(cryptotypes.PubKey)
		require.True(t, ok)
		require.True(t, stored.Equals(key))

		// A separate operator account signs second, so the delegator pays.
		other := base
		other.Validator = b
		tx, _ = one(t, chain, other)
		require.Len(t, tx.Signers, 2)
		require.Equal(t, a, []byte(tx.Signers[0]))
		require.Equal(t, b, []byte(tx.Signers[1]))

		for _, broken := range []func(*fixtureCreateValidator){
			func(m *fixtureCreateValidator) { m.Commission.Rate = "0.1" },
			func(m *fixtureCreateValidator) { m.Commission.MaxRate = "" },
			func(m *fixtureCreateValidator) { m.MinSelfDelegation = "01" },
			func(m *fixtureCreateValidator) { m.Delegator = a[:19] },
			func(m *fixtureCreateValidator) { m.PubKey = nil },
			func(m *fixtureCreateValidator) { m.Value = fixtureCoin{"uatom", "x"} },
		} {
			bad := base
			broken(&bad)
			tx, msg = one(t, chain, bad)
			require.Nil(t, msg.Derived)
			require.NotEmpty(t, msg.Issue)
			require.False(t, tx.SignersComplete)
			require.NotEmpty(t, msg.Raw)
		}
	}
}

func TestAminoEditValidatorProjection(t *testing.T) {
	a := bytes.Repeat([]byte{1}, 20)
	rate, minSelf, zero := "50000000000000000", "1000000", "0"
	for _, chain := range allHubs {
		tx, msg := one(t, chain, fixtureEditValidator{FixtureDescription: FixtureDescription{Moniker: "[do-not-modify]"}, Validator: a})
		require.Empty(t, tx.Issues)
		require.Equal(t, a, []byte(tx.Signers[0]))
		edited := msg.Derived.(*staking.MsgEditValidator)
		require.Equal(t, bech(t, "cosmosvaloper", a), edited.ValidatorAddress)
		require.Equal(t, "[do-not-modify]", edited.Description.Moniker)
		require.Nil(t, edited.CommissionRate)
		require.Nil(t, edited.MinSelfDelegation)

		_, msg = one(t, chain, fixtureEditValidator{Validator: a, CommissionRate: &rate, MinSelfDelegation: &minSelf})
		edited = msg.Derived.(*staking.MsgEditValidator)
		require.Equal(t, "0.050000000000000000", edited.CommissionRate.String())
		require.Equal(t, "1000000", edited.MinSelfDelegation.String())

		// A change to zero is present on the wire and differs from no change.
		_, msg = one(t, chain, fixtureEditValidator{Validator: a, CommissionRate: &zero})
		edited = msg.Derived.(*staking.MsgEditValidator)
		require.NotNil(t, edited.CommissionRate)
		require.True(t, edited.CommissionRate.IsZero())
		require.Nil(t, edited.MinSelfDelegation)

		bad := "1e3"
		tx, msg = one(t, chain, fixtureEditValidator{Validator: a, CommissionRate: &bad})
		require.Nil(t, msg.Derived)
		require.False(t, tx.SignersComplete)
	}
}

func TestAminoVerifyInvariantProjection(t *testing.T) {
	a := bytes.Repeat([]byte{1}, 20)
	for _, chain := range []string{"cosmoshub-2", "cosmoshub-3"} {
		tx, msg := one(t, chain, fixtureInvariant{a, "staking", "module-accounts"})
		require.Empty(t, tx.Issues)
		require.Equal(t, a, []byte(tx.Signers[0]))
		verify := msg.Derived.(*crisis.MsgVerifyInvariant)
		require.Equal(t, bech(t, "cosmos", a), verify.Sender)
		require.Equal(t, "staking", verify.InvariantModuleName)
		require.Equal(t, "module-accounts", verify.InvariantRoute)
	}
}

func TestAminoStakingAmountByEra(t *testing.T) {
	a, b, c := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20), bytes.Repeat([]byte{3}, 20)
	for _, chain := range []string{"cosmoshub-2", "cosmoshub-3"} {
		_, msg := one(t, chain, fixtureUndelegate{a, b, fixtureCoin{"uatom", "40"}})
		require.Equal(t, "40uatom", msg.Derived.(*staking.MsgUndelegate).Amount.String())
		_, msg = one(t, chain, fixtureRedelegate{a, b, c, fixtureCoin{"uatom", "41"}})
		moved := msg.Derived.(*staking.MsgBeginRedelegate)
		require.Equal(t, "41uatom", moved.Amount.String())
		require.Equal(t, bech(t, "cosmosvaloper", c), moved.ValidatorDstAddress)
	}

	// Hub 1 states shares. The signer is exact, so the fee payer resolves,
	// but no coin amount is invented and the message stays flagged.
	for _, msg := range []fixtureMsg{fixtureUndelegateShares{a, b, "40000000000000000000"}, fixtureRedelegateShares{a, b, c, "41000000000000000000"}} {
		tx, decoded := one(t, "cosmoshub-1", msg)
		require.Nil(t, decoded.Derived)
		require.Empty(t, decoded.DerivedTypeURL)
		require.Equal(t, shareValuedIssue, decoded.Issue)
		require.Equal(t, []string{shareValuedIssue}, tx.Issues)
		require.True(t, tx.SignersComplete)
		require.Len(t, tx.Signers, 1)
		require.Equal(t, a, []byte(tx.Signers[0]))
	}
	tx, decoded := one(t, "cosmoshub-1", fixtureUndelegateShares{a, b, "not a number"})
	require.Nil(t, decoded.Derived)
	require.NotEqual(t, shareValuedIssue, decoded.Issue)
	require.False(t, tx.SignersComplete)
	tx, decoded = one(t, "cosmoshub-1", fixtureRedelegateShares{a, b, c[:19], "41"})
	require.Nil(t, decoded.Derived)
	require.NotEqual(t, shareValuedIssue, decoded.Issue)
	require.False(t, tx.SignersComplete)
}
