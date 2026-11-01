package taxapi

import (
	"encoding/json"
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

// IBC refunds are not acquisitions. Until the FIFO engines can undo an exact
// send and restore its original lots, neither that disposal nor subsequent
// inventory is authoritative. Look beyond the requested year so a January
// refund cannot leave a December phantom gain certified by an old report.
func ibcBasisReadiness(events []tax.TaxableEvent, end time.Time) ReportReadiness {
	refunds := map[string]bool{}
	refundOwners := map[string]bool{}
	blocked := map[string]bool{}
	reasons := map[string]bool{}
	for _, e := range events {
		if !isIBCRefund(e.RewardTrigger) {
			continue
		}
		refundOwners[e.ToAddr] = true
		key := tax.IBCPacketKey(e.Detail)
		if key != "" {
			refunds[e.ToAddr+":"+key] = true
		}
		if e.Timestamp.Before(end) {
			blocked[e.ToAddr+":"+key+":"+e.TxHash] = true
			reasons[ibcRefundBasisRequired] = true
		}
	}
	for _, e := range events {
		if e.Category != string(tax.CategoryIBCOut) || !e.Timestamp.Before(end) {
			continue
		}
		key := tax.IBCPacketKey(e.Detail)
		if key != "" && refunds[e.FromAddr+":"+key] {
			blocked[e.FromAddr+":"+key] = true
			reasons[ibcRefundBasisRequired] = true
		} else if key == "" && refundOwners[e.FromAddr] {
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
	r.Scope = "requested chain and wallets, full persisted IBC history including refunds outside the requested period; packet/basis reconciliation is not implemented"
	return r
}

func (s *Server) requireIBCBasisReady(chain string, addresses []string, end time.Time) error {
	var events []tax.TaxableEvent
	if err := s.scopedTaxEvents(chain).Select("taxable_events.*").
		Where("(from_addr IN ? OR to_addr IN ?)", addresses, addresses).
		Where("category = ? OR reward_trigger IN ?", string(tax.CategoryIBCOut), []string{ibcTimeoutRefund, ibcErrorAckRefund}).
		Find(&events).Error; err != nil {
		return err
	}
	if err := s.retainLegacyOutboundIdentities(events); err != nil {
		return err
	}
	if readiness := ibcBasisReadiness(events, end); !readiness.ExportReady {
		return reportReadinessRequired{readiness}
	}
	return nil
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
	for _, e := range events {
		if e.Category == string(tax.CategoryIBCOut) && tax.IBCPacketKey(e.Detail) == "" && owners[e.FromAddr] {
			ids = append(ids, e.MessageID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var attrs []struct {
		MessageID uint
		EventID   uint
		Key       string
		Value     string
	}
	if err := s.db.Table("message_events me").
		Select("me.message_id, me.id AS event_id, k.key, a.value").
		Joins("JOIN message_event_types et ON et.id=me.message_event_type_id").
		Joins("JOIN message_event_attributes a ON a.message_event_id=me.id").
		Joins("JOIN message_event_attribute_keys k ON k.id=a.message_event_attribute_key_id").
		Where("me.message_id IN ? AND et.type = ?", ids, "send_packet").
		Order("me.id, a.index").Scan(&attrs).Error; err != nil {
		return err
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
