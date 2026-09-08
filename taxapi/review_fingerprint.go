package taxapi

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

func reviewFingerprint(rows []Row) string {
	ids := []string{}
	for _, row := range rows {
		if row.AssetIdentity != nil {
			ids = append(ids, row.AssetIdentity.RecordID)
		}
	}
	sort.Strings(ids)
	data, _ := json.Marshal(ids)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// Cached previews must not reuse decisions or evidence after a memo/trace/source
// correction. Only non-native candidate events need this freshness check, not
// the potentially much larger native staking history. It shares the exact row
// identity and suspicion logic used to build the preview.
func (s *Server) currentReviewFingerprint(chain string, addresses []string, start, end time.Time) (string, error) {
	var events []tax.TaxableEvent
	if err := s.db.Where("(from_addr IN ? OR to_addr IN ?) AND timestamp >= ? AND timestamp < ?", addresses, addresses, start, end).Where("denom IS NULL OR denom <> ?", "uatom").Find(&events).Error; err != nil {
		return "", err
	}
	memos, err := s.eventMemos(events)
	if err != nil {
		return "", err
	}
	meta := s.oracle.Denoms(chain)
	rows := []Row{}
	seen := map[string]bool{}
	for _, addr := range addresses {
		if seen[addr] {
			continue
		}
		seen[addr] = true
		for _, e := range events {
			if e.FromAddr != addr && e.ToAddr != addr {
				continue
			}
			row := s.buildRow(chain, meta, e.Timestamp, e.TxHash, e.Category, eventDirection(e, addr), e.Denom, e.Amount, e.FromAddr, e.ToAddr)
			attachSpamEvidence(&row, e, chain, addr, memos[e.TxHash])
			if row.AssetIdentity != nil {
				rows = append(rows, row)
			}
		}
	}
	return reviewFingerprint(rows), nil
}
func eventDirection(e tax.TaxableEvent, addr string) string {
	switch e.Category {
	case string(tax.CategoryTransfer), string(tax.CategoryIBCOut), string(tax.CategoryNFTSale), string(tax.CategorySwap):
		if e.FromAddr == addr {
			return "out"
		}
	}
	return "in"
}
