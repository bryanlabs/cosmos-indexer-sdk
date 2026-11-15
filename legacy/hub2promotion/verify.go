package hub2promotion

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

type Fingerprint struct {
	Rows   int64  `json:"rows"`
	SHA256 string `json:"sha256"`
}
type Snapshot map[string]Fingerprint

func fingerprint(ctx context.Context, q querier, t table, where string, physical bool) (Fingerprint, error) {
	expr := "pg_catalog.to_jsonb(r)::pg_catalog.text"
	if physical {
		expr = "r.xmin::pg_catalog.text || ':' || r.ctid::pg_catalog.text || ':' || " + expr
	}
	rows, e := q.QueryContext(ctx, "SELECT "+expr+" FROM legacy."+t.name+" r WHERE "+where+" ORDER BY "+quotedColumns(t.key))
	if e != nil {
		return Fingerprint{}, e
	}
	defer rows.Close()
	h := sha256.New()
	var n int64
	for rows.Next() {
		var s string
		if e = rows.Scan(&s); e != nil {
			return Fingerprint{}, e
		}
		io.WriteString(h, s+"\n")
		n++
	}
	if e = rows.Err(); e != nil {
		return Fingerprint{}, e
	}
	return Fingerprint{n, hex.EncodeToString(h.Sum(nil))}, nil
}
func snapshot(ctx context.Context, q querier, nonHub2 bool, physical bool) (Snapshot, error) {
	return snapshotChain(ctx, q, Chain, nonHub2, physical)
}
func snapshotChain(ctx context.Context, q querier, chain string, exclude bool, physical bool) (Snapshot, error) {
	if chain != "cosmoshub-1" && chain != "cosmoshub-2" {
		return nil, errors.New("unapproved chain")
	}
	out := Snapshot{}
	where := "true"
	if exclude {
		where = "chain_id <> '" + chain + "'"
	}
	for _, t := range allTables {
		w := where
		if t.name == "eras" {
			w = "true"
		}
		fp, e := fingerprint(ctx, q, t, w, physical)
		if e != nil {
			return nil, e
		}
		out[t.name] = fp
	}
	return out, nil
}
func equalSnapshot(a, b Snapshot) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
func emptyHub2(ctx context.Context, q querier) error { return emptyChain(ctx, q, Chain) }
func emptyChain(ctx context.Context, q querier, chain string) error {
	if chain != "cosmoshub-1" && chain != "cosmoshub-2" {
		return errors.New("unapproved chain")
	}
	for _, t := range allTables {
		if t.name == "eras" {
			continue
		}
		var yes bool
		e := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM legacy."+t.name+" WHERE chain_id=$1)", chain).Scan(&yes)
		if e != nil {
			return e
		}
		if yes {
			return fmt.Errorf("preexisting unowned chain cohort: %s", t.name)
		}
	}
	return nil
}
func erasAndGenesis(ctx context.Context, q querier, m Manifest) error {
	var ok bool
	e := q.QueryRowContext(ctx, `SELECT count(*)=4 AND bool_and(NOT published) AND
 count(*) FILTER(WHERE chain_id='cosmoshub-1' AND era=1 AND first_height=1 AND export_height=500000 AND export_paid_rewards)=1 AND
 count(*) FILTER(WHERE chain_id='cosmoshub-2' AND era=2 AND first_height=1 AND export_height=2902000 AND export_paid_rewards)=1 AND
 count(*) FILTER(WHERE chain_id='cosmoshub-3' AND era=3 AND first_height=1 AND export_height=5200790 AND NOT export_paid_rewards)=1 AND
 count(*) FILTER(WHERE chain_id='cosmoshub-4' AND era=4 AND first_height=5200791 AND export_height IS NULL AND NOT export_paid_rewards)=1 FROM legacy.eras`).Scan(&ok)
	if e != nil {
		return e
	}
	if !ok {
		return errors.New("era definition/publication drift")
	}
	for _, v := range []struct{ chain, sha string }{{"cosmoshub-3", m.Hub3GenesisSHA}, {"cosmoshub-4", m.Hub4GenesisSHA}} {
		var got string
		if e = q.QueryRowContext(ctx, `SELECT sha256 FROM legacy.genesis_files WHERE chain_id=$1`, v.chain).Scan(&got); e != nil {
			return e
		}
		if got != v.sha {
			return errors.New("retained genesis mismatch")
		}
	}
	return nil
}

// ProbeIdentity is called only on a separately supplied, fresh READ ONLY
// auditor transaction. The auditor must be able to read pg_control_system.
// Kubernetes UID provenance is external, supplied by the eventual operator.
func ProbeIdentity(ctx context.Context, q querier, s Scope, runtime Identity) (Identity, error) {
	i := runtime
	e := q.QueryRowContext(ctx, `SELECT current_database(),oid FROM pg_catalog.pg_database WHERE datname=current_database()`).Scan(&i.Database, &i.OID)
	if e != nil {
		return i, e
	}
	var system string
	if e = q.QueryRowContext(ctx, `SELECT system_identifier::pg_catalog.text FROM pg_catalog.pg_control_system()`).Scan(&system); e != nil {
		return i, e
	}
	h := sha256.Sum256([]byte(system))
	i.SystemHash = hex.EncodeToString(h[:])
	i.CatalogSHA, e = CatalogDigest(ctx, q, s.Schema)
	if e != nil {
		return i, e
	}
	i.ScopeSHA, e = ScopeDigest(ctx, q, s)
	return i, e
}
func verifyLocalIdentity(ctx context.Context, q querier, m Manifest) error {
	var name string
	var oid uint32
	if e := q.QueryRowContext(ctx, `SELECT current_database(),oid FROM pg_catalog.pg_database WHERE datname=current_database()`).Scan(&name, &oid); e != nil {
		return e
	}
	if name != m.Destination.Database || oid != m.Destination.OID {
		return errors.New("loader destination mismatch")
	}
	sha, e := CatalogDigest(ctx, q, m.Scope.Schema)
	if e != nil {
		return e
	}
	if sha != m.Destination.CatalogSHA {
		return errors.New("destination catalog drift")
	}
	sha, e = ScopeDigest(ctx, q, m.Scope)
	if e != nil {
		return e
	}
	if sha != m.Destination.ScopeSHA {
		return errors.New("scope role drift")
	}
	return CheckScope(ctx, q, m.Scope)
}
func verifyProjection(ctx context.Context, q querier, m Manifest) error {
	for _, t := range allTables {
		if t.name == "eras" {
			continue
		}
		fp, e := fingerprint(ctx, q, t, "chain_id='"+m.Scope.chain()+"'", false)
		if e != nil {
			return e
		}
		var expected Fingerprint
		if inserted(t.name) {
			for _, p := range m.Tables {
				if p.Table == t.name {
					expected = Fingerprint{p.Rows, p.File.SHA256}
				}
			}
		} else {
			z := sha256.Sum256(nil)
			expected = Fingerprint{0, hex.EncodeToString(z[:])}
		}
		if fp != expected {
			return fmt.Errorf("exact projection mismatch: %s", t.name)
		}
	}
	var genesisSHA string
	var accounts, delegations, unbondings, validators, withdraws int64
	if e := q.QueryRowContext(ctx, `SELECT sha256,accounts,delegations,unbonding_entries,validators,withdraw_addresses FROM legacy.genesis_files WHERE chain_id=$1`, m.Scope.chain()).Scan(&genesisSHA, &accounts, &delegations, &unbondings, &validators, &withdraws); e != nil {
		return e
	}
	expectedCounts := map[string]int64{}
	for _, p := range m.Tables {
		expectedCounts[p.Table] = p.Rows
	}
	if genesisSHA != m.Genesis.SHA256 || accounts != expectedCounts["genesis_accounts"] || delegations != expectedCounts["genesis_delegations"] || unbondings != expectedCounts["genesis_unbondings"] || validators != expectedCounts["genesis_validators"] || withdraws != expectedCounts["genesis_withdraw_addresses"] {
		return errors.New("genuine genesis/count projection mismatch")
	}
	var wrongTxCount int64
	if e := q.QueryRowContext(ctx, `SELECT count(*) FROM legacy.blocks b WHERE b.chain_id=$1 AND b.tx_count<>(SELECT count(*) FROM legacy.tx_inclusions tx WHERE tx.chain_id=b.chain_id AND tx.height=b.height)`, m.Scope.chain()).Scan(&wrongTxCount); e != nil {
		return e
	}
	if wrongTxCount != 0 {
		return errors.New("transaction inclusion/header count mismatch")
	}
	var headers, min, max, distinct int64
	if e := q.QueryRowContext(ctx, `SELECT count(*),coalesce(min(height),0),coalesce(max(height),0),count(DISTINCT height) FROM legacy.blocks WHERE chain_id=$1`, m.Scope.chain()).Scan(&headers, &min, &max, &distinct); e != nil {
		return e
	}
	if headers != m.Last || distinct != headers || min != 1 || max != m.Last {
		return errors.New("header gap/duplicate/tail")
	}
	var payout int64
	if e := q.QueryRowContext(ctx, `SELECT count(*) FROM legacy.taxable_events WHERE chain_id=$2 AND (phase=4 OR amount_source='export_payout') AND NOT (phase=4 AND amount_source='export_payout' AND height=$1)`, m.Canonical, m.Scope.chain()).Scan(&payout); e != nil {
		return e
	}
	if payout != 0 {
		return errors.New("intermediate/misplaced export income")
	}
	var t table
	for _, v := range allTables {
		if v.name == "taxable_events" {
			t = v
		}
	}
	fp, e := fingerprint(ctx, q, t, "chain_id='"+m.Scope.chain()+"' AND phase=4", false)
	if e != nil {
		return e
	}
	if fp.Rows != m.CanonicalRows || fp.SHA256 != m.CanonicalProjectionSHA {
		return errors.New("canonical payout missing/duplicate/unbound")
	}
	var bad int64
	if e = q.QueryRowContext(ctx, `SELECT count(*) FROM legacy.taxable_events e WHERE e.chain_id=$1 AND e.height>0 AND NOT EXISTS(SELECT 1 FROM legacy.blocks b WHERE b.chain_id=e.chain_id AND b.height=e.height)`, m.Scope.chain()).Scan(&bad); e != nil {
		return e
	}
	if bad != 0 {
		return errors.New("tax row has no historical header")
	}
	return nil
}

type Outcome string

const (
	OutcomeExact    Outcome = "exact"
	OutcomeEmpty    Outcome = "empty"
	OutcomeConflict Outcome = "conflict"
)

// AuditOutcome never adopts, repairs or retries. Even an empty result is only a
// report. A new attempt requires independent re-admission by an operator.
func AuditOutcome(ctx context.Context, audit *sql.DB, m Manifest, runtime Identity) (Outcome, error) {
	tx, e := beginAuditBudget(ctx, audit, m.scanBudget())
	if e != nil {
		return OutcomeConflict, e
	}
	defer tx.Rollback()
	i, e := ProbeIdentity(ctx, tx, m.Scope, runtime)
	if e != nil {
		return OutcomeConflict, e
	}
	if i != m.Destination {
		return OutcomeConflict, errors.New("audit identity mismatch")
	}
	if e = verifyLocalIdentity(ctx, tx, m); e != nil {
		return OutcomeConflict, e
	}
	if e = erasAndGenesis(ctx, tx, m); e != nil {
		return OutcomeConflict, e
	}
	protected, e := snapshotChain(ctx, tx, m.Scope.chain(), true, true)
	if e != nil {
		return OutcomeConflict, e
	}
	if !equalSnapshot(protected, m.Protected) {
		return OutcomeConflict, nil
	}
	if emptyChain(ctx, tx, m.Scope.chain()) == nil {
		return OutcomeEmpty, nil
	}
	if verifyProjection(ctx, tx, m) != nil {
		return OutcomeConflict, nil
	}
	return OutcomeExact, nil
}

func freshScopeAudit(ctx context.Context, auditor *sql.DB, m Manifest, runtime Identity) error {
	tx, e := beginAuditBudget(ctx, auditor, m.scanBudget())
	if e != nil {
		return e
	}
	defer tx.Rollback()
	i, e := ProbeIdentity(ctx, tx, m.Scope, runtime)
	if e != nil {
		return e
	}
	if i != m.Destination {
		return errors.New("fresh precommit catalog/scope drift")
	}
	if e = CheckScope(ctx, tx, m.Scope); e != nil {
		return e
	}
	p, e := snapshotChain(ctx, tx, m.Scope.chain(), true, true)
	if e != nil {
		return e
	}
	if !equalSnapshot(p, m.Protected) {
		return errors.New("fresh precommit protected baseline drift")
	}
	return nil
}

func beginAudit(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	return beginAuditBudget(ctx, db, false)
}

// Only the explicit private 1.4M profile receives a larger bounded scan budget.
// Lock acquisition stays at 250ms and every exact comparison remains required.
func beginAuditBudget(ctx context.Context, db *sql.DB, scale bool) (*sql.Tx, error) {
	tx, e := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if e != nil {
		return nil, e
	}
	if _, e = tx.ExecContext(ctx, `SET LOCAL search_path=pg_catalog,pg_temp; SET LOCAL timezone='UTC'; SET LOCAL lock_timeout='250ms'; SET LOCAL statement_timeout='30s'`); e != nil {
		_ = tx.Rollback()
		return nil, e
	}
	if scale {
		if _, e = tx.ExecContext(ctx, `SET LOCAL statement_timeout='10min'`); e != nil {
			_ = tx.Rollback()
			return nil, e
		}
	}
	return tx, nil
}

func qualifiedInsert(s Scope, t table) string {
	return "INSERT INTO " + s.Schema + "." + t.name + " (" + quotedColumns(t.columns) + ") SELECT " + quotedColumns(t.columns) + " FROM pg_catalog.jsonb_populate_recordset(NULL::legacy." + t.name + ",$1::pg_catalog.jsonb)"
}
func projectionColumns(t table) []string { return strings.Split(t.columns, ",") }
