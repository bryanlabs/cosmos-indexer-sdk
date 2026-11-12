// Package legacy decodes the pre-Stargate Hub Amino wire format without loading
// an obsolete SDK into the serving indexer. No Amino payload is a protobuf Any.
package legacy

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

	sdkmath "cosmossdk.io/math"
	"github.com/cosmos/cosmos-sdk/crypto/keys/ed25519"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	sdk "github.com/cosmos/cosmos-sdk/types"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
	distribution "github.com/cosmos/cosmos-sdk/x/distribution/types"
	gov "github.com/cosmos/cosmos-sdk/x/gov/types/v1beta1"
	slashing "github.com/cosmos/cosmos-sdk/x/slashing/types"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
	amino "github.com/tendermint/go-amino"
)

const Encoding = "amino-length-prefixed-stdtx"

// Field retains both the exact encoding and its length-delimited value. These
// are Amino field encodings, not protobuf. Unknown fields are never discarded.
type Field struct {
	Number     uint64
	Wire       uint64
	Uint       uint64
	Value, Raw []byte
}
type Message struct {
	Raw       []byte
	Prefix    string
	AminoName string
	Fields    []Field
	// Derived is an explicit semantic projection. It must not replace Raw or
	// be marshaled into a fake historical transaction. Nil means unsupported.
	Derived        sdk.Msg
	DerivedTypeURL string
	Issue          string
	Signers        []sdk.AccAddress
	// AmountWithoutDenom marks a Hub 3 staking message whose amount names no
	// denom. The chain's handler refused every such message, so it can only
	// sit in a failed transaction.
	AmountWithoutDenom bool
}
type Signature struct {
	Raw                     []byte
	Fields                  []Field
	PublicKeyRaw, Signature []byte
	Address                 sdk.AccAddress
	Issue                   string
}
type Tx struct {
	Raw                 []byte
	Encoding, AminoName string
	Fields              []Field
	Messages            []Message
	FeeRaw              []byte
	Fee                 sdk.Coins
	Gas                 uint64
	Signatures          []Signature
	Memo                []byte
	Signers             []sdk.AccAddress
	SignersComplete     bool
	Issues              []string
}

// Decode preserves the entire input before interpreting it. Errors contain
// schema metadata only, never account bytes, quantities, memo or signature.
// Amino integer and length encodings are checked for truncation/overflow.
func Decode(chain string, input []byte) (*Tx, error) {
	if chain != "cosmoshub-1" && chain != "cosmoshub-2" && chain != "cosmoshub-3" {
		return nil, fmt.Errorf("unsupported legacy chain")
	}
	if len(input) == 0 || len(input) > 16<<20 {
		return nil, fmt.Errorf("invalid Amino transaction size")
	}
	raw := bytes.Clone(input)
	size, n := binary.Uvarint(raw)
	if n <= 0 || size != uint64(len(raw)-n) {
		return nil, fmt.Errorf("invalid Amino transaction length")
	}
	name, body, ok := identify(raw[n:], []string{"auth/StdTx", "cosmos-sdk/StdTx"})
	if !ok {
		return nil, fmt.Errorf("unsupported Amino transaction prefix")
	}
	if (chain == "cosmoshub-3") != (name == "cosmos-sdk/StdTx") {
		return nil, fmt.Errorf("Amino transaction prefix does not match chain era")
	}
	fields, err := scan(body)
	if err != nil {
		return nil, err
	}
	tx := &Tx{Raw: raw, Encoding: Encoding, AminoName: name, Fields: fields, SignersComplete: true}
	seen := map[uint64]bool{}
	for _, f := range fields {
		if f.Number != 1 && f.Number != 3 && seen[f.Number] {
			return nil, fmt.Errorf("duplicate Amino StdTx field")
		}
		seen[f.Number] = true
		switch f.Number {
		case 1:
			if f.Wire != 2 {
				return nil, fmt.Errorf("invalid Amino message field")
			}
			msg, err := decodeMessage(chain, f.Value)
			if err != nil {
				return nil, err
			}
			tx.Messages = append(tx.Messages, msg)
			if msg.Issue != "" {
				tx.Issues = append(tx.Issues, msg.Issue)
				// A message can be unprojectable while its signer is still
				// exact. Only a message with no known signer breaks the union.
				if len(msg.Signers) == 0 {
					tx.SignersComplete = false
				}
			}
			for _, addr := range msg.Signers {
				if !contains(tx.Signers, addr) {
					tx.Signers = append(tx.Signers, addr)
				}
			}
		case 2:
			if f.Wire != 2 {
				return nil, fmt.Errorf("invalid Amino fee field")
			}
			tx.FeeRaw = bytes.Clone(f.Value)
			tx.Fee, tx.Gas, err = decodeFee(f.Value)
			if err != nil {
				return nil, err
			}
		case 3:
			if f.Wire != 2 {
				return nil, fmt.Errorf("invalid Amino signature field")
			}
			sig, err := decodeSignature(f.Value)
			if err != nil {
				return nil, err
			}
			tx.Signatures = append(tx.Signatures, sig)
		case 4:
			if f.Wire != 2 {
				return nil, fmt.Errorf("invalid Amino memo field")
			}
			tx.Memo = bytes.Clone(f.Value)
		default:
			tx.Issues = append(tx.Issues, "unsupported Amino StdTx field")
			tx.SignersComplete = false
		}
	}
	if !seen[2] {
		return nil, fmt.Errorf("missing Amino fee evidence")
	}
	if len(tx.Messages) == 0 {
		tx.Issues = append(tx.Issues, "no Amino messages")
		tx.SignersComplete = false
	}
	// GetSigners in old StdTx is the ordered union of message signers. A
	// multisig signature belongs to its aggregate account, not its members.
	// Never substitute a key's address when a message signer is unresolved.
	return tx, nil
}
func contains(addrs []sdk.AccAddress, addr sdk.AccAddress) bool {
	for _, a := range addrs {
		if bytes.Equal(a, addr) {
			return true
		}
	}
	return false
}
func identify(raw []byte, names []string) (string, []byte, bool) {
	for _, name := range names {
		dis, prefix := amino.NameToDisfix(name)
		if len(raw) >= 4 && bytes.Equal(raw[:4], prefix[:]) {
			return name, raw[4:], true
		}
		if len(raw) >= 8 && raw[0] == 0 && bytes.Equal(raw[1:4], dis[:]) && bytes.Equal(raw[4:8], prefix[:]) {
			return name, raw[8:], true
		}
	}
	return "", nil, false
}
func scan(raw []byte) ([]Field, error) {
	var fields []Field
	for len(raw) > 0 {
		if len(fields) >= 65536 {
			return nil, fmt.Errorf("Amino field count exceeds offline decoder limit")
		}
		original := raw
		key, n := binary.Uvarint(raw)
		if n <= 0 || key>>3 == 0 {
			return nil, fmt.Errorf("invalid Amino field key")
		}
		raw = raw[n:]
		f := Field{Number: key >> 3, Wire: key & 7}
		switch f.Wire {
		case 0:
			v, n := binary.Uvarint(raw)
			if n <= 0 {
				return nil, fmt.Errorf("invalid Amino integer")
			}
			f.Uint = v
			raw = raw[n:]
		case 1:
			if len(raw) < 8 {
				return nil, fmt.Errorf("truncated Amino fixed64")
			}
			f.Value = raw[:8]
			raw = raw[8:]
		case 2:
			l, n := binary.Uvarint(raw)
			if n <= 0 || l > uint64(len(raw)-n) {
				return nil, fmt.Errorf("invalid Amino field length")
			}
			f.Value = raw[n : n+int(l)]
			raw = raw[n+int(l):]
		case 5:
			if len(raw) < 4 {
				return nil, fmt.Errorf("truncated Amino fixed32")
			}
			f.Value = raw[:4]
			raw = raw[4:]
		default:
			return nil, fmt.Errorf("unsupported Amino wire type")
		}
		f.Raw = original[:len(original)-len(raw)]
		fields = append(fields, f)
	}
	return fields, nil
}
func schema(fields []Field, types map[uint64]uint64, repeated ...uint64) error {
	seen := map[uint64]bool{}
	for _, f := range fields {
		wire, ok := types[f.Number]
		if !ok || wire != f.Wire {
			return fmt.Errorf("unsupported Amino schema field")
		}
		repeat := false
		for _, r := range repeated {
			if f.Number == r {
				repeat = true
			}
		}
		if seen[f.Number] && !repeat {
			return fmt.Errorf("duplicate Amino field")
		}
		seen[f.Number] = true
	}
	return nil
}
func value(fields []Field, num uint64) []byte {
	for _, f := range fields {
		if f.Number == num {
			return f.Value
		}
	}
	return nil
}
func uintValue(fields []Field, num uint64) uint64 {
	for _, f := range fields {
		if f.Number == num {
			return f.Uint
		}
	}
	return 0
}
func coin(raw []byte) (sdk.Coin, error) {
	fields, err := scan(raw)
	if err != nil {
		return sdk.Coin{}, err
	}
	if err = schema(fields, map[uint64]uint64{1: 2, 2: 2}); err != nil {
		return sdk.Coin{}, err
	}
	denom, amount := string(value(fields, 1)), string(value(fields, 2))
	// sdk.Int's Amino representation is a decimal string, not a varint.
	number, ok := sdkmath.NewIntFromString(amount)
	if !ok || number.String() != amount || denom == "" || !utf8.ValidString(denom) {
		return sdk.Coin{}, fmt.Errorf("invalid Amino coin")
	}
	return sdk.Coin{Denom: denom, Amount: number}, nil
}

// coinWithoutDenom reads a staking amount that names no denom. The Hub 3
// staking messages validated only that the amount was positive, so such a
// message could be signed and included; the handler then refused it because
// the denom was not the bond denom. The projection keeps the empty denom as
// signed. See AmountWithoutDenom.
func coinWithoutDenom(raw []byte) (sdk.Coin, error) {
	fields, err := scan(raw)
	if err != nil {
		return sdk.Coin{}, err
	}
	if err = schema(fields, map[uint64]uint64{2: 2}); err != nil || len(fields) != 1 {
		return sdk.Coin{}, fmt.Errorf("invalid Amino coin")
	}
	amount := string(fields[0].Value)
	number, ok := sdkmath.NewIntFromString(amount)
	if !ok || number.String() != amount || !number.IsPositive() {
		return sdk.Coin{}, fmt.Errorf("invalid Amino coin")
	}
	return sdk.Coin{Amount: number}, nil
}
func coins(fields []Field, num uint64) (sdk.Coins, error) {
	var result sdk.Coins
	for _, f := range fields {
		if f.Number == num {
			c, err := coin(f.Value)
			if err != nil {
				return nil, err
			}
			result = append(result, c)
		}
	}
	return result, nil
}
func decodeFee(raw []byte) (sdk.Coins, uint64, error) {
	fields, err := scan(raw)
	if err != nil {
		return nil, 0, err
	}
	if err = schema(fields, map[uint64]uint64{1: 2, 2: 0}, 1); err != nil {
		return nil, 0, err
	}
	var amount sdk.Coins
	for _, f := range fields {
		if f.Number != 1 {
			continue
		}
		c, err := coin(f.Value)
		if err != nil {
			if c, err = zeroFeeCoinWithoutDenom(f.Value); err != nil {
				return nil, 0, err
			}
		}
		amount = append(amount, c)
	}
	return amount, uintValue(fields, 2), nil
}

// zeroFeeCoinWithoutDenom accepts the one malformed fee coin the Hub 1 to 3
// chains accepted: amount "0" with no denom. Their ante handlers skipped fee
// deduction, and with it coin validation, whenever every fee coin was zero,
// so such a transaction ran and paid nothing. Any other malformed coin stays
// an error.
func zeroFeeCoinWithoutDenom(raw []byte) (sdk.Coin, error) {
	fields, err := scan(raw)
	if err != nil {
		return sdk.Coin{}, err
	}
	if len(fields) != 1 || fields[0].Number != 2 || string(fields[0].Value) != "0" {
		return sdk.Coin{}, fmt.Errorf("invalid Amino coin")
	}
	return sdk.Coin{Amount: sdkmath.ZeroInt()}, nil
}
func decodeSignature(raw []byte) (Signature, error) {
	fields, err := scan(raw)
	if err != nil {
		return Signature{}, err
	}
	sig := Signature{Raw: bytes.Clone(raw), Fields: fields, PublicKeyRaw: bytes.Clone(value(fields, 1)), Signature: bytes.Clone(value(fields, 2))}
	// Historical optional account-number/sequence fields are retained even
	// where not used by a particular era. They are not reconstructed from state.
	if schema(fields, map[uint64]uint64{1: 2, 2: 2, 3: 0, 4: 0}) != nil {
		sig.Issue = "unsupported Amino signature field"
		return sig, nil
	}
	if len(sig.PublicKeyRaw) == 0 {
		sig.Issue = "public key absent"
		return sig, nil
	}
	address, err := publicKeyAddress(sig.PublicKeyRaw, 0)
	if err != nil {
		sig.Issue = "unsupported or malformed Amino public key"
		return sig, nil
	}
	sig.Address = address
	return sig, nil
}

// publicKeyAddress uses the historical key representation directly. In
// particular, a threshold multisig address hashes the aggregate Amino key,
// never the addresses of its constituent keys. Modern SDK Any unpacking is
// intentionally avoided for this pre-protobuf structure.
func publicKeyAddress(raw []byte, depth int) (sdk.AccAddress, error) {
	failure := fmt.Errorf("unsupported Amino public key")
	if depth > 16 {
		return nil, failure
	}
	name, body, ok := identify(raw, []string{"tendermint/PubKeySecp256k1", "tendermint/PubKeyEd25519", "tendermint/PubKeyMultisigThreshold"})
	if !ok || len(raw) < 4 || raw[0] == 0 {
		return nil, failure
	}
	if name != "tendermint/PubKeyMultisigThreshold" {
		size, n := binary.Uvarint(body)
		if n <= 0 || size != uint64(len(body)-n) {
			return nil, failure
		}
		key := body[n:]
		if name == "tendermint/PubKeySecp256k1" && len(key) == 33 {
			return bytes.Clone((&secp256k1.PubKey{Key: key}).Address()), nil
		}
		if name == "tendermint/PubKeyEd25519" && len(key) == 32 {
			return bytes.Clone((&ed25519.PubKey{Key: key}).Address()), nil
		}
		return nil, failure
	}
	fields, err := scan(body)
	if err != nil || schema(fields, map[uint64]uint64{1: 0, 2: 2}, 2) != nil {
		return nil, failure
	}
	count := uint64(0)
	for _, field := range fields {
		if field.Number == 2 {
			count++
			if _, err := publicKeyAddress(field.Value, depth+1); err != nil {
				return nil, failure
			}
		}
	}
	threshold := uintValue(fields, 1)
	if threshold == 0 || threshold > count {
		return nil, failure
	}
	hash := sha256.Sum256(raw)
	return bytes.Clone(hash[:20]), nil
}

var messageNames = []string{
	"cosmos-sdk/MsgSend", "cosmos-sdk/MsgMultiSend", "cosmos-sdk/MsgDelegate", "cosmos-sdk/MsgUndelegate", "cosmos-sdk/MsgBeginRedelegate",
	"cosmos-sdk/MsgWithdrawDelegationReward", "cosmos-sdk/MsgWithdrawValidatorCommission", "cosmos-sdk/MsgModifyWithdrawAddress",
	"cosmos-sdk/MsgDeposit", "cosmos-sdk/MsgVote", "cosmos-sdk/MsgSubmitProposal", "cosmos-sdk/MsgCreateValidator", "cosmos-sdk/MsgEditValidator", "cosmos-sdk/MsgUnjail", "cosmos-sdk/MsgVerifyInvariant",
}

func decodeMessage(chain string, raw []byte) (Message, error) {
	if len(raw) < 4 {
		return Message{}, fmt.Errorf("truncated Amino message prefix")
	}
	prefixLength := 4
	if raw[0] == 0 {
		prefixLength = 8
	}
	if len(raw) < prefixLength {
		return Message{}, fmt.Errorf("truncated Amino disambiguation prefix")
	}
	msg := Message{Raw: bytes.Clone(raw), Prefix: hex.EncodeToString(raw[:prefixLength])}
	name, body, known := identify(raw, messageNames)
	if !known {
		msg.Issue = "unsupported Amino message prefix"
		return msg, nil
	}
	msg.AminoName = name
	fields, err := scan(body)
	if err != nil {
		return Message{}, err
	}
	msg.Fields = fields
	derived, signers, err := derive(chain, name, fields)
	if errors.Is(err, errShareValued) {
		msg.Issue = shareValuedIssue
		msg.Signers = signers
		return msg, nil
	}
	if err != nil {
		msg.Issue = "unsupported or malformed Amino message schema"
		return msg, nil
	}
	msg.Derived = derived
	msg.Signers = signers
	switch m := derived.(type) {
	case *staking.MsgDelegate:
		msg.AmountWithoutDenom = m.Amount.Denom == ""
	case *staking.MsgUndelegate:
		msg.AmountWithoutDenom = m.Amount.Denom == ""
	case *staking.MsgBeginRedelegate:
		msg.AmountWithoutDenom = m.Amount.Denom == ""
	}
	msg.DerivedTypeURL = sdk.MsgTypeURL(derived)
	return msg, nil
}
func derive(chain, name string, f []Field) (sdk.Msg, []sdk.AccAddress, error) {
	failure := fmt.Errorf("unsupported Amino message projection")
	account := func(num uint64) (string, error) {
		b := value(f, num)
		if len(b) != 20 {
			return "", failure
		}
		return sdk.Bech32ifyAddressBytes("cosmos", b)
	}
	validator := func(num uint64) (string, error) {
		b := value(f, num)
		if len(b) != 20 {
			return "", failure
		}
		return sdk.Bech32ifyAddressBytes("cosmosvaloper", b)
	}
	var derived sdk.Msg
	var signers []sdk.AccAddress
	switch name {
	case "cosmos-sdk/MsgSend":
		if schema(f, map[uint64]uint64{1: 2, 2: 2, 3: 2}, 3) != nil {
			return nil, nil, failure
		}
		from, e := account(1)
		if e != nil {
			return nil, nil, e
		}
		to, e := account(2)
		if e != nil {
			return nil, nil, e
		}
		amount, e := coins(f, 3)
		if e != nil {
			return nil, nil, e
		}
		derived = &bank.MsgSend{FromAddress: from, ToAddress: to, Amount: amount}
		signers = []sdk.AccAddress{bytes.Clone(value(f, 1))}
	case "cosmos-sdk/MsgMultiSend":
		if schema(f, map[uint64]uint64{1: 2, 2: 2}, 1, 2) != nil {
			return nil, nil, failure
		}
		m := &bank.MsgMultiSend{}
		for _, field := range f {
			nested, e := scan(field.Value)
			if e != nil || schema(nested, map[uint64]uint64{1: 2, 2: 2}, 2) != nil {
				return nil, nil, failure
			}
			a := value(nested, 1)
			if len(a) != 20 {
				return nil, nil, failure
			}
			encoded, e := sdk.Bech32ifyAddressBytes("cosmos", a)
			if e != nil {
				return nil, nil, e
			}
			amount, e := coins(nested, 2)
			if e != nil {
				return nil, nil, e
			}
			if field.Number == 1 {
				m.Inputs = append(m.Inputs, bank.Input{Address: encoded, Coins: amount})
				if !contains(signers, a) {
					signers = append(signers, bytes.Clone(a))
				}
			} else {
				m.Outputs = append(m.Outputs, bank.Output{Address: encoded, Coins: amount})
			}
		}
		derived = m
	case "cosmos-sdk/MsgDelegate", "cosmos-sdk/MsgUndelegate", "cosmos-sdk/MsgBeginRedelegate":
		shape := map[uint64]uint64{1: 2, 2: 2, 3: 2}
		amountNum := uint64(3)
		if name == "cosmos-sdk/MsgBeginRedelegate" {
			shape[4] = 2
			amountNum = 4
		}
		if schema(f, shape) != nil {
			return nil, nil, failure
		}
		delegator, e := account(1)
		if e != nil {
			return nil, nil, e
		}
		val, e := validator(2)
		if e != nil {
			return nil, nil, e
		}
		if chain == "cosmoshub-1" && name != "cosmos-sdk/MsgDelegate" {
			// Hub 1 undelegated and redelegated a share quantity, not coins.
			// Turning shares into coins needs the validator's exchange rate
			// at that height, which is state, so no amount is projected.
			if name == "cosmos-sdk/MsgBeginRedelegate" {
				if _, e := validator(3); e != nil {
					return nil, nil, e
				}
			}
			if _, ok := decimal(value(f, amountNum)); !ok {
				return nil, nil, failure
			}
			return nil, []sdk.AccAddress{bytes.Clone(value(f, 1))}, errShareValued
		}
		amount, e := coin(value(f, amountNum))
		if e != nil && chain == "cosmoshub-3" {
			amount, e = coinWithoutDenom(value(f, amountNum))
		}
		if e != nil {
			return nil, nil, e
		}
		switch name {
		case "cosmos-sdk/MsgDelegate":
			derived = &staking.MsgDelegate{DelegatorAddress: delegator, ValidatorAddress: val, Amount: amount}
		case "cosmos-sdk/MsgUndelegate":
			derived = &staking.MsgUndelegate{DelegatorAddress: delegator, ValidatorAddress: val, Amount: amount}
		default:
			dst, e := validator(3)
			if e != nil {
				return nil, nil, e
			}
			derived = &staking.MsgBeginRedelegate{DelegatorAddress: delegator, ValidatorSrcAddress: val, ValidatorDstAddress: dst, Amount: amount}
		}
		signers = []sdk.AccAddress{bytes.Clone(value(f, 1))}
	case "cosmos-sdk/MsgWithdrawDelegationReward", "cosmos-sdk/MsgModifyWithdrawAddress":
		if schema(f, map[uint64]uint64{1: 2, 2: 2}) != nil {
			return nil, nil, failure
		}
		delegator, e := account(1)
		if e != nil {
			return nil, nil, e
		}
		if name == "cosmos-sdk/MsgWithdrawDelegationReward" {
			v, e := validator(2)
			if e != nil {
				return nil, nil, e
			}
			derived = &distribution.MsgWithdrawDelegatorReward{DelegatorAddress: delegator, ValidatorAddress: v}
		} else {
			v, e := account(2)
			if e != nil {
				return nil, nil, e
			}
			derived = &distribution.MsgSetWithdrawAddress{DelegatorAddress: delegator, WithdrawAddress: v}
		}
		signers = []sdk.AccAddress{bytes.Clone(value(f, 1))}
	case "cosmos-sdk/MsgWithdrawValidatorCommission", "cosmos-sdk/MsgUnjail":
		if schema(f, map[uint64]uint64{1: 2}) != nil {
			return nil, nil, failure
		}
		v, e := validator(1)
		if e != nil {
			return nil, nil, e
		}
		if name == "cosmos-sdk/MsgUnjail" {
			derived = &slashing.MsgUnjail{ValidatorAddr: v}
		} else {
			derived = &distribution.MsgWithdrawValidatorCommission{ValidatorAddress: v}
		}
		signers = []sdk.AccAddress{bytes.Clone(value(f, 1))}
	case "cosmos-sdk/MsgDeposit":
		if schema(f, map[uint64]uint64{1: 0, 2: 2, 3: 2}, 3) != nil {
			return nil, nil, failure
		}
		depositor, e := account(2)
		if e != nil {
			return nil, nil, e
		}
		amount, e := coins(f, 3)
		if e != nil {
			return nil, nil, e
		}
		derived = &gov.MsgDeposit{ProposalId: uintValue(f, 1), Depositor: depositor, Amount: amount}
		signers = []sdk.AccAddress{bytes.Clone(value(f, 2))}
	default:
		return deriveGoverning(chain, name, f)
	}
	return derived, signers, nil
}

// InclusionKey is the identity for an eventual staging store. A hash alone is
// neither cross-chain identity nor identity for repeated inclusion in a block.
// It deliberately does not change the serving DB's global hash constraint.
func InclusionKey(chain string, height int64, index int) (string, error) {
	if chain != "cosmoshub-1" && chain != "cosmoshub-2" && chain != "cosmoshub-3" || height <= 0 || index < 0 {
		return "", fmt.Errorf("invalid legacy inclusion identity")
	}
	return chain + "/" + strconv.FormatInt(height, 10) + "/" + strconv.Itoa(index), nil
}
