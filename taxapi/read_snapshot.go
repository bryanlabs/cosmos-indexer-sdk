package taxapi

import (
	"context"
	"database/sql"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

const readSnapshotTimeout = 30 * time.Second

// reportJobReadSnapshotTimeout bounds the snapshots that read a wallet's
// full-history review fingerprint for a report: the background build, and the
// check that a finished report still matches before it is served. A busy
// wallet's fingerprint alone took 18s on the live database.
const reportJobReadSnapshotTimeout = 3 * time.Minute

type walletRows struct {
	Events []tax.TaxableEvent
	Fees   []feeRec
	Memos  map[string]string
	// stampChain is true when earlier Hub chains are in reports; every row
	// then names its chain.
	stampChain            bool
	CommissionCorrections []flowEvent
	// IndexedRewardCorrections is, per message, the reward total of native
	// corrections left out because the indexer now stores exactly those
	// rewards; the trace must credit that message exactly this much.
	IndexedRewardCorrections map[uint]decimal.Decimal
}

type reportInput struct {
	Wallets     []walletRows
	Fingerprint string
}

// withReadSnapshot creates a DB-only facade, NOT a copy of Server (which owns
// mutexes). Only DB query methods may run on it. All pricing, review application
// and rendering run on the original Server, preserving its configuration and
// shared caches. No network calls or report-job writes belong in this callback.
func (s *Server) readSnapshotContext(override time.Duration) (context.Context, context.CancelFunc) {
	timeout := readSnapshotTimeout
	if override > 0 {
		timeout = override
	} else if s.readOnlyLedger && s.privateReadSnapshotTimeout > 0 {
		timeout = s.privateReadSnapshotTimeout
	}
	return context.WithTimeout(s.db.Statement.Context, timeout)
}

func (s *Server) withReadSnapshot(fn func(*Server) error) error {
	return s.withReadSnapshotTimeout(0, fn)
}

// withReadSnapshotTimeout is withReadSnapshot with its own deadline; zero
// keeps the default.
func (s *Server) withReadSnapshotTimeout(timeout time.Duration, fn func(*Server) error) error {
	started := time.Now()
	ctx, cancel := s.readSnapshotContext(timeout)
	defer cancel()
	var pendingCorrections *commissionCorrectionJournal
	if s.commissionSourceCorrections != nil && s.sourceCorrectionReceipts != nil {
		pendingCorrections = newCorrectionJournal()
	}
	var pending *commissionProjectionJournal
	if s.commissionPrivateProfile != nil && s.privateProjectionReceipts != nil {
		pending = &commissionProjectionJournal{rows: map[string]CommissionPrivateProjectionReceipt{}}
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The driver caches prepared statements, and after five runs Postgres
		// may switch to a generic plan that cannot see the period or the type
		// ids; the readiness counts then went from 1s to over 20s.
		if err := tx.Exec("SET LOCAL plan_cache_mode = force_custom_plan").Error; err != nil {
			return err
		}
		reader := &Server{db: tx, readOnlyLedger: s.readOnlyLedger, privateReadSnapshotTimeout: s.privateReadSnapshotTimeout, readSnapshotStarted: started, commissionPrivateProfile: s.commissionPrivateProfile, privateProjectionReceipts: pending, commissionSourceCorrections: s.commissionSourceCorrections, sourceCorrectionReceipts: pendingCorrections, covRollup: s.covRollup}
		if err := reader.validatePrivateProjection(); err != nil {
			return err
		}
		return fn(reader)
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err == nil && pending != nil {
		s.privateProjectionReceipts.merge(pending)
	}
	if err == nil && pendingCorrections != nil {
		s.sourceCorrectionReceipts.merge(pendingCorrections)
	}
	return err
}

func (s *Server) requireReportReady(chain string, addresses []string, start, end time.Time) error {
	_, err := s.reportReadiness(chain, addresses, start, end)
	return err
}

// reportReadiness gates the report and returns the IBC refund pairs its rows
// reconcile.
func (s *Server) reportReadiness(chain string, addresses []string, start, end time.Time) (map[uint]string, error) {
	if err := s.requireClassificationReady(chain, addresses, start, end); err != nil {
		return nil, err
	}
	return s.ibcBasisCheck(chain, addresses, end)
}

// Gates, full-history IBC dependencies, financial rows, fees, memos and optional
// cache evidence all belong to the same PostgreSQL MVCC snapshot. Materialize
// before pricing so external services cannot prolong a database transaction.
func (s *Server) readReportInput(chain string, addresses []string, start, end time.Time, fingerprint bool) (reportInput, error) {
	return s.readWalletInput(chain, addresses, start, end, fingerprint, s.legacyEras(chain), 0)
}

// readReportJobInput is readReportInput for a background report job.
func (s *Server) readReportJobInput(chain string, addresses []string, start, end time.Time) (reportInput, error) {
	return s.readWalletInput(chain, addresses, start, end, true, s.legacyEras(chain), reportJobReadSnapshotTimeout)
}

// readCurrentChainInput is readReportInput on cosmoshub-4 alone, for
// readers keyed by cosmoshub-4 heights (commission flows).
func (s *Server) readCurrentChainInput(chain string, addresses []string, start, end time.Time) (reportInput, error) {
	return s.readWalletInput(chain, addresses, start, end, false, nil, 0)
}

func (s *Server) readWalletInput(chain string, addresses []string, start, end time.Time, fingerprint bool, eras *legacyEraState, timeout time.Duration) (input reportInput, err error) {
	phases := s.privateReadPhases(addresses, start, end, "snapshot")
	defer func() { err = phases.wrap(err) }()
	// The era state is read before the snapshot opens: the reader facade
	// holds only the database. The legacy schema is queried only when an
	// earlier chain is in reports, so a role that cannot read it never aborts
	// a report's transaction.
	err = s.withReadSnapshotTimeout(timeout, func(reader *Server) error {
		phases.next("readiness")
		refundPairs, err := reader.reportReadiness(chain, addresses, start, end)
		if err != nil {
			return err
		}
		for _, addr := range addresses {
			phases.next("wallet_rows")
			rows, err := reader.loadWalletRows(chain, addr, start, end, refundPairs)
			if err != nil {
				return err
			}
			phases.next("native_source_corrections")
			rows, err = reader.applyCommissionSourceCorrections(chain, addr, start, end, rows)
			if err != nil {
				return err
			}
			if eras.active() {
				phases.next("legacy_rows")
				events, fees, err := reader.legacyWalletRows(eras, addr, start, end)
				if err != nil {
					return err
				}
				rows = withLegacyRows(rows, events, fees)
			}
			input.Wallets = append(input.Wallets, rows)
		}
		if fingerprint {
			phases.next("fingerprint")
			var err error
			input.Fingerprint, err = reader.reviewFingerprintInSnapshot(chain, addresses)
			if err != nil {
				return err
			}
		}
		phases.next("commit")
		return nil
	})
	return input, err
}

func (s *Server) reviewedInputRows(chain string, addresses []string, input reportInput, decisions AssetDecisions, allowPending bool) ([]Row, error) {
	var rows []Row
	for i, addr := range addresses {
		rows = append(rows, s.priceWalletRows(chain, addr, input.Wallets[i])...)
	}
	return applyReviewedRows(rows, decisions, allowPending)
}
