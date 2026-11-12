package store

import (
	"fmt"
	"regexp"
	"time"

	"gorm.io/gorm"
)

// Detail is what one inclusion carries beyond its identity: who signed, the
// fee when the chain is known to have charged it, and each message with the
// tax rows classified from it. It hangs off the inclusion's own txes row, so
// it can share an address, denom or message type row with cosmoshub-4 but can
// never share a transaction, message, fee or tax row with it.
type Detail struct {
	Signers  []string
	FeePayer string
	// Fee is empty unless the fee is known to have been charged.
	Fee      []Coin
	Messages []Message
}

type Coin struct{ Denom, Amount string }

// Message is one message in transaction order. TypeURL names the projection.
// No message bytes are stored: the Amino original stays in the capture.
type Message struct {
	TypeURL string
	Rows    []Row
}

// Row is one tax row, in the columns of the serving taxable_events table.
type Row struct {
	Category, Amount, Denom, Asset, From, To, Validator, Trigger, Detail string
}

var wholeNumber = regexp.MustCompile(`^[0-9]{1,78}$`)

// named returns the id of a shared naming row, inserting it only when absent.
// An existing row is read, never rewritten.
func named(tx *gorm.DB, table, column, value string) (uint, error) {
	if value == "" {
		return 0, fmt.Errorf("legacy detail has an empty %s", column)
	}
	if err := tx.Exec(fmt.Sprintf(`INSERT INTO %s (%s) VALUES (?) ON CONFLICT (%s) DO NOTHING`, table, column, column), value).Error; err != nil {
		return 0, err
	}
	var id uint
	if err := tx.Raw(fmt.Sprintf(`SELECT id FROM %s WHERE %s = ?`, table, column), value).Scan(&id).Error; err != nil || id == 0 {
		return 0, fmt.Errorf("legacy naming row unavailable")
	}
	return id, nil
}

// settled says whether a set of child rows is already stored. A partial set
// can only come from a different import, so it is a disagreement.
func settled(tx *gorm.DB, query string, want int, args ...any) (bool, error) {
	var count int64
	if err := tx.Raw(query, args...).Scan(&count).Error; err != nil {
		return false, err
	}
	if count != 0 && count != int64(want) {
		return false, ErrStoredDiffers
	}
	return count != 0, nil
}

func importDetail(tx *gorm.DB, txID uint, hash string, height int64, when time.Time, d Detail) error {
	// A set that is already stored is only compared. Inserting into it could
	// add a row beside the stored ones without the count changing first.
	have, err := settled(tx, `SELECT count(*) FROM tx_signer_addresses WHERE tx_id = ?`, len(d.Signers), txID)
	if err != nil {
		return err
	}
	for _, signer := range d.Signers {
		id, err := named(tx, "addresses", "address", signer)
		if err != nil {
			return err
		}
		if !have {
			if err := tx.Exec(`INSERT INTO tx_signer_addresses (tx_id, address_id) VALUES (?, ?) ON CONFLICT DO NOTHING`, txID, id).Error; err != nil {
				return err
			}
		}
		if ok, err := settled(tx, `SELECT count(*) FROM tx_signer_addresses WHERE tx_id = ? AND address_id = ?`, 1, txID, id); err != nil || !ok {
			return ErrStoredDiffers
		}
	}
	if have, err = settled(tx, `SELECT count(*) FROM fees WHERE tx_id = ?`, len(d.Fee), txID); err != nil {
		return err
	}
	for _, coin := range d.Fee {
		if !wholeNumber.MatchString(coin.Amount) {
			return fmt.Errorf("legacy fee amount must be a whole number")
		}
		payer, err := named(tx, "addresses", "address", d.FeePayer)
		if err != nil {
			return err
		}
		denom, err := named(tx, "denoms", "base", coin.Denom)
		if err != nil {
			return err
		}
		if !have {
			if err := tx.Exec(`INSERT INTO fees (tx_id, amount, denomination_id, payer_address_id) VALUES (?, ?::numeric, ?, ?) ON CONFLICT (tx_id, denomination_id) DO NOTHING`, txID, coin.Amount, denom, payer).Error; err != nil {
				return err
			}
		}
		if ok, err := settled(tx, `SELECT count(*) FROM fees WHERE tx_id = ? AND denomination_id = ? AND amount = ?::numeric AND payer_address_id = ?`, 1, txID, denom, coin.Amount, payer); err != nil || !ok {
			return ErrStoredDiffers
		}
	}
	if _, err := settled(tx, `SELECT count(*) FROM messages WHERE tx_id = ?`, len(d.Messages), txID); err != nil {
		return err
	}
	for index, message := range d.Messages {
		kind, err := named(tx, "message_types", "message_type", message.TypeURL)
		if err != nil {
			return err
		}
		if err := tx.Exec(`INSERT INTO messages (tx_id, message_type_id, message_index, opaque) VALUES (?, ?, ?, false) ON CONFLICT (tx_id, message_index) DO NOTHING`, txID, kind, index).Error; err != nil {
			return err
		}
		var stored struct {
			ID            uint
			MessageTypeID uint
		}
		if err := tx.Raw(`SELECT id, message_type_id FROM messages WHERE tx_id = ? AND message_index = ?`, txID, index).Scan(&stored).Error; err != nil || stored.ID == 0 {
			return fmt.Errorf("legacy message row unavailable")
		}
		if stored.MessageTypeID != kind {
			return ErrStoredDiffers
		}
		if _, err := settled(tx, `SELECT count(*) FROM taxable_events WHERE message_id = ?`, len(message.Rows), stored.ID); err != nil {
			return err
		}
		for sub, row := range message.Rows {
			if row.Amount != "" && !wholeNumber.MatchString(row.Amount) {
				return fmt.Errorf("legacy tax row amount must be a whole number")
			}
			if err := tx.Exec(`INSERT INTO taxable_events (message_id, sub_index, category, amount, denom, asset, from_addr, to_addr, validator_address, reward_trigger, detail, block_height, timestamp, tx_hash)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (message_id, sub_index) DO NOTHING`,
				stored.ID, sub, row.Category, row.Amount, row.Denom, row.Asset, row.From, row.To, row.Validator, row.Trigger, row.Detail, height, when, hash).Error; err != nil {
				return err
			}
			if ok, err := settled(tx, `SELECT count(*) FROM taxable_events WHERE message_id = ? AND sub_index = ? AND category = ? AND amount = ? AND denom = ? AND asset = ?
AND from_addr = ? AND to_addr = ? AND validator_address = ? AND reward_trigger = ? AND detail = ? AND block_height = ? AND timestamp = ? AND tx_hash = ?`, 1,
				stored.ID, sub, row.Category, row.Amount, row.Denom, row.Asset, row.From, row.To, row.Validator, row.Trigger, row.Detail, height, when, hash); err != nil || !ok {
				return ErrStoredDiffers
			}
		}
	}
	return nil
}
