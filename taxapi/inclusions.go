package taxapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Named query parameters the ledger's reads share.
const (
	qpIDs  = "ids"
	qpAddr = "addr"
)

// TxInclusion is one inclusion of a transaction's bytes in a block. The chain
// can include the same bytes more than once (a copy the ante handler turned
// away, then the copy that ran, or the other way round), but txes keeps one
// row per hash, and the indexer's upsert leaves that row at whichever copy it
// wrote last. The row's messages, events and declared fee then belong to the
// copy that ran while the row is dated at a copy that changed nothing.
//
// At most one inclusion of a hash is Effective: the copy the ante handler let
// through, which ran the messages (code 0) or failed after paying the fee.
// Every other inclusion of the same bytes fails the ante handler, since the
// signers' sequences were already used, and changes nothing. The balance
// ledger dates the row's effects at the effective inclusion's block and takes
// its outcome (code, charged fee, fee granter) from it. Rows are only added;
// the txes row itself is never rewritten.
type TxInclusion struct {
	Chain   string `gorm:"primaryKey"`
	Height  int64  `gorm:"primaryKey;autoIncrement:false"`
	TxIndex int    `gorm:"primaryKey;autoIncrement:false"`
	Hash    string `gorm:"not null;index"`
	// TxID is the txes row that holds the hash, wherever it is dated.
	TxID    uint  `gorm:"not null"`
	BlockID uint  `gorm:"not null"`
	Code    int64 `gorm:"not null"`
	// Codespace is the failure's codespace, empty on success.
	Codespace string `gorm:"not null;default:''"`
	Effective bool   `gorm:"not null"`
	// Charged is what a failed effective inclusion's fee deduction moved;
	// empty on success, where the declared fee was paid.
	Charged string `gorm:"not null;default:''"`
	// Granter is the account whose fee grant paid the effective inclusion's
	// fee, if any.
	Granter    string    `gorm:"not null;default:''"`
	RecordedAt time.Time `gorm:"not null"`
}

func (TxInclusion) TableName() string { return "tax_tx_inclusions" }

// IndexHoleClearance records that a hole's block was proven complete after
// its repeated inclusions were placed: every transaction the chain ran in it
// has an index row effective at that block with the chain's outcome, and
// every account the hole names rebuilds across the block to the chain's own
// state. The hole row itself is kept; the ledger skips a cleared one.
type IndexHoleClearance struct {
	Chain     string    `gorm:"primaryKey"`
	Height    int64     `gorm:"primaryKey;autoIncrement:false"`
	ChainTxs  int       `gorm:"not null"`
	Placed    int       `gorm:"not null"`
	Accounts  int       `gorm:"not null"`
	ClearedAt time.Time `gorm:"not null"`
}

func (IndexHoleClearance) TableName() string { return "tax_index_hole_clearances" }

// inclusionsMigrate creates the inclusion and clearance tables. A hash has at
// most one effective inclusion, and so does each txes row.
func inclusionsMigrate(db *gorm.DB) error {
	// Created only when missing, found by name on the search path as the
	// readers find it; an existing table is never altered.
	for _, m := range []interface{ TableName() string }{TxInclusion{}, IndexHoleClearance{}} {
		var exists bool
		if err := db.Raw("SELECT to_regclass(?) IS NOT NULL", m.TableName()).Scan(&exists).Error; err != nil {
			return err
		}
		if !exists {
			if err := db.Migrator().CreateTable(m); err != nil {
				return err
			}
		}
	}
	if err := db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_tax_tx_inclusions_effective_tx ON tax_tx_inclusions (tx_id) WHERE effective").Error; err != nil {
		return err
	}
	return db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_tax_tx_inclusions_effective_hash ON tax_tx_inclusions (chain, hash) WHERE effective").Error
}

// inclusionPreview replaces the inclusion and clearance tables in the ledger's
// reads with planned rows, so a dry run rebuilds balances exactly as they
// would read once the plan is applied, without writing anything. It is set
// only on a Server a repair run owns.
type inclusionPreview struct {
	effective []TxInclusion
	cleared   map[int64]bool
}

// effectiveRel is the relation (tx_id, block_id, code, charged, granter) of
// effective inclusions the ledger dates a txes row by, with the parameters it
// needs. A row with no entry is dated at its own block.
func (s *Server) effectiveRel() (string, map[string]any) {
	p := s.preview
	if p == nil {
		return "(SELECT tx_id, block_id, code, charged, granter FROM tax_tx_inclusions WHERE effective)", map[string]any{}
	}
	if len(p.effective) == 0 {
		return "(SELECT NULL::bigint AS tx_id, NULL::bigint AS block_id, NULL::bigint AS code, NULL::text AS charged, NULL::text AS granter WHERE false)", map[string]any{}
	}
	type row struct {
		TxID    uint   `json:"tx_id"`
		BlockID uint   `json:"block_id"`
		Code    int64  `json:"code"`
		Charged string `json:"charged"`
		Granter string `json:"granter"`
	}
	rows := make([]row, len(p.effective))
	for i, r := range p.effective {
		rows[i] = row{r.TxID, r.BlockID, r.Code, r.Charged, r.Granter}
	}
	raw, _ := json.Marshal(rows)
	return "(SELECT * FROM jsonb_to_recordset(CAST(@pv_rows AS jsonb)) AS pv(tx_id bigint, block_id bigint, code bigint, charged text, granter text))",
		map[string]any{"pv_rows": string(raw)}
}

// clearedHoleCond is a condition on hole alias h that excludes cleared holes.
func (s *Server) clearedHoleCond(h string) (string, map[string]any) {
	p := s.preview
	if p == nil {
		return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM tax_index_hole_clearances c WHERE c.chain = %[1]s.chain AND c.height = %[1]s.height)", h), map[string]any{}
	}
	if len(p.cleared) == 0 {
		return "true", map[string]any{}
	}
	heights := make([]int64, 0, len(p.cleared))
	for k := range p.cleared {
		heights = append(heights, k)
	}
	return h + ".height NOT IN @pv_cleared", map[string]any{"pv_cleared": heights}
}

// withParams merges query parameter maps; later maps win.
func withParams(ms ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// datedBranches dates the txes rows x a query reads at their effective
// inclusion's block, as b. q is one SELECT with {{BLOCK}} where b is joined
// and {{OWN}} at the end of its WHERE clause; {{P}}, where given, joins p,
// the effective inclusion at the row's own block. The result is two branches:
// rows at their own block that no effective inclusion dates elsewhere, then
// rows one does, joined from that inclusion. Neither branch puts an outer
// join in the path a wallet-driven plan takes, so the API's reads keep the
// plans the indexes serve.
//
// hit, when given, is the wallet match as the own branch reads it (through
// the wallet's attribute index) and as the placed branch reads it (checked
// on each event of the few placed rows), put at {{HIT}}.
func datedBranches(q, rel string, hit ...string) string {
	ownHit, placedHit := "", ""
	if len(hit) == 2 {
		ownHit, placedHit = hit[0], hit[1]
	}
	own := strings.NewReplacer(
		"{{BLOCK}}", "JOIN blocks b ON b.id = x.block_id",
		"{{P}}", "LEFT JOIN "+rel+" p ON p.tx_id = x.id",
		"{{OWN}}", "AND NOT EXISTS (SELECT 1 FROM "+rel+" po WHERE po.tx_id = x.id AND po.block_id <> x.block_id)",
		"{{HIT}}", ownHit,
	).Replace(q)
	placed := strings.NewReplacer(
		"{{BLOCK}}", "JOIN "+rel+" p ON p.tx_id = x.id AND p.block_id <> x.block_id\nJOIN blocks b ON b.id = p.block_id",
		"{{P}}", "",
		"{{OWN}}", "",
		"{{HIT}}", placedHit,
	).Replace(q)
	return own + "\nUNION ALL\n" + placed
}

// windowTxs is the relation (tx_id, height, time_stamp) of txes rows dated in
// (@lo, @hi] on @ids, read from the window's blocks: rows at their own block
// with no effective inclusion elsewhere, then rows an effective inclusion
// dates there.
func windowTxs(rel string) string {
	return `(SELECT x.id AS tx_id, b.height, b.time_stamp FROM blocks b
  JOIN txes x ON x.block_id = b.id
  WHERE b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi
  AND NOT EXISTS (SELECT 1 FROM ` + rel + ` p WHERE p.tx_id = x.id AND p.block_id <> x.block_id)
  UNION ALL
  SELECT p.tx_id, b.height, b.time_stamp FROM ` + rel + ` p
  JOIN blocks b ON b.id = p.block_id
  JOIN txes x ON x.id = p.tx_id
  WHERE b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi AND p.block_id <> x.block_id)`
}

// blockReadQueries are ledgerEvents' message event, block event and
// unattributed staking message reads for a window a few blocks wide, driven from the
// window's blocks. Each event's attributes are read by event id and matched to
// the wallet there, never through the wallet's attribute index. The
// materialized steps keep the planner from turning that around.
func blockReadQueries(rel string) (msgQuery, blockQuery, redelQuery string) {
	events := func(types string) string {
		return `ev AS MATERIALIZED (
  SELECT me.id, me.message_id, me.index, w.height, w.time_stamp, t.type FROM ` + windowTxs(rel) + ` w
  JOIN messages m ON m.tx_id = w.tx_id
  JOIN message_events me ON me.message_id = m.id
  JOIN message_event_types t ON t.id = me.message_event_type_id
  WHERE ` + types + `
), attrs AS MATERIALIZED (
  SELECT ev.id, ev.type, a.index, a.message_event_attribute_key_id AS key_id, a.value FROM ev
  JOIN message_event_attributes a ON a.message_event_id = ev.id
), hit AS MATERIALIZED (
  SELECT id FROM attrs GROUP BY id, type
  HAVING bool_or(value = @addr) OR (type = 'create_validator' AND bool_or(value = @op))
)`
	}
	msgQuery = `
WITH ` + events("(t.type IN @types OR t.type = 'create_validator')") + `
SELECT ev.id AS event_id, ev.message_id, ev.height, ev.time_stamp, 1 AS phase, ev.type, k.key, x.value
FROM ev JOIN hit ON hit.id = ev.id
JOIN attrs x ON x.id = ev.id
JOIN message_event_attribute_keys k ON k.id = x.key_id
ORDER BY ev.height, ev.id, x.index`
	blockQuery = `
WITH ev AS MATERIALIZED (
  SELECT be.id, be.lifecycle_position, b.height, b.time_stamp, t.type FROM blocks b
  JOIN block_events be ON be.block_id = b.id
  JOIN block_event_types t ON t.id = be.block_event_type_id
  WHERE b.chain_id IN @ids AND b.height > @lo AND b.height <= @hi AND t.type IN @types
), attrs AS MATERIALIZED (
  SELECT ev.id, a.index, a.block_event_attribute_key_id AS key_id, a.value FROM ev
  JOIN block_event_attributes a ON a.block_event_id = ev.id
), hit AS MATERIALIZED (
  SELECT id FROM attrs GROUP BY id HAVING bool_or(value = @addr)
)
SELECT ev.id AS event_id, ev.height, ev.time_stamp, CASE WHEN ev.lifecycle_position = 0 THEN 0 ELSE 2 END AS phase, ev.type, k.key, x.value
FROM ev JOIN hit ON hit.id = ev.id
JOIN attrs x ON x.id = ev.id
JOIN block_event_attribute_keys k ON k.id = x.key_id
ORDER BY ev.height, ev.id, x.index`
	redelQuery = `
WITH ` + events("t.type IN ('withdraw_rewards', 'message')") + `
, cand AS MATERIALIZED (
  SELECT DISTINCT ev.message_id FROM ev JOIN attrs x ON x.id = ev.id
  WHERE x.value = @addr
  AND EXISTS (` + unnamedStakingEvent("ev.message_id") + `)
)
SELECT message_id FROM cand c WHERE ` + walletStakeMessage("c")
	return msgQuery, blockQuery, redelQuery
}
