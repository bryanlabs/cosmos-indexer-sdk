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
	op := flag.String("operation", "validate", "validate, scope-sql, install-scope, load, audit")
	manifest := flag.String("manifest", "", "absolute manifest path")
	hash := flag.String("sha256", "", "independently pinned manifest hash")
	config := flag.String("connections", "", "private 0600 connection file, never printed")
	runtimeFile := flag.String("runtime", "", "fresh actual Kubernetes destination identity JSON, 0600")
	output := flag.String("output", "", "NEW retained output file for scope-sql")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	m, e := p.ReadManifestPinned(*manifest, *hash)
	if e != nil {
		return errors.New("pinned manifest unreadable")
	}
	if e = m.ValidateApproved(); e != nil {
		return e
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
	if *op != "load" && *op != "audit" && *op != "install-scope" {
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
		return errors.New("atomic load refused; new rows rolled back before commit, inspect retained private failure")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"committed": true, "profile": m.Profile, "raw_stage": m.Scope.RawStage, "financial_complete": !m.Scope.RawStage, "readback": p.OutcomeExact, "rows": result.Rows, "duration_ns": result.Duration})
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
