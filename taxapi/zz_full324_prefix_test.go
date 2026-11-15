//go:build ops818harness

package taxapi

import (
 "bufio"
 "container/heap"
 "encoding/json"
 "fmt"
 "os"
 "path/filepath"
 "sync"
 "testing"
 "time"
 indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
)

type prefixOpening struct {Address string;Day time.Time;Holdings flowHoldings;Known bool;Error string}
type prefixLoader struct {*dbFlowLoader;dir string;openings map[string]prefixOpening;pending string;at time.Time}
func (l *prefixLoader) opening(addr string,day time.Time)(flowHoldings,bool,error){
 if h,ok:=l.openings[addr];ok && h.Day.Equal(day){return h.Holdings,h.Known,nil}
 _=privateJSON(l.pending,map[string]any{"Address":addr,"Since":day.AddDate(0,0,1).Format(time.DateOnly),"End":"2026-11-13","Phase":"opening","Prefix":len(l.openings)})
 return flowHoldings{},false,fmt.Errorf("retained prefix frontier reached")
}
func(l *prefixLoader)stop(addr string)(flowStop,error){
 _=privateJSON(l.pending,map[string]any{"Address":addr,"Phase":"stop","Prefix":len(l.openings),"Since":flowDay(l.at).Format(time.DateOnly),"End":"2026-11-13"})
 return l.dbFlowLoader.stop(addr)
}
func(l *prefixLoader)events(addr string,start,end time.Time)([]flowEvent,error){
 var e []flowEvent;b,err:=os.ReadFile(filepath.Join(l.dir,walletFile(addr)+".events.private.json"));if err!=nil{return nil,err};err=json.Unmarshal(b,&e);return e,err
}
func TestOPS818Full324Prefix(t *testing.T){
 var state struct{Root string};b,e:=os.ReadFile("/Users/danb/.cache/ops818-handoff/full324-new-fee-capture-state.json");if e!=nil{t.Fatal(e)};if json.Unmarshal(b,&state)!=nil{t.Fatal("state")}
 var scope struct{Validators []string;Labels []AddressLabel;From,AsOf string};b,e=os.ReadFile(filepath.Join(state.Root,"scope.private.json"));if e!=nil{t.Fatal(e)};if json.Unmarshal(b,&scope)!=nil{t.Fatal("scope")}
 db,e:=indexerDB.PostgresDbConnect("127.0.0.1","15438","cosmostax","postgres",os.Getenv("H_PASS"),"silent","default_transaction_read_only=on","application_name=ops818-full324-prefix-readonly","statement_timeout=10s","lock_timeout=2s");if e!=nil{t.Fatal("connection")}
 s:=&Server{db:db,readOnlyLedger:true};base:=&dbFlowLoader{s:s,chain:apiMainnet,private:true,labels:map[string]AddressLabel{},chans:map[string]string{},mu:&sync.Mutex{}}
 for _,x:=range scope.Labels{base.labels[x.Address]=x}
 dir:=filepath.Join(state.Root,"validator-01");l:=&prefixLoader{dbFlowLoader:base,dir:dir,openings:map[string]prefixOpening{},pending:"/Users/danb/.cache/ops818-handoff/full324-new-prefix-frontier.private.json"}
 f,e:=os.Open(filepath.Join(dir,"openings.private.jsonl"));if e!=nil{t.Fatal(e)};defer f.Close();sc:=bufio.NewScanner(f);sc.Buffer(make([]byte,1024),8<<20);for sc.Scan(){var x prefixOpening;if json.Unmarshal(sc.Bytes(),&x)!=nil{t.Fatal("opening")};l.openings[x.Address]=x}
 var id struct{Roots []string};b,e=os.ReadFile(filepath.Join(dir,"identity.private.json"));if e!=nil{t.Fatal(e)};if json.Unmarshal(b,&id)!=nil{t.Fatal("identity")}
 from,_:=time.Parse(time.DateOnly,scope.From);asof,_:=time.Parse(time.DateOnly,scope.AsOf);tr:=newFlowTrace(scope.Validators[0],from,asof,l);tr.strict=true;for _,addr:=range id.Roots{if e=tr.discover(addr,0,from,nil);e!=nil{break}};for e==nil&&tr.queue.Len()>0{q:=heap.Pop(&tr.queue).(flowQueued);l.at=q.event.Key.Time;e=tr.apply(q.wallet,q.event,true)};if e==nil{t.Fatal("frontier not reached")};_=privateJSON(l.pending+".error",map[string]any{"Error":fmt.Sprint(e)});t.Log("retained prefix replayed",len(tr.order),"wallets; pending scope retained privately")
}
