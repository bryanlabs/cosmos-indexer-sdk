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
			log.Printf("NATIVE_DECIMALS=%q is not an integer, keeping default %d", dec, native.Decimals)
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

	oracle := taxapi.NewOracle(env("ORACLE_URL", "http://wasm-indexer:8080"), os.Getenv("NODE_REST_API"))
	srv := taxapi.NewServer(db, oracle, nativeAssetFromEnv())

	listen := env("LISTEN", ":8082")
	log.Printf("tax-api listening on %s", listen)
	if err := http.ListenAndServe(listen, srv.Handler()); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
