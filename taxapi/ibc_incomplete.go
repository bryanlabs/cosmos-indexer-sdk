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
	query := s.db.Table("messages m").
		Select("m.id, mt.message_type AS type, m.message_bytes AS raw, b.time_stamp AS timestamp, t.hash").
		Joins("JOIN message_types mt ON mt.id=m.message_type_id").
		Joins("JOIN txes t ON t.id=m.tx_id").
		Joins("JOIN blocks b ON b.id=t.block_id").
		Joins("JOIN chains c ON c.id=b.chain_id").
		Where("c.chain_id IN ? AND t.code=0 AND mt.message_type IN ?", chainIDs(chain), tax.RefundLifecycleTypes()).
		Where(`m.opaque = true
 OR EXISTS (SELECT 1 FROM message_parser_errors e WHERE e.message_id=m.id)
 OR EXISTS (SELECT 1 FROM failed_messages fm WHERE fm.tx_id=t.id)
 OR EXISTS (SELECT 1 FROM failed_txes ft WHERE ft.hash=t.hash AND ft.block_id=t.block_id)`)
	if len(addresses) > 0 {
		query = walletTxScope(query, addresses)
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
