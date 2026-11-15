//go:build ops818harness

package taxapi

import (
 "encoding/json"
 "fmt"
 "os"
 "path/filepath"
 "testing"
 "time"

 indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
 "github.com/shopspring/decimal"
)

// Independent archive receipts for retained openings whose original in-memory
// receipt collection was lost. Existing input and holding files stay immutable.
func TestOPS818MissingOpeningArchive(t *testing.T) {
 var scope struct {
  Wallets []struct { Address string; Day time.Time; Holdings flowHoldings }
 }
 b, err := os.ReadFile(os.Getenv("H_MISSING_OPENING_SCOPE"))
 if err != nil || json.Unmarshal(b, &scope) != nil || len(scope.Wallets) == 0 {
  t.Fatal("exact private opening scope required")
 }
 out := os.Getenv("H_MISSING_OPENING_DIR")
 if os.Mkdir(out, 0700) != nil { t.Fatal("unique opening output required") }
 db, err := indexerDB.PostgresDbConnect("127.0.0.1", "15438", "cosmostax", "postgres", os.Getenv("H_PASS"), "silent", "default_transaction_read_only=on", "application_name=ops818-missing-opening-archive-readonly", "statement_timeout=30s", "lock_timeout=2s")
 if err != nil { t.Fatal("source read connection failed") }
 s := NewServer(db, NewOracle(os.Getenv("ORACLE_URL"), os.Getenv("NODE_REST_API")), DefaultNativeAsset)
 s.readOnlyLedger = true
 archive, err := newFlowArchive(os.Getenv("H_ARCHIVE_RPC"))
 if err != nil { t.Fatal("archive config failed") }
 failed := 0
 for i, w := range scope.Wallets {
  actual, readErr := archive.holdingsAt(s, apiMainnet, w.Address, w.Day)
  matched := readErr == nil && actual.Liquid.Equal(w.Holdings.Liquid)
  if matched {
   for _, pair := range [][2]map[string]decimal.Decimal{{actual.Staked,w.Holdings.Staked},{actual.Unbonding,w.Holdings.Unbonding}} {
    for key, value := range pair[0] { if !value.Equal(pair[1][key]) { matched=false } }
    for key, value := range pair[1] { if !value.Equal(pair[0][key]) { matched=false } }
   }
  }
  record := map[string]any{"address":w.Address,"day":w.Day,"expected":w.Holdings,"actual":actual,"matched":matched,"source_failed":readErr!=nil,"source_error":fmt.Sprint(readErr),"archive":archive.evidence(map[string]bool{w.Address:true})}
  if err := privateJSON(filepath.Join(out,walletFile(w.Address)+".opening-archive.private.json"),record); err!=nil { t.Fatal("exclusive receipt save failed") }
  if !matched { failed++ }
  if err := privateAppend(filepath.Join(out,"progress.private.jsonl"),map[string]any{"ordinal":i+1,"matched":matched,"source_failed":readErr!=nil,"at":time.Now().UTC()}); err!=nil {t.Fatal("progress save failed")}
 }
 t.Log("complete exact opening scope",len(scope.Wallets),"not_matched",failed)
 if failed>0 {t.Fatal("some exact openings remain unresolved")}
}
