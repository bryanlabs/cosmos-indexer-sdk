//go:build ops818astrabatchv1 && ops818stopshared

package taxapi

import (
 "bufio"
 "compress/gzip"
 "context"
 "crypto/sha256"
 "database/sql"
 "encoding/json"
 "fmt"
 "io"
 "os"
 "os/exec"
 "path/filepath"
 "reflect"
 "syscall"
 "testing"
 "time"
 "github.com/jackc/pgx/v5"
 "github.com/jackc/pgx/v5/stdlib"
 "gorm.io/driver/postgres"
 "gorm.io/gorm"
 "gorm.io/gorm/logger"
)
const avCompositeSHA="3d6b7b67e45f86dd5cf1737e7d1a7608d0df62576f845e612ed391f9262b4922"
type avRetainedProof struct {SourceBinding string `json:"source_binding_path"`;SourceBindingSHA string `json:"source_binding_sha256"`;Kind string `json:"kind"`;BindingPath string `json:"binding_path"`;BindingSHA string `json:"binding_sha256"`;RawPath string `json:"raw_receipt_path"`;RawSHA string `json:"raw_receipt_sha256"`;VerifiedPath string `json:"verified_path"`;VerifiedSHA string `json:"verified_sha256"`;EventsPath string `json:"events_path"`;EventsSHA string `json:"events_sha256"`}
func avNative(w sqpWindow,check func(string,string)bool,retained avRetainedProof)error {
 if !check(w.Input,w.InputSHA)||!check(w.Receipt,w.ReceiptSHA){return fmt.Errorf("native hash")}
 b,e:=os.ReadFile(w.Receipt);if e!=nil{return e}
 if w.ReceiptSHA!=avCompositeSHA {
  var r struct{Source string `json:"source"`;InputSHA string `json:"current_input_sha256"`;Exact bool `json:"nonfee_input_exact"`;OldAvailable bool `json:"old_input_available"`;SourceBindingSHA string `json:"source_binding_sha256"`;ReadError string `json:"read_error"`;Window string `json:"window"`}
  if json.Unmarshal(b,&r)!=nil||r.Source!=scpSource||r.InputSHA!=w.InputSHA||(r.ReadError!=""&&r.ReadError!="<nil>"){return fmt.Errorf("native actual receipt")}
  if r.Exact{return nil}
  if r.OldAvailable||r.Window!=w.Key||scpHash([]byte(w.Address+"|"+w.Since.Format(time.RFC3339Nano)+"|"+w.End.Format(time.RFC3339Nano)))!=w.Key{return fmt.Errorf("unproven native difference")}
  if retained.Kind=="actual-current-source-bound-native-cache"{
   if retained.SourceBindingSHA!=r.SourceBindingSHA||!check(retained.SourceBinding,r.SourceBindingSHA)||!check(retained.BindingPath,retained.BindingSHA)||!check(retained.EventsPath,retained.EventsSHA){return fmt.Errorf("retained current source hashes")}
   var source struct{Source string `json:"source"`};sb,err:=os.ReadFile(retained.SourceBinding);if err!=nil||json.Unmarshal(sb,&source)!=nil||source.Source!=scpSource{return fmt.Errorf("retained current source identity")}
   var bind struct{Source string `json:"source"`;Address string `json:"address"`;Input string `json:"source_input_sha256"`;Since time.Time `json:"since"`;End time.Time `json:"asof_exclusive"`;Reused bool `json:"native_input_cache_reused"`};bb,err:=os.ReadFile(retained.BindingPath);if err!=nil||json.Unmarshal(bb,&bind)!=nil||bind.Source!=scpSource||bind.Address!=w.Address||bind.Input!=w.InputSHA||!bind.Since.Equal(w.Since)||!bind.End.Equal(w.End)||!bind.Reused{return fmt.Errorf("retained current native binding")};return nil
  }
  if retained.Kind!="actual-current-input-without-prior-comparison"{return fmt.Errorf("unproven native evidence kind")}
  for _,p:=range [][2]string{{retained.BindingPath,retained.BindingSHA},{retained.RawPath,retained.RawSHA},{retained.VerifiedPath,retained.VerifiedSHA},{retained.EventsPath,retained.EventsSHA}}{if !check(p[0],p[1]){return fmt.Errorf("retained native evidence hash")}}
  var raw struct{Readonly string `json:"readonly"`;XID *int64 `json:"assigned_xid"`;Complete bool `json:"native_complete"`;Error string `json:"read_error"`;Window string `json:"window"`;Address string `json:"address"`;Since time.Time `json:"since"`;End time.Time `json:"asof_exclusive"`;InputSHA string `json:"current_input_sha256"`};rb,err:=os.ReadFile(retained.RawPath)
  if err!=nil||json.Unmarshal(rb,&raw)!=nil||raw.Readonly!="on"||raw.XID!=nil||!raw.Complete||(raw.Error!=""&&raw.Error!="<nil>")||raw.Window!=w.Key||raw.Address!=w.Address||!raw.Since.Equal(w.Since)||!raw.End.Equal(w.End)||raw.InputSHA!=w.InputSHA{return fmt.Errorf("retained actual BANK scope")}
  var binding struct{Source string `json:"source"`;Address string `json:"address"`;Input string `json:"source_input_sha256"`;Receipt string `json:"source_receipt_sha256"`;Events string `json:"converted_events_sha256"`};bb,err:=os.ReadFile(retained.BindingPath);if err!=nil||json.Unmarshal(bb,&binding)!=nil||binding.Source!=scpSource||binding.Address!=w.Address||binding.Input!=w.InputSHA||binding.Receipt!=w.ReceiptSHA||binding.Events!=retained.EventsSHA{return fmt.Errorf("retained conversion identity")}
  return nil
 }
 var r struct {Source string `json:"source"`;Input string `json:"joined_input_sha256"`;Events string `json:"events_sha256"`;Joined bool `json:"joined_input_same_snapshot"`;Fresh bool `json:"fresh_prefix_same_snapshot"`;Dedup bool `json:"event_occurrences_deduplicated"`;Overlap bool `json:"overlap_recaptured"`;Controls struct{Readonly string `json:"readonly"`;XID *int64 `json:"assigned_xid"`} `json:"controls"`;Scope struct{Source,Address,CanonicalWindow,TailInput,TailInputSHA,TailReceipt,TailReceiptSHA,SuffixCredits,SuffixCreditsSHA,UniquenessProof,UniquenessSHA,Since,End string} `json:"scope"`}
 if json.Unmarshal(b,&r)!=nil||r.Source!=scpSource||r.Scope.Source!=scpSource||r.Input!=w.InputSHA||r.Joined||!r.Fresh||r.Dedup||r.Overlap||r.Controls.Readonly!="on"||r.Controls.XID!=nil{return fmt.Errorf("composite controls")}
 x:=r.Scope;if x.Address!=w.Address||x.Since!=w.Since.Format(time.RFC3339Nano)||x.End!=w.End.Format(time.RFC3339Nano)||x.CanonicalWindow!=w.Key||scpHash([]byte(x.Address+"|"+x.Since+"|"+x.End))!=w.Key{return fmt.Errorf("composite exact key")}
 for _,p:=range [][2]string{{x.TailInput,x.TailInputSHA},{x.TailReceipt,x.TailReceiptSHA},{x.SuffixCredits,x.SuffixCreditsSHA},{x.UniquenessProof,x.UniquenessSHA},{filepath.Join(filepath.Dir(w.Receipt),"joined-native.events.private.json"),r.Events}}{if !check(p[0],p[1]){return fmt.Errorf("composite retained source")}}
 return nil
}
func avValidate(m sqpManifest,receipts map[string]avRetainedProof)error {
 verified:=map[string]string{};check:=func(p,w string)bool{h,ok:=verified[p];if !ok{var e error;h,e=sqpDigest(p);if e!=nil{return false};verified[p]=h};return h==w}
 if m.Source!=scpSource||len(m.Candidates)==0{return fmt.Errorf("candidate source")}
 for _,p:=range [][2]string{{m.Queue,m.QueueSHA},{m.Trace,m.TraceSHA},{m.Loader,m.LoaderSHA},{m.States,m.StatesSHA},{m.Restore,m.RestoreSHA}}{if !check(p[0],p[1]){return fmt.Errorf("candidate immutable binding")}}
 wanted:=map[int]sqpOccurrence{};addresses:=map[string]bool{}
 for _,c:=range m.Candidates{if c.Address==""||addresses[c.Address]||len(c.Occurrences)==0{return fmt.Errorf("candidate identity")};addresses[c.Address]=true;for _,o:=range c.Occurrences{if _,ok:=wanted[o.Seq];ok{return fmt.Errorf("duplicate occurrence")};if o.Event.To!=c.Address||o.Event.From!=o.SourceWallet||o.Event.From==o.Event.To||o.Event.Dir!=apiOut||o.Event.Category!=apiTransfer||!o.Event.Amount.IsPositive(){return fmt.Errorf("production route")};if o.Window.Address!=o.SourceWallet||o.Event.Key.Time.Before(o.Window.Since)||!o.Event.Key.Time.Before(o.Window.End){return fmt.Errorf("native scope")};if e:=avNative(o.Window,check,receipts[o.Window.Key]);e!=nil{return e};wanted[o.Seq]=o}}
 f,e:=os.Open(m.Queue);if e!=nil{return e};defer f.Close();z,e:=gzip.NewReader(f);if e!=nil{return e};defer z.Close();h:=sha256.New();scan:=bufio.NewScanner(io.TeeReader(z,h));scan.Buffer(make([]byte,65536),64*1024*1024);found:=map[int]bool{}
 for scan.Scan(){var r struct{Seq int `json:"seq"`;Source string `json:"source_wallet"`;Wallet string `json:"wallet"`;Event flowEvent `json:"event"`};if json.Unmarshal(scan.Bytes(),&r)!=nil{return fmt.Errorf("queue parse")};o,ok:=wanted[r.Seq];if !ok{continue};actor:=r.Source;if actor==""{actor=r.Wallet}else if r.Wallet!=""{return fmt.Errorf("ambiguous actor")};if found[r.Seq]||actor!=o.SourceWallet||!reflect.DeepEqual(r.Event,o.Event)||scpHash(append(append([]byte(nil),scan.Bytes()...), '\n'))!=o.LineSHA{return fmt.Errorf("literal queue altered")};found[r.Seq]=true}
 if scan.Err()!=nil||fmt.Sprintf("%x",h.Sum(nil))!=m.RawSHA||len(found)!=len(wanted){return fmt.Errorf("lossless queue membership")};return nil
}
func avRotate(start time.Time,requests int,now time.Time)bool{return requests>=256||now.Sub(start)>=8*time.Minute}
func avUnseen(items []sqbItem,seen map[string]bool)[]sqbItem{out:=[]sqbItem{};keys:=map[string]bool{};for _,item:=range items{k:=item.Root+"/"+item.Address;if !seen[k]&&!keys[k]{keys[k]=true;out=append(out,item)}};return out}
func avReady(path string,v any)error{tmp:=path+".unpublished";if e:=schWrite(tmp,v);e!=nil{return e};if _,e:=os.Stat(path);!os.IsNotExist(e){return fmt.Errorf("ready destination exists")};if e:=os.Rename(tmp,path);e!=nil{return e};return schSync(filepath.Dir(path))}
func TestOPS818AstraBoundedLimits(t *testing.T){n:=time.Now();if avRotate(n,255,n)||!avRotate(n,256,n)||!avRotate(n,0,n.Add(8*time.Minute)){t.Fatal("rotation bounds")};if len(splitSQB([]byte("{}\npartial")))!=1{t.Fatal("partial record")}}
func TestOPS818AstraBoundedSourceV1(t *testing.T){
 dir:=os.Getenv("OPS818_ASTRA_BATCH_ROOT");if dir==""{t.Skip("private source service only")};syscall.Umask(0077)
 owner:=os.Getppid();ownerCtx,ownerCancel:=context.WithCancel(context.Background());defer ownerCancel();go func(){ticker:=time.NewTicker(250*time.Millisecond);defer ticker.Stop();for{select{case <-ownerCtx.Done():return;case <-ticker.C:if os.Getppid()!=owner{ownerCancel();return}}}}()
 if scpVerifyProduction()!=nil{t.Fatal("source identity")};cmd:=exec.Command("git","diff","--quiet",scpSource,"--");cmd.Dir="/Users/danb/code/worktrees/cosmos-tax-commission-route-8704";if cmd.Run()!=nil{t.Fatal("treewide tracked source purity")}
 var roots []string;if json.Unmarshal([]byte(os.Getenv("OPS818_ASTRA_BATCH_ROOTS")),&roots)!=nil||len(roots)!=3{t.Fatal("three explicit roots")}
 rec:=&scpRecorder{Phase:"connect"};cfg,e:=pgx.ParseConfig("host=127.0.0.1 port=15439 user=postgres dbname=cosmostax sslmode=disable application_name=ops818-private-astra-bounded-v1 default_transaction_read_only=on");if e!=nil{t.Fatal("config")};cfg.Tracer=&scpTracer{rec};pool:=sql.OpenDB(&scpConnector{stdlib.GetConnector(*cfg),rec});defer pool.Close();pool.SetMaxOpenConns(1);db,e:=gorm.Open(postgres.New(postgres.Config{Conn:pool}),&gorm.Config{Logger:logger.Default.LogMode(logger.Silent)});if e!=nil{t.Fatal("source connect")}
 var s *scsSession;var cancel context.CancelFunc;var started time.Time;snap:=0;proofCount:=0;batchCount:=0;seen:=map[string]bool{};proofs:=map[string]schProof{};loaded:=map[string]bool{};var pending []sqbItem;var entries []map[string]any
 flush:=func(){if len(entries)==0{return};batchCount++;p:=filepath.Join(dir,fmt.Sprintf("ready-%06d.private.json",batchCount));if avReady(p,map[string]any{"source":scpSource,"entries":entries,"actual_journal_requests_priority":true,"partial_ready":true,"proofs_may_span_snapshots":true,"emission_snapshot":snap,"snapshot_requests":s.requests,"live_replies_modified":false,"financially_accepted":false})!=nil{t.Fatal("partial batch durability")};entries=nil;fmt.Printf("phase=bounded-partial-ready count=%d\n",batchCount)}
 defer func(){if s!=nil{s.tx.Rollback()};if cancel!=nil{cancel()};_ = schWrite(filepath.Join(dir,"terminal-query-evidence.private.json"),map[string]any{"source":scpSource,"queries":rec.Queries,"wire":rec.Wire,"phase":rec.Phase,"snapshot_count":snap,"proof_count":proofCount,"failed":t.Failed(),"financially_accepted":false})}()
 rotate:=func(){flush();if s!=nil{s.tx.Rollback()};if cancel!=nil{cancel()};snap++;started=time.Now();ctx,c:=context.WithTimeout(ownerCtx,10*time.Minute);cancel=c;rec.Queries=nil;rec.Wire=nil;var err error;s,err=scsNewSession(db.WithContext(ctx),rec,dir,snap);if err!=nil{t.Fatal("bounded snapshot initialization")};if schSync(dir)!=nil{t.Fatal("setup durability")};fmt.Printf("phase=bounded-snapshot-ready count=%d\n",snap)}
 sourceBytes,e:=os.ReadFile("/Users/danb/code/worktrees/cosmos-tax-commission-route-8704/taxapi/commissionflows.go");if e!=nil{t.Fatal("source bind")};lastFlush:=time.Now()
 for {
  if ownerCtx.Err()!=nil{flush();t.Fatal("source supervisor exited")}
  var disk syscall.Statfs_t;if syscall.Statfs(dir,&disk)!=nil||uint64(disk.Bavail)*uint64(disk.Bsize)<4*1024*1024*1024{flush();t.Fatal("4GiB reserve")}
  var priority []sqbItem;for _,root:=range roots{x,err:=sqbNeeded(root);if err!=nil{flush();t.Fatal("actual journal")};priority=append(priority,x...)}
  files,_:=filepath.Glob(filepath.Join(dir,"admissions","*.private.json"));for _,p:=range files{if loaded[p]{continue};b,err:=os.ReadFile(p);var m sqpManifest;var meta struct{Root string `json:"root"`;Receipts map[string]avRetainedProof `json:"actual_native_receipts"`};if err!=nil||json.Unmarshal(b,&m)!=nil||json.Unmarshal(b,&meta)!=nil{t.Fatal("candidate admission")};admitted:=false;for _,r:=range roots{admitted=admitted||r==meta.Root};if !admitted||avValidate(m,meta.Receipts)!=nil{t.Fatal("independent native admission")};for _,c:=range m.Candidates{pending=append(pending,sqbItem{Root:meta.Root,Address:c.Address,Manifest:p,ManifestSHA:scpHash(b),Origin:"immutable-actual-queued-receiver"})};loaded[p]=true}
  items:=avUnseen(append(priority,pending...),seen);pending=nil
  if len(items)==0{flush();if s!=nil&&time.Since(lastFlush)>60*time.Second{s.tx.Rollback();cancel();s=nil};time.Sleep(250*time.Millisecond);continue}
  // Only a bounded slice is processed before journals/admissions are refreshed.
  if len(items)>16{pending=append(pending,items[16:]...);items=items[:16]}
  for _,item:=range items{
   key:=item.Root+"/"+item.Address;if seen[key]{continue}
   if s==nil||avRotate(started,s.requests,time.Now()){rotate()}
   proof,ok:=proofs[item.Address]
   if !ok{
    qstart:=len(rec.Queries);before,err:=scpReadControls(s.tx);if err!=nil||before!=s.c{flush();t.Fatal("source before controls")};qs,ws:=len(rec.Queries),len(rec.Wire);rec.Phase="actual_production_bounded_stop";stop,err:=s.l.stop(item.Address);queries,wire:=rec.Queries[qs:],rec.Wire[ws:];if err!=nil||scpVerifyStopRows(item.Address,s.l,stop,queries)!=nil||schArgs(queries,wire,item.Address)!=nil{flush();t.Fatal("actual classifier proof")};after,err:=scpReadControls(s.tx);if err!=nil||after!=s.c{flush();t.Fatal("source after controls")};s.requests++;proofCount++;p:=filepath.Join(dir,fmt.Sprintf("stop-%06d.proof.private.json",proofCount));record:=map[string]any{"source":scpSource,"address":item.Address,"stop":stop,"controls":after,"queries":rec.Queries[qstart:],"classifier_queries":queries,"actual_wire_queries":wire,"label_setup_path":s.setupPath,"label_setup_sha256":s.setupHash,"commissionflows.go_sha256":scpHash(sourceBytes),"origin":item,"snapshot_request_count":s.requests,"hard_snapshot_deadline":started.Add(10*time.Minute),"audit_requeries":0,"financially_accepted":false};if schWrite(p,record)!=nil{flush();t.Fatal("proof durability")};pb,err:=os.ReadFile(p);if err!=nil{t.Fatal("proof read")};proof=schProof{p,scpHash(pb),stop};proofs[item.Address]=proof
   }
   entries=append(entries,map[string]any{"root":item.Root,"address":item.Address,"stop":proof.Stop,"proof_path":proof.Path,"proof_sha256":proof.Hash,"origin":item});seen[key]=true
   if len(entries)>=16||time.Since(lastFlush)>2*time.Second{flush();lastFlush=time.Now()}
  }
  flush()
  // Retain all raw proofs on disk. Release per-query rows after durable proof.
  rec.Queries=nil;rec.Wire=nil
 }
}
func TestOPS818AstraPendingBounded(t *testing.T){items:=[]sqbItem{};seen:=map[string]bool{};for i:=0;i<64;i++{x:=sqbItem{Root:"fixture",Address:fmt.Sprint(i)};items=append(items,x,x);seen[x.Root+"/"+x.Address]=true};if len(avUnseen(items,seen))!=0{t.Fatal("seen work spun")};if len(avUnseen(items,map[string]bool{}))!=64{t.Fatal("pending duplicates")}}
func TestOPS818AstraAtomicReady(t *testing.T){p:=filepath.Join(t.TempDir(),"ready.private.json");if avReady(p,map[string]any{"complete":true})!=nil{t.Fatal("atomic ready")};b,e:=os.ReadFile(p);var obj map[string]bool;if e!=nil||json.Unmarshal(b,&obj)!=nil||!obj["complete"]{t.Fatal("partial visible")};if avReady(p,map[string]any{"changed":true})==nil{t.Fatal("immutable overwrite")}}
func TestOPS818AstraActualAdmissionOffline(t *testing.T){
 value:=os.Getenv("OPS818_ASTRA_OFFLINE_MANIFESTS");if value==""{t.Skip("explicit retained candidate manifests only")};var paths []string;if json.Unmarshal([]byte(value),&paths)!=nil{t.Fatal("manifest list")};for _,p:=range paths{b,e:=os.ReadFile(p);var m sqpManifest;var meta struct{Receipts map[string]avRetainedProof `json:"actual_native_receipts"`};if e!=nil||json.Unmarshal(b,&m)!=nil||json.Unmarshal(b,&meta)!=nil{t.Fatal("manifest parse")};if e=avValidate(m,meta.Receipts);e!=nil{t.Fatal(e)};m.QueueSHA="invalid";if avValidate(m,meta.Receipts)==nil{t.Fatal("changed queue accepted")}}
}
