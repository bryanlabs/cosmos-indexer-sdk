package taxapi

import (
	"context"
	"database/sql"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"gorm.io/gorm"
)

const readSnapshotTimeout = 30 * time.Second

type walletRows struct {
	Events []tax.TaxableEvent
	Fees   []feeRec
	Memos  map[string]string
}

type reportInput struct {
	Wallets     []walletRows
	Fingerprint string
}

// withReadSnapshot creates a DB-only facade, NOT a copy of Server (which owns
// mutexes). Only DB query methods may run on it. All pricing, review application
// and rendering run on the original Server, preserving its configuration and
// shared caches. No network calls or report-job writes belong in this callback.
func (s *Server) readSnapshotContext() (context.Context, context.CancelFunc) {
	timeout := readSnapshotTimeout
	if s.readOnlyLedger && s.privateReadSnapshotTimeout > 0 {
		timeout = s.privateReadSnapshotTimeout
	}
	return context.WithTimeout(s.db.Statement.Context, timeout)
}

func (s *Server) withReadSnapshot(fn func(*Server) error) error {
	started := time.Now()
	ctx, cancel := s.readSnapshotContext()
	defer cancel()
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
		reader := &Server{db: tx, readOnlyLedger: s.readOnlyLedger, privateReadSnapshotTimeout: s.privateReadSnapshotTimeout, readSnapshotStarted: started, commissionPrivateProfile: s.commissionPrivateProfile, privateProjectionReceipts: pending}
		if err := reader.validatePrivateProjection(); err != nil {
			return err
		}
		return fn(reader)
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err == nil && pending != nil {
		s.privateProjectionReceipts.merge(pending)
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
func (s *Server) readReportInput(chain string, addresses []string, start, end time.Time, fingerprint bool) (input reportInput, err error) {
	phases := s.privateReadPhases(addresses, start, end, "snapshot")
	defer func() { err = phases.wrap(err) }()
	err = s.withReadSnapshot(func(reader *Server) error {
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
