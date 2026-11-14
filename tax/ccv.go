// Cosmos Hub's provider module (Interchain Security) lives in
// github.com/cosmos/interchain-security, which needs a newer SDK than this
// build links, so its messages used to be stored as opaque evidence. Opaque
// rows hold back every report that touches the validator or consumer owner
// who sent them.
//
// None of these messages move coins (their transactions emit only the module's
// own event plus fees), so they are no-effect types and only the signer has to
// decode. Field numbers come from interchain-security
// proto/interchain_security/ccv/provider/v1/tx.proto at v6.4.0. Before ICS v4
// MsgAssignConsumerKey had no signer field and was signed by the operator's own
// account, which is why the provider address is the fallback signer.

package tax

import (
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

// #nosec G101 -- provider message identifiers are public on-chain type URLs.
const (
	TypeURLCCVAssignConsumerKey          = "/interchain_security.ccv.provider.v1.MsgAssignConsumerKey"
	TypeURLCCVOptIn                      = "/interchain_security.ccv.provider.v1.MsgOptIn"
	TypeURLCCVOptOut                     = "/interchain_security.ccv.provider.v1.MsgOptOut"
	TypeURLCCVSetConsumerCommissionRate  = "/interchain_security.ccv.provider.v1.MsgSetConsumerCommissionRate"
	TypeURLCCVCreateConsumer             = "/interchain_security.ccv.provider.v1.MsgCreateConsumer"
	TypeURLCCVUpdateConsumer             = "/interchain_security.ccv.provider.v1.MsgUpdateConsumer"
	TypeURLCCVRemoveConsumer             = "/interchain_security.ccv.provider.v1.MsgRemoveConsumer"
	TypeURLCCVSubmitConsumerMisbehaviour = "/interchain_security.ccv.provider.v1.MsgSubmitConsumerMisbehaviour"
	TypeURLCCVSubmitConsumerDoubleVoting = "/interchain_security.ccv.provider.v1.MsgSubmitConsumerDoubleVoting"
	// ICS v1 to v3 let anyone whitelist a consumer reward denom for a fee paid
	// to the community pool, so unlike the rest this one moves coins.
	TypeURLCCVRegisterConsumerRewardDenom = "/interchain_security.ccv.provider.v1.MsgRegisterConsumerRewardDenom"
)

// TriggerConsumerRewardDenomFee marks the community pool fee paid to register
// a consumer reward denom.
const TriggerConsumerRewardDenomFee = "consumer_reward_denom_fee"

// ProviderMsgTypes maps the provider module's type URLs to registrable instances.
func ProviderMsgTypes() map[string]sdk.Msg {
	return map[string]sdk.Msg{
		TypeURLCCVAssignConsumerKey:           &MsgAssignConsumerKey{},
		TypeURLCCVOptIn:                       &MsgOptIn{},
		TypeURLCCVOptOut:                      &MsgOptOut{},
		TypeURLCCVSetConsumerCommissionRate:   &MsgSetConsumerCommissionRate{},
		TypeURLCCVCreateConsumer:              &MsgCreateConsumer{},
		TypeURLCCVUpdateConsumer:              &MsgUpdateConsumer{},
		TypeURLCCVRemoveConsumer:              &MsgRemoveConsumer{},
		TypeURLCCVSubmitConsumerMisbehaviour:  &MsgSubmitConsumerMisbehaviour{},
		TypeURLCCVSubmitConsumerDoubleVoting:  &MsgSubmitConsumerDoubleVoting{},
		TypeURLCCVRegisterConsumerRewardDenom: &MsgRegisterConsumerRewardDenom{},
	}
}

// providerMsg holds the fields every provider message is identified by. A
// field number of 0 means the message has no such field (wire fields start at 1).
type providerMsg struct {
	rawMsg
	Signer       string `json:"signer,omitempty"`
	ProviderAddr string `json:"provider_addr,omitempty"`
	ConsumerID   string `json:"consumer_id,omitempty"`
	ChainID      string `json:"chain_id,omitempty"`
}

type providerFields struct{ signer, provider, consumer, chain int }

func (m *providerMsg) decode(b []byte, f providerFields) error {
	*m = providerMsg{}
	m.keep(b)
	return scanFields(b, func(w wireField) error {
		switch w.num {
		case f.signer:
			m.Signer = string(w.bytes)
		case f.provider:
			m.ProviderAddr = string(w.bytes)
		case f.consumer:
			m.ConsumerID = string(w.bytes)
		case f.chain:
			m.ChainID = string(w.bytes)
		}
		return nil
	})
}

func (*providerMsg) ProtoMessage()        {}
func (*providerMsg) ValidateBasic() error { return nil }

func (m *providerMsg) GetSigners() []sdk.AccAddress {
	if m.Signer != "" {
		return signerOf(m.Signer)
	}
	if v, err := sdk.ValAddressFromBech32(m.ProviderAddr); err == nil {
		return []sdk.AccAddress{sdk.AccAddress(v)}
	}
	return nil
}

func (m *providerMsg) describe(name string) string {
	return fmt.Sprintf("%s{%s %s%s}", name, m.Signer, m.ConsumerID, m.ChainID)
}

type MsgAssignConsumerKey struct{ providerMsg }

func (m *MsgAssignConsumerKey) Reset()         { *m = MsgAssignConsumerKey{} }
func (m *MsgAssignConsumerKey) String() string { return m.describe("MsgAssignConsumerKey") }
func (m *MsgAssignConsumerKey) Unmarshal(b []byte) error {
	return m.decode(b, providerFields{signer: 4, provider: 2, consumer: 5, chain: 1})
}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*MsgAssignConsumerKey) XXX_MessageName() string { return TypeURLCCVAssignConsumerKey[1:] }

type MsgOptIn struct{ providerMsg }

func (m *MsgOptIn) Reset()         { *m = MsgOptIn{} }
func (m *MsgOptIn) String() string { return m.describe("MsgOptIn") }
func (m *MsgOptIn) Unmarshal(b []byte) error {
	return m.decode(b, providerFields{signer: 4, provider: 2, consumer: 5, chain: 1})
}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*MsgOptIn) XXX_MessageName() string { return TypeURLCCVOptIn[1:] }

type MsgOptOut struct{ providerMsg }

func (m *MsgOptOut) Reset()         { *m = MsgOptOut{} }
func (m *MsgOptOut) String() string { return m.describe("MsgOptOut") }
func (m *MsgOptOut) Unmarshal(b []byte) error {
	return m.decode(b, providerFields{signer: 3, provider: 2, consumer: 4, chain: 1})
}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*MsgOptOut) XXX_MessageName() string { return TypeURLCCVOptOut[1:] }

type MsgSetConsumerCommissionRate struct{ providerMsg }

func (m *MsgSetConsumerCommissionRate) Reset() { *m = MsgSetConsumerCommissionRate{} }
func (m *MsgSetConsumerCommissionRate) String() string {
	return m.describe("MsgSetConsumerCommissionRate")
}

func (m *MsgSetConsumerCommissionRate) Unmarshal(b []byte) error {
	return m.decode(b, providerFields{signer: 4, provider: 1, consumer: 5, chain: 2})
}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*MsgSetConsumerCommissionRate) XXX_MessageName() string {
	return TypeURLCCVSetConsumerCommissionRate[1:]
}

type MsgCreateConsumer struct{ providerMsg }

func (m *MsgCreateConsumer) Reset()         { *m = MsgCreateConsumer{} }
func (m *MsgCreateConsumer) String() string { return m.describe("MsgCreateConsumer") }
func (m *MsgCreateConsumer) Unmarshal(b []byte) error {
	return m.decode(b, providerFields{signer: 1, chain: 2})
}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*MsgCreateConsumer) XXX_MessageName() string { return TypeURLCCVCreateConsumer[1:] }

type MsgUpdateConsumer struct{ providerMsg }

func (m *MsgUpdateConsumer) Reset()         { *m = MsgUpdateConsumer{} }
func (m *MsgUpdateConsumer) String() string { return m.describe("MsgUpdateConsumer") }
func (m *MsgUpdateConsumer) Unmarshal(b []byte) error {
	return m.decode(b, providerFields{signer: 1, consumer: 2})
}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*MsgUpdateConsumer) XXX_MessageName() string { return TypeURLCCVUpdateConsumer[1:] }

type MsgRemoveConsumer struct{ providerMsg }

func (m *MsgRemoveConsumer) Reset()         { *m = MsgRemoveConsumer{} }
func (m *MsgRemoveConsumer) String() string { return m.describe("MsgRemoveConsumer") }
func (m *MsgRemoveConsumer) Unmarshal(b []byte) error {
	return m.decode(b, providerFields{signer: 2, consumer: 1})
}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*MsgRemoveConsumer) XXX_MessageName() string { return TypeURLCCVRemoveConsumer[1:] }

type MsgSubmitConsumerMisbehaviour struct{ providerMsg }

func (m *MsgSubmitConsumerMisbehaviour) Reset() { *m = MsgSubmitConsumerMisbehaviour{} }
func (m *MsgSubmitConsumerMisbehaviour) String() string {
	return m.describe("MsgSubmitConsumerMisbehaviour")
}

func (m *MsgSubmitConsumerMisbehaviour) Unmarshal(b []byte) error {
	return m.decode(b, providerFields{signer: 1, consumer: 3})
}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*MsgSubmitConsumerMisbehaviour) XXX_MessageName() string {
	return TypeURLCCVSubmitConsumerMisbehaviour[1:]
}

type MsgSubmitConsumerDoubleVoting struct{ providerMsg }

func (m *MsgSubmitConsumerDoubleVoting) Reset() { *m = MsgSubmitConsumerDoubleVoting{} }
func (m *MsgSubmitConsumerDoubleVoting) String() string {
	return m.describe("MsgSubmitConsumerDoubleVoting")
}

func (m *MsgSubmitConsumerDoubleVoting) Unmarshal(b []byte) error {
	return m.decode(b, providerFields{signer: 1, consumer: 4})
}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*MsgSubmitConsumerDoubleVoting) XXX_MessageName() string {
	return TypeURLCCVSubmitConsumerDoubleVoting[1:]
}

// MsgRegisterConsumerRewardDenom is read from interchain-security
// proto/interchain_security/ccv/provider/v1/tx.proto at v1.1.0-multiden: denom
// is field 1 and depositor field 2. The depositor signs and pays the
// registration fee.
type MsgRegisterConsumerRewardDenom struct {
	rawMsg
	Denom     string `json:"denom,omitempty"`
	Depositor string `json:"depositor,omitempty"`
}

func (m *MsgRegisterConsumerRewardDenom) Reset()                       { *m = MsgRegisterConsumerRewardDenom{} }
func (*MsgRegisterConsumerRewardDenom) ProtoMessage()                  {}
func (*MsgRegisterConsumerRewardDenom) ValidateBasic() error           { return nil }
func (m *MsgRegisterConsumerRewardDenom) GetSigners() []sdk.AccAddress { return signerOf(m.Depositor) }

func (m *MsgRegisterConsumerRewardDenom) String() string {
	return fmt.Sprintf("MsgRegisterConsumerRewardDenom{%s %s}", m.Depositor, m.Denom)
}

func (m *MsgRegisterConsumerRewardDenom) Unmarshal(b []byte) error {
	*m = MsgRegisterConsumerRewardDenom{}
	m.keep(b)
	return scanFields(b, func(w wireField) error {
		switch w.num {
		case 1:
			m.Denom = string(w.bytes)
		case 2:
			m.Depositor = string(w.bytes)
		}
		return nil
	})
}

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*MsgRegisterConsumerRewardDenom) XXX_MessageName() string {
	return TypeURLCCVRegisterConsumerRewardDenom[1:]
}
