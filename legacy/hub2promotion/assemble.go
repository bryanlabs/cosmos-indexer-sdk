package hub2promotion

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// ExportStaging takes the read-only snapshot of one chain from a private
// staging database into a NEW directory and writes the index the finalizer
// and the manifest assembler read. The staging identity is observed from the
// connection itself, so the index names the database the rows came from.
func ExportStaging(ctx context.Context, source *sql.DB, dir, chain string) (ProjectionIndex, Pin, error) {
	var id Identity
	var system string
	if e := source.QueryRowContext(ctx, `SELECT current_database(),oid FROM pg_catalog.pg_database WHERE datname=current_database()`).Scan(&id.Database, &id.OID); e != nil {
		return ProjectionIndex{}, Pin{}, errors.New("staging database unreachable")
	}
	if e := source.QueryRowContext(ctx, `SELECT system_identifier::pg_catalog.text FROM pg_catalog.pg_control_system()`).Scan(&system); e != nil {
		return ProjectionIndex{}, Pin{}, errors.New("staging server identity unreadable")
	}
	sum := sha256.Sum256([]byte(system))
	id.SystemHash = hex.EncodeToString(sum[:])
	tables, e := ExportApprovedProjection(ctx, source, id, dir, chain)
	if e != nil {
		return ProjectionIndex{}, Pin{}, e
	}
	index := ProjectionIndex{Schema: ProjectionIndexSchema, Chain: chain, Staging: id, Tables: tables}
	pin, e := writeNew(filepath.Join(dir, "projection-index.json"), index)
	return index, pin, e
}

// DestinationCapture is the first baseline (see RecordFirstBaseline): a
// read-only observation of the database a chain will be loaded into: which database it is, a fingerprint of every other chain's
// rows already there, and the later chains' genesis hashes it holds.
type DestinationCapture struct {
	Schema         string   `json:"schema"`
	Chain          string   `json:"chain"`
	Scope          Scope    `json:"scope"`
	Destination    Identity `json:"destination"`
	Server         Pin      `json:"server_observation"`
	TargetEmpty    bool     `json:"target_empty"`
	Protected      Snapshot `json:"protected"`
	Hub3GenesisSHA string   `json:"hub3_genesis_sha"`
	Hub4GenesisSHA string   `json:"hub4_genesis_sha"`
	CapturedUTC    string   `json:"captured_utc"`
}

const DestinationCaptureSchema = "legacy-destination-capture-v1"

// AssembleSpec names the finalizer's report and the destination observation.
type AssembleSpec struct {
	Report         Pin    `json:"report"`
	Destination    Pin    `json:"destination"`
	ApprovalSource string `json:"approval_source"`
}

// AssembleManifest builds the loader's complete manifest from the finalizer
// report. It adds no judgement of its own: every accounting value is copied
// from the report, and the result must pass the loader's own validation
// before the manifest file is written.
func AssembleManifest(spec AssembleSpec, path string) (Manifest, Pin, error) {
	var r FinalReport
	if e := readPinnedJSON(spec.Report, &r); e != nil {
		return Manifest{}, Pin{}, refuse("finalization report", "missing, changed or malformed")
	}
	b, e := approvedBounds(r.Chain)
	if e != nil {
		return Manifest{}, Pin{}, e
	}
	if r.Schema != FinalizationSchema || !r.Accepted || r.FixtureBounds || r.Held != 0 || !r.SourcePreserved || r.Last != b.last || r.Canonical != b.canonical {
		return Manifest{}, Pin{}, refuse("finalization report", "whole-chain acceptance is not true for the real %s boundaries", r.Chain)
	}
	var d DestinationCapture
	if e = readPinnedJSON(spec.Destination, &d); e != nil {
		return Manifest{}, Pin{}, refuse("destination capture", "missing, changed or malformed")
	}
	if d.Schema != DestinationCaptureSchema || d.Chain != r.Chain || d.Scope.chain() != r.Chain {
		return Manifest{}, Pin{}, refuse("destination capture", "taken for a different chain")
	}
	if !d.TargetEmpty {
		return Manifest{}, Pin{}, refuse("destination capture", "the destination already held rows for this chain")
	}
	if scope, _ := ApprovedScope(r.Chain); d.Scope != scope {
		return Manifest{}, Pin{}, refuse("destination capture", "loader object names differ from the reviewed install")
	}
	if r.Chain == "cosmoshub-2" && d.Hub3GenesisSHA != r.NextGenesisSHA {
		return Manifest{}, Pin{}, refuse("destination capture", "the destination's Hub 3 genesis is not the one the balances were compared against")
	}
	if strings.TrimSpace(spec.ApprovalSource) == "" {
		return Manifest{}, Pin{}, refuse("approval source", "where the standing approval was given must be recorded")
	}
	approved := &ApprovedLoad{ApprovalQuote: ApprovalQuote, ApprovalSource: spec.ApprovalSource, CanonicalExport: r.Inputs["canonical_export"], NextGenesis: r.Inputs["next_genesis"]}
	if b.migrates {
		migrated, binary := r.Inputs["migrated_export"], r.Inputs["migration_binary"]
		approved.MigratedExport, approved.MigrationBinary = &migrated, &binary
	}
	m := Manifest{Version: 1, Profile: ApprovedProfile, First: 1, Last: r.Last, Canonical: r.Canonical, MigrationLast: r.Canonical, TailFirst: r.TailFirst, TailLast: r.TailLast,
		BuildSHA: r.BuildSHA, BinarySHA: r.BinarySHA, BuildIdentity: r.Outputs["build_identity"], Source: r.Outputs["source"], Financial: spec.Report, Genesis: r.Inputs["genesis"],
		Hub3GenesisSHA: d.Hub3GenesisSHA, Hub4GenesisSHA: d.Hub4GenesisSHA, Destination: d.Destination, Scope: d.Scope, Components: r.Components, Tables: r.Tables, Protected: d.Protected,
		CanonicalRows: r.CanonicalRows, CanonicalProjectionSHA: r.CanonicalProjectionSHA, Approved: approved}
	if e = m.ValidateApproved(); e != nil {
		return Manifest{}, Pin{}, refuse("assembled manifest", "the loader's own validation refuses it: %v", e)
	}
	pin, e := writeNew(path, m)
	if e != nil {
		return Manifest{}, Pin{}, refuse("manifest path", "must be a NEW file")
	}
	return m, pin, nil
}

// SafeFailure describes a load failure without row contents. PostgreSQL's
// detail and message text can quote keys and values, so only the error class
// and the object names are returned. The full text belongs in a private file.
func SafeFailure(e error) string {
	var pg *pgconn.PgError
	if !errors.As(e, &pg) {
		return ""
	}
	return fmt.Sprintf("PostgreSQL SQLSTATE %s table=%q constraint=%q column=%q", pg.Code, pg.TableName, pg.ConstraintName, pg.ColumnName)
}

// PrivateFailure is the full cause, including the database's own detail. It
// can quote row contents and is only ever written to a private file.
func PrivateFailure(e error) string {
	var pg *pgconn.PgError
	if !errors.As(e, &pg) {
		return e.Error()
	}
	return fmt.Sprintf("%v\nSQLSTATE: %s\nmessage: %s\ndetail: %s\nhint: %s\nwhere: %s\nschema: %s\ntable: %s\ncolumn: %s\nconstraint: %s", e, pg.Code, pg.Message, pg.Detail, pg.Hint, pg.Where, pg.SchemaName, pg.TableName, pg.ColumnName, pg.ConstraintName)
}
