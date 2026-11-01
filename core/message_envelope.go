package core

import (
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
)

// decodeKnownEnvelope preserves a recognized outer message even when recursive
// registry unpacking cannot decode one child. The parser must explicitly report
// unresolved children; the raw outer payload remains the original evidence.
func decodeKnownEnvelope(message *codectypes.Any) sdk.Msg {
	if message == nil || message.TypeUrl != "/cosmos.authz.v1beta1.MsgExec" {
		return nil
	}
	var envelope authz.MsgExec
	if err := envelope.Unmarshal(message.Value); err != nil || envelope.Grantee == "" {
		return nil
	}
	return &envelope
}
