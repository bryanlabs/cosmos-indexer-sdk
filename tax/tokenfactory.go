// Cosmos Hub gained a tokenfactory module, which keeps the original
// osmosis.tokenfactory.v1beta1 proto package name. It is not in this build's
// codec, so those messages were skipped.
//
// That is not a harmless gap. MsgMint succeeds on Cosmos Hub and credits real
// balances to real addresses: the observed traffic is STARS distributed as
// factory/cosmos1s8qx0zvz8yd6e4x0mqmqf7fr9vvfn6226hkvrq/ustars across many Hub
// accounts, several thousand ATOM-equivalent per mint. A recipient acquired an
// asset, so it belongs on their report.
//
// Field numbers from osmosis proto/osmosis/tokenfactory/v1beta1/tx.proto.
// Burning, creating a denom and changing admin move no value to a third party,
// so they are registered to decode but produce no taxable event.

package tax

import (
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

const (
	TypeURLTFMint        = "/osmosis.tokenfactory.v1beta1.MsgMint"
	TypeURLTFBurn        = "/osmosis.tokenfactory.v1beta1.MsgBurn"
	TypeURLTFCreateDenom = "/osmosis.tokenfactory.v1beta1.MsgCreateDenom"
	TypeURLTFChangeAdmin = "/osmosis.tokenfactory.v1beta1.MsgChangeAdmin"
	// Display metadata only; carries no value.
	TypeURLTFSetMetadata = "/osmosis.tokenfactory.v1beta1.MsgSetDenomMetadata"
	// Moves coins between two addresses on the admin's say-so.
	TypeURLTFForceTransfer = "/osmosis.tokenfactory.v1beta1.MsgForceTransfer"
)

// TokenFactoryMsgTypes maps the tokenfactory type URLs to registrable instances.
func TokenFactoryMsgTypes() map[string]sdk.Msg {
	return map[string]sdk.Msg{
		TypeURLTFMint:          &MsgTFMint{},
		TypeURLTFBurn:          &MsgTFBurn{},
		TypeURLTFCreateDenom:   &MsgTFCreateDenom{},
		TypeURLTFChangeAdmin:   &MsgTFChangeAdmin{},
		TypeURLTFSetMetadata:   &MsgTFSetDenomMetadata{},
		TypeURLTFForceTransfer: &MsgTFForceTransfer{},
	}
}

// coinFields decodes a cosmos.base.v1beta1.Coin (denom = 1, amount = 2).
func coinFields(b []byte) (denom string, amount string, err error) {
	err = scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			denom = string(f.bytes)
		case 2:
			amount = string(f.bytes)
		}
		return nil
	})
	return denom, amount, err
}

// --- MsgMint -----------------------------------------------------------------

type MsgTFMint struct {
	rawMsg
	Sender        string `protobuf:"bytes,1,opt,name=sender,proto3" json:"sender,omitempty"`
	Denom         string `json:"denom,omitempty"`
	Amount        string `json:"amount,omitempty"`
	MintToAddress string `protobuf:"bytes,3,opt,name=mintToAddress,proto3" json:"mintToAddress,omitempty"`
}

func (m *MsgTFMint) Reset()        { *m = MsgTFMint{} }
func (m *MsgTFMint) ProtoMessage() {}
func (m *MsgTFMint) String() string {
	return fmt.Sprintf("tokenfactory MsgMint{%s%s -> %s}", m.Amount, m.Denom, m.MintToAddress)
}
func (m *MsgTFMint) ValidateBasic() error         { return nil }
func (m *MsgTFMint) GetSigners() []sdk.AccAddress { return signerOf(m.Sender) }
func (m *MsgTFMint) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			m.Sender = string(f.bytes)
		case 2:
			denom, amount, err := coinFields(f.bytes)
			if err != nil {
				return err
			}
			m.Denom, m.Amount = denom, amount
		case 3:
			m.MintToAddress = string(f.bytes)
		}
		return nil
	})
}

// Recipient is who ends up holding the minted coins. mintToAddress is optional
// on the wire; when it is absent the tokens go to the sender.
func (m *MsgTFMint) Recipient() string {
	if m.MintToAddress != "" {
		return m.MintToAddress
	}
	return m.Sender
}

// --- MsgBurn -----------------------------------------------------------------

type MsgTFBurn struct {
	rawMsg
	Sender          string `protobuf:"bytes,1,opt,name=sender,proto3" json:"sender,omitempty"`
	Denom           string `json:"denom,omitempty"`
	Amount          string `json:"amount,omitempty"`
	BurnFromAddress string `protobuf:"bytes,3,opt,name=burnFromAddress,proto3" json:"burnFromAddress,omitempty"`
}

func (m *MsgTFBurn) Reset()        { *m = MsgTFBurn{} }
func (m *MsgTFBurn) ProtoMessage() {}
func (m *MsgTFBurn) String() string {
	return fmt.Sprintf("tokenfactory MsgBurn{%s%s}", m.Amount, m.Denom)
}
func (m *MsgTFBurn) ValidateBasic() error         { return nil }
func (m *MsgTFBurn) GetSigners() []sdk.AccAddress { return signerOf(m.Sender) }
func (m *MsgTFBurn) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			m.Sender = string(f.bytes)
		case 2:
			denom, amount, err := coinFields(f.bytes)
			if err != nil {
				return err
			}
			m.Denom, m.Amount = denom, amount
		case 3:
			m.BurnFromAddress = string(f.bytes)
		}
		return nil
	})
}

// --- MsgCreateDenom / MsgChangeAdmin -----------------------------------------

type MsgTFCreateDenom struct {
	rawMsg
	Sender   string `protobuf:"bytes,1,opt,name=sender,proto3" json:"sender,omitempty"`
	Subdenom string `protobuf:"bytes,2,opt,name=subdenom,proto3" json:"subdenom,omitempty"`
}

func (m *MsgTFCreateDenom) Reset()        { *m = MsgTFCreateDenom{} }
func (m *MsgTFCreateDenom) ProtoMessage() {}
func (m *MsgTFCreateDenom) String() string {
	return fmt.Sprintf("tokenfactory MsgCreateDenom{%s}", m.Subdenom)
}
func (m *MsgTFCreateDenom) ValidateBasic() error         { return nil }
func (m *MsgTFCreateDenom) GetSigners() []sdk.AccAddress { return signerOf(m.Sender) }
func (m *MsgTFCreateDenom) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			m.Sender = string(f.bytes)
		case 2:
			m.Subdenom = string(f.bytes)
		}
		return nil
	})
}

type MsgTFChangeAdmin struct {
	rawMsg
	Sender   string `protobuf:"bytes,1,opt,name=sender,proto3" json:"sender,omitempty"`
	Denom    string `protobuf:"bytes,2,opt,name=denom,proto3" json:"denom,omitempty"`
	NewAdmin string `protobuf:"bytes,3,opt,name=new_admin,json=newAdmin,proto3" json:"new_admin,omitempty"`
}

func (m *MsgTFChangeAdmin) Reset()        { *m = MsgTFChangeAdmin{} }
func (m *MsgTFChangeAdmin) ProtoMessage() {}
func (m *MsgTFChangeAdmin) String() string {
	return fmt.Sprintf("tokenfactory MsgChangeAdmin{%s}", m.Denom)
}
func (m *MsgTFChangeAdmin) ValidateBasic() error         { return nil }
func (m *MsgTFChangeAdmin) GetSigners() []sdk.AccAddress { return signerOf(m.Sender) }
func (m *MsgTFChangeAdmin) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			m.Sender = string(f.bytes)
		case 2:
			m.Denom = string(f.bytes)
		case 3:
			m.NewAdmin = string(f.bytes)
		}
		return nil
	})
}

// --- MsgSetDenomMetadata -----------------------------------------------------
//
// Display metadata only: name, symbol, decimals. No amount, no recipient, so it
// cannot move value and produces no taxable event. Registered so the generic
// message tables stay complete.

type MsgTFSetDenomMetadata struct {
	rawMsg
	Sender      string `protobuf:"bytes,1,opt,name=sender,proto3" json:"sender,omitempty"`
	MetadataRaw []byte `protobuf:"bytes,2,opt,name=metadata,proto3" json:"metadata,omitempty"`
}

func (m *MsgTFSetDenomMetadata) Reset()        { *m = MsgTFSetDenomMetadata{} }
func (m *MsgTFSetDenomMetadata) ProtoMessage() {}
func (m *MsgTFSetDenomMetadata) String() string {
	return fmt.Sprintf("tokenfactory MsgSetDenomMetadata{%s}", m.Sender)
}
func (m *MsgTFSetDenomMetadata) ValidateBasic() error         { return nil }
func (m *MsgTFSetDenomMetadata) GetSigners() []sdk.AccAddress { return signerOf(m.Sender) }
func (m *MsgTFSetDenomMetadata) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			m.Sender = string(f.bytes)
		case 2:
			m.MetadataRaw = append([]byte(nil), f.bytes...)
		}
		return nil
	})
}

// --- MsgForceTransfer --------------------------------------------------------
//
// A denom admin moving coins between two addresses without their involvement.
// That is a real movement for both parties, so it is classified like a send.
// It has never appeared on Cosmos Hub (zero occurrences from 2025-12-31 to
// 2026-08-18), but it is registered and classified so that if the module is
// ever used that way the value is not silently skipped.

type MsgTFForceTransfer struct {
	rawMsg
	Sender              string `protobuf:"bytes,1,opt,name=sender,proto3" json:"sender,omitempty"`
	Denom               string `json:"denom,omitempty"`
	Amount              string `json:"amount,omitempty"`
	TransferFromAddress string `protobuf:"bytes,3,opt,name=transferFromAddress,proto3" json:"transferFromAddress,omitempty"`
	TransferToAddress   string `protobuf:"bytes,4,opt,name=transferToAddress,proto3" json:"transferToAddress,omitempty"`
}

func (m *MsgTFForceTransfer) Reset()        { *m = MsgTFForceTransfer{} }
func (m *MsgTFForceTransfer) ProtoMessage() {}
func (m *MsgTFForceTransfer) String() string {
	return fmt.Sprintf("tokenfactory MsgForceTransfer{%s%s %s -> %s}",
		m.Amount, m.Denom, m.TransferFromAddress, m.TransferToAddress)
}
func (m *MsgTFForceTransfer) ValidateBasic() error         { return nil }
func (m *MsgTFForceTransfer) GetSigners() []sdk.AccAddress { return signerOf(m.Sender) }
func (m *MsgTFForceTransfer) Unmarshal(b []byte) error {
	m.Reset()
	m.keep(b)
	return scanFields(b, func(f wireField) error {
		switch f.num {
		case 1:
			m.Sender = string(f.bytes)
		case 2:
			denom, amount, err := coinFields(f.bytes)
			if err != nil {
				return err
			}
			m.Denom, m.Amount = denom, amount
		case 3:
			m.TransferFromAddress = string(f.bytes)
		case 4:
			m.TransferToAddress = string(f.bytes)
		}
		return nil
	})
}
