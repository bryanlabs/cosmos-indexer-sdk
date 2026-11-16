// Command legacy-approved-load is the one chain-scoped Hub1/Hub2 production
// adapter. It never reads an environment DSN or changes publication flags.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	p "github.com/DefiantLabs/cosmos-indexer/legacy/hub2promotion"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type connections struct {
	Loader  string `json:"loader_dsn"`
	Auditor string `json:"auditor_dsn"`
	Owner   string `json:"owner_dsn"`
}

func privateJSON(path string, v any) error {
	st, e := os.Lstat(path)
	if e != nil {
		return errors.New("private input unavailable")
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return errors.New("private input must be a regular 0600 file")
	}
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	opened, e := f.Stat()
	if e != nil || !os.SameFile(st, opened) {
		return errors.New("private input changed")
	}
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return errors.New("invalid private configuration")
	}
	return nil
}
func connect(dsn string) (*sql.DB, error) {
	if dsn == "" {
		return nil, errors.New("explicit connection missing")
	}
	c, e := pgx.ParseConfig(dsn)
	if e != nil {
		return nil, errors.New("invalid private connection")
	}
	if c.Database != "cosmostax" || len(c.Fallbacks) > 0 {
		return nil, errors.New("live connection must name cosmostax without fallback")
	}
	c.ConnectTimeout = 5 * time.Second
	db := stdlib.OpenDB(*c)
	db.SetMaxOpenConns(2)
	return db, nil
}
func run() error {
	op := flag.String("operation", "validate", "validate, scope-sql, install-scope, preflight, load, audit")
	manifest := flag.String("manifest", "", "absolute manifest path")
	hash := flag.String("sha256", "", "independently pinned manifest hash")
	config := flag.String("connections", "", "private 0600 connection file, never printed")
	runtimeFile := flag.String("runtime", "", "fresh actual Kubernetes destination identity JSON, 0600")
	output := flag.String("output", "", "NEW retained output file for scope-sql or preflight")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	m, e := p.ReadManifestPinned(*manifest, *hash)
	if e != nil {
		return errors.New("pinned manifest unreadable")
	}
	// Raw loading validates all source bytes inside the core before its first
	// connection and before COMMIT. Other operations retain offline admission.
	if *op != "load" || m.Profile != p.ApprovedRawProfile {
		if e = m.ValidateApproved(); e != nil {
			return e
		}
	}
	if *op == "validate" {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"validated": true, "profile": m.Profile, "raw_stage": m.Scope.RawStage, "production_rows_written": 0})
	}
	if *op == "scope-sql" {
		text, e := m.Scope.SQL()
		if e != nil {
			return e
		}
		f, e := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		defer f.Close()
		if _, e = f.WriteString(text); e != nil {
			return e
		}
		return f.Sync()
	}
	if *op != "load" && *op != "audit" && *op != "install-scope" && *op != "preflight" {
		return errors.New("unknown operation")
	}
	var c connections
	if e = privateJSON(*config, &c); e != nil {
		return e
	}
	var runtime p.Identity
	if e = privateJSON(*runtimeFile, &runtime); e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Hour)
	defer cancel()
	if *op == "install-scope" {
		// The execution owner coordinates this explicit operation with Auth after
		// export-cache restoration. No role, HBA or server restart is performed.
		owner, e := connect(c.Owner)
		if e != nil {
			return e
		}
		defer owner.Close()
		if e = p.InstallApprovedScope(ctx, owner, m, runtime); e != nil {
			return errors.New("atomic scope installation refused")
		}
		return nil
	}
	auditor, e := connect(c.Auditor)
	if e != nil {
		return e
	}
	defer auditor.Close()
	if *op == "preflight" {
		report, e := p.PreflightApprovedTarget(ctx, auditor, m, runtime)
		if e != nil {
			return errors.New("read-only target preflight refused; no baseline adopted")
		}
		f, e := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		defer f.Close()
		if e = json.NewEncoder(f).Encode(report); e != nil {
			return e
		}
		if e = f.Sync(); e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"preflight_observed": true, "target_empty": report.TargetEmpty, "production_rows_written": 0, "manifest_rewritten": false})
	}
	if *op == "audit" {
		outcome, e := p.AuditOutcome(ctx, auditor, m, runtime)
		if e != nil {
			return errors.New("read-only audit refused")
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"outcome": outcome, "production_rows_written": 0})
	}
	loader, e := connect(c.Loader)
	if e != nil {
		return e
	}
	defer loader.Close()
	result, e := p.MergeApproved(ctx, loader, auditor, m, runtime)
	if e != nil {
		if errors.Is(e, p.ErrCommitUnknown) || errors.Is(e, p.ErrCommittedUnverified) {
			return e
		}
		return loadRefused(*manifest, e)
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"committed": true, "profile": m.Profile, "raw_stage": m.Scope.RawStage, "financial_complete": !m.Scope.RawStage, "readback": p.OutcomeExact, "rows": result.Rows, "duration_ns": result.Duration})
}

// loadRefused keeps the real cause. Row contents can appear in a database
// error, so the full text goes to a NEW private file beside the manifest and
// only the error class and object names are printed.
func loadRefused(manifest string, cause error) error {
	text := "atomic load refused; new rows rolled back before commit"
	if safe := p.SafeFailure(cause); safe != "" {
		text += "; " + safe
	}
	path := fmt.Sprintf("%s.load-failure-%d.private.log", manifest, time.Now().UnixNano())
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return errors.New(text + "; private failure file could not be written")
	}
	defer f.Close()
	if _, e = fmt.Fprintln(f, p.PrivateFailure(cause)); e != nil || f.Sync() != nil {
		return errors.New(text + "; private failure file could not be written")
	}
	return errors.New(text + "; full cause retained in " + path)
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
