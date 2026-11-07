package taxapi

import "hash"

// Hash full-history retained evidence, not merely current gate counts. Packet
// events can change legacy identity without changing any financial row, and an
// opaque/parser failure can replace a formerly complete transaction. Block JSON
// also carries classification completion state when the writer publishes it.
// Materializing wallet attribution once avoids a correlated scan per child row.
func (s *Server) writeClassificationFingerprint(h hash.Hash, chain string, addresses []string) error {
	q := s.db.Table("txes t").
		Joins("JOIN blocks b ON b.id=t.block_id").
		Joins("JOIN chains c ON c.id=b.chain_id").
		Where("c.chain_id IN ?", chainIDs(chain))
	q = walletTxScope(q, addresses).Select("t.id, t.hash, t.code, t.block_id, to_jsonb(b) AS block_evidence, c.chain_id")
	rows, err := s.db.Raw(`WITH wallet_t AS MATERIALIZED (?)
 SELECT kind, exact FROM (
 SELECT 'transaction' AS kind, to_jsonb(w)::text AS exact FROM wallet_t w
 UNION ALL
 SELECT 'message', to_jsonb(m)::text FROM wallet_t w JOIN messages m ON m.tx_id=w.id
 UNION ALL
 SELECT 'parser', to_jsonb(pe)::text FROM wallet_t w JOIN messages m ON m.tx_id=w.id JOIN message_parser_errors pe ON pe.message_id=m.id
 UNION ALL
 SELECT 'failed_message', to_jsonb(fm)::text FROM wallet_t w JOIN failed_messages fm ON fm.tx_id=w.id
 UNION ALL
 SELECT 'failed_transaction', to_jsonb(ft)::text FROM wallet_t w JOIN failed_txes ft ON ft.block_id=w.block_id AND ft.hash=w.hash
 UNION ALL
 SELECT 'packet_event', jsonb_build_array(to_jsonb(me), to_jsonb(et))::text
 FROM wallet_t w JOIN messages m ON m.tx_id=w.id JOIN message_events me ON me.message_id=m.id JOIN message_event_types et ON et.id=me.message_event_type_id
 UNION ALL
 SELECT 'packet_attribute', jsonb_build_array(to_jsonb(a), to_jsonb(k))::text
 FROM wallet_t w JOIN messages m ON m.tx_id=w.id JOIN message_events me ON me.message_id=m.id
 JOIN message_event_attributes a ON a.message_event_id=me.id JOIN message_event_attribute_keys k ON k.id=a.message_event_attribute_key_id
 ) evidence ORDER BY kind, exact`, q).Rows()
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, exact string
		if err := rows.Scan(&kind, &exact); err != nil {
			return err
		}
		writeFinancialFingerprintRow(h, kind, exact)
	}
	return rows.Err()
}
