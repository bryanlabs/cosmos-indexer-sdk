package tax

import (
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"gorm.io/gorm"
)

// InvalidateMessage prevents an incomplete replay from leaving prior tax rows
// looking current. The caller records the failure in the same transaction.
func (p *Parser) InvalidateMessage(database *gorm.DB, message models.Message) error {
	return database.Where("message_id = ?", message.ID).Delete(&TaxableEvent{}).Error
}
