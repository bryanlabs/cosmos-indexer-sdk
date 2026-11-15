//go:build ops818stops

package taxapi

import (
 "context"
 "crypto/sha256"
 "database/sql"
 "encoding/hex"
 "encoding/json"
 "fmt"
 "os"
 "os/exec"
 "bytes"
 "path/filepath"
 "sync"
 "syscall"
 "testing"
 "time"
 "gorm.io/driver/postgres"
 "gorm.io/gorm"
 "gorm.io/gorm/logger"
)
const ssSource="6754a0d7d7986e48e3e3467b2448aae84d2c1b92"
const ssRoot="/Users/danb/.cache/ops818-handoff/current-lot-continuation-6754-20261114T1358"
var ssDir=os.Getenv("OPS818_STOP_ROOT")
type ssLogger struct {logger.Interface; Queries []string}
func(l *ssLogger) Trace(ctx context.Context,begin time.Time,fc func()(string,int64),err error){q,_:=fc();l.Queries=append(l.Queries,q)}
func ssHash(b []byte)string{h:=sha256.Sum256(b);return hex.EncodeToString(h[:])}
func ssRead(p string,v any)error{b,e:=os.ReadFile(p);if e!=nil{return e};return json.Unmarshal(b,v)}
func ssWrite(p string,v any)error{if filepath.Base(p)!="source-evidence.private.json"{if _,e:=os.Stat(p);e==nil{return fmt.Errorf("immutable output already exists")}};b,e:=json.Marshal(v);if e!=nil{return e};f,e:=os.OpenFile(p+".tmp",os.O_CREATE|os.O_EXCL|os.O_WRONLY,0600);if e!=nil{return e};if _,e=f.Write(b);e==nil{e=f.Sync()};ce:=f.Close();if e!=nil{return e};if ce!=nil{return ce};if e=os.Rename(p+".tmp",p);e!=nil{return e};d,e:=os.Open(filepath.Dir(p));if e!=nil{return e};defer d.Close();return d.Sync()}
type ssBinding struct{Source string `json:"source"`;RawPath string `json:"raw_path"`;RawSHA string `json:"raw_sha256"`}
func ssCapture(db *gorm.DB,addr string,request json.RawMessage,index int) (retErr error){
 rec:=&ssLogger{Interface:logger.Default.LogMode(logger.Silent)}
 defer func(){if retErr!=nil{_ = ssWrite(filepath.Join(ssDir,fmt.Sprintf("failed-queries-%06d.private.json",index)),map[string]any{"queries":rec.Queries,"error":retErr.Error(),"request":request})}}()
 tx:=db.Session(&gorm.Session{Logger:rec}).Begin(&sql.TxOptions{Isolation:sql.LevelRepeatableRead,ReadOnly:true});if tx.Error!=nil{return tx.Error};defer tx.Rollback()
 if e:=tx.Exec("SET LOCAL statement_timeout='180s'").Error;e!=nil{return e}
 if e:=tx.Exec("SET LOCAL lock_timeout='2s'").Error;e!=nil{return e}
 if e:=tx.Exec("SET LOCAL max_parallel_workers_per_gather=0").Error;e!=nil{return e};if e:=tx.Exec("SET LOCAL synchronize_seqscans=off").Error;e!=nil{return e}
 var controls struct{Readonly string `json:"readonly"`;AssignedXID *int64 `json:"assigned_xid"`;Snapshot string `json:"snapshot"`;Database string `json:"database"`}
 readControls:=func()error{return tx.Raw("SELECT current_setting('transaction_read_only') AS readonly, txid_current_if_assigned() AS assigned_x_id, txid_current_snapshot()::text AS snapshot, current_database() AS database").Scan(&controls).Error}
 if e:=readControls();e!=nil{return e};if controls.Readonly!="on"||controls.AssignedXID!=nil{return fmt.Errorf("read controls rejected")}
 s:=&Server{db:tx,readOnlyLedger:true};validators,e:=s.commissionValidators(apiMainnet);if e!=nil{return e}
 var labels []AddressLabel;if e=tx.Where("chain = ?",apiMainnet).Find(&labels).Error;e!=nil{return e}
 l:=&dbFlowLoader{s:s,chain:apiMainnet,private:true,labels:map[string]AddressLabel{},mu:&sync.Mutex{},chans:map[string]string{}}
 for _,x:=range labels{l.labels[x.Address]=x};derived:=derivedFlowLabels(apiMainnet,validators);for _,x:=range derived{if _,ok:=l.labels[x.Address];!ok{l.labels[x.Address]=x}}
 beforeLabel,hasLabel:=l.labels[addr]
 stop,e:=l.stop(addr);if e!=nil{return e}
 var memoRows []struct{FromAddr string;Memo string};var rowCount *int64
 if (!hasLabel || beforeLabel.Kind==labelValidatorOperator) && len(addr)<=len("cosmos1")+38+1 {
 if e=tx.Raw(`SELECT te.from_addr, t.memo FROM taxable_events te JOIN messages m ON m.id = te.message_id JOIN txes t ON t.id = m.tx_id WHERE te.to_addr = ? AND te.category IN ('transfer', 'ibc_in') AND t.memo <> '' LIMIT ?`,addr,flowMemoSample).Scan(&memoRows).Error;e!=nil{return e}
 senders:=map[string]bool{};for _,r:=range memoRows{if r.FromAddr!=""&&depositMemo(r.Memo){senders[r.FromAddr]=true}}
 if len(senders)>=flowMemoMinSenders {if stop.Class!=flowExchangeLike{return fmt.Errorf("memo raw proof disagrees")}} else {n,e:=s.walletRows(addr,ledgerHeavyRows);if e!=nil{return e};rowCount=&n;if n>ledgerHeavyRows {if stop.Class!=flowBusyWallet{return fmt.Errorf("count proof disagrees")}} else if stop.Class!=""{return fmt.Errorf("empty classifier proof disagrees")}}
 }
 if e=readControls();e!=nil{return e};if controls.Readonly!="on"||controls.AssignedXID!=nil{return fmt.Errorf("post read controls rejected")}
 proof:=map[string]any{"source":ssSource,"controls":controls,"request":request,"address":addr,"stop":stop,"memo_rows":memoRows,"wallet_rows":rowCount,"stored_labels":labels,"all_commission_validators":validators,"derived_labels":derived,"effective_label":beforeLabel,"has_effective_label":hasLabel,"queries":rec.Queries,"classifier":"commissionflows.go:(*dbFlowLoader).stop","private":true,"readOnlyLedger":true,"financially_accepted":false}
 for _,p:=range []string{"commissionflows.go","balancedays.go","zz_ops818_stop_supplier_test.go"}{b,e:=os.ReadFile(filepath.Join("/Users/danb/code/worktrees/cosmos-tax-commission-route-8704/taxapi",p));if e!=nil{return e};proof[p+"_sha256"]=ssHash(b)}
 proofPath:=filepath.Join(ssDir,fmt.Sprintf("stop-%06d.proof.private.json",index));if e=ssWrite(proofPath,proof);e!=nil{return e};pb,e:=os.ReadFile(proofPath);if e!=nil{return e}
 var binding ssBinding;bindingPath:=filepath.Join(ssRoot,"source-evidence.private.json");bindingBytes,e:=os.ReadFile(bindingPath);if e!=nil{return e};if e=json.Unmarshal(bindingBytes,&binding);e!=nil{return e};var bindingMap map[string]any;if e=json.Unmarshal(bindingBytes,&bindingMap);e!=nil{return e};priorBytes,e:=os.ReadFile(binding.RawPath);if e!=nil{return e};if binding.Source!=ssSource||ssHash(priorBytes)!=binding.RawSHA{return fmt.Errorf("prior binding rejected")}
 var cumulative map[string]any;if e=json.Unmarshal(priorBytes,&cumulative);e!=nil{return e};if cumulative["source"]!=ssSource||cumulative["readonly"]!="on"||cumulative["assigned_xid"]!=nil{return fmt.Errorf("prior controls rejected")}
 channels,ok:=cumulative["channels"].([]any);if !ok||len(channels)<44{return fmt.Errorf("retained channels rejected")}
 entries,_:=cumulative["stops"].([]any);for _,entry:=range entries{x:=entry.(map[string]any);if x["address"]==addr{return fmt.Errorf("already supplied request")}}
 cumulative["stops"]=append(entries,map[string]any{"address":addr,"stop":stop,"proof_path":proofPath,"proof_sha256":ssHash(pb)})
 priors,_:=cumulative["prior_bindings"].([]any);cumulative["prior_bindings"]=append(priors,map[string]any{"sha256":ssHash(bindingBytes),"binding":bindingMap})
 cumulative["readonly"]=controls.Readonly;cumulative["assigned_xid"]=controls.AssignedXID
 refs,_:=cumulative["external_proofs"].([]any);cumulative["external_proofs"]=append(refs,map[string]any{"path":binding.RawPath,"sha256":binding.RawSHA},map[string]any{"path":proofPath,"sha256":ssHash(pb)})
 rawPath:=filepath.Join(ssDir,fmt.Sprintf("cumulative-%06d.private.json",index));if e=ssWrite(rawPath,cumulative);e!=nil{return e};raw,e:=os.ReadFile(rawPath);if e!=nil{return e}
 currentBytes,e:=os.ReadFile(bindingPath);if e!=nil{return e};if !bytes.Equal(currentBytes,bindingBytes){return fmt.Errorf("reply changed concurrently")}
 newBinding:=map[string]any{};for k,v:=range bindingMap{newBinding[k]=v};newBinding["source"]=ssSource;newBinding["raw_path"]=rawPath;newBinding["raw_sha256"]=ssHash(raw)
 if e=ssWrite(bindingPath,newBinding);e!=nil{return e}
 fmt.Printf("phase=stop-supplied count=%d proof_sha256=%s\n",index,ssHash(pb));return nil
}
func TestOPS818StopSupplier(t *testing.T){
 if ssDir==""{t.Skip("private run only")};syscall.Umask(0077);if e:=os.MkdirAll(ssDir,0700);e!=nil{t.Fatal("private directory failure")}
 head,e:=exec.Command("git","rev-parse","HEAD").Output();if e!=nil||string(bytes.TrimSpace(head))!=ssSource{t.Fatal("source HEAD mismatch")}
 repoBytes,e:=exec.Command("git","rev-parse","--show-toplevel").Output();if e!=nil{t.Fatal("source root unavailable")};repo:=string(bytes.TrimSpace(repoBytes));cmd:=exec.Command("git","diff","--quiet",ssSource,"--","taxapi",":(exclude)taxapi/zz_*");cmd.Dir=repo;if cmd.Run()!=nil{t.Fatal("production source changed")}
 for _,p:=range []string{"commissionflows.go","balancedays.go","server.go","commissionflows_engine.go","readiness.go","api_constants.go"}{want,e:=exec.Command("git","show",ssSource+":taxapi/"+p).Output();if e!=nil{t.Fatal("source reference unavailable")};got,e:=os.ReadFile(filepath.Join(repo,"taxapi",p));if e!=nil||!bytes.Equal(want,got){t.Fatal("executed classifier source mismatch")}}
 db,e:=gorm.Open(postgres.Open("host=127.0.0.1 port=15439 user=postgres dbname=cosmostax sslmode=disable application_name=ops818-private-stop-supplier default_transaction_read_only=on"),&gorm.Config{Logger:logger.Default.LogMode(logger.Silent)});if e!=nil{_ = ssWrite(filepath.Join(ssDir,"connection-failure.private.json"),map[string]any{"error":e.Error()});t.Fatal("readonly connection failed")};pool,e:=db.DB();if e!=nil{t.Fatal("pool failed")};defer pool.Close();pool.SetMaxOpenConns(1)
 seen:=map[string]bool{};count:=0
 var startupBinding ssBinding;if e=ssRead(filepath.Join(ssRoot,"source-evidence.private.json"),&startupBinding);e!=nil{t.Fatal("startup binding unavailable")};startupBytes,e:=os.ReadFile(startupBinding.RawPath);if e!=nil||ssHash(startupBytes)!=startupBinding.RawSHA||startupBinding.Source!=ssSource{t.Fatal("startup binding invalid")};var startupRaw struct{Stops []struct{Address string `json:"address"`} `json:"stops"`};if json.Unmarshal(startupBytes,&startupRaw)!=nil{t.Fatal("startup raw malformed")};for _,x:=range startupRaw.Stops{seen[ssHash([]byte(ssSource+"/"+x.Address))]=true};count=len(startupRaw.Stops)
 for{var stat syscall.Statfs_t;if e=syscall.Statfs(ssDir,&stat);e!=nil||uint64(stat.Bavail)*uint64(stat.Bsize)<4*1024*1024*1024{t.Fatal("reserve stop")}
 b,e:=os.ReadFile(filepath.Join(ssRoot,"extra-bank-request.private.jsonl"));if e!=nil{t.Fatal("request journal unavailable")}
 var requests []json.RawMessage;for _,line:=range splitSS(b){var r struct{Kind string `json:"kind"`;Source string `json:"source"`;Request struct{Address string `json:"address"`} `json:"request"`};if json.Unmarshal(line,&r)!=nil{t.Fatal("request malformed")};if r.Kind!="source-bound-stop"{continue};if r.Source!=ssSource||r.Request.Address==""{t.Fatal("request guard rejected")};key:=ssHash([]byte(r.Source+"/"+r.Request.Address));if seen[key]{continue};requests=append(requests,line);count++;if e=ssCapture(db,r.Request.Address,line,count);e!=nil{_ = ssWrite(filepath.Join(ssDir,"failure.private.json"),map[string]any{"error":e.Error(),"request":line,"count":count});t.Fatal("source capture failed explicitly, see private failure")};seen[key]=true};_=requests;time.Sleep(time.Second)
 }
}
func splitSS(b []byte)[][]byte{var out [][]byte;start:=0;for i,c:=range b{if c=='\n'{if i>start{out=append(out,b[start:i])};start=i+1}};return out}
