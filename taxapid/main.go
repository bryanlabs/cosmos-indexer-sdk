// taxapid serves the tax query/export API over the database the tax indexer
// writes, using the wasm-indexer oracle for USD pricing + denom resolution.
//
// Env: DB_HOST DB_PORT DB_NAME DB_USER DB_PASS, ORACLE_URL (wasm-indexer base,
// e.g. http://wasm-indexer:8080), NODE_REST_API (chain REST host, also used by
// /balance; fallback for denom metadata the oracle doesn't have), LISTEN
// (default :8082). NATIVE_DENOM/NATIVE_DECIMALS/NATIVE_SYMBOL configure this
// deployment's chain-native asset (default uatom/6/ATOM for cosmoshub;
// override per chain when deploying an additional chain, see INF-208).
package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/taxapi"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func nativeAssetFromEnv() taxapi.NativeAsset {
	native := taxapi.DefaultNativeAsset
	if d := os.Getenv("NATIVE_DENOM"); d != "" {
		native.Denom = d
	}
	if s := os.Getenv("NATIVE_SYMBOL"); s != "" {
		native.Symbol = s
	}
	if dec := os.Getenv("NATIVE_DECIMALS"); dec != "" {
		if n, err := strconv.Atoi(dec); err == nil {
			native.Decimals = n
		} else {
			log.Printf("NATIVE_DECIMALS is not an integer, keeping default %d", native.Decimals)
		}
	}
	return native
}

func main() {
	db, err := indexerDB.PostgresDbConnect(
		env("DB_HOST", "localhost"),
		env("DB_PORT", "5432"),
		env("DB_NAME", "indexer"),
		env("DB_USER", "postgres"),
		env("DB_PASS", "postgres"),
		env("DB_LOG_LEVEL", "warn"),
	)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}

	// Own the balance-snapshot table (the SDK now serves /balance, replacing the
	// legacy cosmos-tax-cli). Live reads come from NODE_REST_API.
	if err := db.AutoMigrate(&taxapi.BalanceSnapshot{}); err != nil {
		log.Printf("balance table migrate: %v", err)
	}
	// WASM contract indexing intake (INF-209): user submissions + admin triage.
	if err := db.AutoMigrate(&taxapi.WasmSubmission{}); err != nil {
		log.Printf("wasm submission table migrate: %v", err)
	}
	// Delegator-report widget for validators (INF-210): API keys + usage log.
	if err := db.AutoMigrate(&taxapi.ValidatorKey{}, &taxapi.ValidatorReportUsage{}); err != nil {
		log.Printf("validator key table migrate: %v", err)
	}
	// Durable async report cache: computed once, served instantly until newer
	// on-chain activity, a max age, or a resync request invalidates it.
	if err := db.AutoMigrate(&taxapi.ReportJob{}); err != nil {
		log.Fatalf("report job table migrate: %v", err)
	}
	// Release watcher runs (taxwatch CronJob) served at /chain/releases.
	if err := db.AutoMigrate(&taxapi.ChainReleaseCheck{}); err != nil {
		log.Printf("chain release table migrate: %v", err)
	}

	// Validator jurisdiction attestations: operator-signed memos found in txes.
	if err := taxapi.AttestationsMigrate(db); err != nil {
		log.Printf("attestation table migrate: %v", err)
	}

	oracle := taxapi.NewOracle(env("ORACLE_URL", "http://wasm-indexer:8080"), os.Getenv("NODE_REST_API"))
	srv := taxapi.NewServer(db, oracle, nativeAssetFromEnv())
	srv.StartAttestationScanner(2*time.Minute, log.Printf)

	listen := env("LISTEN", ":8082")
	log.Printf("tax-api listening on %s", listen)
	httpServer := &http.Server{
		Addr:              listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
