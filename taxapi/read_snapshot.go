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
func (s *Server) withReadSnapshot(fn func(*Server) error) error {
	ctx, cancel := context.WithTimeout(s.db.Statement.Context, readSnapshotTimeout)
	defer cancel()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(&Server{db: tx})
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
}

func (s *Server) requireReportReady(chain string, addresses []string, start, end time.Time) error {
	if err := s.requireClassificationReady(chain, addresses, start, end); err != nil {
		return err
	}
	return s.requireIBCBasisReady(chain, addresses, end)
}

// Gates, full-history IBC dependencies, financial rows, fees, memos and optional
// cache evidence all belong to the same PostgreSQL MVCC snapshot. Materialize
// before pricing so external services cannot prolong a database transaction.
func (s *Server) readReportInput(chain string, addresses []string, start, end time.Time, fingerprint bool) (reportInput, error) {
	var input reportInput
	err := s.withReadSnapshot(func(reader *Server) error {
		if err := reader.requireReportReady(chain, addresses, start, end); err != nil {
			return err
		}
		for _, addr := range addresses {
			rows, err := reader.loadWalletRows(chain, addr, start, end)
			if err != nil {
				return err
			}
			input.Wallets = append(input.Wallets, rows)
		}
		if fingerprint {
			var err error
			input.Fingerprint, err = reader.reviewFingerprintInSnapshot(chain, addresses)
			return err
		}
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
