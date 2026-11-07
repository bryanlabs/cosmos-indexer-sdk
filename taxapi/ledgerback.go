package taxapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/DefiantLabs/cosmos-indexer/taxapi/internal/servicehttp"
)

// NewArchiveLedgerNode reads blocks from a block cache directory and state at
// a height from an archive node's REST API, for heights the serving node no
// longer holds.
func NewArchiveLedgerNode(cacheDir, archiveREST string) (*ledgerNode, error) {
	if cacheDir == "" || archiveREST == "" {
		return nil, errors.New("a block cache directory and an archive REST origin are both required")
	}
	rest, err := servicehttp.New(&http.Client{Timeout: 60 * time.Second}, archiveREST)
	if err != nil {
		return nil, err
	}
	return &ledgerNode{rest: rest, cache: cacheDir}, nil
}

// ErrLedgerScanUnindexed is where a backward scan stops at a block that is
// not fully indexed yet. It is the normal end of a run while lower years are
// still being indexed.
var ErrLedgerScanUnindexed = errors.New("reached a block that is not fully indexed")

// backTx is one indexed transaction of a block the backward scan compares.
type backTx struct {
	indexedTx
	TxID uint
}

// backBlock is what the scan of one block records.
type backBlock struct {
	height   int64
	chainTxs int
	accounts []string
	fees     []FailedTxFee
	grants   []FeeGrantUse
}

// scanBackBlock compares one block's transactions on chain with the index and
// collects the hole, the failed transactions' fee outcomes and the fee grant
// payers to record for it.
func scanBackBlock(height int64, chain []nodeTx, index []backTx) backBlock {
	b := backBlock{height: height}
	plain := make([]indexedTx, len(index))
	byHash := map[string]uint{}
	for i, x := range index {
		plain[i] = x.indexedTx
		if x.Hash != "" {
			byHash[strings.ToUpper(x.Hash)] = x.TxID
		}
	}
	b.chainTxs, b.accounts = scanHole(chain, plain)
	seen := map[uint]bool{}
	for _, t := range chain {
		id, ok := byHash[t.Hash]
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		if t.Code != 0 {
			b.fees = append(b.fees, FailedTxFee{TxID: id, Charged: t.Charged})
		}
		if t.Granter != "" {
			b.grants = append(b.grants, FeeGrantUse{TxID: id, Granter: t.Granter})
		}
	}
	return b
}

// ExtendLedgerScanBack lowers the start of the scanned span one chunk at a
// time, down to floor, reading each block from node: it records the blocks
// missing a transaction or holding a successful one without its messages,
// the fee each failed transaction was charged, and the granter of each fee a
// grant paid, then moves the span's start below the chunk in the same
// database transaction. It stops at the first block that is not fully
// indexed or cannot be read, so a later run resumes where this one stopped.
// It returns the new start of the span.
func (s *Server) ExtendLedgerScanBack(chain string, node *ledgerNode, floor, chunk int64, workers int, logf func(string, ...any)) (int64, error) {
	ids, err := s.chainRowIDs(chain)
	if err != nil {
		return 0, err
	}
	scan, err := s.ledgerScan(chain)
	if err != nil {
		return 0, err
	}
	if scan.FeeGrantsFrom == 0 {
		return 0, errors.New("the ledger scan has not started")
	}
	from := scan.FeeGrantsFrom
	for from > floor {
		hi := from - 1
		lo := max(floor, hi-chunk+1)
		var rows []backTx
		if err := s.db.Raw(`
SELECT b.height, b.tx_indexed AND b.block_events_indexed AS indexed, COALESCE(x.id, 0) AS tx_id, COALESCE(x.hash, '') AS hash, COALESCE(x.code, 0) AS code,
       EXISTS (SELECT 1 FROM messages m WHERE m.tx_id = x.id) AS messages
FROM blocks b LEFT JOIN txes x ON x.block_id = b.id
WHERE b.chain_id IN @ids AND b.height BETWEEN @lo AND @hi
ORDER BY b.height`, map[string]any{"ids": ids, "lo": lo, "hi": hi}).Scan(&rows).Error; err != nil {
			return from, fmt.Errorf("index rows %d to %d: %w", lo, hi, err)
		}
		byHeight := map[int64][]backTx{}
		for _, r := range rows {
			byHeight[r.Height] = append(byHeight[r.Height], r)
		}
		// Read the chunk's blocks in parallel, then walk down from its top so
		// the span stays contiguous.
		read := make([][]nodeTx, hi-lo+1)
		readErr := make([]error, hi-lo+1)
		heights := make(chan int64)
		var wg sync.WaitGroup
		for w := 0; w < max(workers, 1); w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for h := range heights {
					read[h-lo], readErr[h-lo] = node.blockTxs(h)
				}
			}()
		}
		for h := hi; h >= lo; h-- {
			if idx, ok := byHeight[h]; ok && idx[0].Indexed {
				heights <- h
			}
		}
		close(heights)
		wg.Wait()

		var scanned []backBlock
		next := from
		var stop error
		for h := hi; h >= lo; h-- {
			idx, ok := byHeight[h]
			if !ok || !idx[0].Indexed {
				stop = fmt.Errorf("%w: %d", ErrLedgerScanUnindexed, h)
				break
			}
			if readErr[h-lo] != nil {
				stop = fmt.Errorf("block %d: %w", h, readErr[h-lo])
				break
			}
			scanned = append(scanned, scanBackBlock(h, read[h-lo], idx))
			next = h
		}
		if next < from {
			holes, err := s.recordBackScan(chain, scanned, next)
			if err != nil {
				return from, fmt.Errorf("record scan of %d to %d: %w", next, from-1, err)
			}
			logf("ledger scan back: span starts at %d (%d blocks, %d with missing transactions)", next, from-next, holes)
			from = next
		}
		if stop != nil {
			return from, stop
		}
	}
	return from, nil
}

// recordBackScan stores one chunk's records and moves the span's start to
// from, all or nothing.
func (s *Server) recordBackScan(chain string, blocks []backBlock, from int64) (int, error) {
	holes := 0
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var fees []FailedTxFee
		var grants []FeeGrantUse
		for _, b := range blocks {
			fees = append(fees, b.fees...)
			grants = append(grants, b.grants...)
			if b.chainTxs == 0 {
				continue
			}
			holes++
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&IndexHole{Chain: chain, Height: b.height, ChainTxs: b.chainTxs, AddressesKnown: true}).Error; err != nil {
				return err
			}
			rows := make([]IndexHoleAddress, 0, len(b.accounts))
			for _, a := range b.accounts {
				rows = append(rows, IndexHoleAddress{Chain: chain, Height: b.height, Address: a})
			}
			if len(rows) > 0 {
				if err := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 500).Error; err != nil {
					return err
				}
			}
		}
		if len(fees) > 0 {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(fees, 500).Error; err != nil {
				return err
			}
		}
		if len(grants) > 0 {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(grants, 500).Error; err != nil {
				return err
			}
		}
		return tx.Model(&LedgerScan{}).Where("chain = ? AND fee_grants_from > ?", chain, from).Update("fee_grants_from", from).Error
	})
	return holes, err
}

// BackfillValidatorRates records every validator's tokens and shares, and the
// validator set's consensus addresses, at the end of each UTC day from the
// start of the scanned span up to the first day already recorded, and at the
// blocks around each slash on those days, reading state from node. Heights
// already recorded are skipped, so a run resumes where the last one stopped.
// It fails on the first height the node cannot serve: a missing day would
// leave every date before it without the rates it needs.
func (s *Server) BackfillValidatorRates(chain string, node *ledgerNode, logf func(string, ...any)) error {
	ids, err := s.chainRowIDs(chain)
	if err != nil {
		return err
	}
	scan, err := s.ledgerScan(chain)
	if err != nil {
		return err
	}
	var first struct{ Height int64 }
	if err := s.db.Raw(`SELECT COALESCE(min(height), 0) AS height FROM tax_validator_rates WHERE chain = ?`, chain).Scan(&first).Error; err != nil {
		return err
	}
	if first.Height == 0 {
		return errors.New("no validator rates are recorded yet; the sweeper records the first ones")
	}
	var b heightTime
	if err := s.db.Table("blocks").Select("height, time_stamp").Where("chain_id IN ? AND height >= ?", ids, scan.FeeGrantsFrom).Order("height").Limit(1).Scan(&b).Error; err != nil {
		return err
	}
	if b.Height == 0 {
		return fmt.Errorf("no indexed block at or above %d", scan.FeeGrantsFrom)
	}
	var have []int64
	if err := s.db.Raw(`SELECT DISTINCT height FROM tax_validator_rates WHERE chain = ? AND height < ?`, chain, first.Height).Scan(&have).Error; err != nil {
		return err
	}
	recorded := map[int64]bool{}
	for _, h := range have {
		recorded[h] = true
	}
	for day := b.TimeStamp.UTC().Truncate(24 * time.Hour); ; day = day.AddDate(0, 0, 1) {
		end, err := s.dayEndBlock(ids, day)
		if err != nil {
			return fmt.Errorf("end of %s: %w", day.Format(apiDateLayout), err)
		}
		if end.Height >= first.Height {
			return nil
		}
		heights, err := s.slashBlocksOn(ids, day, end.Height)
		if err != nil {
			return fmt.Errorf("slashes on %s: %w", day.Format(apiDateLayout), err)
		}
		heights = append(heights, end.Height)
		n := 0
		for _, h := range heights {
			if recorded[h] {
				continue
			}
			if err := s.recordValidatorRates(chain, node, h); err != nil {
				return fmt.Errorf("validator rates at %d: %w", h, err)
			}
			recorded[h] = true
			n++
		}
		if n > 0 {
			if err := s.recordValidators(node, end.Height); err != nil {
				return fmt.Errorf("validator set at %d: %w", end.Height, err)
			}
			logf("validator rates: %s recorded at %d heights", day.Format(apiDateLayout), n)
		}
	}
}
