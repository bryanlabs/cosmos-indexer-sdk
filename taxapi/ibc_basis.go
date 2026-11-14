package taxapi

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	transfertypes "github.com/cosmos/ibc-go/v7/modules/apps/transfer/types"
)

const (
	ibcRefundBasisRequired    = "ibc_refund_basis_reconciliation_required"
	ibcPacketIdentityRequired = "ibc_outbound_packet_identity_required"
	ibcTimeoutRefund          = "ibc_timeout_refund"
	ibcErrorAckRefund         = "ibc_error_ack_refund"
	// A send whose packet came back: the coins sat in the channel's escrow and
	// returned, so the send is a record-only protocol deposit, and its refund a
	// record-only withdrawal, under these triggers.
	ibcRefundedSend       = "ibc_refunded_send"
	ibcRefundReconciled   = "ibc_refund_reconciled"
	ibcRefundRoleSend     = "send"
	ibcRefundRoleReturned = "refund"
)

func normalizedWalletScope(addresses []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, addr := range addresses {
		addr = strings.TrimSpace(addr)
		if addr != "" && !seen[addr] {
			seen[addr] = true
			out = append(out, addr)
		}
	}
	return out
}

func isIBCRefund(trigger string) bool {
	return trigger == ibcTimeoutRefund || trigger == ibcErrorAckRefund
}

// ibcRefundDenom identifies a voucher by the hash of its complete ICS-20
// trace. Legacy sends can retain the trace while native refunds name its hash.
// Never strip a hop or compare base denoms: distinct routes are distinct coins.
func ibcRefundDenom(denom string) string {
	trace := transfertypes.ParseDenomTrace(denom)
	if trace.Path != "" && trace.Validate() == nil {
		return trace.IBCDenom()
	}
	return denom
}

// matchIBCRefunds pairs each refund with the one send it returned: same owner,
// same packet identity, same coin, and the refund no earlier than the send. A
// matched send never left the wallet, so undoing it keeps the original lots
// with their dates and cost. Two refunds or two sends for one packet, a coin
// that differs, or a row without an id is left unmatched and keeps blocking.
// The result maps taxable event id to its role in the pair.
func matchIBCRefunds(events []tax.TaxableEvent) map[uint]string {
	refunds := map[string][]tax.TaxableEvent{}
	sends := map[string][]tax.TaxableEvent{}
	for _, e := range events {
		switch {
		case isIBCRefund(e.RewardTrigger):
			if key := tax.IBCPacketKey(e.Detail); key != "" {
				refunds[e.ToAddr+":"+key] = append(refunds[e.ToAddr+":"+key], e)
			}
		case e.Category == string(tax.CategoryIBCOut):
			if key := tax.IBCPacketKey(e.Detail); key != "" {
				sends[e.FromAddr+":"+key] = append(sends[e.FromAddr+":"+key], e)
			}
		}
	}
	pairs := map[uint]string{}
	for key, returned := range refunds {
		sent := sends[key]
		if len(returned) != 1 || len(sent) != 1 {
			continue
		}
		r, o := returned[0], sent[0]
		if r.ID == 0 || o.ID == 0 || r.Denom == "" || ibcRefundDenom(r.Denom) != ibcRefundDenom(o.Denom) || r.Amount == "" || r.Amount != o.Amount || r.Timestamp.Before(o.Timestamp) {
			continue
		}
		pairs[o.ID] = ibcRefundRoleSend
		pairs[r.ID] = ibcRefundRoleReturned
	}
	return pairs
}

// reconcileIBCRefunds rewrites matched pairs as the escrow round trip they
// were: the send a record-only protocol deposit, the refund a record-only
// withdrawal. Neither is a disposal or an acquisition, so the FIFO engines
// keep the original lots in place.
func reconcileIBCRefunds(events []tax.TaxableEvent, pairs map[uint]string) {
	for i := range events {
		switch pairs[events[i].ID] {
		case ibcRefundRoleSend:
			events[i].Category = string(tax.CategoryProtocolDeposit)
			events[i].RewardTrigger = ibcRefundedSend
		case ibcRefundRoleReturned:
			events[i].RewardTrigger = ibcRefundReconciled
		}
	}
}

// IBC refunds are not acquisitions. A refund matched to its send is undone by
// reconcileIBCRefunds; any other refund leaves neither the disposal nor later
// inventory authoritative. Look beyond the requested year so a January refund
// cannot leave a December phantom gain certified by an old report.
func ibcBasisReadiness(events []tax.TaxableEvent, end time.Time) ReportReadiness {
	pairs := matchIBCRefunds(events)
	refunds := map[string]bool{}
	refundOwners := map[string]bool{}
	unknownRefundOwners := map[string]bool{}
	blocked := map[string]bool{}
	reasons := map[string]bool{}
	for _, e := range events {
		if !isIBCRefund(e.RewardTrigger) || pairs[e.ID] != "" {
			continue
		}
		refundOwners[e.ToAddr] = true
		key := tax.IBCPacketKey(e.Detail)
		if key != "" {
			refunds[e.ToAddr+":"+key] = true
		} else {
			unknownRefundOwners[e.ToAddr] = true
		}
		if e.Timestamp.Before(end) {
			blocked[e.ToAddr+":"+key+":"+e.TxHash] = true
			reasons[ibcRefundBasisRequired] = true
		}
	}
	for _, e := range events {
		if e.Category != string(tax.CategoryIBCOut) || !e.Timestamp.Before(end) || pairs[e.ID] != "" {
			continue
		}
		key := tax.IBCPacketKey(e.Detail)
		switch {
		case unknownRefundOwners[e.FromAddr]:
			blocked[e.FromAddr+":unknown-refund:"+e.TxHash] = true
			reasons[ibcPacketIdentityRequired] = true
		case key != "" && refunds[e.FromAddr+":"+key]:
			blocked[e.FromAddr+":"+key] = true
			reasons[ibcRefundBasisRequired] = true
		case key == "" && refundOwners[e.FromAddr]:
			// Legacy rows need retained packet evidence or replay. Do not guess
			// a match from identical coin quantities, memos or nearby timestamps.
			blocked[e.FromAddr+":missing:"+e.TxHash] = true
			reasons[ibcPacketIdentityRequired] = true
		}
	}
	r := readinessForRows(nil)
	r.PendingSemanticReviews = len(blocked)
	r.ExportReady = len(blocked) == 0
	for reason := range reasons {
		r.ReviewRequired = append(r.ReviewRequired, reason)
	}
	sort.Strings(r.ReviewRequired)
	r.Scope = "requested chain and wallets, full persisted IBC history including refunds outside the requested period; only a refund matched to its send by packet identity and coin is reconciled"
	return r
}

func (s *Server) requireIBCBasisReady(chain string, addresses []string, end time.Time) error {
	_, err := s.ibcBasisCheck(chain, addresses, end)
	return err
}

// ibcBasisCheck gates the report on IBC refunds and returns the refund pairs
// that loadWalletRows reconciles, read from the same snapshot.
func (s *Server) ibcBasisCheck(chain string, addresses []string, end time.Time) (map[uint]string, error) {
	var events []tax.TaxableEvent
	if err := s.scopedTaxEvents(chain).Select("taxable_events.*").
		Where("(from_addr IN ? OR to_addr IN ?)", addresses, addresses).
		Where("category = ? OR reward_trigger IN ?", string(tax.CategoryIBCOut), []string{ibcTimeoutRefund, ibcErrorAckRefund}).
		Find(&events).Error; err != nil {
		return nil, err
	}
	if err := s.retainLegacyOutboundIdentities(events); err != nil {
		return nil, err
	}
	incomplete, err := s.incompleteIBCRefunds(chain, addresses)
	if err != nil {
		return nil, err
	}
	events = append(events, incomplete...)
	if readiness := ibcBasisReadiness(events, end); !readiness.ExportReady {
		return nil, reportReadinessRequired{readiness}
	}
	return matchIBCRefunds(events), nil
}

// Older outbound classifications omitted Detail. Recover identity from exact
// retained message events, not a fuzzy amount/time match, without DB mutation.
func (s *Server) retainLegacyOutboundIdentities(events []tax.TaxableEvent) error {
	owners := map[string]bool{}
	for _, e := range events {
		if isIBCRefund(e.RewardTrigger) {
			owners[e.ToAddr] = true
		}
	}
	var ids []uint
	seen := map[uint]bool{}
	for _, e := range events {
		if e.Category == string(tax.CategoryIBCOut) && tax.IBCPacketKey(e.Detail) == "" && owners[e.FromAddr] && !seen[e.MessageID] {
			ids = append(ids, e.MessageID)
			seen[e.MessageID] = true
		}
	}
	if len(ids) == 0 {
		return nil
	}
	type packetAttribute struct {
		MessageID uint
		EventID   uint
		Key       string
		Value     string
	}
	var attrs []packetAttribute
	const packetIdentityBatch = 32
	for start := 0; start < len(ids); start += packetIdentityBatch {
		end := start + packetIdentityBatch
		if end > len(ids) {
			end = len(ids)
		}
		var batch []packetAttribute
		if err := s.db.Table("message_events me").
			Select("me.message_id, me.id AS event_id, k.key, a.value").
			Joins("JOIN message_event_types et ON et.id=me.message_event_type_id").
			// Preserve per-event indexed reads instead of exposing all wallet
			// packet IDs to a single global attribute-table join.
			Joins("JOIN LATERAL (SELECT message_event_attribute_key_id, value, index FROM message_event_attributes WHERE message_event_id=me.id OFFSET 0) a ON true").
			Joins("JOIN message_event_attribute_keys k ON k.id=a.message_event_attribute_key_id").
			Where("me.message_id IN ? AND et.type = ?", ids[start:end], "send_packet").
			Order("me.id, a.index").Scan(&batch).Error; err != nil {
			return fmt.Errorf("outbound packet identities batch %d/%d: %w", start/packetIdentityBatch+1, (len(ids)+packetIdentityBatch-1)/packetIdentityBatch, err)
		}
		attrs = append(attrs, batch...)
	}
	logs := map[uint]*txlog.LogMessage{}
	var last uint
	for _, a := range attrs {
		if logs[a.MessageID] == nil {
			logs[a.MessageID] = &txlog.LogMessage{}
		}
		log := logs[a.MessageID]
		if a.EventID != last || len(log.Events) == 0 {
			log.Events = append(log.Events, txlog.LogMessageEvent{Type: "send_packet"})
		}
		last = a.EventID
		i := len(log.Events) - 1
		log.Events[i].Attributes = append(log.Events[i].Attributes, txlog.Attribute{Key: a.Key, Value: a.Value})
	}
	for i := range events {
		e := &events[i]
		if e.Category != string(tax.CategoryIBCOut) || tax.IBCPacketKey(e.Detail) != "" {
			continue
		}
		d := tax.OutboundPacketDetail(logs[e.MessageID], &transfertypes.MsgTransfer{})
		if d != nil {
			b, _ := json.Marshal(d)
			e.Detail = string(b)
		}
	}
	return nil
}
