// taxapid serves the tax query/export API over the database the tax indexer
// writes, using the wasm-indexer oracle for USD pricing + denom resolution.
//
// Env: DB_HOST DB_PORT DB_NAME DB_USER DB_PASS, ORACLE_URL (wasm-indexer base,
// e.g. http://wasm-indexer:8080), LISTEN (default :8082).
package main

import (
	"log"
	"net/http"
	"os"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/taxapi"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
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

	oracle := taxapi.NewOracle(env("ORACLE_URL", "http://wasm-indexer:8080"))
	srv := taxapi.NewServer(db, oracle)

	listen := env("LISTEN", ":8082")
	log.Printf("tax-api listening on %s", listen)
	if err := http.ListenAndServe(listen, srv.Handler()); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
