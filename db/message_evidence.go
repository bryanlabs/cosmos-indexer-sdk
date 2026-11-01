package db

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// An indexing pass without raw capture must not erase previously stored evidence.
func messageEvidenceUpdates() clause.Set {
	return clause.Assignments(map[string]interface{}{
		"message_type_id": gorm.Expr("EXCLUDED.message_type_id"),
		"message_bytes":   gorm.Expr("COALESCE(EXCLUDED.message_bytes, messages.message_bytes)"),
	})
}
