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
	// stampChain is true when earlier Hub chains are in reports; every row
	// then names its chain.
	stampChain bool
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
		// The driver caches prepared statements, and after five runs Postgres
		// may switch to a generic plan that cannot see the period or the type
		// ids; the readiness counts then went from 1s to over 20s.
		if err := tx.Exec("SET LOCAL plan_cache_mode = force_custom_plan").Error; err != nil {
			return err
		}
		return fn(&Server{db: tx})
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
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
	return s.readWalletInput(chain, addresses, start, end, fingerprint, s.legacyEras(chain))
}

// readCurrentChainInput is readReportInput on cosmoshub-4 alone, for
// readers keyed by cosmoshub-4 heights (commission flows).
func (s *Server) readCurrentChainInput(chain string, addresses []string, start, end time.Time) (reportInput, error) {
	return s.readWalletInput(chain, addresses, start, end, false, nil)
}

func (s *Server) readWalletInput(chain string, addresses []string, start, end time.Time, fingerprint bool, eras *legacyEraState) (reportInput, error) {
	var input reportInput
	// The era state is read before the snapshot opens: the reader facade
	// holds only the database. The legacy schema is queried only when an
	// earlier chain is in reports, so a role that cannot read it never aborts
	// a report's transaction.
	err := s.withReadSnapshot(func(reader *Server) error {
		refundPairs, err := reader.reportReadiness(chain, addresses, start, end)
		if err != nil {
			return err
		}
		for _, addr := range addresses {
			rows, err := reader.loadWalletRows(chain, addr, start, end, refundPairs)
			if err != nil {
				return err
			}
			if eras.active() {
				events, fees, err := reader.legacyWalletRows(eras, addr, start, end)
				if err != nil {
					return err
				}
				rows = withLegacyRows(rows, events, fees)
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
