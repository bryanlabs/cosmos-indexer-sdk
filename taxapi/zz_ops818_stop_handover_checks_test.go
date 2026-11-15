//go:build ops818stopshared

package taxapi

import (
 "database/sql/driver"
 "os"
 "path/filepath"
 "testing"
 "time"
 "github.com/jackc/pgx/v5"
)
func schFixture()([]*scpQuery,[]*scpWire){args:=[]driver.NamedValue{{Ordinal:1,Value:"fixture-address"},{Ordinal:2,Value:3000}};q:=&scpQuery{SQL:"SELECT te.from_addr, t.memo",Args:args,Columns:[]string{"from_addr","memo"},Rows:[][]driver.Value{{"sender","memo123"}},EOF:true,Closed:true};w:=&scpWire{SQL:q.SQL,Args:[]any{pgx.QueryResultFormatsByOID{},"fixture-address",3000},End:time.Now(),Tag:"SELECT 1"};return []*scpQuery{q},[]*scpWire{w}}
func TestOPS818HandoverWireIdentity(t *testing.T){q,w:=schFixture();if schArgs(q,w,"fixture-address")!=nil{t.Fatal("actual matching args rejected")};w[0].SQL="different";if schArgs(q,w,"fixture-address")==nil{t.Fatal("wrong wire SQL accepted")}}
func TestOPS818HandoverArgumentGuard(t *testing.T){q,w:=schFixture();if schArgs(q,w,"wrong-address")==nil{t.Fatal("wrong wallet accepted")};q[0].Args[1].Value=4000;w[0].Args[2]=4000;if schArgs(q,w,"fixture-address")==nil{t.Fatal("wrong sample limit accepted")}}
func TestOPS818HandoverMemoConsumedOnly(t *testing.T){q,w:=schFixture();q[0].DrainedRows=1;if schArgs(q,w,"fixture-address")==nil{t.Fatal("unconsumed memo rows accepted")}}
func TestOPS818HandoverWireCountGuard(t *testing.T){q,w:=schFixture();w[0].Tag="SELECT 2";if schArgs(q,w,"fixture-address")==nil{t.Fatal("wire count mismatch accepted")}}
func TestOPS818HandoverOldPIDGuard(t *testing.T){if schGuardOld([]int{os.Getpid()})==nil{t.Fatal("dual writer guard allowed live pid")}}
func TestOPS818HandoverDurableImmutable(t *testing.T){p:=filepath.Join(t.TempDir(),"proof.private.json");if schWrite(p,map[string]int{"n":1})!=nil{t.Fatal("durable proof failed")};if schWrite(p,map[string]int{"n":2})==nil{t.Fatal("proof overwritten")}}
