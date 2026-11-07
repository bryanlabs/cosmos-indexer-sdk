package tax

// NoTaxEffectTypeURLs are message types that never move coins on Cosmos Hub,
// so they need no classification. A decoded message whose type is neither here
// nor in MessageTypeURLs holds a report back until it is classified, because
// it may have moved coins the tax layer never saw. Types that can move coins
// (MsgCancelProposal burns part of a deposit, MsgStoreAndInstantiateContract
// can carry funds) stay out of this list on purpose. An interchain account's
// MsgSendTx moves coins only on the host chain, from the host account, never
// the owner's Cosmos Hub balance.
var NoTaxEffectTypeURLs = []string{
	"/cosmos.authz.v1beta1.MsgGrant",
	"/cosmos.authz.v1beta1.MsgRevoke",
	"/cosmos.distribution.v1beta1.MsgSetWithdrawAddress",
	"/cosmos.feegrant.v1beta1.MsgGrantAllowance",
	"/cosmos.feegrant.v1beta1.MsgRevokeAllowance",
	"/cosmos.gov.v1.MsgVote",
	"/cosmos.gov.v1.MsgVoteWeighted",
	"/cosmos.gov.v1beta1.MsgVote",
	"/cosmos.gov.v1beta1.MsgVoteWeighted",
	"/cosmos.slashing.v1beta1.MsgUnjail",
	"/cosmos.staking.v1beta1.MsgEditValidator",
	"/gaia.liquid.v1beta1.MsgDisableTokenizeShares",
	"/gaia.liquid.v1beta1.MsgEnableTokenizeShares",
	"/cosmwasm.wasm.v1.MsgStoreCode",
	"/cosmwasm.wasm.v1.MsgUpdateAdmin",
	"/cosmwasm.wasm.v1.MsgClearAdmin",
	"/cosmwasm.wasm.v1.MsgUpdateInstantiateConfig",
	"/ibc.core.client.v1.MsgCreateClient",
	"/ibc.core.client.v1.MsgUpdateClient",
	"/ibc.core.client.v1.MsgUpgradeClient",
	"/ibc.core.client.v1.MsgSubmitMisbehaviour",
	"/ibc.core.connection.v1.MsgConnectionOpenInit",
	"/ibc.core.connection.v1.MsgConnectionOpenTry",
	"/ibc.core.connection.v1.MsgConnectionOpenAck",
	"/ibc.core.connection.v1.MsgConnectionOpenConfirm",
	"/ibc.core.channel.v1.MsgChannelOpenInit",
	"/ibc.core.channel.v1.MsgChannelOpenTry",
	"/ibc.core.channel.v1.MsgChannelOpenAck",
	"/ibc.core.channel.v1.MsgChannelOpenConfirm",
	"/ibc.core.channel.v1.MsgChannelCloseInit",
	"/ibc.core.channel.v1.MsgChannelCloseConfirm",
	"/ibc.core.client.v2.MsgRegisterCounterparty",
	"/ibc.core.client.v2.MsgUpdateClientConfig",
	"/ibc.applications.interchain_accounts.controller.v1.MsgRegisterInterchainAccount",
	"/ibc.applications.interchain_accounts.controller.v1.MsgSendTx",
	"/interchain_security.ccv.provider.v1.MsgAssignConsumerKey",
	"/interchain_security.ccv.provider.v1.MsgCreateConsumer",
	"/interchain_security.ccv.provider.v1.MsgUpdateConsumer",
	"/interchain_security.ccv.provider.v1.MsgOptIn",
	"/interchain_security.ccv.provider.v1.MsgOptOut",
	"/interchain_security.ccv.provider.v1.MsgRemoveConsumer",
	"/interchain_security.ccv.provider.v1.MsgSetConsumerCommissionRate",
	"/interchain_security.ccv.provider.v1.MsgSubmitConsumerMisbehaviour",
	"/interchain_security.ccv.provider.v1.MsgSubmitConsumerDoubleVoting",
	"/osmosis.tokenfactory.v1beta1.MsgChangeAdmin",
	"/osmosis.tokenfactory.v1beta1.MsgSetDenomMetadata",
}

// ReviewedTypeURLs is every message type the tax layer has a decision for.
func ReviewedTypeURLs() []string {
	out := make([]string, 0, len(MessageTypeURLs)+len(NoTaxEffectTypeURLs))
	out = append(out, MessageTypeURLs...)
	return append(out, NoTaxEffectTypeURLs...)
}
