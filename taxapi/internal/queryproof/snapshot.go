package queryproof

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const identitySQL = `SELECT system_identifier::text || ':' || current_database() || ':' || (SELECT oid::text FROM pg_database WHERE datname=current_database()) FROM pg_control_system()`

type Admission struct {
	MainRootSHA256                          string
	CoordinatorReceiptSHA256                string
	RootSHA256                              string
	ObservedAt                              time.Time
	ExpiresAt                               time.Time
	PGUnpinned, NodeHeadroom, ReplayHealthy bool
}

// Export owns the actual read-only RR exporting transaction until Close.
// Scratch mode additionally requires an explicit loopback, named scratch DB.
// Future admitted-source capture is an execution scope, not a financial gate.
type Exported struct {
	Source Source
	db     *sql.DB
	tx     *sql.Tx
}

func Export(ctx context.Context, dsn, kind string, admission *Admission) (*Exported, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || dsn == "" {
		return nil, errors.New("explicit proof DSN required")
	}
	switch kind {
	case "scratch":
		if (cfg.Host != "127.0.0.1" && cfg.Host != "::1" && cfg.Host != "localhost") || !strings.HasPrefix(cfg.Database, "query_proof_scratch_") {
			return nil, errors.New("scratch proof requires explicit loopback named scratch database")
		}
	case "admitted-source":
		now := time.Now()
		if admission == nil || len(admission.MainRootSHA256) != 64 || len(admission.CoordinatorReceiptSHA256) != 64 || len(admission.RootSHA256) != 64 || !admission.PGUnpinned || !admission.NodeHeadroom || !admission.ReplayHealthy || now.Before(admission.ObservedAt) || now.Sub(admission.ObservedAt) > 30*time.Second || !admission.ExpiresAt.After(now) {
			return nil, errors.New("fresh Main Root and existing coordinator source admission required")
		}
	default:
		return nil, errors.New("proof capture kind required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, errors.New("proof exporter open failed")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		_ = db.Close()
		return nil, errors.New("proof exporter read-only transaction failed")
	}
	e := &Exported{db: db, tx: tx}
	fail := func() (*Exported, error) {
		_ = e.Close()
		return nil, errors.New("proof exporter identity/snapshot guard failed")
	}
	var ident, readOnly, isolation string
	if err = tx.QueryRowContext(ctx, "SELECT pg_export_snapshot(), pg_current_snapshot()::text, current_setting('transaction_read_only'), current_setting('transaction_isolation')").Scan(&e.Source.ExportedSnapshot, &e.Source.PGSnapshot, &readOnly, &isolation); err != nil {
		return fail()
	}
	if err = tx.QueryRowContext(ctx, identitySQL).Scan(&ident); err != nil || readOnly != "on" || isolation != "repeatable read" {
		return fail()
	}
	if admission != nil {
		e.Source.AdmissionSHA256 = Digest(encoded(admission))
		e.Source.AdmittedRootSHA256 = admission.RootSHA256
	}
	e.Source.Kind = kind
	e.Source.DatabaseIdentitySHA256 = Digest([]byte(ident))
	e.Source.ReadOnly = true
	e.Source.Isolation = isolation
	return e, nil
}
func (e *Exported) Close() error {
	if e.tx != nil {
		_ = e.tx.Rollback()
		e.tx = nil
	}
	if e.db != nil {
		return e.db.Close()
	}
	return nil
}
