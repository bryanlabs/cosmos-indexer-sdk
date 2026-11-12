// Package store is the offline prototype of inclusion-scoped persistence for
// Cosmos Hub 1 to 3. It is additive: one new table, tx_inclusions, and an
// optional guard trigger. Signers, fees, messages and tax rows go into the
// existing tables as new rows under the inclusion's own txes row. Nothing here is wired into the serving indexer.
//
// The serving schema identifies a transaction by txes.hash alone, and the
// generic indexer upserts on that hash, overwriting code and block_id. Heights
// restart on every hub and the same bytes can be included more than once, so
// a hash is not an identity for these chains. This writer therefore:
//
//   - identifies an inclusion by (chain, height, index) in tx_inclusions and
//     keeps the true transaction hash there as a plain, non-unique column;
//   - gives each inclusion its own txes row whose hash column holds the
//     inclusion key "chain/height/index". A key always contains "/", which no
//     hex transaction hash can, so it can never conflict with a hash-keyed row;
//   - only ever inserts. It issues no UPDATE and no hash upsert, and a rerun
//     that disagrees with what is stored fails instead of rewriting it.
package store

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/legacy"
	"gorm.io/gorm"
)

// TxInclusion is one appearance of a transaction in a block. Hash is evidence
// and deliberately not unique: repeated and cross-chain inclusions of the same
// bytes each keep their own row, code and block.
type TxInclusion struct {
	ID           uint64 `gorm:"primaryKey"`
	ChainID      uint   `gorm:"not null;uniqueIndex:idx_tx_inclusions_identity,priority:1"`
	Height       int64  `gorm:"not null;uniqueIndex:idx_tx_inclusions_identity,priority:2"`
	TxIndex      int    `gorm:"not null;uniqueIndex:idx_tx_inclusions_identity,priority:3"`
	Hash         string `gorm:"not null;index:idx_tx_inclusions_hash"`
	Code         uint32 `gorm:"not null"`
	Codespace    string `gorm:"not null;default:''"`
	CodeEvidence string `gorm:"not null;default:''"`
	Encoding     string `gorm:"not null;default:''"`
	BlockID      uint   `gorm:"not null;index:idx_tx_inclusions_block"`
	// TxID is the txes row that carries this inclusion's messages and fees.
	// It is not unique so a later change can attach several inclusions of a
	// hash-keyed row without another migration.
	TxID uint `gorm:"not null;index:idx_tx_inclusions_tx"`
}

func (TxInclusion) TableName() string { return "tx_inclusions" }

// Inclusion is the writer's input for one transaction in a legacy block.
type Inclusion struct {
	Index        int
	Hash         string
	Code         uint32
	Codespace    string
	CodeEvidence string
	// Detail is optional. An inclusion stored without it holds identity and
	// result only.
	Detail *Detail
}

// Block is one legacy height with every transaction it included, in order.
type Block struct {
	Chain      string
	Height     int64
	Time       time.Time
	Inclusions []Inclusion
}

var transactionHash = regexp.MustCompile(`^[0-9A-F]{64}$`)

// ErrStoredDiffers reports a rerun whose content disagrees with what an
// earlier import stored for the same identity. Nothing is rewritten.
var ErrStoredDiffers = errors.New("stored legacy inclusion differs from this import")

// Migrate creates tx_inclusions. It adds no column to and rewrites no row of
// an existing table.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&TxInclusion{})
}

// InstallCrossChainGuard adds a trigger that refuses to move a txes row to a
// block on a different chain. The legacy writer never updates txes, so this is
// a second, independent barrier against any other writer (for example the
// generic hash upsert pointed at a legacy chain by mistake). It reads two
// blocks rows by primary key, and only when block_id actually changes.
func InstallCrossChainGuard(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`
CREATE OR REPLACE FUNCTION txes_block_chain_guard() RETURNS trigger AS $$
BEGIN
  IF NEW.block_id IS DISTINCT FROM OLD.block_id AND
     (SELECT chain_id FROM blocks WHERE id = OLD.block_id) IS DISTINCT FROM
     (SELECT chain_id FROM blocks WHERE id = NEW.block_id) THEN
    RAISE EXCEPTION 'a txes row may not move to a block on another chain'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql`).Error; err != nil {
			return err
		}
		if err := tx.Exec(`DROP TRIGGER IF EXISTS txes_block_chain_guard ON txes`).Error; err != nil {
			return err
		}
		return tx.Exec(`CREATE TRIGGER txes_block_chain_guard BEFORE UPDATE OF block_id ON txes
FOR EACH ROW EXECUTE FUNCTION txes_block_chain_guard()`).Error
	})
}

// ImportBlock stores one legacy block and its inclusions in a single database
// transaction. It leaves tx_indexed and block_events_indexed false: an
// inclusion row is not an indexed transaction, and nothing may read these
// heights as covered until messages, fees and events are stored too.
func ImportBlock(db *gorm.DB, block Block) error {
	if _, err := legacy.InclusionKey(block.Chain, block.Height, 0); err != nil {
		return err
	}
	if block.Time.IsZero() {
		return fmt.Errorf("legacy block time missing")
	}
	for i, inc := range block.Inclusions {
		if inc.Index != i {
			return fmt.Errorf("legacy inclusions must be complete and in block order")
		}
		if !transactionHash.MatchString(inc.Hash) {
			return fmt.Errorf("legacy inclusion hash must be 64 upper case hex characters")
		}
	}
	// Postgres keeps microseconds. Truncate first so a rerun compares equal.
	blockTime := block.Time.UTC().Truncate(time.Microsecond)
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`INSERT INTO chains (chain_id, name) VALUES (?, ?) ON CONFLICT (chain_id) DO NOTHING`, block.Chain, block.Chain).Error; err != nil {
			return err
		}
		var chainID uint
		if err := tx.Raw(`SELECT id FROM chains WHERE chain_id = ?`, block.Chain).Scan(&chainID).Error; err != nil || chainID == 0 {
			return fmt.Errorf("legacy chain row unavailable")
		}
		if err := tx.Exec(`INSERT INTO blocks (time_stamp, height, chain_id, tx_indexed, block_events_indexed)
VALUES (?, ?, ?, false, false) ON CONFLICT (height, chain_id) DO NOTHING`, blockTime, block.Height, chainID).Error; err != nil {
			return err
		}
		var stored struct {
			ID        uint
			TimeStamp time.Time
		}
		if err := tx.Raw(`SELECT id, time_stamp FROM blocks WHERE chain_id = ? AND height = ?`, chainID, block.Height).Scan(&stored).Error; err != nil || stored.ID == 0 {
			return fmt.Errorf("legacy block row unavailable")
		}
		if !stored.TimeStamp.Equal(blockTime) {
			return ErrStoredDiffers
		}
		var count int64
		if err := tx.Raw(`SELECT count(*) FROM tx_inclusions WHERE chain_id = ? AND height = ?`, chainID, block.Height).Scan(&count).Error; err != nil {
			return err
		}
		if count != 0 && count != int64(len(block.Inclusions)) {
			return ErrStoredDiffers
		}
		for _, inc := range block.Inclusions {
			key, err := legacy.InclusionKey(block.Chain, block.Height, inc.Index)
			if err != nil {
				return err
			}
			if err := tx.Exec(`INSERT INTO txes (hash, code, block_id, memo) VALUES (?, ?, ?, '') ON CONFLICT (hash) DO NOTHING`, key, inc.Code, stored.ID).Error; err != nil {
				return err
			}
			var row struct {
				ID      uint
				Code    uint32
				BlockID uint
			}
			if err := tx.Raw(`SELECT id, code, block_id FROM txes WHERE hash = ?`, key).Scan(&row).Error; err != nil || row.ID == 0 {
				return fmt.Errorf("legacy transaction row unavailable")
			}
			if row.Code != inc.Code || row.BlockID != stored.ID {
				return ErrStoredDiffers
			}
			want := TxInclusion{ChainID: chainID, Height: block.Height, TxIndex: inc.Index, Hash: inc.Hash, Code: inc.Code,
				Codespace: inc.Codespace, CodeEvidence: inc.CodeEvidence, Encoding: legacy.Encoding, BlockID: stored.ID, TxID: row.ID}
			if err := tx.Exec(`INSERT INTO tx_inclusions (chain_id, height, tx_index, hash, code, codespace, code_evidence, encoding, block_id, tx_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (chain_id, height, tx_index) DO NOTHING`,
				want.ChainID, want.Height, want.TxIndex, want.Hash, want.Code, want.Codespace, want.CodeEvidence, want.Encoding, want.BlockID, want.TxID).Error; err != nil {
				return err
			}
			var got TxInclusion
			if err := tx.Raw(`SELECT * FROM tx_inclusions WHERE chain_id = ? AND height = ? AND tx_index = ?`, chainID, block.Height, inc.Index).Scan(&got).Error; err != nil {
				return err
			}
			want.ID = got.ID
			if got != want {
				return ErrStoredDiffers
			}
			if inc.Detail != nil {
				if err := importDetail(tx, row.ID, inc.Hash, block.Height, blockTime, *inc.Detail); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
