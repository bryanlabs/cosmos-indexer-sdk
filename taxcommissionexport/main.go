// taxcommissionexport exports explicitly selected private results read-only.
// It never runs migrations, a collector, or a serving change.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/taxapi"
)

func fail(message string) {
	// Driver errors can contain a private hostname, identity or parameter.
	// Never print them from this private export tool.
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}

func main() {
	version := flag.String("version", "", "exact private output version")
	build := flag.String("build", "", "exact original computation build, never a compatibility override")
	chain := flag.String("chain", "mainnet", "exact stored chain")
	fromText := flag.String("from", "", "exact opening day YYYY-MM-DD")
	asOfText := flag.String("as-of", "", "exact exclusive end day YYYY-MM-DD")
	members := flag.String("validators-file", "", "private JSON array of explicit validators")
	output := flag.String("output", "", "new private export file, never stdout or overwrite")
	timeout := flag.Duration("timeout", 60*time.Second, "whole read/export budget, not a provider restriction")
	flag.Parse()
	if *output == "" || *members == "" || *timeout <= 0 {
		fail("private output, explicit membership and positive timeout are required")
	}
	from, err := time.Parse(time.DateOnly, *fromText)
	if err != nil {
		fail("invalid opening day")
	}
	asOf, err := time.Parse(time.DateOnly, *asOfText)
	if err != nil {
		fail("invalid exclusive end day")
	}
	data, err := os.ReadFile(*members)
	if err != nil {
		fail("cannot read private membership file")
	}
	var validators []string
	if err := json.Unmarshal(data, &validators); err != nil || len(validators) == 0 {
		fail("invalid private membership")
	}
	for _, key := range []string{"DB_HOST", "DB_NAME", "DB_USER", "DB_PASS"} {
		if os.Getenv(key) == "" {
			fail("explicit database configuration is required")
		}
	}
	port := os.Getenv("DB_PORT")
	if port == "" {
		port = "5432"
	}
	db, err := indexerDB.PostgresDbConnect(os.Getenv("DB_HOST"), port, os.Getenv("DB_NAME"), os.Getenv("DB_USER"), os.Getenv("DB_PASS"), "silent", "default_transaction_read_only=on")
	if err != nil {
		fail("cannot connect to configured private result reader")
	}
	conn, err := db.DB()
	if err != nil {
		fail("cannot configure private result reader")
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	db = db.WithContext(ctx)
	// Partial exports are retained, never mislabeled complete or overwritten.
	partial, err := os.OpenFile(*output+".partial", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fail("cannot create exclusive private partial export")
	}
	hash := sha256.New()
	srv := taxapi.NewServer(db, nil, taxapi.DefaultNativeAsset)
	err = srv.ExportCommissionFlowVersions(taxapi.CommissionFlowBuildOptions{Version: *version, Build: *build, AsOf: asOf}, *chain, from, validators, io.MultiWriter(partial, hash))
	if err != nil {
		_ = partial.Close()
		fail("private export incomplete, partial file retained and no complete file published")
	}
	if err := partial.Sync(); err != nil {
		_ = partial.Close()
		fail("private export flush failed, partial file retained")
	}
	if err := partial.Close(); err != nil {
		fail("private export close failed, partial file retained")
	}
	if err := os.Link(*output+".partial", *output); err != nil {
		fail("cannot exclusively publish complete private export, prior files retained")
	}
	dir, err := os.Open(filepath.Dir(*output))
	if err != nil {
		fail("cannot verify private export directory durability")
	}
	err = dir.Sync()
	_ = dir.Close()
	if err != nil {
		fail("cannot flush private export directory")
	}
	fmt.Printf("exported=%d sha256=%s\n", len(validators), hex.EncodeToString(hash.Sum(nil)))
}
