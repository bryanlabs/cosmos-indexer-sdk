// taxledger extends the past-date balance ledger to heights the serving node
// no longer holds: it scans blocks from the archive block cache back from the
// start of the scanned span, then records validator rates for those days from
// an archive node's REST API. Both steps resume where the last run stopped.
// The scan is a dry run unless --apply, which needs a new --backup file:
// every chunk's plan is synced there before its database transaction.
//
// With -days-from it instead stores every address's holdings at the end of
// each day from -days-from to -days-to, walking back from the serving node
// (NODE_REST_API) the way the API does. It resumes an interrupted build of
// the same span.
//
// With -days-append it extends a finished build with the days after it that
// the index, the ledger scan and the validator rates cover, up to yesterday,
// rebuilding only the wallets those days can have changed.
//
// With -days-rebuild it rebuilds the stored days of the addresses whose ids
// the file lists over the whole built span, after a correction to their
// history, writing the rows it replaces, the rows it stores and a rollback
// script to -days-rebuild-out. Without -days-rebuild-write it writes only
// those files and its session is read only.
//
// With -commission-flows it traces every validator's withdrawn commission
// from -commission-flows-from to today and stores where it went (OPS-818).
//
// With -failed-fees-to it instead settles the fee outcome of every failed
// transaction from -floor up to that height that has none yet, reading the
// archive block cache, and records them with -failed-fees-write. Before Gaia v6 the block results carry no coin
// events, so the outcome follows from where the transaction failed.
//
// Env: DB_HOST DB_PORT DB_NAME DB_USER DB_PASS, and NODE_REST_API for
// -days-from, -days-append, -days-rebuild and -commission-flows (which also reads
// ORACLE_URL for denoms).
package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/taxapi"
	"github.com/spf13/viper"
	"gorm.io/gorm"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func commissionRuntimeParams(flows bool, version string) []string {
	if !flows {
		return nil
	}
	params := []string{"jit=off"}
	if version != "" {
		// Private full-history bitmap reads benefit from page prefetch. This
		// is a connection startup parameter, never a global database change.
		params = append(params, "effective_io_concurrency=200")
	}
	return params
}

func validateCommissionReadTimeout(flows bool, version string, timeout time.Duration) error {
	if timeout < 0 {
		return fmt.Errorf("commission read snapshot timeout must not be negative")
	}
	if timeout != 0 && (!flows || strings.TrimSpace(version) == "") {
		return fmt.Errorf("commission read snapshot timeout requires private versioned commission collection")
	}
	return nil
}

func main() {
	chain := flag.String("chain", "mainnet", "chain the ledger scan belongs to")
	cache := flag.String("cache", "/cache", "archive block cache directory for this chain")
	archive := flag.String("archive-rest", os.Getenv("ARCHIVE_REST"), "archive node REST origin for validator state")
	archiveRPC := flag.String("archive-rpc", os.Getenv("ARCHIVE_RPC"), "archive node RPC origin for staking store reads where its delegation query fails")
	floor := flag.Int64("floor", 0, "lowest height to scan back to")
	chunk := flag.Int64("chunk", 5000, "blocks recorded per database transaction")
	workers := flag.Int("workers", 16, "blocks read in parallel")
	rates := flag.Bool("rates", true, "record validator rates for the scanned days afterwards")
	daysFrom := flag.String("days-from", "", "first day (YYYY-MM-DD) of a stored balance build")
	daysTo := flag.String("days-to", "", "last day of the build, default yesterday (UTC)")
	daysBatch := flag.Int("days-batch", 2000, "addresses read per batch of the build")
	daysWorkers := flag.Int("days-workers", 8, "addresses built in parallel")
	daysAppend := flag.Bool("days-append", false, "append the finished days after the stored balance build")
	daysRebuild := flag.String("days-rebuild", "", "file of address ids, one per line, whose stored balance days are rebuilt over the built span")
	daysRebuildSHA := flag.String("days-rebuild-sha256", "", "exact digest of -days-rebuild")
	daysRebuildOut := flag.String("days-rebuild-out", "", "new directory for the rebuild's before.jsonl, after.jsonl and rollback.sql")
	daysRebuildWrite := flag.Bool("days-rebuild-write", false, "replace the rows; without it the rebuild only writes its files")
	flows := flag.Bool("commission-flows", false, "trace where each validator's commission went")
	flowsFrom := flag.String("commission-flows-from", "2023-01-01", "first day of the commission trace")
	flowsOnly := flag.String("commission-validators", "", "comma-separated valoper addresses to trace, default every validator")
	flowsWorkers := flag.Int("commission-workers", 4, "validators traced in parallel")
	flowsVersion := flag.String("commission-output-version", "", "required immutable output version, leaves existing serving rows untouched")
	flowsBuild := flag.String("commission-build", "", "required image digest or source build for this output version")
	flowsAsOf := flag.String("commission-as-of", "", "required exclusive end date (YYYY-MM-DD UTC) for this output version")
	flowsArchive := flag.String("commission-archive-config", "", "optional Secret-mounted probe.toml containing exact historical archive RPC")
	flowsProfile := flag.String("commission-private-profile", "", "source-bound read-only private staging profile; never enables ordinary readiness")
	flowsProfileSHA := flag.String("commission-private-profile-sha256", "", "exact immutable staging profile digest")
	flowsCorrections := flag.String("commission-source-corrections", "", "exact retained native source corrections for private commission reads only")
	flowsCorrectionsSHA := flag.String("commission-source-corrections-sha256", "", "exact immutable native correction manifest digest")
	flowsMaxHops := flag.Int("commission-max-hops", 0, "private versioned runs only: wallets followed past the receiving wallets (0 keeps the default 3); the version must name it, e.g. -hops5")
	flowsLabels := flag.String("commission-label-file", "", "private versioned runs only: an immutable label file whose digest the version names (-labels<first 12>)")
	flowsLabelsSHA := flag.String("commission-label-file-sha256", "", "exact digest of --commission-label-file")
	deriveLabels := flag.String("commission-derive-follow-labels", "", "read only: write the deposit-follow label file for --commission-flows-from to --commission-as-of to this path and exit")
	deriveTimeout := flag.Duration("commission-derive-statement-timeout", 2*time.Minute, "read only: per-query limit while deriving labels; a condition whose query hits it is recorded unmeasured")
	flowsPrefetch := flag.Int("commission-wallet-prefetch", 0, "opt-in private versioned wallets one trace reads from the index ahead of its queue, each on its own connection (0 reads each wallet only when asked; results do not depend on it)")
	flowsReadTimeout := flag.Duration("commission-read-snapshot-timeout", 0, "opt-in private versioned read snapshot deadline (0 keeps 30s; does not change public API reads)")
	failedTo := flag.Int64("failed-fees-to", 0, "record failed-tx fee outcomes from -floor below this height, then stop")
	failedWrite := flag.Bool("failed-fees-write", false, "write the outcomes -failed-fees-to settles; without it the run only counts them")
	recheck := flag.Bool("recheck-holes", false, "recheck recorded index holes against cached blocks (dry run unless --apply)")
	apply := flag.Bool("apply", false, "apply an index-hole recheck or a ledger scan back, requires --backup")
	backup := flag.String("backup", "", "new private JSONL backup file on durable storage for an index-hole recheck or a ledger scan back")
	repeated := flag.Bool("repeated-inclusions", false, "date transactions the chain included twice at the copy that took effect and clear the holes this proves (dry run unless --apply)")
	repeatedHeights := flag.String("hole-heights", "", "comma-separated recorded hole heights for --repeated-inclusions, default every uncleared hole")
	compareHeights := flag.String("compare-heights", "", "comma-separated hole heights whose accounts --repeated-inclusions also rebuilds the way the API answers and compares with chain state")
	compareAccounts := flag.Int("compare-accounts", 1, "accounts compared per --compare-heights hole")
	flag.Parse()
	if (*flowsProfile != "" || *flowsProfileSHA != "") && (!*flows || strings.TrimSpace(*flowsVersion) == "" || *flowsProfile == "" || *flowsProfileSHA == "") {
		log.Fatal("private profile requires versioned commission collection and exact digest")
	}
	if (*flowsCorrections != "" || *flowsCorrectionsSHA != "") && (!*flows || strings.TrimSpace(*flowsVersion) == "" || *flowsProfile == "" || *flowsCorrections == "" || *flowsCorrectionsSHA == "") {
		log.Fatal("native source corrections require exact manifests and private versioned commission collection")
	}
	if err := validateCommissionReadTimeout(*flows, *flowsVersion, *flowsReadTimeout); err != nil {
		log.Fatal(err)
	}
	if *flowsPrefetch != 0 && (!*flows || strings.TrimSpace(*flowsVersion) == "") {
		log.Fatal("commission wallet prefetch requires private versioned commission collection")
	}
	otherOp := *flows || *daysAppend || *daysFrom != "" || *failedTo > 0 || *deriveLabels != ""
	if *repeated && (otherOp || *recheck) {
		log.Fatal("--repeated-inclusions cannot be combined with another ledger operation")
	}
	if (*apply || *backup != "") && otherOp {
		log.Fatal("--apply and --backup are only supported with --recheck-holes, --repeated-inclusions or the ledger scan back")
	}
	if *recheck && otherOp {
		log.Fatal("--recheck-holes cannot be combined with another ledger operation")
	}
	if *apply && *backup == "" {
		log.Fatal("--apply requires --backup on durable storage")
	}

	// The commission trace runs thousands of small per-wallet queries whose
	// plans cross jit_above_cost; JIT compiling each took about 1 s for 40 ms
	// of work. The repeated-inclusion repair reads the same way, one account
	// and one block at a time. Session-only, so other jobs and the API keep
	// their settings.
	runtime := commissionRuntimeParams(*flows, *flowsVersion)
	if *repeated {
		runtime = append(runtime, "jit=off")
	}
	if *deriveLabels != "" {
		runtime = append(runtime, "default_transaction_read_only=on", fmt.Sprintf("statement_timeout=%d", deriveTimeout.Milliseconds()), "plan_cache_mode=force_custom_plan")
	}
	if *daysRebuild != "" && !*daysRebuildWrite {
		runtime = append(runtime, "default_transaction_read_only=on")
	}
	db, err := indexerDB.PostgresDbConnect(
		env("DB_HOST", "localhost"),
		env("DB_PORT", "5432"),
		env("DB_NAME", "indexer"),
		env("DB_USER", "postgres"),
		env("DB_PASS", "postgres"),
		env("DB_LOG_LEVEL", "warn"),
		runtime...,
	)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	if *deriveLabels != "" {
		deriveFollowLabels(db, *chain, *flowsFrom, *flowsAsOf, *deriveLabels)
		return
	}
	if *flows {
		var profile *taxapi.CommissionPrivateProfile
		if *flowsProfile != "" || *flowsProfileSHA != "" {
			profile, err = taxapi.LoadCommissionPrivateProfile(*flowsProfile, *flowsProfileSHA)
			if err != nil {
				log.Fatal(err)
			}
		}
		var corrections *taxapi.CommissionSourceCorrections
		if *flowsCorrections != "" {
			corrections, err = taxapi.LoadCommissionSourceCorrections(*flowsCorrections, *flowsCorrectionsSHA)
			if err != nil {
				log.Fatal(err)
			}
		}
		var labels *taxapi.CommissionLabelFile
		if *flowsLabels != "" || *flowsLabelsSHA != "" {
			if *flowsProfile == "" {
				log.Fatal("a label file requires private versioned commission collection")
			}
			labels, err = taxapi.LoadCommissionLabelFile(*flowsLabels, *flowsLabelsSHA, *chain)
			if err != nil {
				log.Fatal(err)
			}
		}
		commissionFlows(db, *chain, *flowsFrom, *flowsOnly, *flowsWorkers, *flowsVersion, *flowsBuild, *flowsAsOf, *flowsArchive, *flowsReadTimeout, profile, corrections, labels, *flowsMaxHops, *flowsPrefetch)
		return
	}
	if *daysAppend {
		appendDays(db, *chain, *daysWorkers)
		return
	}
	if *daysRebuild != "" {
		rebuildDays(db, *chain, *daysRebuild, *daysRebuildSHA, *daysRebuildOut, *daysWorkers, *daysRebuildWrite)
		return
	}
	if *daysFrom != "" {
		buildDays(db, *chain, *daysFrom, *daysTo, *daysBatch, *daysWorkers)
		return
	}
	node, err := taxapi.NewArchiveLedgerNode(*cache, *archive)
	if err != nil {
		log.Fatal(err)
	}
	if *archiveRPC != "" {
		if err := node.UseArchiveRPC(*archiveRPC); err != nil {
			log.Fatal(err)
		}
	}
	srv := taxapi.NewServer(db, nil, taxapi.DefaultNativeAsset)
	if *failedTo > 0 {
		n, err := srv.RecordFailedTxFees(*chain, node, *floor, *failedTo, *chunk, *workers, *failedWrite, log.Printf)
		if err != nil {
			log.Fatalf("failed-tx fees: %v", err)
		}
		log.Printf("failed-tx fees: %d charged, %d charged nothing, %d left unsettled (write=%v)", n.Charged, n.Uncharged, n.Unsettled, *failedWrite)
		return
	}
	if *repeated {
		hs, err := heightList(*repeatedHeights)
		if err != nil {
			log.Fatal(err)
		}
		cs, err := heightList(*compareHeights)
		if err != nil {
			log.Fatal(err)
		}
		sum, err := srv.RepairRepeatedInclusions(*chain, node, taxapi.RepeatedInclusionsOptions{Apply: *apply, BackupPath: *backup, Heights: hs, Compare: cs, CompareAccounts: *compareAccounts, Workers: *workers}, log.Printf)
		if out, jerr := json.Marshal(sum); jerr == nil {
			log.Printf("repeated inclusions summary: %s", out)
		}
		if err != nil {
			log.Fatalf("repeated inclusions: %v", err)
		}
		if !*apply {
			log.Printf("repeated inclusions: dry run, nothing written")
		}
		return
	}
	if *recheck {
		if _, _, err := srv.RecheckIndexHoles(*chain, node, taxapi.RecheckIndexHolesOptions{Apply: *apply, BackupPath: *backup}, log.Printf); err != nil {
			log.Fatalf("index holes: %v", err)
		}
		return
	}
	from, sum, err := srv.ExtendLedgerScanBack(*chain, node, *floor, *chunk, *workers, taxapi.LedgerBackOptions{Apply: *apply, BackupPath: *backup}, log.Printf)
	if out, jerr := json.Marshal(sum); jerr == nil {
		log.Printf("ledger scan back summary: %s", out)
	}
	switch {
	case errors.Is(err, taxapi.ErrLedgerScanUnindexed):
		log.Printf("ledger scan back: stopped at the span start %d: %v", from, err)
	case err != nil:
		log.Fatalf("ledger scan back: stopped at the span start %d: %v", from, err)
	default:
		log.Printf("ledger scan back: span starts at the floor %d", from)
	}
	if !*apply {
		log.Printf("ledger scan back: dry run, nothing written; validator rates are recorded only with --apply")
		return
	}
	if *rates {
		if err := srv.BackfillValidatorRates(*chain, node, log.Printf); err != nil {
			log.Fatalf("validator rates: %v", err)
		}
		log.Printf("validator rates: recorded for every scanned day")
	}
}

func buildDays(db *gorm.DB, chain, fromText, toText string, batch, workers int) {
	from, err := time.Parse(time.DateOnly, fromText)
	if err != nil {
		log.Fatalf("-days-from: %v", err)
	}
	to := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	if toText != "" {
		if to, err = time.Parse(time.DateOnly, toText); err != nil {
			log.Fatalf("-days-to: %v", err)
		}
	}
	if err := taxapi.BalanceHistoryMigrate(db); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	node, err := taxapi.NewLedgerNode()
	if err != nil {
		log.Fatal(err)
	}
	srv := taxapi.NewServer(db, nil, taxapi.DefaultNativeAsset)
	if err := srv.BuildBalanceDays(chain, "cosmos", node, from, to, batch, workers, log.Printf); err != nil {
		log.Fatalf("balance days: %v", err)
	}
}

func rebuildDays(db *gorm.DB, chain, idsPath, wantSHA, outDir string, workers int, write bool) {
	raw, err := os.ReadFile(idsPath)
	if err != nil {
		log.Fatal(err)
	}
	if sum := fmt.Sprintf("%x", sha256.Sum256(raw)); wantSHA == "" || sum != wantSHA {
		log.Fatalf("-days-rebuild digest %s does not match -days-rebuild-sha256", sum)
	}
	var ids []int64
	for _, line := range strings.Fields(string(raw)) {
		id, err := strconv.ParseInt(line, 10, 64)
		if err != nil || id <= 0 {
			log.Fatalf("-days-rebuild: bad address id %q", line)
		}
		ids = append(ids, id)
	}
	if outDir == "" {
		log.Fatal("-days-rebuild requires -days-rebuild-out")
	}
	if err := os.Mkdir(outDir, 0o700); err != nil {
		log.Fatal(err)
	}
	create := func(name string) *os.File {
		f, err := os.OpenFile(filepath.Join(outDir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			log.Fatal(err)
		}
		return f
	}
	before, after, rollback := create("before.jsonl"), create("after.jsonl"), create("rollback.sql")
	node, err := taxapi.NewLedgerNode()
	if err != nil {
		log.Fatal(err)
	}
	srv := taxapi.NewServer(db, nil, taxapi.DefaultNativeAsset)
	r, err := srv.RebuildBalanceDaysFor(chain, "cosmos", node, ids, workers, write, before, after, rollback, log.Printf)
	for _, f := range []*os.File{before, after, rollback} {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	if err != nil {
		log.Fatalf("balance rebuild: %v", err)
	}
	log.Printf("balance rebuild: %d addresses, %s to %s, %d rows before, %d after, written=%v", r.Addresses, r.From.Format(time.DateOnly), r.To.Format(time.DateOnly), r.Before, r.After, r.Written)
}

func appendDays(db *gorm.DB, chain string, workers int) {
	if err := taxapi.BalanceHistoryMigrate(db); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	node, err := taxapi.NewLedgerNode()
	if err != nil {
		log.Fatal(err)
	}
	srv := taxapi.NewServer(db, nil, taxapi.DefaultNativeAsset)
	through := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	if err := srv.AppendBalanceDays(chain, "cosmos", node, through, workers, log.Printf); err != nil {
		log.Fatalf("balance days: %v", err)
	}
}

func commissionFlows(db *gorm.DB, chain, fromText, only string, workers int, version, build, asOfText, archiveConfig string, readTimeout time.Duration, profile *taxapi.CommissionPrivateProfile, corrections *taxapi.CommissionSourceCorrections, labels *taxapi.CommissionLabelFile, maxHops, prefetch int) {
	if strings.TrimSpace(version) == "" || strings.TrimSpace(build) == "" || asOfText == "" {
		log.Fatal("commission flows require --commission-output-version, --commission-build and --commission-as-of; replacement of prior output is disabled")
	}
	asOf, err := time.Parse(time.DateOnly, asOfText)
	if err != nil {
		log.Fatalf("-commission-as-of: %v", err)
	}
	from, err := time.Parse(time.DateOnly, fromText)
	if err != nil {
		log.Fatalf("-commission-flows-from: %v", err)
	}
	node, err := taxapi.NewLedgerNode()
	if err != nil {
		log.Fatal(err)
	}
	oracle := taxapi.NewOracle(env("ORACLE_URL", "http://wasm-indexer:8080"), os.Getenv("NODE_REST_API"))
	srv := taxapi.NewServer(db, oracle, taxapi.DefaultNativeAsset)
	var validators []string
	for _, v := range strings.Split(only, ",") {
		if v = strings.TrimSpace(v); v != "" {
			validators = append(validators, v)
		}
	}
	archiveRPC := ""
	if archiveConfig != "" {
		config := viper.New()
		config.SetConfigFile(archiveConfig)
		if err := config.ReadInConfig(); err != nil {
			log.Fatal("cannot read commission archive configuration")
		}
		archiveRPC = config.GetString("probe.rpc")
		if archiveRPC == "" {
			log.Fatal("commission archive configuration has no RPC endpoint")
		}
	}
	if err := srv.BuildCommissionFlows(chain, node, from, validators, workers, log.Printf, taxapi.CommissionFlowBuildOptions{Version: version, Build: build, AsOf: asOf, ArchiveRPC: archiveRPC, ReadSnapshotTimeout: readTimeout, WalletPrefetch: prefetch, PrivateProfile: profile, SourceCorrections: corrections, Labels: labels, MaxHops: maxHops}); err != nil {
		log.Fatalf("commission flows: %v", err)
	}
}

// deriveFollowLabels writes the deposit-follow label file and its digest.
func deriveFollowLabels(db *gorm.DB, chain, fromText, asOfText, out string) {
	from, err := time.Parse(time.DateOnly, fromText)
	if err != nil {
		log.Fatalf("-commission-flows-from: %v", err)
	}
	asOf, err := time.Parse(time.DateOnly, asOfText)
	if err != nil {
		log.Fatalf("-commission-as-of: %v", err)
	}
	lf, err := taxapi.NewServer(db, nil, taxapi.DefaultNativeAsset).DeriveDepositFollowLabels(chain, from, asOf)
	if err != nil {
		log.Fatalf("derive labels: %v", err)
	}
	digest, err := taxapi.WriteCommissionLabelFile(out, lf)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("label file %s: %d labels, %d unfollowed, %d profiles, sha256 %s", out, len(lf.Labels), len(lf.Unfollowed), len(lf.Profiles), digest)
}

func heightList(v string) ([]int64, error) {
	var out []int64
	for _, f := range strings.Split(v, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		h, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("height %q: %w", f, err)
		}
		out = append(out, h)
	}
	return out, nil
}
