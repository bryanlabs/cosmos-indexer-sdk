package taxapi

import (
	"time"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	"github.com/DefiantLabs/cosmos-indexer/tax"
)

// Incomplete lifecycle messages remain basis obligations even when no financial
// row survived classification. In particular, a next-year refund cannot silently
// certify an earlier disposal just because the parser correctly removed its row.
func (s *Server) incompleteIBCRefunds(chain string, addresses []string) ([]tax.TaxableEvent, error) {
	var messages []struct {
		ID        uint
		Type      string
		Raw       []byte
		Timestamp time.Time
		Hash      string
	}
	// Incomplete means opaque, a parser error, or a failed message or failed
	// transaction record in the same transaction. Starting from those sparse
	// sets avoids testing every acknowledgement on the chain, and the OFFSET 0
	// fence runs the wallet checks only on the few candidates left.
	candidates := s.db.Table("messages m").
		Select("m.id AS message_id, mt.message_type AS type, m.message_bytes AS raw, b.time_stamp AS timestamp, t.hash, t.id").
		Joins("JOIN message_types mt ON mt.id=m.message_type_id").
		Joins("JOIN txes t ON t.id=m.tx_id").
		Joins("JOIN blocks b ON b.id=t.block_id").
		Joins("JOIN chains c ON c.id=b.chain_id").
		Where("c.chain_id IN ? AND t.code=0 AND mt.message_type IN ?", chainIDs(chain), tax.RefundLifecycleTypes()).
		Where(`m.id IN (
 SELECT id FROM messages WHERE opaque = true
 UNION SELECT message_id FROM message_parser_errors
 UNION SELECT fm_m.id FROM failed_messages fm JOIN messages fm_m ON fm_m.tx_id=fm.tx_id
 UNION SELECT ft_m.id FROM failed_txes ft JOIN txes ft_t ON ft_t.hash=ft.hash AND ft_t.block_id=ft.block_id JOIN messages ft_m ON ft_m.tx_id=ft_t.id
 )`)
	query := s.db.Table("(? OFFSET 0) t", candidates).Select("t.message_id AS id, t.type, t.raw, t.timestamp, t.hash")
	if len(addresses) > 0 {
		query = walletFewTxScope(query, addresses)
	}
	if err := query.Find(&messages).Error; err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, nil
	}
	ids := make([]uint, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	var attrs []struct {
		MessageID uint
		EventID   uint
		Type      string
		Key       string
		Value     string
	}
	if err := s.db.Table("message_events me").
		Select("me.message_id, me.id AS event_id, et.type, k.key, a.value").
		Joins("JOIN message_event_types et ON et.id=me.message_event_type_id").
		Joins("JOIN message_event_attributes a ON a.message_event_id=me.id").
		Joins("JOIN message_event_attribute_keys k ON k.id=a.message_event_attribute_key_id").
		Where("me.message_id IN ?", ids).Order("me.message_id, me.index, a.index").Scan(&attrs).Error; err != nil {
		return nil, err
	}
	logs := map[uint]*txlog.LogMessage{}
	var lastEvent uint
	for _, attr := range attrs {
		if logs[attr.MessageID] == nil {
			logs[attr.MessageID] = &txlog.LogMessage{}
		}
		log := logs[attr.MessageID]
		if lastEvent != attr.EventID || len(log.Events) == 0 {
			log.Events = append(log.Events, txlog.LogMessageEvent{Type: attr.Type})
		}
		lastEvent = attr.EventID
		i := len(log.Events) - 1
		log.Events[i].Attributes = append(log.Events[i].Attributes, txlog.Attribute{Key: attr.Key, Value: attr.Value})
	}
	owners := map[string]bool{}
	for _, address := range addresses {
		owners[address] = true
	}
	var out []tax.TaxableEvent
	for _, message := range messages {
		for _, dependency := range tax.UnresolvedRefundDependencies(message.Type, message.Raw, logs[message.ID]) {
			if len(owners) != 0 && !owners[dependency.Owner] {
				continue
			}
			out = append(out, tax.TaxableEvent{
				ToAddr: dependency.Owner, Detail: dependency.Detail,
				Timestamp: message.Timestamp, TxHash: message.Hash, RewardTrigger: ibcTimeoutRefund,
			})
		}
	}
	return out, nil
}
