//go:build ops818harness

package taxapi

import (
 "encoding/json"
 "crypto/sha256"
 "fmt"
 "os"
 "sync"
 "testing"
 "time"
 indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
)

func TestOPS818ExactHoldingProvenance(t *testing.T) {
 var scope struct{Address,Day string}
 b,err:=os.ReadFile(os.Getenv("H_EXACT_INPUT"));if err!=nil {t.Fatal(err)}
 if err=json.Unmarshal(b,&scope);err!=nil {t.Fatal(err)}
 day,err:=time.Parse(time.DateOnly,scope.Day);if err!=nil {t.Fatal("invalid private day")}
 db,err:=indexerDB.PostgresDbConnect("127.0.0.1","15438","cosmostax","postgres",os.Getenv("H_PASS"),"silent","default_transaction_read_only=on","application_name=ops818-exact-provenance-read","jit=off","statement_timeout=30s","lock_timeout=2s")
 if err!=nil {t.Fatal("source connection failed")}
 s:=NewServer(db,NewOracle(os.Getenv("ORACLE_URL"),os.Getenv("NODE_REST_API")),DefaultNativeAsset);s.readOnlyLedger=true
 archive,err:=newFlowArchive(os.Getenv("H_ARCHIVE_RPC"));if err!=nil {t.Fatal("archive configuration failed")}
 h,err:=archive.holdingsAt(s,apiMainnet,scope.Address,day);if err!=nil {t.Fatal(err)}
 source:=archive.holdingSource(scope.Address,day)
 if source.Kind!="archive_exact" || source.Height<=0 || len(source.ReceiptHashes)==0 {t.Fatal("exact provenance missing")}
 receipts:=archive.evidence(map[string]bool{scope.Address:true})
 hashes:=map[string]bool{}
 for _,r:=range receipts {if fmt.Sprintf("%x",sha256.Sum256([]byte(r.Response)))!=r.SHA256 {t.Fatal("receipt hash mismatch")};hashes[r.SHA256]=true}
 for _,hash:=range source.ReceiptHashes {if !hashes[hash] {t.Fatal("provenance link missing")}}
 loader:=&dbFlowLoader{s:s,chain:apiMainnet,private:true,archive:archive,mu:&sync.Mutex{}}
 cached,known,err:=loader.holdingsAt(scope.Address,day);if err!=nil || !known || !cached.total().Equal(h.total()) {t.Fatal("cached exact holding mismatch")}
 if cached.Source.Kind!="archive_exact" {t.Fatal("cached provenance not propagated")}
 result:=struct{Address string;Day string;Source CommissionFlowHoldingSource;Receipts []FlowArchiveReceipt;Holdings flowHoldings}{scope.Address,scope.Day,source,receipts,h}
 b,err=json.Marshal(result);if err!=nil {t.Fatal(err)}
 if err=os.WriteFile(os.Getenv("H_EXACT_OUTPUT"),b,0600);err!=nil {t.Fatal(err)}
 t.Log("exact archive and cached private source verified", "receipts",len(receipts),"source_hash_links",len(source.ReceiptHashes))
}

func TestOPS818ExplicitRuntimeRead(t *testing.T) {
 db,err:=indexerDB.PostgresDbConnect("127.0.0.1","15438","cosmostax","postgres",os.Getenv("H_PASS"),"silent","default_transaction_read_only=on","application_name=ops818-explicit-prefetch-test","statement_timeout=5s","lock_timeout=2s","effective_io_concurrency=200")
 if err!=nil {t.Fatal("source connection failed")}
 pool,err:=db.DB();if err!=nil {t.Fatal(err)}
 first,err:=pool.Conn(db.Statement.Context);if err!=nil {t.Fatal(err)};defer first.Close()
 second,err:=pool.Conn(db.Statement.Context);if err!=nil {t.Fatal(err)};defer second.Close()
 var a,b string
 if err=first.QueryRowContext(db.Statement.Context,"SHOW effective_io_concurrency").Scan(&a);err!=nil {t.Fatal(err)}
 if err=second.QueryRowContext(db.Statement.Context,"SHOW effective_io_concurrency").Scan(&b);err!=nil {t.Fatal(err)}
 if a!="200"||b!="200" {t.Fatal("connection startup prefetch not applied across pool")}
 t.Log("explicit native-driver private startup parameter verified on two pooled connections")
}

func TestOPS818PGOptionsRead(t *testing.T) {
 db,err:=indexerDB.PostgresDbConnect("127.0.0.1","15438","cosmostax","postgres",os.Getenv("H_PASS"),"silent","default_transaction_read_only=on","application_name=ops818-pgoptions-test","statement_timeout=5s","lock_timeout=2s")
 if err!=nil {t.Fatal("source connection failed")}
 var value string;if err=db.Raw("SHOW effective_io_concurrency").Scan(&value).Error;err!=nil {t.Fatal(err)}
 if value!="200" {t.Fatal("PGOPTIONS did not set private session prefetch")}
 t.Log("PGOPTIONS private session prefetch verified",value)
}

func TestOPS818ActualFeeWalletRead(t *testing.T) {
 var scope struct{Address,Since,End string}
 b,err:=os.ReadFile(os.Getenv("H_FEE_INPUT"));if err!=nil {t.Fatal(err)}
 if err=json.Unmarshal(b,&scope);err!=nil {t.Fatal(err)}
 start,err:=time.Parse(time.DateOnly,scope.Since);if err!=nil {t.Fatal(err)}
 end,err:=time.Parse(time.DateOnly,scope.End);if err!=nil {t.Fatal(err)}
 db,err:=indexerDB.PostgresDbConnect("127.0.0.1","15438","cosmostax","postgres",os.Getenv("H_PASS"),"silent","default_transaction_read_only=on","application_name=ops818-actual-fee-wallet-read","jit=off","plan_cache_mode=force_custom_plan","statement_timeout=0","lock_timeout=2s","effective_io_concurrency=200")
 if err!=nil {t.Fatal("source connection failed")}
 db=recordNextReadDB(db,os.Getenv("H_FEE_OUTPUT")+".queries.private.json")
 s:=NewServer(db,NewOracle(os.Getenv("ORACLE_URL"),os.Getenv("NODE_REST_API")),DefaultNativeAsset);s.readOnlyLedger=true
 began:=time.Now();input,err:=s.readReportInput(apiMainnet,[]string{scope.Address},start,end,false);elapsed:=time.Since(began)
 result:=struct{Milliseconds int64;Fees,Events int;Error string}{Milliseconds:elapsed.Milliseconds()}
 if err!=nil {result.Error=err.Error()} else {result.Fees=len(input.Wallets[0].Fees);result.Events=len(input.Wallets[0].Events)}
 b,_=json.Marshal(result);if writeErr:=os.WriteFile(os.Getenv("H_FEE_OUTPUT"),b,0600);writeErr!=nil {t.Fatal(writeErr)}
 if err!=nil {t.Fatal("actual wallet read failed; details retained privately")}
 t.Log("actual wallet complete 30s snapshot read", "ms",result.Milliseconds,"fees",result.Fees,"events",result.Events)
}

func TestOPS818ActualFeeBounds(t *testing.T) {
 var scope struct{Address,Since,End string}
 b,err:=os.ReadFile(os.Getenv("H_FEE_INPUT"));if err!=nil {t.Fatal(err)}
 if err=json.Unmarshal(b,&scope);err!=nil {t.Fatal(err)}
 start,err:=time.Parse(time.DateOnly,scope.Since);if err!=nil {t.Fatal(err)}
 end,err:=time.Parse(time.DateOnly,scope.End);if err!=nil {t.Fatal(err)}
 db,err:=indexerDB.PostgresDbConnect("127.0.0.1","15438","cosmostax","postgres",os.Getenv("H_PASS"),"silent","default_transaction_read_only=on","application_name=ops818-actual-fee-bounds","jit=off","statement_timeout=10s","lock_timeout=2s")
 if err!=nil {t.Fatal("source connection failed")}
 s:=NewServer(db,NewOracle(os.Getenv("ORACLE_URL"),os.Getenv("NODE_REST_API")),DefaultNativeAsset);s.readOnlyLedger=true
 ids,err:=s.chainRowIDs(apiMainnet);if err!=nil {t.Fatal(err)}
 lo,err:=s.dayEndBlock(ids,start.AddDate(0,0,-1));if err!=nil {t.Fatal(err)}
 hi,err:=s.dayEndBlock(ids,end.AddDate(0,0,-1));if err!=nil {t.Fatal(err)}
 var payer struct{ID uint}
 if err=s.db.Table("addresses").Select("id").Where("address = ?",scope.Address).Take(&payer).Error;err!=nil {t.Fatal(err)}
 result:=struct{Payer uint;Low,High int64;ChainIDs []int64}{payer.ID,lo.Height,hi.Height,ids}
 b,err=json.Marshal(result);if err!=nil {t.Fatal(err)}
 if err=os.WriteFile(os.Getenv("H_FEE_OUTPUT"),b,0600);err!=nil {t.Fatal(err)}
 t.Log("actual fee payer and exact indexed boundaries captured", "chains",len(ids),"height_span",hi.Height-lo.Height)
}

func TestOPS818GATAActualWalletReadAfterAlias(t *testing.T) {
 var scope struct{Address,Since string}
 b,err:=os.ReadFile(os.Getenv("H_SEMANTIC_INPUT"));if err!=nil {t.Fatal(err)}
 if err=json.Unmarshal(b,&scope);err!=nil {t.Fatal(err)}
 from,err:=time.Parse(time.DateOnly,scope.Since);if err!=nil {t.Fatal("invalid private scope start")}
 db,err:=indexerDB.PostgresDbConnect("127.0.0.1","15438","cosmostax","postgres",os.Getenv("H_PASS"),"silent","default_transaction_read_only=on","application_name=ops818-gata-alias-wallet-read","jit=off","statement_timeout=30s","lock_timeout=2s")
 if err!=nil {t.Fatal("source connection failed")}
 s:=NewServer(db,NewOracle(os.Getenv("ORACLE_URL"),os.Getenv("NODE_REST_API")),DefaultNativeAsset);s.readOnlyLedger=true
 started:=time.Now()
 input,err:=s.readReportInput(apiMainnet,[]string{scope.Address},from,time.Date(2026,11,13,0,0,0,0,time.UTC),false)
 if err!=nil {t.Fatal(err)}
 t.Log("actual whole-wallet snapshot read succeeded", "elapsed",time.Since(started),"wallets",len(input.Wallets))
}
