//go:build ops818harness

package taxapi

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
 "gorm.io/gorm"
 "gorm.io/gorm/callbacks"
	"gorm.io/gorm/logger"
)

func connect(t *testing.T, runtime ...string) *Server {
 db,err:=indexerDB.PostgresDbConnect("127.0.0.1","15438","cosmostax","postgres",os.Getenv("H_PASS"),"silent",append([]string{"default_transaction_read_only=on","application_name=ops818-new-batched-actual-failure-probe"},runtime...)...)
 if err!=nil {t.Fatal("read-only source connection failed")}
 return NewServer(db,NewOracle(os.Getenv("ORACLE_URL"),os.Getenv("NODE_REST_API")),DefaultNativeAsset)
}
type privateTimeoutQuery struct {
	SQL string
	Milliseconds int64
	Rows int64
	Error string
}
type privateTimeoutRecorder struct {
	logger.Interface
	Wallet string
	Stage string
	Started time.Time
	Queries []privateTimeoutQuery
	Captured bool
}
func (r *privateTimeoutRecorder) Trace(_ context.Context, began time.Time, query func()(string,int64), err error) {
	sql,rows:=query()
	q:=privateTimeoutQuery{SQL:sql,Milliseconds:time.Since(began).Milliseconds(),Rows:rows}
	if err!=nil {q.Error=err.Error()}
	r.Queries=append(r.Queries,q)
 if file,e:=os.OpenFile(os.Getenv("H_TIMEOUT_OUTPUT")+".history.private.jsonl",os.O_CREATE|os.O_APPEND|os.O_WRONLY,0600);e==nil {
  data,_:=json.Marshal(struct{Wallet,Stage string;At time.Time;Query privateTimeoutQuery}{r.Wallet,r.Stage,time.Now(),q});_,_=file.Write(append(data,'\n'));_=file.Close()
 }
	if err!=nil {
		r.Captured=true
		b,_:=json.MarshalIndent(struct{Wallet,Stage string;ElapsedMS int64;Queries []privateTimeoutQuery}{r.Wallet,r.Stage,time.Since(r.Started).Milliseconds(),r.Queries},"","  ")
		_ = os.WriteFile(os.Getenv("H_TIMEOUT_OUTPUT"),b,0600)
	}
}
func attachPrivateTimeoutRecorder(s *Server,r *privateTimeoutRecorder) {
 s.db=s.db.Session(&gorm.Session{Logger:r})
 capture:=func(tx *gorm.DB) {
  callbacks.BuildQuerySQL(tx)
  sql:=tx.Dialector.Explain(tx.Statement.SQL.String(),tx.Statement.Vars...)
  data,_:=json.Marshal(struct{Wallet,Stage string;At time.Time;SQL string}{r.Wallet,r.Stage,time.Now(),sql})
  _=os.WriteFile(os.Getenv("H_TIMEOUT_OUTPUT")+".pending.private.json",data,0600)
 }
 _=s.db.Callback().Query().Before("gorm:query").Register("ops818:private-before-query",capture)
 _=s.db.Callback().Row().Before("gorm:row").Register("ops818:private-before-row",capture)
}

type privateTimeoutLoader struct {*dbFlowLoader; recorder *privateTimeoutRecorder}
func(l *privateTimeoutLoader) events(addr string,start,end time.Time)([]flowEvent,error){
	l.recorder.Wallet=addr;l.recorder.Stage="events/readReportInput";l.recorder.Started=time.Now();l.recorder.Queries=nil
 progress,_:=json.Marshal(struct{Stage string;At time.Time}{l.recorder.Stage,time.Now()});_ = os.WriteFile(os.Getenv("H_TIMEOUT_OUTPUT")+".progress",progress,0600)
	return l.dbFlowLoader.events(addr,start,end)
}
func(l *privateTimeoutLoader) opening(addr string,day time.Time)(flowHoldings,bool,error){
 l.recorder.Wallet=addr;l.recorder.Stage="opening/holdingsAt";l.recorder.Started=time.Now();l.recorder.Queries=nil
 return l.dbFlowLoader.opening(addr,day)
}
func(l *privateTimeoutLoader) stop(addr string)(flowStop,error){
	l.recorder.Wallet=addr;l.recorder.Stage="destination/stop/memo";l.recorder.Started=time.Now();l.recorder.Queries=nil
	return l.dbFlowLoader.stop(addr)
}
func TestOPS818ReplayActualWallet(t *testing.T){
	s:=connect(t,"jit=off","statement_timeout=0","lock_timeout=2s","max_parallel_workers_per_gather=2")
	s.readOnlyLedger=true
	var input struct{Address,Since,End string}
	b,err:=os.ReadFile(os.Getenv("H_REPLAY_INPUT"));if err!=nil{t.Fatal(err)}
	if err=json.Unmarshal(b,&input);err!=nil{t.Fatal(err)}
	start,err:=time.Parse(time.DateOnly,input.Since);if err!=nil{t.Fatal(err)}
	end,err:=time.Parse(time.DateOnly,input.End);if err!=nil{t.Fatal(err)}
	r:=&privateTimeoutRecorder{Interface:logger.Default.LogMode(logger.Silent),Wallet:input.Address,Stage:"c660/full-flowEvents",Started:time.Now()}
	s.db=s.db.Session(&gorm.Session{Logger:r})
	rows,readErr:=s.flowEvents(apiMainnet,input.Address,start,end)
	result:=struct{ElapsedMS int64;Events int;Error string;Queries []privateTimeoutQuery}{ElapsedMS:time.Since(r.Started).Milliseconds(),Events:len(rows),Queries:r.Queries}
	if readErr!=nil{result.Error=readErr.Error()}
	b,err=json.MarshalIndent(result,"","  ");if err!=nil{t.Fatal(err)}
	if err=os.WriteFile(os.Getenv("H_REPLAY_OUTPUT"),b,0600);err!=nil{t.Fatal(err)}
	t.Log("same actual wallet full native input path ms",result.ElapsedMS,"native events",result.Events,"error present",readErr!=nil)
}
func TestOPS818ActualTimeoutWallet(t *testing.T){
	s:=connect(t,"jit=off","statement_timeout=0","lock_timeout=2s","max_parallel_workers_per_gather=2")
	s.readOnlyLedger=true
	r:=&privateTimeoutRecorder{Interface:logger.Default.LogMode(logger.Silent)}
	attachPrivateTimeoutRecorder(s,r)
	var labels []AddressLabel
	if err:=s.db.Where("chain = ?",apiMainnet).Find(&labels).Error;err!=nil{t.Fatal(err)}
	node,err:=NewLedgerNode();if err!=nil{t.Fatal(err)}
	base:=&dbFlowLoader{s:s,chain:apiMainnet,node:node,private:true,mu:&sync.Mutex{},labels:map[string]AddressLabel{},chans:map[string]string{}}
	archive,err:=newFlowArchive(os.Getenv("H_ARCHIVE_RPC"));if err!=nil{t.Fatal("archive configuration failed")};base.archive=archive
 for _,l:=range labels{base.labels[l.Address]=l}
 var previous struct{Labels []AddressLabel;Channels map[string]string}
 prior,err:=os.ReadFile(os.Getenv("H_PREVIOUS_EVIDENCE"));if err!=nil{t.Fatal(err)}
 if err=json.Unmarshal(prior,&previous);err!=nil{t.Fatal(err)}
 for _,label:=range previous.Labels{base.labels[label.Address]=label}
 for key,chain:=range previous.Channels{base.chans[key]=chain}

	validators,err:=s.commissionValidators(apiMainnet);if err!=nil{t.Fatal(err)}
	for _,label:=range derivedFlowLabels(apiMainnet,validators){if _,exists:=base.labels[label.Address];!exists{base.labels[label.Address]=label}}
	v:=os.Getenv("H_VALS")
	receivers,err:=s.commissionReceivers(apiMainnet,v);if err!=nil{t.Fatal(err)}
	var group []string;seen:=map[string]bool{}
	for _,r:=range receivers{if r.Address!=""&&!seen[r.Address]{group=append(group,r.Address);seen[r.Address]=true}}
	if op:=operatorAccount(v);op!=""&&!seen[op]{group=append(group,op)}
	l:=&privateTimeoutLoader{base,r}
	tr:=newFlowTrace(v,time.Date(2024,3,21,0,0,0,0,time.UTC),time.Date(2026,11,13,0,0,0,0,time.UTC),l);tr.strict=true
	err=tr.run(group)
	if !r.Captured{t.Fatal("actual timeout not captured; trace stopped before target or completed",err)}
	t.Log("actual private wallet, SQL parameters and whole-wallet stage timings preserved")
}
