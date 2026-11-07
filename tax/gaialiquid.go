// Gaia's x/liquid module (the Liquid Staking Module) ships with Cosmos Hub but
// its Go types live in github.com/cosmos/gaia, which needs cosmos-sdk v0.50+.
// This indexer is on v0.47, so those messages are absent from the codec and
// every one of them used to fail to decode.
//
// Rather than drag in an incompatible SDK, we declare the wire shape ourselves
// and register it under the real type URLs. Field numbers are taken from
// gaia/proto/gaia/liquid/v1beta1/tx.proto. Only the fields the tax layer needs
// are typed; a nested Coin is kept as its raw length-delimited bytes, because
// tokenizing and redeeming shares are conversions rather than disposals, so no
// taxable event depends on the amount.
//
// The two reward messages matter: they are ATOM income. Like the distribution
// module's withdraw, the amount is not on the message at all, it comes from the
// transaction's withdraw_tokenize_share_reward events, so decoding the owner address is enough
// to classify them.

package tax

import (
	"encoding/binary"
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

// Type URLs as they appear on chain. These are public protobuf identifiers, not credentials.
// #nosec G101 -- tokenization message identifiers are public on-chain type URLs.
const (
	TypeURLTokenizeShares                 = "/gaia.liquid.v1beta1.MsgTokenizeShares"
	TypeURLRedeemTokensForShares          = "/gaia.liquid.v1beta1.MsgRedeemTokensForShares"
	TypeURLTransferTokenizeShareRecord    = "/gaia.liquid.v1beta1.MsgTransferTokenizeShareRecord"
	TypeURLDisableTokenizeShares          = "/gaia.liquid.v1beta1.MsgDisableTokenizeShares"
	TypeURLEnableTokenizeShares           = "/gaia.liquid.v1beta1.MsgEnableTokenizeShares"
	TypeURLWithdrawTokenizeShareReward    = "/gaia.liquid.v1beta1.MsgWithdrawTokenizeShareRecordReward"
	TypeURLWithdrawAllTokenizeShareReward = "/gaia.liquid.v1beta1.MsgWithdrawAllTokenizeShareRecordReward"
)

// GaiaLiquidMsgTypes maps every x/liquid type URL to a registrable instance.
// taxindexer's main() hands this to RegisterCustomMsgTypesByTypeURLs.
func GaiaLiquidMsgTypes() map[string]sdk.Msg {
	return map[string]sdk.Msg{
		TypeURLTokenizeShares:                 &MsgTokenizeShares{},
		TypeURLRedeemTokensForShares:          &MsgRedeemTokensForShares{},
		TypeURLTransferTokenizeShareRecord:    &MsgTransferTokenizeShareRecord{},
		TypeURLDisableTokenizeShares:          &MsgDisableTokenizeShares{},
		TypeURLEnableTokenizeShares:           &MsgEnableTokenizeShares{},
		TypeURLWithdrawTokenizeShareReward:    &MsgWithdrawTokenizeShareRecordReward{},
		TypeURLWithdrawAllTokenizeShareReward: &MsgWithdrawAllTokenizeShareRecordReward{},
	}
}

// LegacyLSMMsgTypes covers the pre-v24 SDK fork without registering the same Go
// type under two URLs (the registry's reverse mapping must remain unambiguous).
// Wire shapes: cosmos/cosmos-sdk tag v0.50.11-lsm, proto/cosmos/{staking,
// distribution}/v1beta1/tx.proto. Gaia v23.3.0 go.mod pins that exact fork.
func LegacyLSMMsgTypes() map[string]sdk.Msg {
	return map[string]sdk.Msg{
		LegacyTypeURLTokenizeShares:                 &LegacyMsgTokenizeShares{},
		LegacyTypeURLRedeemTokensForShares:          &LegacyMsgRedeemTokensForShares{},
		LegacyTypeURLTransferTokenizeShareRecord:    &LegacyMsgTransferTokenizeShareRecord{},
		LegacyTypeURLDisableTokenizeShares:          &LegacyMsgDisableTokenizeShares{},
		LegacyTypeURLEnableTokenizeShares:           &LegacyMsgEnableTokenizeShares{},
		LegacyTypeURLValidatorBond:                  &LegacyMsgValidatorBond{},
		LegacyTypeURLUnbondValidator:                &LegacyMsgUnbondValidator{},
		LegacyTypeURLWithdrawTokenizeShareReward:    &LegacyMsgWithdrawTokenizeShareRecordReward{},
		LegacyTypeURLWithdrawAllTokenizeShareReward: &LegacyMsgWithdrawAllTokenizeShareRecordReward{},
	}
}

// #nosec G101 -- legacy tokenization identifiers are public on-chain type URLs.
const (
	LegacyTypeURLTokenizeShares                 = "/cosmos.staking.v1beta1.MsgTokenizeShares"
	LegacyTypeURLRedeemTokensForShares          = "/cosmos.staking.v1beta1.MsgRedeemTokensForShares"
	LegacyTypeURLTransferTokenizeShareRecord    = "/cosmos.staking.v1beta1.MsgTransferTokenizeShareRecord"
	LegacyTypeURLDisableTokenizeShares          = "/cosmos.staking.v1beta1.MsgDisableTokenizeShares"
	LegacyTypeURLEnableTokenizeShares           = "/cosmos.staking.v1beta1.MsgEnableTokenizeShares"
	LegacyTypeURLValidatorBond                  = "/cosmos.staking.v1beta1.MsgValidatorBond"
	LegacyTypeURLUnbondValidator                = "/cosmos.staking.v1beta1.MsgUnbondValidator"
	LegacyTypeURLWithdrawTokenizeShareReward    = "/cosmos.distribution.v1beta1.MsgWithdrawTokenizeShareRecordReward"
	LegacyTypeURLWithdrawAllTokenizeShareReward = "/cosmos.distribution.v1beta1.MsgWithdrawAllTokenizeShareRecordReward"
)

// The migrated messages have identical numbered fields. Embedding shares only
// wire handling, not Go/protobuf identity. Explicit names also keep Any packing
// independent of gogoproto's global registration table.
type LegacyMsgTokenizeShares struct{ MsgTokenizeShares }

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*LegacyMsgTokenizeShares) XXX_MessageName() string { return LegacyTypeURLTokenizeShares[1:] }

type LegacyMsgRedeemTokensForShares struct{ MsgRedeemTokensForShares }

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*LegacyMsgRedeemTokensForShares) XXX_MessageName() string {
	return LegacyTypeURLRedeemTokensForShares[1:]
}

type LegacyMsgTransferTokenizeShareRecord struct{ MsgTransferTokenizeShareRecord }

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*LegacyMsgTransferTokenizeShareRecord) XXX_MessageName() string {
	return LegacyTypeURLTransferTokenizeShareRecord[1:]
}

type LegacyMsgDisableTokenizeShares struct{ MsgDisableTokenizeShares }

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*LegacyMsgDisableTokenizeShares) XXX_MessageName() string {
	return LegacyTypeURLDisableTokenizeShares[1:]
}

type LegacyMsgEnableTokenizeShares struct{ MsgEnableTokenizeShares }

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*LegacyMsgEnableTokenizeShares) XXX_MessageName() string {
	return LegacyTypeURLEnableTokenizeShares[1:]
}

type LegacyMsgWithdrawTokenizeShareRecordReward struct {
	MsgWithdrawTokenizeShareRecordReward
}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*LegacyMsgWithdrawTokenizeShareRecordReward) XXX_MessageName() string {
	return LegacyTypeURLWithdrawTokenizeShareReward[1:]
}

type LegacyMsgWithdrawAllTokenizeShareRecordReward struct {
	MsgWithdrawAllTokenizeShareRecordReward
}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*LegacyMsgWithdrawAllTokenizeShareRecordReward) XXX_MessageName() string {
	return LegacyTypeURLWithdrawAllTokenizeShareReward[1:]
}

// ValidatorBond marks an existing delegation as validator bond, not a deposit
// or income. UnbondValidator changes validator status, not delegator balances.
type LegacyMsgValidatorBond struct {
	rawMsg
	DelegatorAddress string `protobuf:"bytes,1,opt,name=delegator_address,json=delegatorAddress,proto3"`
	ValidatorAddress string `protobuf:"bytes,2,opt,name=validator_address,json=validatorAddress,proto3"`
}

func (m *LegacyMsgValidatorBond) Reset()      { *m = LegacyMsgValidatorBond{} }
func (*LegacyMsgValidatorBond) ProtoMessage() {}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*LegacyMsgValidatorBond) XXX_MessageName() string { return LegacyTypeURLValidatorBond[1:] }

func (m *LegacyMsgValidatorBond) String() string {
	return fmt.Sprintf("MsgValidatorBond{%s}", m.DelegatorAddress)
}
func (*LegacyMsgValidatorBond) ValidateBasic() error           { return nil }
func (m *LegacyMsgValidatorBond) GetSigners() []sdk.AccAddress { return signerOf(m.DelegatorAddress) }

func (m *LegacyMsgValidatorBond) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			m.DelegatorAddress = string(f.bytes)
		case 2:
			m.ValidatorAddress = string(f.bytes)
		}
		return nil
	})
}

type LegacyMsgUnbondValidator struct {
	rawMsg
	ValidatorAddress string `protobuf:"bytes,1,opt,name=validator_address,json=validatorAddress,proto3"`
}

func (m *LegacyMsgUnbondValidator) Reset()      { *m = LegacyMsgUnbondValidator{} }
func (*LegacyMsgUnbondValidator) ProtoMessage() {}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*LegacyMsgUnbondValidator) XXX_MessageName() string { return LegacyTypeURLUnbondValidator[1:] }

func (m *LegacyMsgUnbondValidator) String() string {
	return fmt.Sprintf("MsgUnbondValidator{%s}", m.ValidatorAddress)
}
func (*LegacyMsgUnbondValidator) ValidateBasic() error           { return nil }
func (m *LegacyMsgUnbondValidator) GetSigners() []sdk.AccAddress { return signerOf(m.ValidatorAddress) }

func (m *LegacyMsgUnbondValidator) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		if f.num == 1 {
			m.ValidatorAddress = string(f.bytes)
		}
		return nil
	})
}

// rawMsg preserves the exact bytes a message was decoded from.
//
// The interface registry re-marshals every message it unpacks, to cache it back
// into the Any. Left to gogoproto's reflection marshaller that panics on
// hand-written types, so each of these messages marshals by handing back the
// bytes it came from. That is also strictly more faithful than re-encoding:
// fields we chose not to model still survive a round trip.
type rawMsg struct{ raw []byte }

func (r *rawMsg) Marshal() ([]byte, error) { return r.raw, nil }
func (r *rawMsg) Size() int                { return len(r.raw) }
func (r *rawMsg) keep(b []byte)            { r.raw = append([]byte(nil), b...) }

// --- minimal protobuf wire reader -------------------------------------------
//
// gogoproto calls Unmarshal when a type provides it, exactly as it does for
// generated code, so decoding is explicit here rather than left to the
// reflection fallback. Only the wire types x/liquid actually uses are handled;
// anything else is skipped so an added field cannot break decoding.

type wireField struct {
	num    int
	bytes  []byte // length-delimited payload
	varint uint64
}

func scanFields(b []byte, visit func(wireField) error) error {
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return fmt.Errorf("bad field key")
		}
		b = b[n:]
		field := wireField{num: int(key >> 3)}
		switch key & 0x7 {
		case 0: // varint
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return fmt.Errorf("bad varint for field %d", field.num)
			}
			b = b[n:]
			field.varint = v
		case 2: // length-delimited
			l, n := binary.Uvarint(b)
			if n <= 0 {
				return fmt.Errorf("bad length for field %d", field.num)
			}
			b = b[n:]
			if uint64(len(b)) < l {
				return fmt.Errorf("field %d truncated", field.num)
			}
			field.bytes = b[:l]
			b = b[l:]
		case 5: // fixed32
			if len(b) < 4 {
				return fmt.Errorf("field %d truncated", field.num)
			}
			b = b[4:]
			continue
		case 1: // fixed64
			if len(b) < 8 {
				return fmt.Errorf("field %d truncated", field.num)
			}
			b = b[8:]
			continue
		default:
			return fmt.Errorf("unsupported wire type on field %d", field.num)
		}
		if err := visit(field); err != nil {
			return err
		}
	}
	return nil
}

func signerOf(addr string) []sdk.AccAddress {
	a, err := sdk.AccAddressFromBech32(addr)
	if err != nil {
		return nil
	}
	return []sdk.AccAddress{a}
}

// --- MsgTokenizeShares -------------------------------------------------------

type MsgTokenizeShares struct {
	rawMsg
	DelegatorAddress    string `protobuf:"bytes,1,opt,name=delegator_address,json=delegatorAddress,proto3" json:"delegator_address,omitempty"`
	ValidatorAddress    string `protobuf:"bytes,2,opt,name=validator_address,json=validatorAddress,proto3" json:"validator_address,omitempty"`
	AmountRaw           []byte `protobuf:"bytes,3,opt,name=amount,proto3" json:"amount,omitempty"`
	TokenizedShareOwner string `protobuf:"bytes,4,opt,name=tokenized_share_owner,json=tokenizedShareOwner,proto3" json:"tokenized_share_owner,omitempty"`
}

func (m *MsgTokenizeShares) Reset()        { *m = MsgTokenizeShares{} }
func (m *MsgTokenizeShares) ProtoMessage() {}
func (m *MsgTokenizeShares) String() string {
	return fmt.Sprintf("MsgTokenizeShares{%s}", m.DelegatorAddress)
}
func (m *MsgTokenizeShares) ValidateBasic() error         { return nil }
func (m *MsgTokenizeShares) GetSigners() []sdk.AccAddress { return signerOf(m.DelegatorAddress) }
func (m *MsgTokenizeShares) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			m.DelegatorAddress = string(f.bytes)
		case 2:
			m.ValidatorAddress = string(f.bytes)
		case 3:
			m.AmountRaw = append([]byte(nil), f.bytes...)
		case 4:
			m.TokenizedShareOwner = string(f.bytes)
		}
		return nil
	})
}

// --- MsgRedeemTokensForShares ------------------------------------------------

type MsgRedeemTokensForShares struct {
	rawMsg
	DelegatorAddress string `protobuf:"bytes,1,opt,name=delegator_address,json=delegatorAddress,proto3" json:"delegator_address,omitempty"`
	AmountRaw        []byte `protobuf:"bytes,2,opt,name=amount,proto3" json:"amount,omitempty"`
}

func (m *MsgRedeemTokensForShares) Reset()        { *m = MsgRedeemTokensForShares{} }
func (m *MsgRedeemTokensForShares) ProtoMessage() {}
func (m *MsgRedeemTokensForShares) String() string {
	return fmt.Sprintf("MsgRedeemTokensForShares{%s}", m.DelegatorAddress)
}
func (m *MsgRedeemTokensForShares) ValidateBasic() error         { return nil }
func (m *MsgRedeemTokensForShares) GetSigners() []sdk.AccAddress { return signerOf(m.DelegatorAddress) }

func (m *MsgRedeemTokensForShares) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			m.DelegatorAddress = string(f.bytes)
		case 2:
			m.AmountRaw = append([]byte(nil), f.bytes...)
		}
		return nil
	})
}

// --- MsgTransferTokenizeShareRecord ------------------------------------------

type MsgTransferTokenizeShareRecord struct {
	rawMsg
	TokenizeShareRecordID uint64 `protobuf:"varint,1,opt,name=tokenize_share_record_id,json=tokenizeShareRecordId,proto3" json:"tokenize_share_record_id,omitempty"`
	Sender                string `protobuf:"bytes,2,opt,name=sender,proto3" json:"sender,omitempty"`
	NewOwner              string `protobuf:"bytes,3,opt,name=new_owner,json=newOwner,proto3" json:"new_owner,omitempty"`
}

func (m *MsgTransferTokenizeShareRecord) Reset()        { *m = MsgTransferTokenizeShareRecord{} }
func (m *MsgTransferTokenizeShareRecord) ProtoMessage() {}
func (m *MsgTransferTokenizeShareRecord) String() string {
	return fmt.Sprintf("MsgTransferTokenizeShareRecord{%d}", m.TokenizeShareRecordID)
}
func (m *MsgTransferTokenizeShareRecord) ValidateBasic() error         { return nil }
func (m *MsgTransferTokenizeShareRecord) GetSigners() []sdk.AccAddress { return signerOf(m.Sender) }
func (m *MsgTransferTokenizeShareRecord) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			m.TokenizeShareRecordID = f.varint
		case 2:
			m.Sender = string(f.bytes)
		case 3:
			m.NewOwner = string(f.bytes)
		}
		return nil
	})
}

// --- MsgDisableTokenizeShares / MsgEnableTokenizeShares ----------------------

type MsgDisableTokenizeShares struct {
	rawMsg
	DelegatorAddress string `protobuf:"bytes,1,opt,name=delegator_address,json=delegatorAddress,proto3" json:"delegator_address,omitempty"`
}

func (m *MsgDisableTokenizeShares) Reset()        { *m = MsgDisableTokenizeShares{} }
func (m *MsgDisableTokenizeShares) ProtoMessage() {}
func (m *MsgDisableTokenizeShares) String() string {
	return fmt.Sprintf("MsgDisableTokenizeShares{%s}", m.DelegatorAddress)
}
func (m *MsgDisableTokenizeShares) ValidateBasic() error         { return nil }
func (m *MsgDisableTokenizeShares) GetSigners() []sdk.AccAddress { return signerOf(m.DelegatorAddress) }

func (m *MsgDisableTokenizeShares) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		if f.num == 1 {
			m.DelegatorAddress = string(f.bytes)
		}
		return nil
	})
}

type MsgEnableTokenizeShares struct {
	rawMsg
	DelegatorAddress string `protobuf:"bytes,1,opt,name=delegator_address,json=delegatorAddress,proto3" json:"delegator_address,omitempty"`
}

func (m *MsgEnableTokenizeShares) Reset()        { *m = MsgEnableTokenizeShares{} }
func (m *MsgEnableTokenizeShares) ProtoMessage() {}
func (m *MsgEnableTokenizeShares) String() string {
	return fmt.Sprintf("MsgEnableTokenizeShares{%s}", m.DelegatorAddress)
}
func (m *MsgEnableTokenizeShares) ValidateBasic() error         { return nil }
func (m *MsgEnableTokenizeShares) GetSigners() []sdk.AccAddress { return signerOf(m.DelegatorAddress) }

func (m *MsgEnableTokenizeShares) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		if f.num == 1 {
			m.DelegatorAddress = string(f.bytes)
		}
		return nil
	})
}

// --- the two reward messages (income) ----------------------------------------

type MsgWithdrawTokenizeShareRecordReward struct {
	rawMsg
	OwnerAddress string `protobuf:"bytes,1,opt,name=owner_address,json=ownerAddress,proto3" json:"owner_address,omitempty"`
	RecordID     uint64 `protobuf:"varint,2,opt,name=record_id,json=recordId,proto3" json:"record_id,omitempty"`
}

func (m *MsgWithdrawTokenizeShareRecordReward) Reset()        { *m = MsgWithdrawTokenizeShareRecordReward{} }
func (m *MsgWithdrawTokenizeShareRecordReward) ProtoMessage() {}
func (m *MsgWithdrawTokenizeShareRecordReward) String() string {
	return fmt.Sprintf("MsgWithdrawTokenizeShareRecordReward{%s}", m.OwnerAddress)
}
func (m *MsgWithdrawTokenizeShareRecordReward) ValidateBasic() error { return nil }
func (m *MsgWithdrawTokenizeShareRecordReward) GetSigners() []sdk.AccAddress {
	return signerOf(m.OwnerAddress)
}

func (m *MsgWithdrawTokenizeShareRecordReward) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			m.OwnerAddress = string(f.bytes)
		case 2:
			m.RecordID = f.varint
		}
		return nil
	})
}

type MsgWithdrawAllTokenizeShareRecordReward struct {
	rawMsg
	OwnerAddress string `protobuf:"bytes,1,opt,name=owner_address,json=ownerAddress,proto3" json:"owner_address,omitempty"`
}

func (m *MsgWithdrawAllTokenizeShareRecordReward) Reset() {
	*m = MsgWithdrawAllTokenizeShareRecordReward{}
}
func (m *MsgWithdrawAllTokenizeShareRecordReward) ProtoMessage() {}
func (m *MsgWithdrawAllTokenizeShareRecordReward) String() string {
	return fmt.Sprintf("MsgWithdrawAllTokenizeShareRecordReward{%s}", m.OwnerAddress)
}
func (m *MsgWithdrawAllTokenizeShareRecordReward) ValidateBasic() error { return nil }
func (m *MsgWithdrawAllTokenizeShareRecordReward) GetSigners() []sdk.AccAddress {
	return signerOf(m.OwnerAddress)
}

func (m *MsgWithdrawAllTokenizeShareRecordReward) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		if f.num == 1 {
			m.OwnerAddress = string(f.bytes)
		}
		return nil
	})
}
