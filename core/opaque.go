package core

import sdk "github.com/cosmos/cosmos-sdk/types"

// OpaqueMessage retains evidence for a message absent from the codec. It has no
// inferred signers or decoded fields and must not be classified as a known Msg.
// Dispatch continues to use the original Any type URL, not this Go type.
type OpaqueMessage struct {
	TypeURL string
	Raw     []byte
}

func (m *OpaqueMessage) Reset()                     { *m = OpaqueMessage{} }
func (m *OpaqueMessage) String() string             { return m.TypeURL }
func (*OpaqueMessage) ProtoMessage()                {}
func (*OpaqueMessage) ValidateBasic() error         { return nil }
func (*OpaqueMessage) GetSigners() []sdk.AccAddress { return nil }
func (m *OpaqueMessage) Marshal() ([]byte, error)   { return append([]byte(nil), m.Raw...), nil }
func (m *OpaqueMessage) Size() int                  { return len(m.Raw) }

// messageEvents uses the original message index, not the position in a sparse
// ABCI log array. Missing logs (including failed transactions) have no events.
func messageEvents(logs sdk.ABCIMessageLogs, index int) sdk.StringEvents {
	for _, log := range logs {
		if int(log.MsgIndex) == index {
			return log.Events
		}
	}
	return nil
}
