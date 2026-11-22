package tax

import (
	"encoding/hex"
	"encoding/json"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	transfertypes "github.com/cosmos/ibc-go/v7/modules/apps/transfer/types"
)

const (
	eventIBCSourceCallback    = "ibc_src_callback"
	attributeCallbackAddress  = "callback_address"
	attributeCallbackType     = "callback_type"
	attributeCallbackResult   = "callback_result"
	callbackTypeTimeout       = "timeout_packet"
	callbackTypeAcknowledge   = "acknowledgement_packet"
	callbackResultSuccess     = "success"
	attributePacketData       = "packet_data"
	attributePacketDataHex    = "packet_data_hex"
	eventSendPacket           = "send_packet"
	ibcRefundOnwardResend     = "resend"
	ibcRefundOnwardCallback   = "callback"
	ibcRefundOnwardNone       = ""
	detailRefundPassedThrough = "refund_passed_through"
	detailPrincipalReturn     = "principal_return"
)

// A refund that the receiving account moves onward in the same message never
// rests with that account. Two shapes are recognized, both with exact coin,
// owner and committed-event evidence:
//
//   - Packet-forward middleware retries a timed-out forward: the refund lands
//     on the forward's intermediate account and the same coins leave it again
//     in a new send_packet whose sender is that account. The intermediate's
//     receive was already a pass-through (ibcV1ForwardPassThrough), so its
//     refund is too, and it gets no row.
//   - An IBC source callback contract that sent the packet receives the
//     refund and, inside its successful callback, bank-sends exactly those
//     coins to one other account. The refund is booked as that transfer.
//
// Anything else keeps the refund row.
func ibcRefundOnward(log *txlog.LogMessage, receiver string, coin sdk.Coin, sequence string, timeout bool) (string, string) {
	if log == nil || receiver == "" || !ibcDebited(log, receiver, coin) {
		return ibcRefundOnwardNone, ""
	}
	if ibcRefundResent(log, receiver, coin) {
		return ibcRefundOnwardResend, ""
	}
	if to := ibcRefundCallbackRecipient(log, receiver, coin, sequence, timeout); to != "" {
		return ibcRefundOnwardCallback, to
	}
	return ibcRefundOnwardNone, ""
}

// ibcDebited reports a committed bank debit of the exact coin from owner, read
// from coin_spent, or from transfer senders in older event sets without it.
func ibcDebited(log *txlog.LogMessage, owner string, coin sdk.Coin) bool {
	spent, sent := false, false
	for _, ev := range log.Events {
		if !ibcCommittedEvent(ev) {
			continue
		}
		switch ev.Type {
		case eventCoinSpent:
			for _, a := range ibcAttributeGroups(ev, attributeSpender, attributeAmount) {
				if a[attributeSpender] == owner && ibcCoinsInclude(a[attributeAmount], coin) {
					spent = true
				}
			}
		case string(CategoryTransfer):
			for _, a := range ibcAttributeGroups(ev, attributeRecipient, attributeSender, attributeAmount) {
				if a[attributeSender] == owner && ibcCoinsInclude(a[attributeAmount], coin) {
					sent = true
				}
			}
		}
	}
	return spent || sent
}

func ibcCoinsInclude(amount string, coin sdk.Coin) bool {
	coins, err := sdk.ParseCoinsNormalized(amount)
	return err == nil && coins.AmountOf(coin.Denom).GTE(coin.Amount) && coin.Amount.IsPositive()
}

// ibcRefundResent finds a committed v1 send_packet whose ICS-20 data names the
// refund receiver as sender and carries the exact refunded coin.
func ibcRefundResent(log *txlog.LogMessage, receiver string, coin sdk.Coin) bool {
	for _, ev := range log.Events {
		if ev.Type != eventSendPacket || !ibcCommittedEvent(ev) {
			continue
		}
		for _, a := range ibcAttributeGroups(ev, attributePacketData, attributePacketDataHex) {
			raw := []byte(a[attributePacketData])
			if h, err := hex.DecodeString(a[attributePacketDataHex]); err == nil && len(h) != 0 {
				raw = h
			}
			var data transfertypes.FungibleTokenPacketData
			if len(raw) == 0 || json.Unmarshal(raw, &data) != nil {
				continue
			}
			if data.Sender != receiver || data.Amount != coin.Amount.String() {
				continue
			}
			for _, denom := range ibcRefundCandidates(data.Denom) {
				if denom == coin.Denom {
					return true
				}
			}
		}
	}
	return false
}

// ibcRefundCallbackRecipient returns the one account a successful source
// callback at the refund receiver paid the exact refunded coin to.
func ibcRefundCallbackRecipient(log *txlog.LogMessage, receiver string, coin sdk.Coin, sequence string, timeout bool) string {
	kind := callbackTypeAcknowledge
	if timeout {
		kind = callbackTypeTimeout
	}
	callback := false
	for _, ev := range log.Events {
		if ev.Type != eventIBCSourceCallback || !ibcCommittedEvent(ev) {
			continue
		}
		a := ibcAttrs(ev)
		if a[attributeCallbackAddress] == receiver && a[attributeCallbackType] == kind && a[attributeCallbackResult] == callbackResultSuccess &&
			(a[attributeIBCPacketSequence] == "" || sequence == "" || a[attributeIBCPacketSequence] == sequence) {
			callback = true
		}
	}
	if !callback {
		return ""
	}
	recipients := map[string]bool{}
	for _, ev := range log.Events {
		if ev.Type != string(CategoryTransfer) || !ibcCommittedEvent(ev) {
			continue
		}
		for _, a := range ibcAttributeGroups(ev, attributeRecipient, attributeSender, attributeAmount) {
			if a[attributeSender] != receiver || a[attributeRecipient] == "" || a[attributeRecipient] == receiver {
				continue
			}
			coins, err := sdk.ParseCoinsNormalized(a[attributeAmount])
			if err == nil && coins.Len() == 1 && coins[0].IsEqual(coin) {
				recipients[a[attributeRecipient]] = true
			}
		}
	}
	if len(recipients) != 1 {
		return ""
	}
	for to := range recipients {
		return to
	}
	return ""
}

// ibcRefundRows books a refund by where its coins went within the message.
func ibcRefundRows(log *txlog.LogMessage, receiver string, coin sdk.Coin, timeout bool, detail map[string]string) []TaxableEvent {
	switch kind, to := ibcRefundOnward(log, receiver, coin, detail[attributeIBCPacketSequence], timeout); kind {
	case ibcRefundOnwardResend:
		return nil
	case ibcRefundOnwardCallback:
		d := map[string]string{detailRefundPassedThrough: attributeTrue}
		for k, v := range detail {
			if k != detailPrincipalReturn {
				d[k] = v
			}
		}
		return []TaxableEvent{{Category: string(CategoryTransfer), FromAddr: receiver, ToAddr: to, Amount: coin.Amount.String(), Denom: coin.Denom, Detail: detailJSON(d)}}
	}
	return ibcRefundRow(receiver, coin, timeout, detail)
}
