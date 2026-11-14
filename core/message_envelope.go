package core

import (
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	govv1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1"
	govv1beta1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1beta1"
	clienttypes "github.com/cosmos/ibc-go/v7/modules/core/02-client/types"
)

// decodeKnownEnvelope preserves a recognized outer message even when recursive
// registry unpacking cannot decode one child. The parser must explicitly report
// unresolved children; the raw outer payload remains the original evidence.
//
// The IBC client and governance envelopes carry children that never move coins
// themselves: a light client header or state (08-wasm on Cosmos Hub is newer
// than this build's ibc-go), or proposal content that only executes later in
// the gov end blocker. Their signer and deposit live on the outer message, so
// the outer message is all classification needs.
func decodeKnownEnvelope(message *codectypes.Any) sdk.Msg {
	if message == nil {
		return nil
	}
	var envelope interface {
		sdk.Msg
		Unmarshal([]byte) error
	}
	var signer func() string
	switch message.TypeUrl {
	case "/cosmos.authz.v1beta1.MsgExec":
		m := &authz.MsgExec{}
		envelope, signer = m, func() string { return m.Grantee }
	case "/ibc.core.client.v1.MsgUpdateClient":
		m := &clienttypes.MsgUpdateClient{}
		envelope, signer = m, func() string { return m.Signer }
	case "/ibc.core.client.v1.MsgCreateClient":
		m := &clienttypes.MsgCreateClient{}
		envelope, signer = m, func() string { return m.Signer }
	case "/ibc.core.client.v1.MsgUpgradeClient":
		m := &clienttypes.MsgUpgradeClient{}
		envelope, signer = m, func() string { return m.Signer }
	case "/ibc.core.client.v1.MsgSubmitMisbehaviour":
		m := &clienttypes.MsgSubmitMisbehaviour{}
		envelope, signer = m, func() string { return m.Signer }
	case "/cosmos.gov.v1.MsgSubmitProposal":
		m := &govv1.MsgSubmitProposal{}
		envelope, signer = m, func() string { return m.Proposer }
	case "/cosmos.gov.v1beta1.MsgSubmitProposal":
		m := &govv1beta1.MsgSubmitProposal{}
		envelope, signer = m, func() string { return m.Proposer }
	default:
		return nil
	}
	if err := envelope.Unmarshal(message.Value); err != nil || signer() == "" {
		return nil
	}
	return envelope
}
