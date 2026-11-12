package legacy

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// The bytes a Hub 1 to 3 signer signed are the sorted Amino JSON of the
// account number, chain id, fee, memo, messages and sequence. They are rebuilt
// here from the stored binary fields with one table per message, so a
// signature that verifies proves every field of the transaction was read as
// the signer wrote it, and proves the sequence number the signer used.

type signKind uint8

const (
	signAccount signKind = iota
	signValidator
	signText
	signUint
	signInt
	signOptionalInt
	signDec
	signCoin
	signCoins
	signStruct
	signStructs
	signConsensusKey
	signVote
	signProposalKind
	signContent
)

type signField struct {
	num  uint64
	name string
	kind signKind
	sub  []signField
	omit bool
}

var (
	signCoinFields = []signField{{num: 1, name: "denom", kind: signText}, {num: 2, name: "amount", kind: signInt}}
	signFeeFields  = []signField{{num: 1, name: "amount", kind: signCoins}, {num: 2, name: "gas", kind: signUint}}
	signTransfer   = []signField{{num: 1, name: "address", kind: signAccount}, {num: 2, name: "coins", kind: signCoins}}
	signDescribed  = []signField{{num: 1, name: "moniker", kind: signText}, {num: 2, name: "identity", kind: signText}, {num: 3, name: "website", kind: signText}, {num: 4, name: "details", kind: signText}}
	signRates      = []signField{{num: 1, name: "rate", kind: signDec}, {num: 2, name: "max_rate", kind: signDec}, {num: 3, name: "max_change_rate", kind: signDec}}
	signTitled     = []signField{{num: 1, name: "title", kind: signText}, {num: 2, name: "description", kind: signText}}
	signContents   = map[string][]signField{
		"cosmos-sdk/TextProposal":            signTitled,
		"cosmos-sdk/SoftwareUpgradeProposal": signTitled,
		"cosmos-sdk/ParameterChangeProposal": append(append([]signField{}, signTitled...), signField{num: 3, name: "changes", kind: signStructs, sub: []signField{
			{num: 1, name: "subspace", kind: signText}, {num: 2, name: "key", kind: signText}, {num: 3, name: "subkey", kind: signText, omit: true}, {num: 4, name: "value", kind: signText}}}),
		"cosmos-sdk/CommunityPoolSpendProposal": append(append([]signField{}, signTitled...), signField{num: 3, name: "recipient", kind: signAccount}, signField{num: 4, name: "amount", kind: signCoins}),
	}
)

func signMessageFields(chain, name string) []signField {
	first := chain == "cosmoshub-1"
	switch name {
	case "cosmos-sdk/MsgSend":
		return []signField{{num: 1, name: "from_address", kind: signAccount}, {num: 2, name: "to_address", kind: signAccount}, {num: 3, name: "amount", kind: signCoins}}
	case "cosmos-sdk/MsgMultiSend":
		return []signField{{num: 1, name: "inputs", kind: signStructs, sub: signTransfer}, {num: 2, name: "outputs", kind: signStructs, sub: signTransfer}}
	case "cosmos-sdk/MsgDelegate", "cosmos-sdk/MsgUndelegate":
		amount := signField{num: 3, name: "amount", kind: signCoin}
		if first && name == "cosmos-sdk/MsgDelegate" {
			amount.name = "value"
		} else if first {
			amount = signField{num: 3, name: "shares_amount", kind: signDec}
		}
		return []signField{{num: 1, name: "delegator_address", kind: signAccount}, {num: 2, name: "validator_address", kind: signValidator}, amount}
	case "cosmos-sdk/MsgBeginRedelegate":
		amount := signField{num: 4, name: "amount", kind: signCoin}
		if first {
			amount = signField{num: 4, name: "shares_amount", kind: signDec}
		}
		return []signField{{num: 1, name: "delegator_address", kind: signAccount}, {num: 2, name: "validator_src_address", kind: signValidator}, {num: 3, name: "validator_dst_address", kind: signValidator}, amount}
	case "cosmos-sdk/MsgWithdrawDelegationReward":
		return []signField{{num: 1, name: "delegator_address", kind: signAccount}, {num: 2, name: "validator_address", kind: signValidator}}
	case "cosmos-sdk/MsgModifyWithdrawAddress":
		return []signField{{num: 1, name: "delegator_address", kind: signAccount}, {num: 2, name: "withdraw_address", kind: signAccount}}
	case "cosmos-sdk/MsgWithdrawValidatorCommission":
		return []signField{{num: 1, name: "validator_address", kind: signValidator}}
	case "cosmos-sdk/MsgUnjail":
		return []signField{{num: 1, name: "address", kind: signValidator}}
	case "cosmos-sdk/MsgDeposit":
		return []signField{{num: 1, name: "proposal_id", kind: signUint}, {num: 2, name: "depositor", kind: signAccount}, {num: 3, name: "amount", kind: signCoins}}
	case "cosmos-sdk/MsgVote":
		return []signField{{num: 1, name: "proposal_id", kind: signUint}, {num: 2, name: "voter", kind: signAccount}, {num: 3, name: "option", kind: signVote}}
	case "cosmos-sdk/MsgSubmitProposal":
		if chain == "cosmoshub-3" {
			return []signField{{num: 1, name: "content", kind: signContent}, {num: 2, name: "initial_deposit", kind: signCoins}, {num: 3, name: "proposer", kind: signAccount}}
		}
		return []signField{{num: 1, name: "title", kind: signText}, {num: 2, name: "description", kind: signText}, {num: 3, name: "proposal_type", kind: signProposalKind}, {num: 4, name: "proposer", kind: signAccount}, {num: 5, name: "initial_deposit", kind: signCoins}}
	case "cosmos-sdk/MsgCreateValidator":
		return []signField{{num: 1, name: "description", kind: signStruct, sub: signDescribed}, {num: 2, name: "commission", kind: signStruct, sub: signRates}, {num: 3, name: "min_self_delegation", kind: signInt},
			{num: 4, name: "delegator_address", kind: signAccount}, {num: 5, name: "validator_address", kind: signValidator}, {num: 6, name: "pubkey", kind: signConsensusKey}, {num: 7, name: "value", kind: signCoin}}
	case "cosmos-sdk/MsgEditValidator":
		// The description is an embedded struct, which Amino JSON names by its type.
		return []signField{{num: 1, name: "Description", kind: signStruct, sub: signDescribed}, {num: 2, name: "address", kind: signValidator}, {num: 3, name: "commission_rate", kind: signDec}, {num: 4, name: "min_self_delegation", kind: signOptionalInt}}
	case "cosmos-sdk/MsgVerifyInvariant":
		return []signField{{num: 1, name: "sender", kind: signAccount}, {num: 2, name: "invariant_module_name", kind: signText}, {num: 3, name: "invariant_route", kind: signText}}
	}
	return nil
}

var errSignBytes = fmt.Errorf("sign bytes not reconstructable")

func signNumber(raw []byte) (string, bool) {
	text := string(raw)
	digits := strings.TrimPrefix(text, "-")
	if digits == "" || len(digits) > 80 || (len(digits) > 1 && digits[0] == '0') || (digits == "0" && text != "0") {
		return "", false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return text, true
}

func signBech32(prefix string, raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	return sdk.Bech32ifyAddressBytes(prefix, raw)
}

func signValue(fields []Field, specs []signField) (map[string]any, error) {
	out := map[string]any{}
	for _, f := range fields {
		known := false
		for _, s := range specs {
			if s.num != f.Number {
				continue
			}
			known = true
			wire := uint64(2)
			if s.kind == signUint || s.kind == signVote || s.kind == signProposalKind {
				wire = 0
			}
			if f.Wire != wire {
				return nil, errSignBytes
			}
		}
		if !known {
			return nil, errSignBytes
		}
	}
	for _, s := range specs {
		var found []Field
		for _, f := range fields {
			if f.Number == s.num {
				found = append(found, f)
			}
		}
		if len(found) > 1 && s.kind != signCoins && s.kind != signStructs {
			return nil, errSignBytes
		}
		var raw []byte
		var number uint64
		if len(found) == 1 {
			raw, number = found[0].Value, found[0].Uint
		}
		var v any
		var err error
		switch s.kind {
		case signAccount:
			v, err = signBech32("cosmos", raw)
		case signValidator:
			v, err = signBech32("cosmosvaloper", raw)
		case signConsensusKey:
			if len(raw) == 0 {
				return nil, errSignBytes
			}
			v, err = signBech32("cosmosvalconspub", raw)
		case signText:
			if s.omit && len(raw) == 0 {
				continue
			}
			v = string(raw)
		case signUint:
			v = strconv.FormatUint(number, 10)
		case signVote:
			v = map[uint64]string{1: "Yes", 2: "Abstain", 3: "No", 4: "NoWithVeto"}[number]
		case signProposalKind:
			v = map[uint64]string{1: "Text", 2: "ParameterChange", 3: "SoftwareUpgrade"}[number]
		case signInt, signOptionalInt:
			if len(found) == 0 {
				if s.kind == signInt {
					v = "0"
				}
				break
			}
			text, ok := signNumber(raw)
			if !ok {
				return nil, errSignBytes
			}
			v = text
		case signDec:
			if len(found) == 0 {
				break
			}
			text, ok := signNumber(raw)
			if !ok {
				return nil, errSignBytes
			}
			digits := strings.TrimPrefix(text, "-")
			if len(digits) <= sdk.Precision {
				digits = strings.Repeat("0", sdk.Precision-len(digits)+1) + digits
			}
			digits = digits[:len(digits)-sdk.Precision] + "." + digits[len(digits)-sdk.Precision:]
			if strings.HasPrefix(text, "-") {
				digits = "-" + digits
			}
			v = digits
		case signCoin, signStruct:
			sub := s.sub
			if s.kind == signCoin {
				sub = signCoinFields
			}
			nested, e := scan(raw)
			if e != nil {
				return nil, errSignBytes
			}
			v, err = signValue(nested, sub)
		case signCoins, signStructs:
			sub := s.sub
			if s.kind == signCoins {
				sub = signCoinFields
			}
			var list []any
			for _, f := range found {
				nested, e := scan(f.Value)
				if e != nil {
					return nil, errSignBytes
				}
				item, e := signValue(nested, sub)
				if e != nil {
					return nil, e
				}
				list = append(list, item)
			}
			if list != nil {
				v = list
			}
		case signContent:
			if len(found) == 0 {
				break
			}
			names := make([]string, 0, len(signContents))
			for name := range signContents {
				names = append(names, name)
			}
			name, body, ok := identify(raw, names)
			if !ok {
				return nil, errSignBytes
			}
			nested, e := scan(body)
			if e != nil {
				return nil, errSignBytes
			}
			inner, e := signValue(nested, signContents[name])
			if e != nil {
				return nil, e
			}
			v = map[string]any{"type": name, "value": inner}
		}
		if err != nil {
			return nil, errSignBytes
		}
		out[s.name] = v
	}
	return out, nil
}

// SignDoc is a transaction's sign bytes with the account number and the
// sequence left open.
type SignDoc struct{ middle []byte }

// SignDoc rebuilds what the signers of the transaction signed. It fails for
// any message or field it cannot reproduce exactly.
func (tx *Tx) SignDoc(chain string) (*SignDoc, error) {
	feeFields, err := scan(tx.FeeRaw)
	if err != nil {
		return nil, errSignBytes
	}
	fee, err := signValue(feeFields, signFeeFields)
	if err != nil {
		return nil, err
	}
	if fee["amount"] == nil {
		fee["amount"] = []any{}
	}
	var msgs []any
	for _, msg := range tx.Messages {
		specs := signMessageFields(chain, msg.AminoName)
		if specs == nil {
			return nil, errSignBytes
		}
		inner, err := signValue(msg.Fields, specs)
		if err != nil {
			return nil, err
		}
		// Until Hub 3 the slashing module signed with a codec that had no
		// names registered, so an unjail was signed as its bare value.
		if msg.AminoName == "cosmos-sdk/MsgUnjail" && chain != "cosmoshub-3" {
			msgs = append(msgs, inner)
			continue
		}
		msgs = append(msgs, map[string]any{"type": msg.AminoName, "value": inner})
	}
	doc := map[string]any{"chain_id": chain, "fee": fee, "memo": string(tx.Memo), "msgs": nil}
	if msgs != nil {
		doc["msgs"] = msgs
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, errSignBytes
	}
	return &SignDoc{middle: encoded[1 : len(encoded)-1]}, nil
}

// Bytes returns the sign bytes for one account number and sequence.
func (d *SignDoc) Bytes(account, sequence uint64) []byte {
	out := make([]byte, 0, len(d.middle)+80)
	out = append(out, `{"account_number":"`...)
	out = strconv.AppendUint(out, account, 10)
	out = append(out, `",`...)
	out = append(out, d.middle...)
	out = append(out, `,"sequence":"`...)
	out = strconv.AppendUint(out, sequence, 10)
	return append(out, `"}`...)
}

// VerifySignature reports whether an Amino public key signed the message. It
// accepts secp256k1, ed25519 and threshold multisignature keys as the Hub 1 to
// 3 ante handler did.
func VerifySignature(publicKey, signature, message []byte) bool {
	return verifySignature(publicKey, signature, message, 0)
}

func verifySignature(publicKey, signature, message []byte, depth int) bool {
	name, body, ok := identify(publicKey, []string{"tendermint/PubKeySecp256k1", "tendermint/PubKeyEd25519", "tendermint/PubKeyMultisigThreshold"})
	if !ok || depth > 16 || len(publicKey) < 4 || publicKey[0] == 0 {
		return false
	}
	if name == "tendermint/PubKeyMultisigThreshold" {
		keyFields, err := scan(body)
		if err != nil || schema(keyFields, map[uint64]uint64{1: 0, 2: 2}, 2) != nil {
			return false
		}
		var keys [][]byte
		for _, f := range keyFields {
			if f.Number == 2 {
				keys = append(keys, f.Value)
			}
		}
		sigFields, err := scan(signature)
		if err != nil || schema(sigFields, map[uint64]uint64{1: 2, 2: 2}, 2) != nil {
			return false
		}
		bits, err := scan(value(sigFields, 1))
		if err != nil || schema(bits, map[uint64]uint64{1: 0, 2: 2}) != nil {
			return false
		}
		elems, extra := value(bits, 2), uintValue(bits, 1)
		size := uint64(len(elems)) * 8
		if extra != 0 {
			if len(elems) == 0 || extra > 7 {
				return false
			}
			size = uint64(len(elems)-1)*8 + extra
		}
		var sigs [][]byte
		for _, f := range sigFields {
			if f.Number == 2 {
				sigs = append(sigs, f.Value)
			}
		}
		threshold := uintValue(keyFields, 1)
		if size != uint64(len(keys)) || threshold == 0 || uint64(len(sigs)) < threshold || len(sigs) > len(keys) {
			return false
		}
		next := 0
		for i := range keys {
			if elems[i>>3]&(1<<(7-uint(i)%8)) == 0 {
				continue
			}
			if next >= len(sigs) || !verifySignature(keys[i], sigs[next], message, depth+1) {
				return false
			}
			next++
		}
		return next == len(sigs)
	}
	size, n := binary.Uvarint(body)
	if n <= 0 || size != uint64(len(body)-n) {
		return false
	}
	key := body[n:]
	if name == "tendermint/PubKeyEd25519" {
		return len(key) == ed25519.PublicKeySize && ed25519.Verify(ed25519.PublicKey(key), message, signature)
	}
	if len(key) != 33 || len(signature) != 64 {
		return false
	}
	parsed, err := secp256k1.ParsePubKey(key)
	if err != nil {
		return false
	}
	var r, s secp256k1.ModNScalar
	if r.SetByteSlice(signature[:32]) || s.SetByteSlice(signature[32:]) {
		return false
	}
	hash := sha256.Sum256(message)
	return ecdsa.NewSignature(&r, &s).Verify(hash[:], parsed)
}
