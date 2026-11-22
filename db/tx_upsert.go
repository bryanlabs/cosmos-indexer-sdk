package db

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The chain can include the same transaction bytes in more than one block: a
// copy the ante handler rejects (a wrong account sequence, for one) next to
// the copy that ran. txes.hash is unique, so every copy lands on one row, and
// the row must describe the copy that ran. Only two writes change a stored
// row: a re-index of the block it already names, and the successful copy
// replacing a failed one. A failed copy never replaces the row, whether the
// stored copy ran or also failed, so re-indexing one block cannot move a
// transaction stored at another height. Before this, the newer write won, and
// a rejected copy indexed after the successful one made the transaction read
// as failed at the wrong height with its fee never charged. The update always
// runs so the insert returns the row's id for the fee, signer and message rows
// that follow.
func txUpsertConflict() clause.OnConflict {
	take := "EXCLUDED.block_id = txes.block_id OR (EXCLUDED.code = 0 AND txes.code <> 0)"
	return clause.OnConflict{
		Columns: []clause.Column{{Name: "hash"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"code":     gorm.Expr("CASE WHEN " + take + " THEN EXCLUDED.code ELSE txes.code END"),
			"block_id": gorm.Expr("CASE WHEN " + take + " THEN EXCLUDED.block_id ELSE txes.block_id END"),
		}),
	}
}
