package taxapi

import (
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
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
	if err := db.AutoMigrate(&TxInclusion{}, &IndexHoleClearance{}); err != nil {
		return err
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
