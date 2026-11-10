package taxapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RecheckIndexHolesOptions defaults to a read-only dry run. Apply requires a
// new backup file on durable storage. Backup records contain wallet addresses
// and must never be logged or committed.
type RecheckIndexHolesOptions struct {
	Apply      bool
	BackupPath string
}

// indexHoleChange records the original rows and the proposed replacement.
// A durable record precedes the database commit, so a record is a plan, not
// proof that a write committed. Compare the database with After to reconcile.
type indexHoleChange struct {
	Version         int                `json:"version"`
	PlannedAt       time.Time          `json:"planned_at"`
	Apply           bool               `json:"apply"`
	Before          IndexHole          `json:"before"`
	BeforeAddresses []IndexHoleAddress `json:"before_addresses"`
	After           *IndexHole         `json:"after"`
	AfterAddresses  []IndexHoleAddress `json:"after_addresses"`
}

func openIndexHoleBackup(opts RecheckIndexHolesOptions) (*os.File, error) {
	if opts.BackupPath == "" {
		if opts.Apply {
			return nil, errors.New("applying an index-hole recheck requires --backup on durable storage")
		}
		return nil, nil
	}
	f, err := os.OpenFile(opts.BackupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create index-hole backup: %w", err)
	}
	// Persist the directory entry too, before allowing any database writes.
	dir, err := os.Open(filepath.Dir(opts.BackupPath))
	if err == nil {
		err = dir.Sync()
		closeErr := dir.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("persist index-hole backup directory: %w", err)
	}
	return f, nil
}

// RecheckIndexHoles compares recorded holes against retained block evidence.
// Dry runs use read-only database transactions. Applying takes a row lock and
// syncs the before/after record to disk before changing either hole table.
// The counts describe the proposed result of a successful full pass, or only
// completed plans on error. No wallet balances or tax event rows are changed.
func (s *Server) RecheckIndexHoles(chain string, node *ledgerNode, opts RecheckIndexHolesOptions, logf func(string, ...any)) (int, int, error) {
	backup, err := openIndexHoleBackup(opts)
	if err != nil {
		return 0, 0, err
	}
	if backup != nil {
		defer backup.Close()
	}
	ids, err := s.chainRowIDs(chain)
	if err != nil {
		return 0, 0, err
	}
	var heights []int64
	if err := s.db.Model(&IndexHole{}).Where("chain = ?", chain).Order("height").Pluck("height", &heights).Error; err != nil {
		return 0, 0, err
	}
	dropped := 0
	for _, h := range heights {
		txs, err := node.blockTxs(h)
		if err != nil {
			return dropped, len(heights) - dropped, fmt.Errorf("block %d: %w", h, err)
		}
		removed := false
		err = s.db.Transaction(func(tx *gorm.DB) error {
			change := indexHoleChange{Version: 1, PlannedAt: time.Now().UTC(), Apply: opts.Apply}
			q := tx.Where("chain = ? AND height = ?", chain, h)
			if opts.Apply {
				q = q.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			if err := q.First(&change.Before).Error; err != nil {
				return err
			}
			if err := tx.Where("chain = ? AND height = ?", chain, h).Order("address").Find(&change.BeforeAddresses).Error; err != nil {
				return err
			}
			var index []indexedTx
			if err := tx.Raw(`
SELECT b.height, b.tx_indexed AND b.block_events_indexed AS indexed, COALESCE(x.hash, '') AS hash, COALESCE(x.code, 0) AS code,
       EXISTS (SELECT 1 FROM messages m WHERE m.tx_id = x.id) AS messages
FROM blocks b LEFT JOIN txes x ON x.block_id = b.id
WHERE b.chain_id IN ? AND b.height = ?`, ids, h).Scan(&index).Error; err != nil {
				return err
			}
			n, accounts := scanHole(txs, index)
			removed = n == 0
			if !removed {
				change.After = &IndexHole{Chain: chain, Height: h, ChainTxs: n, AddressesKnown: true}
				for _, a := range accounts {
					change.AfterAddresses = append(change.AfterAddresses, IndexHoleAddress{Chain: chain, Height: h, Address: a})
				}
			}
			if backup != nil {
				if err := json.NewEncoder(backup).Encode(change); err != nil {
					return fmt.Errorf("write backup: %w", err)
				}
				if err := backup.Sync(); err != nil {
					return fmt.Errorf("persist backup: %w", err)
				}
			}
			if !opts.Apply {
				return nil
			}
			if err := tx.Where("chain = ? AND height = ?", chain, h).Delete(&IndexHoleAddress{}).Error; err != nil {
				return err
			}
			if removed {
				return tx.Where("chain = ? AND height = ?", chain, h).Delete(&IndexHole{}).Error
			}
			if err := tx.Model(&IndexHole{}).Where("chain = ? AND height = ?", chain, h).Updates(map[string]any{"chain_txs": n, "addresses_known": true}).Error; err != nil {
				return err
			}
			if len(change.AfterAddresses) == 0 {
				return nil
			}
			return tx.CreateInBatches(change.AfterAddresses, 500).Error
		}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: !opts.Apply})
		if err != nil {
			return dropped, len(heights) - dropped, fmt.Errorf("hole at %d: %w", h, err)
		}
		if removed {
			dropped++
		}
	}
	mode := "dry-run"
	if opts.Apply {
		mode = "applied"
	}
	logf("index holes: %s, rechecked %d, proposed removals %d, proposed remaining %d", mode, len(heights), dropped, len(heights)-dropped)
	return dropped, len(heights) - dropped, nil
}
