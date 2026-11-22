package db

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The chain can include the same transaction bytes in more than one block: a
// copy the ante handler rejects (a wrong account sequence, for one) next to
// the copy that ran. txes.hash is unique, so every copy lands on one row, and
// the row must describe the copy that ran. A failed copy indexed after the
// successful one keeps the successful code and block; otherwise the
// transaction reads as failed at the earlier height with the successful
// copy's messages and events, and its fee as never charged. Between two
// failed copies, or a failed row and its successful copy, the newer write
// still wins, as before. The update always runs so the insert returns the
// row's id for the fee, signer and message rows that follow.
func txUpsertConflict() clause.OnConflict {
	keep := "txes.code = 0 AND EXCLUDED.code <> 0"
	return clause.OnConflict{
		Columns: []clause.Column{{Name: "hash"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"code":     gorm.Expr("CASE WHEN " + keep + " THEN txes.code ELSE EXCLUDED.code END"),
			"block_id": gorm.Expr("CASE WHEN " + keep + " THEN txes.block_id ELSE EXCLUDED.block_id END"),
		}),
	}
}
