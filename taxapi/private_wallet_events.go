package taxapi

// Fence the address lookup before the time filter, then resolve each distinct
// authoritative metadata ID once, in primary-key order. The lookup barriers
// prevent a scan of global metadata tables without assuming nonnullable FKs or
// trusting denormalized event heights. The final joins preserve event multiplicity.
const privateWalletTaxEventsSQL = `WITH address_events AS MATERIALIZED (
 SELECT * FROM taxable_events WHERE from_addr = ? OR to_addr = ?
), period_events AS MATERIALIZED (
 SELECT * FROM address_events WHERE timestamp >= ? AND timestamp < ?
), selected_message_ids AS MATERIALIZED (
 SELECT DISTINCT message_id AS id FROM period_events WHERE message_id IS NOT NULL ORDER BY message_id
), selected_messages AS MATERIALIZED (
 SELECT m.id, m.tx_id FROM selected_message_ids ids
 CROSS JOIN LATERAL (SELECT id, tx_id FROM messages WHERE id = ids.id OFFSET 0) m
), selected_tx_ids AS MATERIALIZED (
 SELECT DISTINCT tx_id AS id FROM selected_messages WHERE tx_id IS NOT NULL ORDER BY tx_id
), selected_txes AS MATERIALIZED (
 SELECT t.id, t.block_id FROM selected_tx_ids ids
 CROSS JOIN LATERAL (SELECT id, block_id FROM txes WHERE id = ids.id OFFSET 0) t
), selected_block_ids AS MATERIALIZED (
 SELECT DISTINCT block_id AS id FROM selected_txes WHERE block_id IS NOT NULL ORDER BY block_id
), selected_blocks AS MATERIALIZED (
 SELECT b.id, b.chain_id FROM selected_block_ids ids
 CROSS JOIN LATERAL (SELECT id, chain_id FROM blocks WHERE id = ids.id OFFSET 0) b
)
SELECT taxable_events.* FROM period_events taxable_events
JOIN selected_messages scope_m ON scope_m.id = taxable_events.message_id
JOIN selected_txes scope_t ON scope_t.id = scope_m.tx_id
JOIN selected_blocks scope_b ON scope_b.id = scope_t.block_id
JOIN chains scope_c ON scope_c.id = scope_b.chain_id
WHERE scope_c.chain_id IN ?
ORDER BY timestamp ASC, taxable_events.id ASC`
