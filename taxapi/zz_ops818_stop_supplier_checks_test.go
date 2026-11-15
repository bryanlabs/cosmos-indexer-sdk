//go:build ops818stops

package taxapi

import (
 "os"
 "path/filepath"
 "testing"
)
func TestOPS818StopPrivateProofImmutability(t *testing.T){p:=filepath.Join(t.TempDir(),"proof.private.json");if e:=ssWrite(p,map[string]int{"n":1});e!=nil{t.Fatal(e)};before,e:=os.ReadFile(p);if e!=nil{t.Fatal(e)};if ssWrite(p,map[string]int{"n":2})==nil{t.Fatal("immutable proof overwritten")};after,_:=os.ReadFile(p);if ssHash(before)!=ssHash(after){t.Fatal("proof changed")}}
func TestOPS818StopPrivateReplyAtomicUpdate(t *testing.T){p:=filepath.Join(t.TempDir(),"source-evidence.private.json");if e:=ssWrite(p,map[string]int{"n":1});e!=nil{t.Fatal(e)};if e:=ssWrite(p,map[string]int{"n":2});e!=nil{t.Fatal(e)};var v map[string]int;if e:=ssRead(p,&v);e!=nil||v["n"]!=2{t.Fatal("reply not updated")}}
func TestOPS818StopPrivateJournalClosedRecords(t *testing.T){rows:=splitSS([]byte("one\ntwo\npartial"));if len(rows)!=2||string(rows[1])!="two"{t.Fatal("unclosed request read")}}
func TestOPS818StopPrivateHashGuard(t *testing.T){if ssHash([]byte("a"))==ssHash([]byte("b")){t.Fatal("hash mutation not detected")}}
