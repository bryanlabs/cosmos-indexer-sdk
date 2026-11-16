// Command legacy-whole-chain joins a chain's independently sealed parts into
// one accepted history and hands it to legacy-approved-load. It never writes
// to a database: export reads, the rest work on pinned files.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	p "github.com/DefiantLabs/cosmos-indexer/legacy/hub2promotion"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type connections struct {
	Staging string `json:"staging_dsn"`
	Auditor string `json:"auditor_dsn"`
}

func readJSON(path string, private bool, v any) error {
	st, e := os.Lstat(path)
	if e != nil || !st.Mode().IsRegular() {
		return errors.New("input file unavailable: " + path)
	}
	if private && st.Mode().Perm() != 0600 {
		return errors.New("private input must be a regular 0600 file")
	}
	f, e := os.Open(path)
	if e != nil {
		return errors.New("input file unavailable: " + path)
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return errors.New("input file is not the expected JSON: " + path)
	}
	return nil
}

// open refuses any connection the operation is not meant for. The staging
// export only ever talks to a private loopback rehearsal database, and port
// 15432 is the customary forward to production.
func open(dsn string, staging bool) (*sql.DB, error) {
	c, e := pgx.ParseConfig(dsn)
	if e != nil || len(c.Fallbacks) > 0 {
		return nil, errors.New("invalid private connection")
	}
	if staging {
		ip := net.ParseIP(c.Host)
		if ip == nil || !ip.IsLoopback() || c.Port == 15432 || !strings.HasPrefix(c.Database, "legacy_replay_rehearsal_") {
			return nil, errors.New("staging export needs a private loopback rehearsal database, never production")
		}
	} else {
		return nil, errors.New("only the staging export connects to a database")
	}
	c.ConnectTimeout = 5 * time.Second
	db := stdlib.OpenDB(*c)
	db.SetMaxOpenConns(2)
	return db, nil
}
func show(v any) error { return json.NewEncoder(os.Stdout).Encode(v) }
func run() error {
	op := flag.String("operation", "", "export, components, finalize, manifest")
	chain := flag.String("chain", "", "cosmoshub-1 or cosmoshub-2 (export)")
	config := flag.String("connections", "", "private 0600 connection file, never printed (export)")
	spec := flag.String("spec", "", "ordered input list of paths and hashes (components, finalize)")
	dir := flag.String("output-dir", "", "NEW absolute directory (export, finalize)")
	output := flag.String("output", "", "NEW absolute file (manifest)")
	report := flag.String("report", "", "finalization report (manifest)")
	reportSHA := flag.String("report-sha256", "", "finalization report hash (manifest)")
	destination := flag.String("destination", "", "first baseline written by legacy-approved-load -operation baseline (manifest)")
	destinationSHA := flag.String("destination-sha256", "", "first baseline hash (manifest)")
	approval := flag.String("approval-source", "", "where the standing approval was given (manifest)")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Hour)
	defer cancel()
	switch *op {
	case "export":
		var c connections
		if e := readJSON(*config, true, &c); e != nil {
			return e
		}
		db, e := open(c.Staging, true)
		if e != nil {
			return e
		}
		defer db.Close()
		index, pin, e := p.ExportStaging(ctx, db, *dir, *chain)
		if e != nil {
			return errors.New("staging export refused: " + e.Error())
		}
		rows := map[string]int64{}
		for _, t := range index.Tables {
			rows[t.Table] = t.Rows
		}
		return show(map[string]any{"exported": true, "chain": index.Chain, "rows": rows, "projection_index": pin, "staging_rows_written": 0})
	case "components", "finalize":
		var s p.FinalizeSpec
		if e := readJSON(*spec, false, &s); e != nil {
			return e
		}
		if *op == "components" {
			chain, e := p.VerifyComponents(s)
			type part struct {
				First, Last int64
				Profile     string
			}
			var parts []part
			for _, c := range chain.Components {
				parts = append(parts, part{c.First, c.Last, c.Profile})
			}
			if e != nil {
				_ = show(map[string]any{"verified_through": chain.Through, "verified_parts": parts, "seams": chain.Seams})
				return e
			}
			return show(map[string]any{"joined": true, "chain": chain.Chain, "verified_through": chain.Through, "verified_parts": parts, "seams": chain.Seams, "full_chain_books_tax_acceptance": false})
		}
		r, pin, e := p.Finalize(ctx, s, *dir)
		if e != nil {
			return e
		}
		return show(map[string]any{"full_chain_books_tax_acceptance": r.Accepted, "chain": r.Chain, "first": r.First, "last": r.Last, "parts": len(r.Components), "restart_payout_rows": r.CanonicalRows, "accounts_compared": r.Balance.AccountsChecked, "differences": 0, "report": pin})
	case "manifest":
		m, pin, e := p.AssembleManifest(p.AssembleSpec{Report: p.Pin{Path: *report, SHA256: *reportSHA}, Destination: p.Pin{Path: *destination, SHA256: *destinationSHA}, ApprovalSource: *approval}, *output)
		if e != nil {
			return e
		}
		return show(map[string]any{"assembled": true, "profile": m.Profile, "chain": m.Scope.ChainID, "manifest": pin, "loader_validation": "passed"})
	}
	return errors.New("unknown operation")
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
