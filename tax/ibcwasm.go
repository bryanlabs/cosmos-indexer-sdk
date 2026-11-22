// The 08-wasm light client (ibc-go modules/light-clients/08-wasm) is not in
// this build's ibc-go v7, so its messages are declared here by wire shape.
// They store, migrate or remove light client code. Only the governance
// authority may send them, and on the Hub that authority acts through an
// authz grant, so they arrive as the inner message of a nested MsgExec. None
// moves coins (the transaction emits only store_wasm_code or migrate_contract
// beside its message events). Undecoded, the exec was a parser error that
// held back the grantee's reports. Field numbers come from ibc-go
// modules/light-clients/08-wasm proto/ibc/lightclients/wasm/v1/tx.proto at
// v10.3.0.

package tax

import (
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

const (
	TypeURLIBCWasmStoreCode       = "/ibc.lightclients.wasm.v1.MsgStoreCode"
	TypeURLIBCWasmMigrateContract = "/ibc.lightclients.wasm.v1.MsgMigrateContract"
	TypeURLIBCWasmRemoveChecksum  = "/ibc.lightclients.wasm.v1.MsgRemoveChecksum"
)

// IBCWasmMsgTypes maps the 08-wasm light client type URLs to registrable
// instances.
func IBCWasmMsgTypes() map[string]sdk.Msg {
	return map[string]sdk.Msg{
		TypeURLIBCWasmStoreCode:       &MsgIBCWasmStoreCode{},
		TypeURLIBCWasmMigrateContract: &MsgIBCWasmMigrateContract{},
		TypeURLIBCWasmRemoveChecksum:  &MsgIBCWasmRemoveChecksum{},
	}
}

// ibcWasmMsg holds the signer (field 1 in all three messages) and, for a
// migration, the client it migrates. The code and checksums stay in the raw
// bytes.
type ibcWasmMsg struct {
	rawMsg
	Signer   string `json:"signer,omitempty"`
	ClientID string `json:"client_id,omitempty"`
}

func (m *ibcWasmMsg) decode(b []byte, clientField int) error {
	*m = ibcWasmMsg{}
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			m.Signer = string(f.bytes)
		case clientField:
			m.ClientID = string(f.bytes)
		}
		return nil
	})
}

func (*ibcWasmMsg) ProtoMessage()                  {}
func (*ibcWasmMsg) ValidateBasic() error           { return nil }
func (m *ibcWasmMsg) GetSigners() []sdk.AccAddress { return signerOf(m.Signer) }

type MsgIBCWasmStoreCode struct{ ibcWasmMsg }

func (m *MsgIBCWasmStoreCode) Reset() { *m = MsgIBCWasmStoreCode{} }
func (m *MsgIBCWasmStoreCode) String() string {
	return fmt.Sprintf("08-wasm MsgStoreCode{%s}", m.Signer)
}
func (m *MsgIBCWasmStoreCode) Unmarshal(b []byte) error { return m.decode(b, 0) }

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*MsgIBCWasmStoreCode) XXX_MessageName() string { return TypeURLIBCWasmStoreCode[1:] }

type MsgIBCWasmMigrateContract struct{ ibcWasmMsg }

func (m *MsgIBCWasmMigrateContract) Reset() { *m = MsgIBCWasmMigrateContract{} }
func (m *MsgIBCWasmMigrateContract) String() string {
	return fmt.Sprintf("08-wasm MsgMigrateContract{%s}", m.ClientID)
}
func (m *MsgIBCWasmMigrateContract) Unmarshal(b []byte) error { return m.decode(b, 2) }

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*MsgIBCWasmMigrateContract) XXX_MessageName() string { return TypeURLIBCWasmMigrateContract[1:] }

type MsgIBCWasmRemoveChecksum struct{ ibcWasmMsg }

func (m *MsgIBCWasmRemoveChecksum) Reset() { *m = MsgIBCWasmRemoveChecksum{} }
func (m *MsgIBCWasmRemoveChecksum) String() string {
	return fmt.Sprintf("08-wasm MsgRemoveChecksum{%s}", m.Signer)
}
func (m *MsgIBCWasmRemoveChecksum) Unmarshal(b []byte) error { return m.decode(b, 0) }

//revive:disable-next-line var-naming -- gogoproto requires this exact interface method name.
func (*MsgIBCWasmRemoveChecksum) XXX_MessageName() string { return TypeURLIBCWasmRemoveChecksum[1:] }
