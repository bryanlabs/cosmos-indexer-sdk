package parsers

import (
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"gorm.io/gorm"
)

// MessageParseFailureInvalidator removes stale derived data when a replacement
// parse is incomplete. Raw message evidence and its parser error are retained.
type MessageParseFailureInvalidator interface {
	InvalidateMessage(*gorm.DB, models.Message) error
}
