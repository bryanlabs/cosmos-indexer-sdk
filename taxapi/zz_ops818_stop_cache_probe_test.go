//go:build ops818stopcacheprobe || ops818stopshared

package taxapi

import (
 "bytes"
 "context"
 "crypto/sha256"
 "database/sql"
 "database/sql/driver"
 "encoding/json"
 "fmt"
 "io"
 "os"
 "os/exec"
 "path/filepath"
 "reflect"
 "runtime/debug"
 "strings"
 "sync"
 "testing"
 "time"

 "github.com/jackc/pgx/v5"
 "github.com/jackc/pgx/v5/stdlib"
 "gorm.io/driver/postgres"
 "gorm.io/gorm"
 "gorm.io/gorm/logger"
)
const scpSource="6754a0d7d7986e48e3e3467b2448aae84d2c1b92"
const scpLiveRoot="/Users/danb/.cache/ops818-handoff/current-lot-continuation-6754-20261114T1358"
type scpQuery struct {
 Phase string `json:"phase"`
 Sequence int `json:"sequence"`
 ColumnTypes []string `json:"column_types,omitempty"`
 FirstRowAt *time.Time `json:"first_row_at,omitempty"`
 SQL string `json:"sql"`
 Args []driver.NamedValue `json:"args"`
 Columns []string `json:"columns,omitempty"`
 Rows [][]driver.Value `json:"rows,omitempty"`
 Started time.Time `json:"started_at"`
 QueryMillis float64 `json:"query_millis"`
 FullMillis float64 `json:"full_millis"`
 EOF bool `json:"eof"`
 Closed bool `json:"closed"`
 CloseBeforeEOF bool `json:"close_before_eof"`
 DrainedRows int `json:"drained_rows"`
 Error string `json:"error,omitempty"`
}
type scpWire struct {Phase string `json:"phase"`;SQL string `json:"sql"`;Args []any `json:"args"`;Start time.Time `json:"start"`;End time.Time `json:"end"`;Tag string `json:"tag"`;Error string `json:"error,omitempty"`}
type scpRecorder struct {Queries []*scpQuery;Wire []*scpWire;Phase string}
func(r *scpRecorder) start(q string,a []driver.NamedValue)*scpQuery{x:=&scpQuery{SQL:q,Args:append([]driver.NamedValue(nil),a...),Started:time.Now().UTC(),Phase:r.Phase,Sequence:len(r.Queries)+1};r.Queries=append(r.Queries,x);return x}
type scpTracer struct{r *scpRecorder}
type scpTraceKey struct{}
func(t *scpTracer)TraceQueryStart(ctx context.Context,c *pgx.Conn,d pgx.TraceQueryStartData)context.Context{w:=&scpWire{Phase:t.r.Phase,SQL:d.SQL,Args:append([]any(nil),d.Args...),Start:time.Now().UTC()};t.r.Wire=append(t.r.Wire,w);return context.WithValue(ctx,scpTraceKey{},w)}
func(t *scpTracer)TraceQueryEnd(ctx context.Context,c *pgx.Conn,d pgx.TraceQueryEndData){w,ok:=ctx.Value(scpTraceKey{}).(*scpWire);if !ok{return};w.End=time.Now().UTC();w.Tag=d.CommandTag.String();if d.Err!=nil{w.Error=d.Err.Error()}}
func scpSameOptionalInterfaces(a,b any)bool{checks:=[]reflect.Type{reflect.TypeOf((*driver.QueryerContext)(nil)).Elem(),reflect.TypeOf((*driver.ExecerContext)(nil)).Elem(),reflect.TypeOf((*driver.ConnBeginTx)(nil)).Elem(),reflect.TypeOf((*driver.ConnPrepareContext)(nil)).Elem(),reflect.TypeOf((*driver.NamedValueChecker)(nil)).Elem(),reflect.TypeOf((*driver.SessionResetter)(nil)).Elem(),reflect.TypeOf((*driver.Validator)(nil)).Elem(),reflect.TypeOf((*driver.Pinger)(nil)).Elem(),reflect.TypeOf((*driver.RowsColumnTypeDatabaseTypeName)(nil)).Elem(),reflect.TypeOf((*driver.RowsColumnTypeLength)(nil)).Elem(),reflect.TypeOf((*driver.RowsColumnTypeNullable)(nil)).Elem(),reflect.TypeOf((*driver.RowsColumnTypePrecisionScale)(nil)).Elem(),reflect.TypeOf((*driver.RowsColumnTypeScanType)(nil)).Elem(),reflect.TypeOf((*driver.RowsNextResultSet)(nil)).Elem()};for _,check:=range checks{if reflect.TypeOf(a).Implements(check)!=reflect.TypeOf(b).Implements(check){return false}};return true}
func scpErr(q *scpQuery,e error){if e!=nil&&e!=io.EOF{q.Error=e.Error()}}
func scpCopyValues(in []driver.Value)[]driver.Value{out:=make([]driver.Value,len(in));for i,v:=range in{if b,ok:=v.([]byte);ok{out[i]=append([]byte(nil),b...)}else{out[i]=v}};return out}
func scpObserveNext(q *scpQuery,dest []driver.Value,e error){if e==nil{if q.FirstRowAt==nil{now:=time.Now().UTC();q.FirstRowAt=&now};q.Rows=append(q.Rows,scpCopyValues(dest))}else{q.EOF=e==io.EOF;q.FullMillis=float64(time.Since(q.Started).Microseconds())/1000;scpErr(q,e)}}
type scpConnector struct {driver.Connector;r *scpRecorder}
func(c *scpConnector)Connect(ctx context.Context)(driver.Conn,error){conn,e:=c.Connector.Connect(ctx);if e!=nil{return nil,e};pc,ok:=conn.(*stdlib.Conn);if !ok{conn.Close();return nil,fmt.Errorf("unexpected pgx connector")};wrapped:=&scpConn{pc,c.r};if !scpSameOptionalInterfaces(pc,wrapped){pc.Close();return nil,fmt.Errorf("optional connection interface mismatch")};return wrapped,nil}
type scpConn struct {*stdlib.Conn;r *scpRecorder}
func(c *scpConn)QueryContext(ctx context.Context,q string,args []driver.NamedValue)(driver.Rows,error){if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(q)),"SELECT"){return nil,fmt.Errorf("probe rejected non-SELECT query")};rec:=c.r.start(q,args);rows,e:=c.Conn.QueryContext(ctx,q,args);rec.QueryMillis=float64(time.Since(rec.Started).Microseconds())/1000;scpErr(rec,e);if e!=nil{return nil,e};pr,ok:=rows.(*stdlib.Rows);if !ok{rows.Close();return nil,fmt.Errorf("unexpected pgx rows")};rec.Columns=append([]string(nil),pr.Columns()...);for i:=range rec.Columns{rec.ColumnTypes=append(rec.ColumnTypes,pr.ColumnTypeDatabaseTypeName(i))};wrapped:=&scpRows{pr,rec};if !scpSameOptionalInterfaces(pr,wrapped){pr.Close();return nil,fmt.Errorf("optional rows interface mismatch")};return wrapped,nil}
func(c *scpConn)ExecContext(ctx context.Context,q string,args []driver.NamedValue)(driver.Result,error){if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(q)),"SET LOCAL "){return nil,fmt.Errorf("probe rejected non-session exec")};rec:=c.r.start(q,args);result,e:=c.Conn.ExecContext(ctx,q,args);rec.QueryMillis=float64(time.Since(rec.Started).Microseconds())/1000;rec.FullMillis=rec.QueryMillis;rec.EOF=e==nil;rec.Closed=true;scpErr(rec,e);return result,e}
type scpRows struct {*stdlib.Rows;q *scpQuery}
func(r *scpRows)Next(dest []driver.Value)error{e:=r.Rows.Next(dest);scpObserveNext(r.q,dest,e);return e}
func scpDrainClose(q *scpQuery,next func([]driver.Value)error,n int)error{if q.EOF||q.Error!=""{return nil};q.CloseBeforeEOF=true;for{dest:=make([]driver.Value,n);e:=next(dest);scpObserveNext(q,dest,e);if e==nil{q.DrainedRows++;continue};if e==io.EOF{return nil};return e}}
func(r *scpRows)Close()error{drainErr:=scpDrainClose(r.q,r.Rows.Next,len(r.q.Columns));e:=r.Rows.Close();if e==nil{e=drainErr};r.q.Closed=true;if r.q.FullMillis==0{r.q.FullMillis=float64(time.Since(r.q.Started).Microseconds())/1000};scpErr(r.q,e);return e}
func scpHash(b []byte)string{return fmt.Sprintf("%x",sha256.Sum256(b))}
func scpWrite(p string,v any)error{b,e:=json.Marshal(v);if e!=nil{return e};f,e:=os.OpenFile(p,os.O_CREATE|os.O_EXCL|os.O_WRONLY,0600);if e!=nil{return e};if _,e=f.Write(b);e==nil{e=f.Sync()};ce:=f.Close();if e!=nil{return e};return ce}
type scpControls struct{Readonly string `json:"readonly"`;AssignedXID *int64 `json:"assigned_xid"`;Snapshot string `json:"snapshot"`;BackendPID int `json:"backend_pid"`;TransactionAt time.Time `json:"transaction_at"`;IdleTimeout string `json:"idle_in_transaction_session_timeout"`}
func scpReadControls(db *gorm.DB)(scpControls,error){var c scpControls;e:=db.Raw("SELECT current_setting('transaction_read_only') AS readonly, txid_current_if_assigned() AS assigned_x_id, txid_current_snapshot()::text AS snapshot, pg_backend_pid() AS backend_pid, transaction_timestamp() AS transaction_at, current_setting('idle_in_transaction_session_timeout') AS idle_timeout").Scan(&c).Error;if e==nil&&(c.Readonly!="on"||c.AssignedXID!=nil){e=fmt.Errorf("snapshot controls rejected")};return c,e}
func scpVerifyProduction()error{repo:="/Users/danb/code/worktrees/cosmos-tax-commission-route-8704";cmd:=exec.Command("git","rev-parse","HEAD");cmd.Dir=repo;b,e:=cmd.Output();if e!=nil||string(bytes.TrimSpace(b))!=scpSource{return fmt.Errorf("source HEAD rejected")};cmd=exec.Command("git","diff","--quiet",scpSource,"--","taxapi",":(exclude)taxapi/zz_*");cmd.Dir=repo;if cmd.Run()!=nil{return fmt.Errorf("production source changed")};return nil}
func scpRequests()([]string,error){b,e:=os.ReadFile(filepath.Join(scpLiveRoot,"extra-bank-request.private.jsonl"));if e!=nil{return nil,e};seen:=map[string]bool{};var addresses []string;for _,line:=range bytes.Split(b,[]byte("\n")){if len(line)==0{continue};var r struct{Kind string `json:"kind"`;Source string `json:"source"`;Request struct{Address string `json:"address"`} `json:"request"`};if json.Unmarshal(line,&r)!=nil{return nil,fmt.Errorf("request parse")};if r.Kind!="source-bound-stop"{continue};if r.Source!=scpSource||r.Request.Address==""{return nil,fmt.Errorf("request source guard")};if !seen[r.Request.Address]{seen[r.Request.Address]=true;addresses=append(addresses,r.Request.Address)}};return addresses,nil}
func TestOPS818StopCacheProbe(t *testing.T){
 root:=os.Getenv("OPS818_STOP_CACHE_PROBE_ROOT");if root==""{t.Skip("private pilot only")};if e:=os.MkdirAll(root,0700);e!=nil{t.Fatal("owned output failed")};if e:=scpVerifyProduction();e!=nil{t.Fatal(e)}
 rec:=&scpRecorder{Phase:"connection"};cfg,e:=pgx.ParseConfig("host=127.0.0.1 port=15439 user=postgres dbname=cosmostax sslmode=disable application_name=ops818-private-stop-cache-probe default_transaction_read_only=on");if e!=nil{t.Fatal("config failed")};cfg.Tracer=&scpTracer{rec};pool:=sql.OpenDB(&scpConnector{stdlib.GetConnector(*cfg),rec});defer pool.Close();pool.SetMaxOpenConns(1)
 db,e:=gorm.Open(postgres.New(postgres.Config{Conn:pool}),&gorm.Config{Logger:logger.Default.LogMode(logger.Silent)});if e!=nil{t.Fatal("private connector failed")};tx:=db.Begin(&sql.TxOptions{Isolation:sql.LevelRepeatableRead,ReadOnly:true});if tx.Error!=nil{t.Fatal("readonly begin failed")};defer tx.Rollback()
 defer func(){_ = scpWrite(filepath.Join(root,"all-actual-query-rows-and-timings.private.json"),map[string]any{"queries":rec.Queries,"wire_queries":rec.Wire,"timing_kind":"client-side send/first-row/EOF-close, not server execution time"})}()
 rec.Phase="session_controls"
 for _,q:=range []string{"SET LOCAL statement_timeout='180s'","SET LOCAL lock_timeout='2s'","SET LOCAL max_parallel_workers_per_gather=0","SET LOCAL synchronize_seqscans=off"}{if tx.Exec(q).Error!=nil{t.Fatal("private session controls failed")}}
 rec.Phase="initial_snapshot_controls";controls,e:=scpReadControls(tx);if e!=nil{t.Fatal("actual controls failed")};setupStart:=len(rec.Queries)
 s:=&Server{db:tx,readOnlyLedger:true};rec.Phase="all_validators_once";validators,e:=s.commissionValidators(apiMainnet);if e!=nil{t.Fatal("all validators source failed")};rec.Phase="stored_labels_once";var labels []AddressLabel;if tx.Where("chain = ?",apiMainnet).Find(&labels).Error!=nil{t.Fatal("stored labels failed")};derived:=derivedFlowLabels(apiMainnet,validators)
 l:=&dbFlowLoader{s:s,chain:apiMainnet,private:true,labels:map[string]AddressLabel{},mu:&sync.Mutex{},chans:map[string]string{}};for _,lab:=range labels{l.labels[lab.Address]=lab};for _,lab:=range derived{if _,ok:=l.labels[lab.Address];!ok{l.labels[lab.Address]=lab}}
 sourceBytes,e:=os.ReadFile("/Users/danb/code/worktrees/cosmos-tax-commission-route-8704/taxapi/commissionflows.go");if e!=nil{t.Fatal("classifier unavailable")};setup:=map[string]any{"source":scpSource,"controls":controls,"all_commission_validators":validators,"stored_labels":labels,"derived_labels":derived,"actual_queries_and_rows":rec.Queries[setupStart:],"commissionflows.go_sha256":scpHash(sourceBytes),"statement_cache_mode":cfg.DefaultQueryExecMode.String(),"snapshot_scope":"entire setup and every pilot classification remain inside this ONE still-open repeatable-read readonly transaction","financially_accepted":false}
 var settings []struct{Name string;Setting string};rec.Phase="settings";if tx.Raw("SELECT name, setting FROM pg_settings WHERE name IN ('max_parallel_workers_per_gather','synchronize_seqscans','plan_cache_mode','idle_in_transaction_session_timeout','transaction_read_only','default_transaction_read_only') ORDER BY name").Scan(&settings).Error!=nil{t.Fatal("settings proof failed")};setup["settings"]=settings;if info,ok:=debug.ReadBuildInfo();ok{setup["go_build_info"]=info};for _,name:=range []string{"commissionflows.go","balancedays.go","zz_ops818_stop_cache_probe_test.go"}{b,e:=os.ReadFile(filepath.Join("/Users/danb/code/worktrees/cosmos-tax-commission-route-8704/taxapi",name));if e!=nil{t.Fatal("source proof file failed")};setup[name+"_sha256"]=scpHash(b)}
 setupPath:=filepath.Join(root,"label-setup-once.private.json");if scpWrite(setupPath,setup)!=nil{t.Fatal("setup proof write failed")};setupBytes,_:=os.ReadFile(setupPath)
 addresses,e:=scpRequests();if e!=nil||len(addresses)==0{t.Fatal("no actual source-stop requests")};if len(addresses)>3{addresses=addresses[len(addresses)-3:]}
 for i,addr:=range addresses{if time.Since(controls.TransactionAt)>10*time.Minute{t.Fatal("pilot snapshot lifetime cap")};rec.Phase="before_stop_controls";before,e:=scpReadControls(tx);if e!=nil||before!=controls{t.Fatal("single snapshot guard before failed")};queryStart:=len(rec.Queries);start:=time.Now();rec.Phase="actual_production_stop";stop,e:=l.stop(addr);queryEnd:=len(rec.Queries);if e!=nil{t.Fatal("actual classifier failed")};if e=scpVerifyStopRows(addr,l,stop,rec.Queries[queryStart:queryEnd]);e!=nil{t.Fatal("actual raw classifier rows disagree")};rec.Phase="after_stop_controls";after,e:=scpReadControls(tx);if e!=nil||after!=controls{t.Fatal("single snapshot guard after failed")};for _,q:=range rec.Queries[queryStart:]{if !q.EOF||!q.Closed||q.Error!=""{t.Fatal("incomplete actual row capture")}}
 proof:=map[string]any{"source":scpSource,"address":addr,"stop":stop,"controls":after,"label_setup_path":setupPath,"label_setup_sha256":scpHash(setupBytes),"queries":rec.Queries[queryStart:queryEnd],"elapsed_millis":float64(time.Since(start).Microseconds())/1000,"commissionflows.go_sha256":scpHash(sourceBytes),"raw_rows_are_actual_classifier_inputs":true,"audit_requeries":0,"financially_accepted":false,"not_a_live_reply":true};if scpWrite(filepath.Join(root,fmt.Sprintf("pilot-%02d.proof.private.json",i+1)),proof)!=nil{t.Fatal("pilot proof write failed")};fmt.Printf("phase=private-cache-pilot count=%d query_count=%d millis=%.3f\n",i+1,len(rec.Queries)-queryStart,float64(time.Since(start).Microseconds())/1000)
 }
}
func TestOPS818StopCacheCopyDriverBytes(t *testing.T){in:=[]driver.Value{[]byte("memo123"),"sender",int64(2),nil};q:=&scpQuery{Started:time.Now()};scpObserveNext(q,in,nil);in[0].([]byte)[0]='X';if string(q.Rows[0][0].([]byte))!="memo123"{t.Fatal("driver byte buffer alias")};scpObserveNext(q,nil,io.EOF);if !q.EOF||len(q.Rows)!=1{t.Fatal("EOF did not close raw evidence")}}
func TestOPS818StopCacheCaptureErrors(t *testing.T){q:=&scpQuery{Started:time.Now()};scpObserveNext(q,nil,fmt.Errorf("fixture failure"));if q.EOF||q.Error==""||len(q.Rows)!=0{t.Fatal("failure was fabricated as empty success")}}
func TestOPS818StopCacheNoWrites(t *testing.T){c:=&scpConn{};if _,e:=c.ExecContext(context.Background(),"INSERT INTO tax_address_labels VALUES ('x')",nil);e==nil{t.Fatal("write not blocked")};if _,e:=c.QueryContext(context.Background(),"DELETE FROM taxable_events RETURNING *",nil);e==nil{t.Fatal("write query not blocked")}}
