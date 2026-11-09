// The ibc.core.client.v2 messages arrived with IBC v2 in ibc-go v10, so like
// the channel v2 messages (see ibcv2.go) they are declared here by wire shape.
// They only configure a client's counterparty and relayer allowlist and move
// no coins, but undecoded they were stored as opaque evidence that held back
// the relayer's reports. Field numbers come from ibc-go
// proto/ibc/core/client/v2/tx.proto at v10.3.0.

package tax

import (
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

const (
	TypeURLV2RegisterCounterparty = "/ibc.core.client.v2.MsgRegisterCounterparty"
	TypeURLV2UpdateClientConfig   = "/ibc.core.client.v2.MsgUpdateClientConfig"
)

// IBCClientV2MsgTypes maps the client v2 type URLs to registrable instances.
func IBCClientV2MsgTypes() map[string]sdk.Msg {
	return map[string]sdk.Msg{
		TypeURLV2RegisterCounterparty: &MsgRegisterCounterpartyV2{},
		TypeURLV2UpdateClientConfig:   &MsgUpdateClientConfigV2{},
	}
}

type MsgRegisterCounterpartyV2 struct {
	rawMsg
	ClientID             string `protobuf:"bytes,1,opt,name=client_id,json=clientId,proto3" json:"client_id,omitempty"`
	CounterpartyClientID string `protobuf:"bytes,3,opt,name=counterparty_client_id,json=counterpartyClientId,proto3" json:"counterparty_client_id,omitempty"`
	Signer               string `protobuf:"bytes,4,opt,name=signer,proto3" json:"signer,omitempty"`
}

func (m *MsgRegisterCounterpartyV2) Reset()        { *m = MsgRegisterCounterpartyV2{} }
func (m *MsgRegisterCounterpartyV2) ProtoMessage() {}
func (m *MsgRegisterCounterpartyV2) String() string {
	return fmt.Sprintf("client/v2 MsgRegisterCounterparty{%s}", m.ClientID)
}
func (m *MsgRegisterCounterpartyV2) ValidateBasic() error         { return nil }
func (m *MsgRegisterCounterpartyV2) GetSigners() []sdk.AccAddress { return signerOf(m.Signer) }
func (m *MsgRegisterCounterpartyV2) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			m.ClientID = string(f.bytes)
		case 3:
			m.CounterpartyClientID = string(f.bytes)
		case 4:
			m.Signer = string(f.bytes)
		}
		return nil
	})
}

type MsgUpdateClientConfigV2 struct {
	rawMsg
	ClientID string `protobuf:"bytes,1,opt,name=client_id,json=clientId,proto3" json:"client_id,omitempty"`
	Signer   string `protobuf:"bytes,3,opt,name=signer,proto3" json:"signer,omitempty"`
}

func (m *MsgUpdateClientConfigV2) Reset()        { *m = MsgUpdateClientConfigV2{} }
func (m *MsgUpdateClientConfigV2) ProtoMessage() {}
func (m *MsgUpdateClientConfigV2) String() string {
	return fmt.Sprintf("client/v2 MsgUpdateClientConfig{%s}", m.ClientID)
}
func (m *MsgUpdateClientConfigV2) ValidateBasic() error         { return nil }
func (m *MsgUpdateClientConfigV2) GetSigners() []sdk.AccAddress { return signerOf(m.Signer) }
func (m *MsgUpdateClientConfigV2) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			m.ClientID = string(f.bytes)
		case 3:
			m.Signer = string(f.bytes)
		}
		return nil
	})
}
