package taxapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"gorm.io/gorm"
)

// CommissionFlowCheckpointOptions is programmatic only. No CLI or environment
// enables it. Snapshot must be exported by a separately surviving read-only
// transaction on the same database. Losing that owner invalidates further
// imports; a new epoch requires a NEW empty directory, never an old prefix.
// Existing per-read authz/correction seals cover particular source rows, not
// every discovered wallet. They are retained, not mistaken for a ledger epoch.
type CommissionFlowCheckpointOptions struct {
	Directory      string
	Snapshot       string
	DatabaseSHA256 string
}

// CommissionFlowCheckpointEpoch owns an exported PostgreSQL MVCC snapshot.
// Its owner must outlive collector SIGTERM/deadline boundaries. Keeping it in
// the collector itself is NOT cross-process recovery. Close releases retained
// database history. This API neither starts an owner nor changes any actors.
type CommissionFlowCheckpointEpoch struct {
	Snapshot       string
	DatabaseSHA256 string
	tx             *sql.Tx
	conn           *sql.Conn
	closeOnce      sync.Once
	closeErr       error
}

const commissionCheckpointDatabaseSQL = `SELECT jsonb_build_array(system_identifier::text,current_database(),(SELECT oid::text FROM pg_database WHERE datname=current_database()),extract(epoch from pg_postmaster_start_time())::text)::text FROM pg_control_system()`

var commissionSnapshotToken = regexp.MustCompile(`^[0-9A-Fa-f]+-[0-9A-Fa-f]+-[0-9]+$`)

func (s *Server) BeginCommissionFlowCheckpointEpoch(ctx context.Context) (*CommissionFlowCheckpointEpoch, error) {
	db, err := s.db.DB()
	if err != nil {
		return nil, errors.New("checkpoint database unavailable")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, errors.New("checkpoint epoch connection unavailable")
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		conn.Close()
		return nil, errors.New("checkpoint epoch transaction unavailable")
	}
	epoch := &CommissionFlowCheckpointEpoch{tx: tx, conn: conn}
	var identity string
	if err := tx.QueryRowContext(ctx, commissionCheckpointDatabaseSQL).Scan(&identity); err != nil {
		epoch.Close()
		return nil, errors.New("checkpoint database identity unavailable")
	}
	if err := tx.QueryRowContext(ctx, "SELECT pg_export_snapshot()").Scan(&epoch.Snapshot); err != nil || !commissionSnapshotToken.MatchString(epoch.Snapshot) {
		epoch.Close()
		return nil, errors.New("checkpoint snapshot export failed")
	}
	epoch.DatabaseSHA256 = privateSHA([]byte(identity))
	return epoch, nil
}
func (e *CommissionFlowCheckpointEpoch) Close() error {
	if e == nil {
		return nil
	}
	e.closeOnce.Do(func() {
		if e.tx != nil {
			err := e.tx.Rollback()
			if !errors.Is(err, sql.ErrTxDone) {
				e.closeErr = err
			}
		}
		if e.conn != nil {
			e.closeErr = errors.Join(e.closeErr, e.conn.Close())
		}
	})
	return e.closeErr
}

// Imported MVCC, rather than a string label or a fresh per-read snapshot,
// supplies the actual cross-process source-epoch proof. PostgreSQL rejects an
// expired token before any source read. All DB inputs (including receivers,
// labels, memo heuristics and channels) use this transaction. Exact-height
// archive protocol and private source seals remain unchanged.
func (s *Server) traceCommissionCheckpoint(chain string, base *dbFlowLoader, validator string, from, asOf time.Time, save func(CommissionFlowRun, []CommissionFlow, CommissionFlowEvidence) error) (run CommissionFlowRun, err error) {
	o := base.checkpointOptions
	if o == nil || !base.private || !commissionSnapshotToken.MatchString(o.Snapshot) || !correctionHash(o.DatabaseSHA256) {
		return run, errors.New("checkpoint requires a private exported source epoch")
	}
	// A live-node channel fallback is not covered by the database epoch. Refuse
	// this configuration, rather than quietly changing or mixing its semantics.
	if base.node != nil && base.node.rest != nil {
		return run, errors.New("checkpoint source epoch does not cover live channel fallback")
	}
	var completed bool
	var finalRows []CommissionFlow
	var finalEvidence CommissionFlowEvidence
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if e := tx.Exec("SET TRANSACTION SNAPSHOT '" + o.Snapshot + "'").Error; e != nil {
			return errors.New("checkpoint source epoch unavailable; fresh attempt required")
		}
		var identity string
		if e := tx.Raw(commissionCheckpointDatabaseSQL).Scan(&identity).Error; e != nil || privateSHA([]byte(identity)) != o.DatabaseSHA256 {
			return errors.New("checkpoint database identity changed")
		}
		reader, loader := s.privateProjectionTrace(base)
		reader.db = tx
		loader.checkpointEpochBound = true
		loader.chans = map[string]string{}
		loader.labels = map[string]AddressLabel{}
		var labels []AddressLabel
		if e := tx.Where("chain = ?", chain).Find(&labels).Error; e != nil {
			return errors.New("checkpoint source labels unavailable")
		}
		validators, e := reader.commissionValidators(chain)
		if e != nil {
			return errors.New("checkpoint source validators unavailable")
		}
		for _, label := range labels {
			loader.labels[label.Address] = label
		}
		for _, label := range derivedFlowLabels(chain, validators) {
			if _, ok := loader.labels[label.Address]; !ok {
				loader.labels[label.Address] = label
			}
		}
		if base.archive != nil {
			loader.archive = &flowArchive{rpc: base.archive.rpc, holdings: map[string]flowHoldings{}}
		}
		// Root-private journals and archive ownership prevent other workers' reads
		// from appearing in this root's checkpoint or evidence.
		run, e = reader.traceCommissionSaved(chain, loader, validator, from, asOf, func(r CommissionFlowRun, rows []CommissionFlow, evidence CommissionFlowEvidence) error {
			if completed || r.Status == flowStatusError || evidence.Checkpoint == nil {
				return errors.New("checkpoint final capture invalid")
			}
			completed, finalRows, finalEvidence = true, rows, evidence
			return nil
		})
		return e
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err == nil {
		if !completed {
			return run, errors.New("checkpoint root did not complete")
		}
		err = save(run, finalRows, finalEvidence)
	}
	return run, err
}

func checkpointExecutableSHA() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", errors.New("checkpoint code identity unavailable")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("checkpoint code identity unavailable")
	}
	h := sha256.New()
	_, readErr := io.Copy(h, f)
	if err := errors.Join(readErr, f.Close()); err != nil {
		return "", errors.New("checkpoint code identity unavailable")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (l *dbFlowLoader) openCheckpoint(validator string, from, asOf time.Time, receivers []CommissionReceiver, group []string) (*checkpointFlowLoader, error) {
	code, err := checkpointExecutableSHA()
	if err != nil {
		return nil, err
	}
	settings, err := json.Marshal([]any{l.checkpointSettings, l.s.native, l.private})
	if err != nil {
		return nil, errors.New("checkpoint settings identity unavailable")
	}
	rb, err := json.Marshal([]any{receivers, group})
	if err != nil {
		return nil, err
	}
	id := flowCheckpointIdentity{Schema: 1, Root: validator, Chain: l.chain, Build: code, SourceEpoch: l.checkpointOptions.Snapshot, SourceSeal: l.checkpointOptions.DatabaseSHA256, SettingsSHA256: privateSHA(settings), ReceiversSHA256: privateSHA(rb), From: from, AsOf: asOf}
	dir := filepath.Join(l.checkpointOptions.Directory, privateSHA([]byte(validator)))
	if !filepath.IsAbs(l.checkpointOptions.Directory) {
		return nil, errors.New("checkpoint root must be absolute")
	}
	info, err := os.Lstat(l.checkpointOptions.Directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("checkpoint parent must be private mode 0700")
	}
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return nil, errors.New("checkpoint root creation failed")
	}
	if err := syncCheckpointDir(l.checkpointOptions.Directory); err != nil {
		return nil, errors.New("checkpoint root sync failed")
	}
	hooks := l.checkpointReceiptHooks()
	var importedSnapshot string
	hooks.VerifySource = func() error {
		var state struct{ Snapshot, RO, Isolation string }
		if err := l.s.db.Raw("SELECT pg_current_snapshot()::text AS snapshot,current_setting('transaction_read_only') AS ro,current_setting('transaction_isolation') AS isolation").Scan(&state).Error; err != nil || state.RO != "on" || state.Isolation != "repeatable read" || state.Snapshot == "" {
			return errors.New("checkpoint imported source transaction lost")
		}
		if importedSnapshot == "" {
			importedSnapshot = state.Snapshot
		}
		if importedSnapshot != state.Snapshot {
			return errors.New("checkpoint imported source snapshot changed")
		}
		return nil
	}
	return openFlowCheckpoint(dir, id, l, hooks)
}

// Capture emits only newly observed receipts, never the growing evidence set.
// Replay reinserts original snapshot identifiers and exact response bytes.
// It does not relabel old receipts as having been read in a new transaction.
func (l *dbFlowLoader) checkpointReceiptHooks() flowCheckpointHooks {
	l.checkpointPending = &CommissionFlowEvidence{Channels: map[string]string{}}
	for _, lab := range l.labels {
		l.checkpointPending.Labels = append(l.checkpointPending.Labels, lab)
	}
	for k, v := range l.chans {
		l.checkpointPending.Channels[k] = v
	}
	if j := l.s.privateProjectionReceipts; j != nil {
		j.checkpointEnabled = true
	}
	if j := l.s.sourceCorrectionReceipts; j != nil {
		j.checkpointEnabled = true
	}
	archiveCursor := 0
	drain := func() CommissionFlowEvidence {
		l.mu.Lock()
		e := *l.checkpointPending
		l.checkpointPending = &CommissionFlowEvidence{Channels: map[string]string{}}
		l.mu.Unlock()
		sort.Slice(e.Labels, func(i, j int) bool { return e.Labels[i].Address < e.Labels[j].Address })
		if j := l.s.privateProjectionReceipts; j != nil {
			j.mu.Lock()
			e.PrivateProjections = j.checkpointPending
			j.checkpointPending = nil
			j.mu.Unlock()
		}
		if j := l.s.sourceCorrectionReceipts; j != nil {
			j.mu.Lock()
			e.SourceCorrectionReads = j.checkpointPending
			j.checkpointPending = nil
			j.mu.Unlock()
		}
		if a := l.archive; a != nil {
			a.mu.Lock()
			e.Archive = append([]FlowArchiveReceipt(nil), a.receipts[archiveCursor:]...)
			archiveCursor = len(a.receipts)
			a.mu.Unlock()
		}
		return e
	}
	return flowCheckpointHooks{
		Capture: func() (json.RawMessage, error) {
			e := drain()
			sort.Slice(e.PrivateProjections, func(i, j int) bool {
				a, b := e.PrivateProjections[i], e.PrivateProjections[j]
				return a.WalletScopeSHA256+a.Start.UTC().String()+a.End.UTC().String()+a.Snapshot < b.WalletScopeSHA256+b.Start.UTC().String()+b.End.UTC().String()+b.Snapshot
			})
			type keyedCorrection struct {
				key     string
				receipt CommissionSourceCorrectionReceipt
			}
			ordered := make([]keyedCorrection, len(e.SourceCorrectionReads))
			for i, r := range e.SourceCorrectionReads {
				b, err := json.Marshal(r)
				if err != nil {
					return nil, err
				}
				ordered[i] = keyedCorrection{privateSHA(b), r}
			}
			sort.Slice(ordered, func(i, j int) bool { return ordered[i].key < ordered[j].key })
			for i, r := range ordered {
				e.SourceCorrectionReads[i] = r.receipt
			}
			b, err := json.Marshal(e)
			return b, err
		},
		Restore: func(raw json.RawMessage) error {
			var e CommissionFlowEvidence
			if json.Unmarshal(raw, &e) != nil || e.Checkpoint != nil || e.ReadFailure != nil || len(e.Wallets) > 0 || e.PrivateProfile != nil || e.SourceCorrectionProfile != nil {
				return errors.New("checkpoint read receipt schema mismatch")
			}
			// Validate the complete bounded delta before mutating any journal.
			for _, r := range e.PrivateProjections {
				if l.s.commissionPrivateProfile == nil || r.ProfileSHA256 != l.s.commissionPrivateProfile.digest || r.SourceSHA256 != l.s.commissionPrivateProfile.SourceSHA256 || r.Snapshot == "" {
					return errors.New("checkpoint projection receipt mismatch")
				}
			}
			for _, r := range e.SourceCorrectionReads {
				if l.s.commissionSourceCorrections == nil || r.ProfileSHA256 != l.s.commissionSourceCorrections.digest || r.Snapshot == "" {
					return errors.New("checkpoint correction receipt mismatch")
				}
			}
			for _, r := range e.Archive {
				if l.archive == nil || privateSHA([]byte(r.Response)) != r.SHA256 {
					return errors.New("checkpoint archive receipt mismatch")
				}
			}
			for _, lab := range e.Labels {
				if lab.Chain != l.chain {
					return errors.New("checkpoint label scope mismatch")
				}
			}
			l.mu.Lock()
			for _, lab := range e.Labels {
				l.labels[lab.Address] = lab
			}
			for k, v := range e.Channels {
				l.chans[k] = v
			}
			l.mu.Unlock()
			if j := l.s.privateProjectionReceipts; j != nil {
				j.mu.Lock()
				for _, r := range e.PrivateProjections {
					key := r.WalletScopeSHA256 + r.Start.String() + r.End.String() + r.Snapshot
					j.rows[key] = r
				}
				j.mu.Unlock()
			}
			if j := l.s.sourceCorrectionReceipts; j != nil {
				for _, r := range e.SourceCorrectionReads {
					j.retain(r)
				}
			}
			if a := l.archive; a != nil {
				for _, r := range e.Archive {
					if r.Method == "block" && !r.Reused {
						if _, _, err := a.headers.get(r.Height, func() (string, error) { return r.Response, nil }); err != nil {
							return errors.New("checkpoint archive cache restore failed")
						}
					}
				}
				a.mu.Lock()
				a.receipts = append(a.receipts, e.Archive...)
				a.mu.Unlock()
			}
			drain()
			return nil
		},
	}
}

// CommissionFlowCheckpointEvidence belongs only to a fully finished root.
// A partial journal has no such record and cannot be a financial result.
type CommissionFlowCheckpointEvidence struct {
	SourceEpochSHA256 string
	DatabaseSHA256    string
	CodeSHA256        string
	SettingsSHA256    string
	ReceiversSHA256   string
	ReadCount         int
	ReadChainSHA256   string
}

func (l *checkpointFlowLoader) completedEvidence() *CommissionFlowCheckpointEvidence {
	return &CommissionFlowCheckpointEvidence{SourceEpochSHA256: privateSHA([]byte(l.identity.SourceEpoch)), DatabaseSHA256: l.identity.SourceSeal, CodeSHA256: l.identity.Build, SettingsSHA256: l.identity.SettingsSHA256, ReceiversSHA256: l.identity.ReceiversSHA256, ReadCount: l.count, ReadChainSHA256: l.previous}
}
func (l *dbFlowLoader) restoreCheckpointResult(req flowCheckpointRequest, r flowCheckpointResult) {
	if l.archive != nil && (req.Kind == "opening" || req.Kind == "closing") && r.Known {
		l.archive.mu.Lock()
		l.archive.holdings[req.Address+"|"+flowDay(req.Start).Format(time.DateOnly)] = r.Holdings
		l.archive.mu.Unlock()
	}
}
