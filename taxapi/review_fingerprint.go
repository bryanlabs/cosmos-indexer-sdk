package taxapi

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash"
	"sort"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

const financialEventKind = "event"

func reviewFingerprint(rows []Row) string {
	ids := []string{}
	for _, row := range rows {
		if row.AssetIdentity != nil {
			ids = append(ids, row.AssetIdentity.RecordID)
		}
		if reason := semanticReviewReason(row); reason != "" {
			ids = append(ids, "semantic:"+row.TxHash+":"+row.Direction+":"+row.Denom+":"+reason+":"+row.Detail)
		}
	}
	sort.Strings(ids)
	data, _ := json.Marshal(ids)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// Hash exact persisted financial evidence, including native rows and payer-side
// fees. Full wallet history deliberately includes out-of-period lot sources and
// future refunds. Inserts, successful edits/replays and deletions all change the
// digest, regardless of the original blockchain timestamp. JSON framing avoids
// delimiter collisions and SQL text keeps decimal amounts lossless.
func (s *Server) currentReviewFingerprint(chain string, addresses []string, _, _ time.Time) (string, error) {
	h := sha256.New()
	writeFinancialFingerprintRow(h, "version", "financial-history:v1")
	events, err := s.scopedTaxEvents(chain).
		Select("jsonb_build_array(to_jsonb(taxable_events), scope_t.memo, scope_t.hash, scope_t.code, scope_b.time_stamp, scope_c.chain_id)::text").
		Where("(from_addr IN ? OR to_addr IN ?)", addresses, addresses).
		Order("taxable_events.id").Rows()
	if err != nil {
		return "", err
	}
	for events.Next() {
		var exact string
		if err := events.Scan(&exact); err != nil {
			_ = events.Close()
			return "", err
		}
		writeFinancialFingerprintRow(h, financialEventKind, exact)
	}
	err = events.Err()
	_ = events.Close()
	if err != nil {
		return "", err
	}
	fees, err := s.db.Table("fees f").
		Select("jsonb_build_array(to_jsonb(f), a.address, d.base, t.memo, t.hash, t.code, b.time_stamp, c.chain_id)::text").
		Joins("JOIN addresses a ON a.id=f.payer_address_id").
		Joins("JOIN denoms d ON d.id=f.denomination_id").
		Joins("JOIN txes t ON t.id=f.tx_id").
		Joins("JOIN blocks b ON b.id=t.block_id").
		Joins("JOIN chains c ON c.id=b.chain_id").
		Where("c.chain_id IN ? AND a.address IN ?", chainIDs(chain), addresses).
		Order("f.id").Rows()
	if err != nil {
		return "", err
	}
	defer fees.Close()
	for fees.Next() {
		var exact string
		if err := fees.Scan(&exact); err != nil {
			return "", err
		}
		writeFinancialFingerprintRow(h, "fee", exact)
	}
	if err := fees.Err(); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func writeFinancialFingerprintRow(h hash.Hash, kind, exact string) {
	data, _ := json.Marshal([]string{kind, exact})
	_, _ = h.Write(data)
}

// eventDirection is the row's direction from addr's point of view: out when
// addr is the sender. Rewards and commission are always received, and a
// self-transfer counts as a receive.
func eventDirection(e tax.TaxableEvent, addr string) string {
	switch e.Category {
	case string(tax.CategoryReward), string(tax.CategoryCommission):
		return "in"
	}
	if e.FromAddr == addr && e.ToAddr != addr {
		return apiOut
	}
	return "in"
}
