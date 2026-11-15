//go:build ops818harness

package taxapi

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

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
	if err!=nil {
		r.Captured=true
		b,_:=json.MarshalIndent(struct{Wallet,Stage string;ElapsedMS int64;Queries []privateTimeoutQuery}{r.Wallet,r.Stage,time.Since(r.Started).Milliseconds(),r.Queries},"","  ")
		_ = os.WriteFile(os.Getenv("H_TIMEOUT_OUTPUT"),b,0600)
	}
}
type privateTimeoutLoader struct {*dbFlowLoader; recorder *privateTimeoutRecorder}
func(l *privateTimeoutLoader) events(addr string,start,end time.Time)([]flowEvent,error){
	l.recorder.Wallet=addr;l.recorder.Stage="events/readReportInput";l.recorder.Started=time.Now();l.recorder.Queries=nil
	return l.dbFlowLoader.events(addr,start,end)
}
func(l *privateTimeoutLoader) stop(addr string)(flowStop,error){
	l.recorder.Wallet=addr;l.recorder.Stage="destination/stop/memo";l.recorder.Started=time.Now();l.recorder.Queries=nil
	return l.dbFlowLoader.stop(addr)
}
func TestOPS818ReplayActualWallet(t *testing.T){
	s:=connect(t,"jit=off","statement_timeout=90s","lock_timeout=2s","max_parallel_workers_per_gather=2")
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
	s:=connect(t,"jit=off","statement_timeout=90s","lock_timeout=2s","max_parallel_workers_per_gather=2")
	s.readOnlyLedger=true
	r:=&privateTimeoutRecorder{Interface:logger.Default.LogMode(logger.Silent)}
	s.db=s.db.Session(&gorm.Session{Logger:r})
	var labels []AddressLabel
	if err:=s.db.Where("chain = ?",apiMainnet).Find(&labels).Error;err!=nil{t.Fatal(err)}
	node,err:=NewLedgerNode();if err!=nil{t.Fatal(err)}
	base:=&dbFlowLoader{s:s,chain:apiMainnet,node:node,private:true,mu:&sync.Mutex{},labels:map[string]AddressLabel{},chans:map[string]string{}}
	for _,l:=range labels{base.labels[l.Address]=l}
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
