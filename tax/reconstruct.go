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
		if e.Type == "message" && a[attributeAction] == typeURL && a[attributeSender] != "" {
			if sender != "" && sender != a[attributeSender] {
				return nil, fmt.Errorf("ambiguous message sender")
			}
			sender = a[attributeSender]
		}
	}
	needSender := func() error {
		if sender == "" {
			return fmt.Errorf("stored message has no action/sender event")
		}
		return nil
	}
	switch typeURL {
	case typeURLDelegate:
		return &stakingtypes.MsgDelegate{DelegatorAddress: sender}, needSender()
	case typeURLUndelegate:
		return &stakingtypes.MsgUndelegate{DelegatorAddress: sender}, needSender()
	case typeURLBeginRedelegate:
		return &stakingtypes.MsgBeginRedelegate{DelegatorAddress: sender}, needSender()
	case typeURLCancelUnbondingDelegation:
		return &stakingtypes.MsgCancelUnbondingDelegation{DelegatorAddress: sender}, needSender()
	case typeURLWithdrawDelegatorReward:
		return &disttypes.MsgWithdrawDelegatorReward{DelegatorAddress: sender}, needSender()
	case typeURLGovDeposit:
		return &govv1.MsgDeposit{Depositor: sender}, needSender()
	case typeURLGovSubmitProposal:
		return &govv1.MsgSubmitProposal{Proposer: sender}, needSender()
	case typeURLLegacyGovDeposit:
		return &govv1beta1.MsgDeposit{Depositor: sender}, needSender()
	case typeURLLegacyGovSubmitProposal:
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
	case typeURLExecuteContract:
		// Every wasm rule reads events; the contract and funds are on them.
		return &wasmtypes.MsgExecuteContract{Sender: sender}, nil
	case typeURLIBCTransfer:
		for _, e := range log.Events {
			if e.Type != "ibc_transfer" {
				continue
			}
			a := attrMap(e)
			amt, ok := sdk.NewIntFromString(a[attributeAmount])
			if !ok || a[attributeDenom] == "" {
				return nil, fmt.Errorf("ibc_transfer event has no usable amount")
			}
			return &transfertypes.MsgTransfer{
				Sender: a[attributeSender], Receiver: a[attributeReceiver], Memo: a["memo"],
				Token: sdk.Coin{Denom: a[attributeDenom], Amount: amt},
			}, nil
		}
		return nil, fmt.Errorf("stored MsgTransfer has no ibc_transfer event")
	case typeURLIBCRecvPacket:
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
	typeURLDelegate,
	typeURLUndelegate,
	typeURLBeginRedelegate,
	typeURLCancelUnbondingDelegation,
	typeURLWithdrawDelegatorReward,
	typeURLGovDeposit,
	typeURLGovSubmitProposal,
	typeURLLegacyGovDeposit,
	typeURLLegacyGovSubmitProposal,
	TypeURLTokenizeShares,
	TypeURLRedeemTokensForShares,
	TypeURLTransferTokenizeShareRecord,
	TypeURLWithdrawTokenizeShareReward,
	TypeURLWithdrawAllTokenizeShareReward,
	typeURLExecuteContract,
	typeURLIBCTransfer,
	typeURLIBCRecvPacket,
	TypeURLV2RecvPacket,
}

// StakingPrincipalFromLog exposes the staking principal rule for replay tools
// that classify authz inner messages event by event.
func StakingPrincipalFromLog(log *indexerTxTypes.LogMessage, delegator string) []TaxableEvent {
	return stakingPrincipalEvents(log, delegator)
}
