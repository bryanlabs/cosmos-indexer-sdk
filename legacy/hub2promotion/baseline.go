package hub2promotion

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// The complete manifest binds the destination's catalog and loader-scope
// digests, and those only exist once the loader scope is installed. So the
// first steps on a destination cannot ask for a complete manifest:
//
//  1. ObserveServer (read-only) records which physical server this is.
//  2. InstallScopeForChain installs the fixed, reviewed loader objects for one
//     chain on exactly that server.
//  3. RecordFirstBaseline (read-only) records the installed scope digests and
//     the fingerprint of every other chain's rows already there.
//
// The manifest is assembled from the baseline, and preflight, load and audit
// then validate the complete manifest exactly as before. None of these steps
// relaxes anything checked before rows are written.
const ServerObservationSchema = "legacy-server-observation-v1"

// ServerObservation names the physical database server. Its hash is pinned
// into the scope install and the baseline, so both act on the server that was
// looked at and compared with the known production identity.
type ServerObservation struct {
	Schema      string   `json:"schema"`
	Server      Identity `json:"server"`
	ObservedUTC string   `json:"observed_utc"`
}

func completeRuntime(i Identity) bool {
	return i.DeploymentUID != "" && i.PodUID != "" && i.PVCUID != "" && i.Endpoint != ""
}
func sameRuntime(a, b Identity) bool {
	return a.DeploymentUID == b.DeploymentUID && a.PodUID == b.PodUID && a.PVCUID == b.PVCUID && a.Endpoint == b.Endpoint
}
func serverIdentity(ctx context.Context, q querier, runtime Identity) (Identity, error) {
	i := Identity{DeploymentUID: runtime.DeploymentUID, PodUID: runtime.PodUID, PVCUID: runtime.PVCUID, Endpoint: runtime.Endpoint}
	if e := q.QueryRowContext(ctx, `SELECT current_database(),oid FROM pg_catalog.pg_database WHERE datname=current_database()`).Scan(&i.Database, &i.OID); e != nil {
		return i, e
	}
	var system string
	if e := q.QueryRowContext(ctx, `SELECT system_identifier::pg_catalog.text FROM pg_catalog.pg_control_system()`).Scan(&system); e != nil {
		return i, e
	}
	h := sha256.Sum256([]byte(system))
	i.SystemHash = hex.EncodeToString(h[:])
	return i, nil
}

// ObserveServer writes nothing and needs no loader scope.
func ObserveServer(ctx context.Context, auditor *sql.DB, runtime Identity) (ServerObservation, error) {
	out := ServerObservation{Schema: ServerObservationSchema}
	if !completeRuntime(runtime) {
		return out, errors.New("runtime identity incomplete")
	}
	tx, e := beginAudit(ctx, auditor)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	if out.Server, e = serverIdentity(ctx, tx, runtime); e != nil {
		return out, e
	}
	if out.Server.Database != "cosmostax" {
		return out, errors.New("server observation must name cosmostax")
	}
	out.ObservedUTC = time.Now().UTC().Format(time.RFC3339)
	return out, nil
}
func readObservation(server Pin, runtime Identity) (Identity, error) {
	var o ServerObservation
	if e := readPinnedJSON(server, &o); e != nil {
		return Identity{}, errors.New("pinned server observation missing, changed or malformed")
	}
	s := o.Server
	if o.Schema != ServerObservationSchema || s.Database != "cosmostax" || s.OID == 0 || !shaRE.MatchString(s.SystemHash) || !completeRuntime(s) || s.CatalogSHA != "" || s.ScopeSHA != "" {
		return Identity{}, errors.New("server observation incomplete")
	}
	if !sameRuntime(s, runtime) {
		return Identity{}, errors.New("runtime identity differs from the observed server")
	}
	return s, nil
}

// installPinned refuses unless the connection is the pinned physical server,
// then creates the scope objects and checks them in one transaction. Existing
// objects are never replaced: a second install fails and rolls back.
func installPinned(ctx context.Context, owner *sql.DB, s Scope, server Identity) error {
	tx, e := owner.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SET LOCAL search_path=pg_catalog,pg_temp; SET LOCAL lock_timeout='250ms'; SET LOCAL statement_timeout='10s'`); e != nil {
		return e
	}
	got, e := serverIdentity(ctx, tx, server)
	if e != nil {
		return e
	}
	if got.Database != server.Database || got.OID != server.OID || got.SystemHash != server.SystemHash {
		return errors.New("scope installation server identity mismatch")
	}
	text, e := s.SQL()
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, text); e != nil {
		return e
	}
	if e = CheckScope(ctx, tx, s); e != nil {
		return e
	}
	return tx.Commit()
}

// InstallScopeForChain installs the one fixed scope for an approved chain. It
// takes no row data and no accounting input, so it needs no complete manifest:
// the object names come from ApprovedScope and the server from the pinned
// observation.
func InstallScopeForChain(ctx context.Context, owner *sql.DB, chain string, server Pin, runtime Identity) error {
	s, e := ApprovedScope(chain)
	if e != nil {
		return e
	}
	id, e := readObservation(server, runtime)
	if e != nil {
		return e
	}
	return installPinned(ctx, owner, s, id)
}

// RecordFirstBaseline is the first read of a destination that has no manifest
// yet. It runs in one READ ONLY transaction and adopts nothing: it refuses
// unless this is the pinned server, the installed loader scope passes the
// same check the load runs, the four eras are defined and unpublished, the
// retained Hub 3 genesis is the genuine one, and the target chain holds no
// rows. What it records is checked again by preflight, by the load before its
// first row and before commit, and by audit.
func RecordFirstBaseline(ctx context.Context, auditor *sql.DB, chain string, server Pin, runtime Identity) (DestinationCapture, error) {
	scope, e := ApprovedScope(chain)
	out := DestinationCapture{Schema: DestinationCaptureSchema, Chain: chain, Scope: scope, Server: server}
	if e != nil {
		return out, e
	}
	id, e := readObservation(server, runtime)
	if e != nil {
		return out, e
	}
	tx, e := beginAuditBudget(ctx, auditor, true)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	if out.Destination, e = ProbeIdentity(ctx, tx, scope, runtime); e != nil {
		return out, errors.New("destination identity or installed loader scope unreadable")
	}
	if e = validateIdentity(out.Destination); e != nil {
		return out, e
	}
	if out.Destination.Database != id.Database || out.Destination.OID != id.OID || out.Destination.SystemHash != id.SystemHash {
		return out, errors.New("this is not the observed server")
	}
	if e = CheckScope(ctx, tx, scope); e != nil {
		return out, e
	}
	for chain, into := range map[string]*string{"cosmoshub-3": &out.Hub3GenesisSHA, "cosmoshub-4": &out.Hub4GenesisSHA} {
		if e = tx.QueryRowContext(ctx, `SELECT sha256 FROM legacy.genesis_files WHERE chain_id=$1`, chain).Scan(into); e != nil {
			return out, errors.New("destination holds no genesis for " + chain)
		}
	}
	if out.Hub3GenesisSHA != acceptedHub3GenesisSHA {
		return out, errors.New("destination Hub 3 genesis is not the genuine one")
	}
	if e = erasAndGenesis(ctx, tx, Manifest{Hub3GenesisSHA: out.Hub3GenesisSHA, Hub4GenesisSHA: out.Hub4GenesisSHA}); e != nil {
		return out, e
	}
	if e = emptyChain(ctx, tx, scope.chain()); e != nil {
		return out, e
	}
	out.TargetEmpty = true
	if out.Protected, e = snapshotChain(ctx, tx, scope.chain(), true, true); e != nil {
		return out, errors.New("existing history fingerprint unreadable")
	}
	out.CapturedUTC = time.Now().UTC().Format(time.RFC3339)
	return out, nil
}
