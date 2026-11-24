package taxapi

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash"
	"sort"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

const financialEventKind = "event"

var blockFingerprintEnd = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

func reviewFingerprint(rows []Row) string {
	ids := []string{}
	for _, row := range rows {
		if row.AssetIdentity != nil {
			ids = append(ids, row.AssetIdentity.RecordID)
		}
		if reason := rawSemanticReviewReason(row); reason != "" {
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
	var fingerprint string
	err := s.withReadSnapshot(func(reader *Server) error {
		var err error
		fingerprint, err = reader.reviewFingerprintInSnapshot(chain, normalizedWalletScope(addresses))
		return err
	})
	return fingerprint, err
}

// The caller owns a repeatable-read snapshot. Never call this on the live pool:
// financial scans and retained classification/packet evidence must agree.
func (s *Server) reviewFingerprintInSnapshot(chain string, addresses []string) (string, error) {
	return s.reviewEvidenceInSnapshot(chain, addresses, nil)
}

// reviewEvidenceInSnapshot is reviewFingerprintInSnapshot that also fills ev,
// when given, with what a later view needs to confirm the digest unchanged
// from the change journal (see evidencejournal.go). The digest is the same
// either way.
func (s *Server) reviewEvidenceInSnapshot(chain string, addresses []string, ev *evidenceCollector) (string, error) {
	h := sha256.New()
	writeFinancialFingerprintRow(h, "version", "reader-snapshot:v1")
	events, err := s.scopedTaxEvents(chain).
		Select("jsonb_build_array(to_jsonb(taxable_events), scope_t.memo, scope_t.hash, scope_t.code, scope_b.time_stamp, scope_c.chain_id)::text, taxable_events.\"timestamp\"").
		Where("(from_addr IN ? OR to_addr IN ?)", addresses, addresses).
		Order("taxable_events.id").Rows()
	if err != nil {
		return "", err
	}
	for events.Next() {
		var exact string
		var at sql.NullTime
		if err := events.Scan(&exact, &at); err != nil {
			_ = events.Close()
			return "", err
		}
		writeFinancialFingerprintRow(h, financialEventKind, exact)
		if ev != nil && at.Valid {
			ev.time(at.Time)
		}
	}
	err = events.Err()
	_ = events.Close()
	if err != nil {
		return "", err
	}
	// A failed transaction's recorded fee outcome decides whether its fee
	// counts, so it is hashed too. Its writers do not reach the evidence
	// journal; they mark the payers' reports stale instead
	// (recordFailedTxFees). Successful transactions add nothing, keeping
	// their wallets' digests as they were.
	fees, err := s.db.Table("fees f").
		Select("jsonb_build_array(to_jsonb(f), a.address, d.base, t.memo, t.hash, t.code, b.time_stamp, c.chain_id)::text, "+
			"CASE WHEN t.code <> 0 THEN jsonb_build_array(f.id, o.charged)::text END").
		Joins("JOIN addresses a ON a.id=f.payer_address_id").
		Joins("JOIN denoms d ON d.id=f.denomination_id").
		Joins("JOIN txes t ON t.id=f.tx_id").
		Joins("JOIN blocks b ON b.id=t.block_id").
		Joins("JOIN chains c ON c.id=b.chain_id").
		Joins("LEFT JOIN tax_failed_tx_fees o ON o.tx_id=f.tx_id").
		Where("c.chain_id IN ? AND a.address IN ?", chainIDs(chain), addresses).
		Order("f.id").Rows()
	if err != nil {
		return "", err
	}
	for fees.Next() {
		var exact string
		var outcome sql.NullString
		if err := fees.Scan(&exact, &outcome); err != nil {
			_ = fees.Close()
			return "", err
		}
		writeFinancialFingerprintRow(h, "fee", exact)
		if outcome.Valid {
			writeFinancialFingerprintRow(h, "fee_outcome", outcome.String)
		}
	}
	err = fees.Err()
	_ = fees.Close()
	if err != nil {
		return "", err
	}
	// Block event rows are derived at read time, so hash what they derive to.
	var blockRows hash.Hash
	if ev != nil {
		blockRows = sha256.New()
	}
	if err := s.writeBlockEventFingerprint(h, blockRows, chain, addresses); err != nil {
		return "", err
	}
	if ev != nil {
		ev.blockRows = fmt.Sprintf("%x", blockRows.Sum(nil))
	}
	if err := s.writeClassificationFingerprint(h, chain, addresses, ev); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// writeBlockEventFingerprint hashes the block event rows into h and, when
// given, into also.
func (s *Server) writeBlockEventFingerprint(h, also hash.Hash, chain string, addresses []string) error {
	for _, addr := range addresses {
		rows, err := s.blockEventRows(chain, addr, time.Time{}, blockFingerprintEnd)
		if err != nil {
			return err
		}
		for _, row := range rows {
			data, _ := json.Marshal(row)
			writeFinancialFingerprintRow(h, "block_event", string(data))
			if also != nil {
				writeFinancialFingerprintRow(also, "block_event", string(data))
			}
		}
	}
	return nil
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
