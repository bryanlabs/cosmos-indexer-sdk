package tax

import (
	"encoding/hex"
	"fmt"
	"strings"

	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"
	indexerTxTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	disttypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	govv1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1"
	govv1beta1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1beta1"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	transfertypes "github.com/cosmos/ibc-go/v7/modules/apps/transfer/types"
	chantypes "github.com/cosmos/ibc-go/v7/modules/core/04-channel/types"
)

// ReconstructMsg builds the minimal message classify needs for a stored
// message whose body bytes were not kept. Production stores every event but no
// message bytes, and every classification rule reads events, so the only
// message fields that matter are the identities the events do not repeat:
// the signer, and for an IBC receive the packet data (which the recv_packet
// event carries hex-encoded).
//
// Nothing reconstructed here is signed, submitted or broadcast. It exists so
// taxrepair can replay the production parser over the stored event log.
func ReconstructMsg(typeURL string, log *indexerTxTypes.LogMessage) (sdk.Msg, error) {
	sender := ""
	for _, e := range log.Events {
		a := attrMap(e)
		if e.Type == "message" && a["action"] == typeURL && a["sender"] != "" {
			if sender != "" && sender != a["sender"] {
				return nil, fmt.Errorf("ambiguous message sender")
			}
			sender = a["sender"]
		}
	}
	needSender := func() error {
		if sender == "" {
			return fmt.Errorf("stored message has no action/sender event")
		}
		return nil
	}
	switch typeURL {
	case "/cosmos.staking.v1beta1.MsgDelegate":
		return &stakingtypes.MsgDelegate{DelegatorAddress: sender}, needSender()
	case "/cosmos.staking.v1beta1.MsgUndelegate":
		return &stakingtypes.MsgUndelegate{DelegatorAddress: sender}, needSender()
	case "/cosmos.staking.v1beta1.MsgBeginRedelegate":
		return &stakingtypes.MsgBeginRedelegate{DelegatorAddress: sender}, needSender()
	case "/cosmos.staking.v1beta1.MsgCancelUnbondingDelegation":
		return &stakingtypes.MsgCancelUnbondingDelegation{DelegatorAddress: sender}, needSender()
	case "/cosmos.distribution.v1beta1.MsgWithdrawDelegatorReward":
		return &disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: sender}, needSender()
	case "/cosmos.gov.v1.MsgDeposit":
		return &govv1.MsgDeposit{Depositor: sender}, needSender()
	case "/cosmos.gov.v1.MsgSubmitProposal":
		return &govv1.MsgSubmitProposal{Proposer: sender}, needSender()
	case "/cosmos.gov.v1beta1.MsgDeposit":
		return &govv1beta1.MsgDeposit{Depositor: sender}, needSender()
	case "/cosmos.gov.v1beta1.MsgSubmitProposal":
		return &govv1beta1.MsgSubmitProposal{Proposer: sender}, needSender()
	case TypeURLTokenizeShares:
		return &MsgTokenizeShares{DelegatorAddress: sender}, needSender()
	case TypeURLRedeemTokensForShares:
		return &MsgRedeemTokensForShares{DelegatorAddress: sender}, needSender()
	case TypeURLTransferTokenizeShareRecord:
		return &MsgTransferTokenizeShareRecord{Sender: sender}, needSender()
	case TypeURLWithdrawTokenizeShareReward:
		return &MsgWithdrawTokenizeShareRecordReward{OwnerAddress: sender}, needSender()
	case TypeURLWithdrawAllTokenizeShareReward:
		return &MsgWithdrawAllTokenizeShareRecordReward{OwnerAddress: sender}, needSender()
	case "/cosmwasm.wasm.v1.MsgExecuteContract":
		// Every wasm rule reads events; the contract and funds are on them.
		return &wasmtypes.MsgExecuteContract{Sender: sender}, nil
	case "/ibc.applications.transfer.v1.MsgTransfer":
		for _, e := range log.Events {
			if e.Type != "ibc_transfer" {
				continue
			}
			a := attrMap(e)
			amt, ok := sdk.NewIntFromString(a["amount"])
			if !ok || a["denom"] == "" {
				return nil, fmt.Errorf("ibc_transfer event has no usable amount")
			}
			return &transfertypes.MsgTransfer{
				Sender: a["sender"], Receiver: a["receiver"], Memo: a["memo"],
				Token: sdk.Coin{Denom: a["denom"], Amount: amt},
			}, nil
		}
		return nil, fmt.Errorf("stored MsgTransfer has no ibc_transfer event")
	case "/ibc.core.channel.v1.MsgRecvPacket":
		for _, e := range log.Events {
			if e.Type != "recv_packet" {
				continue
			}
			a := attrMap(e)
			data, err := hex.DecodeString(a["packet_data_hex"])
			if err != nil {
				return nil, fmt.Errorf("recv_packet packet_data_hex: %w", err)
			}
			return &chantypes.MsgRecvPacket{Packet: chantypes.Packet{
				Data:               data,
				SourcePort:         a["packet_src_port"],
				SourceChannel:      a["packet_src_channel"],
				DestinationPort:    a["packet_dst_port"],
				DestinationChannel: a["packet_dst_channel"],
			}}, nil
		}
		return nil, fmt.Errorf("stored MsgRecvPacket has no recv_packet event")
	case TypeURLV2RecvPacket:
		return &MsgRecvPacketV2{}, nil
	}
	if strings.HasPrefix(typeURL, "/cosmos.authz.") {
		return nil, fmt.Errorf("authz needs the original transaction body")
	}
	return nil, fmt.Errorf("unsupported stored type %s", typeURL)
}

// ReplayableTypeURLs lists the message types ReconstructMsg can rebuild from
// events alone, so a repair can replay them without an archive node.
var ReplayableTypeURLs = []string{
	"/cosmos.staking.v1beta1.MsgDelegate",
	"/cosmos.staking.v1beta1.MsgUndelegate",
	"/cosmos.staking.v1beta1.MsgBeginRedelegate",
	"/cosmos.staking.v1beta1.MsgCancelUnbondingDelegation",
	"/cosmos.distribution.v1beta1.MsgWithdrawDelegatorReward",
	"/cosmos.gov.v1.MsgDeposit",
	"/cosmos.gov.v1.MsgSubmitProposal",
	"/cosmos.gov.v1beta1.MsgDeposit",
	"/cosmos.gov.v1beta1.MsgSubmitProposal",
	TypeURLTokenizeShares,
	TypeURLRedeemTokensForShares,
	TypeURLTransferTokenizeShareRecord,
	TypeURLWithdrawTokenizeShareReward,
	TypeURLWithdrawAllTokenizeShareReward,
	"/cosmwasm.wasm.v1.MsgExecuteContract",
	"/ibc.applications.transfer.v1.MsgTransfer",
	"/ibc.core.channel.v1.MsgRecvPacket",
	TypeURLV2RecvPacket,
}
