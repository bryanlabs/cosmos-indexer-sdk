package taxapi

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"gorm.io/gorm"
)

// A repeat view of a ready report (OPS-817).
//
// The full check (requireReportReady plus the review fingerprint) reads the
// wallet's whole history, 55 s for a busy wallet. When it passes, the job
// keeps an evidence check: the snapshot it ran in, the blocks of the wallet's
// transactions, the digest of its block event rows, and the latest time of
// its evidence. A later view first runs reportJobFastPath, which serves the
// cached job only when, in a new snapshot, the change journal shows nothing
// written since that snapshot that the full check could see:
//
//   - the journal is installed and trusted, its triggers, functions, the
//     tables' columns and foreign keys unchanged since the check, and none of
//     the journal rows the check could not see have been pruned;
//   - no journal row since names one of the wallet's blocks, one of its
//     addresses, or NULL (anything may have changed);
//   - the block event rows, which are not journaled, still hash the same (they
//     are cheap to read);
//   - the period end is the same, or both ends lie after every dated piece of
//     the wallet's evidence (the default end moves with the clock).
//
// Then the gates and the fingerprint read exactly what they read at the check,
// so they would pass and match again. The served check moves its snapshot
// forward, so the next view reads only what changed since this one. Anything
// else, including any error, runs the full check as before; the 24 hour age
// limit still recomputes every report.

// evidenceCheckVersion changes whenever what a check proves changes.
const evidenceCheckVersion = "evidence-check:v1"

// The verifier binds a check to this exact binary, so a deploy that changes
// how the gates or the fingerprint read evidence never trusts a check made by
// the previous one: the first view after a deploy runs the full check.
var (
	evidenceVerifierOnce sync.Once
	evidenceVerifierBase string
)

func evidenceVerifier(chain string, addresses []string) string {
	evidenceVerifierOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			return
		}
		f, err := os.Open(exe)
		if err != nil {
			return
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return
		}
		data, _ := json.Marshal([]any{evidenceCheckVersion, MethodologyVersion, tax.ReviewedTypeURLs(), tax.RefundLifecycleTypes()})
		h.Write(data)
		evidenceVerifierBase = fmt.Sprintf("%x", h.Sum(nil))
	})
	if evidenceVerifierBase == "" {
		return ""
	}
	data, _ := json.Marshal([]any{evidenceVerifierBase, chain, addresses})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// evidenceCheck is stored in report_jobs.evidence_check. Its snapshot is kept
// apart, in evidence_snapshot, so serving a view moves it forward without
// rewriting the block list.
type evidenceCheck struct {
	Verifier    string    `json:"v"`
	Fingerprint string    `json:"fp"`
	Meta        string    `json:"meta"`
	BlockRows   string    `json:"block_rows"`
	End         time.Time `json:"end"`
	MaxTime     time.Time `json:"max_time"`
	Blocks      []int64   `json:"blocks"`
}

// evidenceCollector gathers an evidence check while the fingerprint is read.
type evidenceCollector struct {
	blocks    map[int64]struct{}
	maxTime   time.Time
	blockRows string
}

func newEvidenceCollector() *evidenceCollector {
	return &evidenceCollector{blocks: map[int64]struct{}{}}
}

func (c *evidenceCollector) time(t time.Time) {
	if t.After(c.maxTime) {
		c.maxTime = t
	}
}

// transaction reads one wallet transaction row of the classification digest.
func (c *evidenceCollector) transaction(exact string) error {
	var row struct {
		BlockID  *int64 `json:"block_id"`
		Evidence struct {
			TimeStamp *time.Time `json:"time_stamp"`
		} `json:"block_evidence"`
	}
	if err := json.Unmarshal([]byte(exact), &row); err != nil {
		return fmt.Errorf("evidence check: transaction row: %w", err)
	}
	if row.BlockID == nil || row.Evidence.TimeStamp == nil {
		return errors.New("evidence check: transaction row without block or time")
	}
	c.blocks[*row.BlockID] = struct{}{}
	c.time(*row.Evidence.TimeStamp)
	return nil
}

func (c *evidenceCollector) check(verifier, fingerprint, meta string, end time.Time) evidenceCheck {
	blocks := make([]int64, 0, len(c.blocks))
	for b := range c.blocks {
		blocks = append(blocks, b)
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i] < blocks[j] })
	return evidenceCheck{Verifier: verifier, Fingerprint: fingerprint, Meta: meta, BlockRows: c.blockRows, End: end.UTC(), MaxTime: c.maxTime.UTC(), Blocks: blocks}
}

// evidenceRecord is a check ready to store: its JSON and its snapshot column.
type evidenceRecord struct {
	check    string
	snapshot string
}

func evidenceNonce(check string) string {
	sum := sha256.Sum256([]byte(check))
	return fmt.Sprintf("%x", sum[:8])
}

func evidenceSnapshotValue(check, snapshot string) string {
	return evidenceNonce(check) + "|" + snapshot
}

// evidenceJournalOn reports whether reports may use the journal at all.
// TAX_REPORT_EVIDENCE_JOURNAL=off makes every view run the full check.
func (s *Server) evidenceJournalOn(addresses []string) bool {
	if os.Getenv("TAX_REPORT_EVIDENCE_JOURNAL") == "off" {
		return false
	}
	if s.commissionPrivateProfile != nil || s.commissionSourceCorrections != nil {
		return false
	}
	if len(addresses) == 0 {
		return false
	}
	for _, a := range addresses {
		if !evidenceAddressRecorded(a) {
			return false
		}
	}
	return true
}

// reportEvidenceInSnapshot runs the review fingerprint inside the caller's
// snapshot and, when the journal can vouch for later views, the evidence
// check that goes with it. A nil record means the next view runs the full
// check.
func (s *Server) reportEvidenceInSnapshot(chain string, addresses []string, end time.Time) (string, *evidenceRecord, error) {
	if !s.evidenceJournalOn(addresses) {
		fp, err := s.reviewFingerprintInSnapshot(chain, addresses)
		return fp, nil, err
	}
	verifier := evidenceVerifier(chain, addresses)
	// A savepoint keeps a failed journal read (the journal missing, or a
	// role without access) from aborting the snapshot the full check needs.
	if err := s.db.SavePoint("report_evidence").Error; err != nil {
		return "", nil, err
	}
	trust, err := readEvidenceTrust(s.db)
	if err != nil {
		if rerr := s.db.RollbackTo("report_evidence").Error; rerr != nil {
			return "", nil, rerr
		}
	}
	if err != nil || verifier == "" || !trust.ok() {
		if err != nil {
			log.Printf("report evidence journal unreadable, full checks only: %v", err)
		}
		fp, err := s.reviewFingerprintInSnapshot(chain, addresses)
		return fp, nil, err
	}
	ev := newEvidenceCollector()
	fp, err := s.reviewEvidenceInSnapshot(chain, addresses, ev)
	if err != nil {
		return "", nil, err
	}
	data, err := json.Marshal(ev.check(verifier, fp, trust.Meta, end))
	if err != nil {
		return "", nil, err
	}
	return fp, &evidenceRecord{check: string(data), snapshot: evidenceSnapshotValue(string(data), trust.Snapshot)}, nil
}

// storeEvidenceCheck keeps the record on a ready job whose fingerprint is the
// one the record was made with.
func (s *Server) storeEvidenceCheck(id uint, fingerprint string, rec *evidenceRecord) {
	if rec == nil {
		return
	}
	if err := s.db.Model(&ReportJob{}).
		Where("id = ? AND status = ? AND review_fingerprint = ?", id, ReportJobReady, fingerprint).
		UpdateColumns(map[string]any{"evidence_check": rec.check, "evidence_snapshot": rec.snapshot}).Error; err != nil {
		log.Printf("report job %d: store evidence check: %v", id, err)
	}
}

// errEvidenceFallback marks a view the journal cannot vouch for; it is not a
// failure.
var errEvidenceFallback = errors.New("full check needed")

// reportJobFastPath serves a ready job from its evidence check, or returns
// false for the full check.
func (s *Server) reportJobFastPath(key, chain string, addresses []string, end time.Time) (*ReportJob, bool) {
	if !s.evidenceJournalOn(addresses) {
		return nil, false
	}
	verifier := evidenceVerifier(chain, addresses)
	if verifier == "" {
		return nil, false
	}
	var job ReportJob
	var nextSnapshot string
	err := s.withReadSnapshotTimeout(20*time.Second, func(reader *Server) error {
		if err := reader.db.Where(apiKey, key).First(&job).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errEvidenceFallback
			}
			return err
		}
		if job.Status != ReportJobReady || s.reportIsStale(&job, addresses) || job.EvidenceCheck == "" {
			return errEvidenceFallback
		}
		nonce, checked, ok := strings.Cut(job.EvidenceSnapshot, "|")
		if !ok || nonce != evidenceNonce(job.EvidenceCheck) || checked == "" {
			return errEvidenceFallback
		}
		var check evidenceCheck
		if err := json.Unmarshal([]byte(job.EvidenceCheck), &check); err != nil {
			return errEvidenceFallback
		}
		if check.Verifier != verifier || check.Fingerprint != job.ReviewFingerprint {
			return errEvidenceFallback
		}
		// The gates read evidence dated before the period end. The same end,
		// or two ends both after every dated piece of evidence, read the same.
		sameEnd := end.Equal(check.End)
		bothAfter := check.MaxTime.Before(check.End) && !end.Before(check.End)
		if !sameEnd && !bothAfter {
			return errEvidenceFallback
		}
		trust, err := readEvidenceTrust(reader.db)
		if err != nil {
			return err
		}
		if !trust.ok() || trust.Meta != check.Meta {
			return errEvidenceFallback
		}
		var pruneOK bool
		if err := reader.db.Raw("SELECT ?::xid8 <= pg_snapshot_xmin(?::pg_snapshot)", trust.PrunedBelow, checked).Row().Scan(&pruneOK); err != nil {
			return err
		}
		if !pruneOK {
			return errEvidenceFallback
		}
		var linked bool
		if err := reader.db.Raw(`SELECT EXISTS (SELECT 1 FROM report_evidence_link
 WHERE (address IS NULL OR address IN ?) AND xid >= pg_snapshot_xmin(?::pg_snapshot) AND NOT pg_visible_in_snapshot(xid, ?::pg_snapshot))`,
			addresses, checked, checked).Row().Scan(&linked); err != nil {
			return err
		}
		if linked {
			return errEvidenceFallback
		}
		const maxChangedBlocks = 500000
		rows, err := reader.db.Raw(`SELECT block_id FROM report_evidence_log
 WHERE xid >= pg_snapshot_xmin(?::pg_snapshot) AND NOT pg_visible_in_snapshot(xid, ?::pg_snapshot)
 GROUP BY block_id LIMIT ?`, checked, checked, maxChangedBlocks+1).Rows()
		if err != nil {
			return err
		}
		wallet := make(map[int64]struct{}, len(check.Blocks))
		for _, b := range check.Blocks {
			wallet[b] = struct{}{}
		}
		n := 0
		hit := false
		for rows.Next() {
			var block *int64
			if err := rows.Scan(&block); err != nil {
				_ = rows.Close()
				return err
			}
			n++
			if block == nil {
				hit = true
			} else if _, ok := wallet[*block]; ok {
				hit = true
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		if hit || n > maxChangedBlocks {
			return errEvidenceFallback
		}
		h := sha256.New()
		if err := reader.writeBlockEventFingerprint(h, nil, chain, addresses); err != nil {
			return err
		}
		if fmt.Sprintf("%x", h.Sum(nil)) != check.BlockRows {
			return errEvidenceFallback
		}
		nextSnapshot = evidenceSnapshotValue(job.EvidenceCheck, trust.Snapshot)
		return nil
	})
	if err != nil {
		if !errors.Is(err, errEvidenceFallback) {
			log.Printf("report job fast check failed, running the full check: %v", err)
		}
		return nil, false
	}
	if nextSnapshot != job.EvidenceSnapshot {
		if err := s.db.Model(&ReportJob{}).
			Where("id = ? AND status = ? AND review_fingerprint = ? AND evidence_snapshot = ?", job.ID, ReportJobReady, job.ReviewFingerprint, job.EvidenceSnapshot).
			UpdateColumn("evidence_snapshot", nextSnapshot).Error; err != nil {
			log.Printf("report job %d: advance evidence snapshot: %v", job.ID, err)
		}
	}
	return &job, true
}

// StartEvidenceJournalPrune trims the evidence journal every interval. A
// database without the journal logs nothing.
func (s *Server) StartEvidenceJournalPrune(every time.Duration, logf func(string, ...any)) {
	go func() {
		for {
			if trust, err := readEvidenceTrust(s.db); err == nil && trust.Installed {
				if n, err := PruneEvidenceJournal(s.db); err != nil {
					logf("report evidence journal prune: %v", err)
				} else if n > 0 {
					logf("report evidence journal prune: %d rows past %s removed", n, evidenceKeep)
				}
			}
			time.Sleep(every)
		}
	}()
}
