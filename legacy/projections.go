package legacy

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"unicode/utf8"

	sdkmath "cosmossdk.io/math"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto/keys/ed25519"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	crisis "github.com/cosmos/cosmos-sdk/x/crisis/types"
	distribution "github.com/cosmos/cosmos-sdk/x/distribution/types"
	gov "github.com/cosmos/cosmos-sdk/x/gov/types/v1beta1"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// errShareValued marks a Hub 1 undelegation or redelegation. Its signer is
// exact but its amount is a share quantity that only state can price.
var errShareValued = errors.New("share-valued staking message")

const shareValuedIssue = "share-valued staking amount needs validator exchange rate"

func has(fields []Field, num uint64) bool {
	for _, f := range fields {
		if f.Number == num {
			return true
		}
	}
	return false
}

// text reads an optional Amino string. Amino omits an empty string, so an
// absent field is the empty string and not an error.
func text(fields []Field, num uint64) (string, bool) {
	b := value(fields, num)
	return string(b), utf8.Valid(b)
}

// decimal reads the historical sdk.Dec encoding: the decimal text of an
// integer carrying eighteen implied fractional digits in every Hub 1 to 3 SDK.
func decimal(raw []byte) (sdk.Dec, bool) {
	number, ok := new(big.Int).SetString(string(raw), 10)
	if !ok || number.String() != string(raw) || number.BitLen() > 256 {
		return sdk.Dec{}, false
	}
	return sdk.NewDecFromBigIntWithPrec(number, sdk.Precision), true
}

func integer(raw []byte) (sdkmath.Int, bool) {
	number, ok := sdkmath.NewIntFromString(string(raw))
	if !ok || number.String() != string(raw) {
		return sdkmath.Int{}, false
	}
	return number, true
}

// description maps the four historical fields by name. The modern struct has
// a security contact in fourth position, so positions must not be reused.
func description(raw []byte) (staking.Description, error) {
	failure := fmt.Errorf("unsupported Amino validator description")
	fields, err := scan(raw)
	if err != nil || schema(fields, map[uint64]uint64{1: 2, 2: 2, 3: 2, 4: 2}) != nil {
		return staking.Description{}, failure
	}
	var out [4]string
	for i := range out {
		s, ok := text(fields, uint64(i+1))
		if !ok {
			return staking.Description{}, failure
		}
		out[i] = s
	}
	return staking.Description{Moniker: out[0], Identity: out[1], Website: out[2], Details: out[3]}, nil
}

// consensusKey reads a single historical Amino public key. A validator
// consensus key is never a multisig.
func consensusKey(raw []byte) (cryptotypes.PubKey, bool) {
	name, body, ok := identify(raw, []string{"tendermint/PubKeyEd25519", "tendermint/PubKeySecp256k1"})
	if !ok || len(raw) < 4 || raw[0] == 0 {
		return nil, false
	}
	if len(body) == 0 || int(body[0]) != len(body)-1 {
		return nil, false
	}
	key := bytes.Clone(body[1:])
	if name == "tendermint/PubKeyEd25519" && len(key) == 32 {
		return &ed25519.PubKey{Key: key}, true
	}
	if name == "tendermint/PubKeySecp256k1" && len(key) == 33 {
		return &secp256k1.PubKey{Key: key}, true
	}
	return nil, false
}

// proposalContent projects only contents the serving SDK can state exactly.
// Any other content stays nil; the message bytes still hold it in full.
func proposalContent(raw []byte) gov.Content {
	name, body, ok := identify(raw, []string{"cosmos-sdk/TextProposal", "cosmos-sdk/CommunityPoolSpendProposal"})
	if !ok {
		return nil
	}
	fields, err := scan(body)
	if err != nil {
		return nil
	}
	title, okTitle := text(fields, 1)
	detail, okDetail := text(fields, 2)
	if !okTitle || !okDetail {
		return nil
	}
	if name == "cosmos-sdk/TextProposal" {
		if schema(fields, map[uint64]uint64{1: 2, 2: 2}) != nil {
			return nil
		}
		return gov.NewTextProposal(title, detail)
	}
	if schema(fields, map[uint64]uint64{1: 2, 2: 2, 3: 2, 4: 2}, 4) != nil || len(value(fields, 3)) != 20 {
		return nil
	}
	recipient, err := sdk.Bech32ifyAddressBytes("cosmos", value(fields, 3))
	if err != nil {
		return nil
	}
	amount, err := coins(fields, 4)
	if err != nil {
		return nil
	}
	return &distribution.CommunityPoolSpendProposal{Title: title, Description: detail, Recipient: recipient, Amount: amount}
}

// deriveGoverning projects the governance, validator and crisis messages.
// None of them moves coins except a proposal's initial deposit and a new
// validator's self-bond, but each names the account that paid the fee.
func deriveGoverning(chain, name string, f []Field) (sdk.Msg, []sdk.AccAddress, error) {
	failure := fmt.Errorf("unsupported Amino message projection")
	address := func(num uint64, prefix string) (string, error) {
		b := value(f, num)
		if len(b) != 20 {
			return "", failure
		}
		return sdk.Bech32ifyAddressBytes(prefix, b)
	}
	signer := func(num uint64) []sdk.AccAddress { return []sdk.AccAddress{bytes.Clone(value(f, num))} }
	switch name {
	case "cosmos-sdk/MsgVote":
		if schema(f, map[uint64]uint64{1: 0, 2: 2, 3: 0}) != nil {
			return nil, nil, failure
		}
		voter, err := address(2, "cosmos")
		if err != nil {
			return nil, nil, err
		}
		option := uintValue(f, 3)
		if option < 1 || option > 4 {
			return nil, nil, failure
		}
		return &gov.MsgVote{ProposalId: uintValue(f, 1), Voter: voter, Option: gov.VoteOption(option)}, signer(2), nil
	case "cosmos-sdk/MsgSubmitProposal":
		// Hub 3 carries a typed content, a deposit and a proposer. Hub 1 and
		// Hub 2 carry title, description, a kind byte, proposer and deposit.
		// Both start with two length-delimited fields, so the era decides.
		msg := &gov.MsgSubmitProposal{}
		proposerNum, depositNum := uint64(3), uint64(2)
		var content gov.Content
		if chain == "cosmoshub-3" {
			if schema(f, map[uint64]uint64{1: 2, 2: 2, 3: 2}, 2) != nil {
				return nil, nil, failure
			}
			content = proposalContent(value(f, 1))
		} else {
			if schema(f, map[uint64]uint64{1: 2, 2: 2, 3: 0, 4: 2, 5: 2}, 5) != nil {
				return nil, nil, failure
			}
			proposerNum, depositNum = 4, 5
			kind := uintValue(f, 3)
			if kind < 1 || kind > 3 {
				return nil, nil, failure
			}
			title, okTitle := text(f, 1)
			detail, okDetail := text(f, 2)
			if kind == 1 && okTitle && okDetail {
				content = gov.NewTextProposal(title, detail)
			}
		}
		proposer, err := address(proposerNum, "cosmos")
		if err != nil {
			return nil, nil, err
		}
		deposit, err := coins(f, depositNum)
		if err != nil {
			return nil, nil, err
		}
		msg.Proposer, msg.InitialDeposit = proposer, deposit
		if content != nil {
			if err := msg.SetContent(content); err != nil {
				return nil, nil, failure
			}
		}
		return msg, signer(proposerNum), nil
	case "cosmos-sdk/MsgCreateValidator":
		if schema(f, map[uint64]uint64{1: 2, 2: 2, 3: 2, 4: 2, 5: 2, 6: 2, 7: 2}) != nil {
			return nil, nil, failure
		}
		desc, err := description(value(f, 1))
		if err != nil {
			return nil, nil, err
		}
		rates, err := scan(value(f, 2))
		if err != nil || schema(rates, map[uint64]uint64{1: 2, 2: 2, 3: 2}) != nil {
			return nil, nil, failure
		}
		rate, okRate := decimal(value(rates, 1))
		maxRate, okMax := decimal(value(rates, 2))
		maxChange, okChange := decimal(value(rates, 3))
		minSelf, okSelf := integer(value(f, 3))
		if !okRate || !okMax || !okChange || !okSelf {
			return nil, nil, failure
		}
		delegator, err := address(4, "cosmos")
		if err != nil {
			return nil, nil, err
		}
		operator, err := address(5, "cosmosvaloper")
		if err != nil {
			return nil, nil, err
		}
		key, ok := consensusKey(value(f, 6))
		if !ok {
			return nil, nil, failure
		}
		packed, err := codectypes.NewAnyWithValue(key)
		if err != nil {
			return nil, nil, failure
		}
		bond, err := coin(value(f, 7))
		if err != nil {
			return nil, nil, err
		}
		signers := signer(4)
		// A validator created from another account must sign as well, after
		// the delegator, who therefore still pays the fee.
		if !bytes.Equal(value(f, 4), value(f, 5)) {
			signers = append(signers, bytes.Clone(value(f, 5)))
		}
		return &staking.MsgCreateValidator{
			Description: desc, Commission: staking.CommissionRates{Rate: rate, MaxRate: maxRate, MaxChangeRate: maxChange},
			MinSelfDelegation: minSelf, DelegatorAddress: delegator, ValidatorAddress: operator, Pubkey: packed, Value: bond,
		}, signers, nil
	case "cosmos-sdk/MsgEditValidator":
		if schema(f, map[uint64]uint64{1: 2, 2: 2, 3: 2, 4: 2}) != nil {
			return nil, nil, failure
		}
		desc, err := description(value(f, 1))
		if err != nil {
			return nil, nil, err
		}
		operator, err := address(2, "cosmosvaloper")
		if err != nil {
			return nil, nil, err
		}
		msg := &staking.MsgEditValidator{Description: desc, ValidatorAddress: operator}
		// Both changes are optional pointers. Absent means unchanged, which
		// is different from a change to zero.
		if has(f, 3) {
			rate, ok := decimal(value(f, 3))
			if !ok {
				return nil, nil, failure
			}
			msg.CommissionRate = &rate
		}
		if has(f, 4) {
			minSelf, ok := integer(value(f, 4))
			if !ok {
				return nil, nil, failure
			}
			msg.MinSelfDelegation = &minSelf
		}
		return msg, signer(2), nil
	case "cosmos-sdk/MsgVerifyInvariant":
		if schema(f, map[uint64]uint64{1: 2, 2: 2, 3: 2}) != nil {
			return nil, nil, failure
		}
		sender, err := address(1, "cosmos")
		if err != nil {
			return nil, nil, err
		}
		module, okModule := text(f, 2)
		route, okRoute := text(f, 3)
		if !okModule || !okRoute {
			return nil, nil, failure
		}
		return &crisis.MsgVerifyInvariant{Sender: sender, InvariantModuleName: module, InvariantRoute: route}, signer(1), nil
	}
	return nil, nil, failure
}
