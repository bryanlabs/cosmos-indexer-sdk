//go:build ops818harness

package taxapi

import (
 "encoding/json"
 "fmt"
 "os"
 "reflect"
 "testing"
 "time"
 indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
)

func TestOPS818Full324FailedScope(t *testing.T){
 var scope struct{Address,Since,End string};b,err:=os.ReadFile("/Users/danb/.cache/ops818-handoff/full324-new-replay-failed-wallet.private.json");if err!=nil{t.Fatal(err)};if json.Unmarshal(b,&scope)!=nil{t.Fatal("scope")};start,_:=time.Parse(time.DateOnly,scope.Since);end,_:=time.Parse(time.DateOnly,scope.End)
 budget:=60*time.Second;if v:=os.Getenv("H_BUDGET");v!=""{budget,err=time.ParseDuration(v);if err!=nil{t.Fatal("budget")}}
 db,err:=indexerDB.PostgresDbConnect("127.0.0.1","15438","cosmostax","postgres",os.Getenv("H_PASS"),"silent","default_transaction_read_only=on","application_name=ops818-full324-failed-scope-readonly","jit=off","plan_cache_mode=force_custom_plan","statement_timeout=0","lock_timeout=2s","effective_io_concurrency=200");if err!=nil{t.Fatal("connection")};db=recordNextReadDB(db,os.Getenv("H_FEE_OUTPUT")+".queries.private.json");s:=&Server{db:db,readOnlyLedger:true,privateReadSnapshotTimeout:budget}
 began:=time.Now();result:=map[string]any{"BudgetSeconds":budget.Seconds()};var input reportInput
 err=s.withReadSnapshot(func(reader *Server)error{
  pairs,e:=reader.reportReadiness(apiMainnet,[]string{scope.Address},start,end);if e!=nil{return e};rows,e:=reader.loadWalletRows(apiMainnet,scope.Address,start,end,pairs);if e!=nil{return e};input.Wallets=[]walletRows{rows};result["ReadMilliseconds"]=time.Since(began).Milliseconds();result["Events"]=len(rows.Events);result["Fees"]=len(rows.Fees);if e=privateJSON(os.Getenv("H_FEE_OUTPUT")+".input.private.json",input);e!=nil{return e}
  var legacy []feeRec;e=reader.db.Table("fees").Select("fees.amount::text AS amount,denoms.base AS denom,blocks.time_stamp AS ts,txes.hash AS hash").Joins("JOIN addresses ON addresses.id=fees.payer_address_id").Joins("JOIN denoms ON denoms.id=fees.denomination_id").Joins("JOIN txes ON txes.id=fees.tx_id").Joins("JOIN blocks ON blocks.id=txes.block_id").Joins("JOIN chains ON chains.id=blocks.chain_id").Where("chains.chain_id IN ?",chainIDs(apiMainnet)).Where("addresses.address=? AND blocks.time_stamp>=? AND blocks.time_stamp<?",scope.Address,start,end).Order("blocks.time_stamp asc,fees.id asc").Scan(&legacy).Error;if e!=nil{return e};equal:=reflect.DeepEqual(rows.Fees,legacy);if len(legacy)==0&&len(rows.Fees)==0{equal=true};result["LegacyFeeReflectIdenticalSameSnapshot"]=equal;if !equal{return fmt.Errorf("legacy fee parity failed")};return nil
 });result["Milliseconds"]=time.Since(began).Milliseconds();result["Error"]=fmt.Sprint(err);_=privateJSON(os.Getenv("H_FEE_OUTPUT"),result);if err!=nil{t.Fatal("failed-scope private diagnostic failed; evidence retained")};_=privateJSON(os.Getenv("H_FEE_OUTPUT")+".input.private.json",input);t.Log("all source input retained and every fee matches independent legacy query in same read-only snapshot")
}
