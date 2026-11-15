//go:build ops818stopshared || ops818stopcacheprobe

package taxapi

import (
 "database/sql/driver"
 "fmt"
 "io"
 "testing"
 "time"
)
func TestOPS818StopCacheScalarCloseDrainsEOF(t *testing.T){q:=&scpQuery{Started:time.Now()};scpObserveNext(q,[]driver.Value{int64(5)},nil);calls:=0;e:=scpDrainClose(q,func(v []driver.Value)error{calls++;return io.EOF},1);if e!=nil||!q.EOF||!q.CloseBeforeEOF||len(q.Rows)!=1||q.DrainedRows!=0||calls!=1{t.Fatal("one-row scalar EOF not preserved")}}
func TestOPS818StopCacheCloseKeepsRawRemainder(t *testing.T){q:=&scpQuery{Started:time.Now()};calls:=0;e:=scpDrainClose(q,func(v []driver.Value)error{calls++;if calls==1{v[0]=[]byte("retained");return nil};return io.EOF},1);if e!=nil||!q.EOF||q.DrainedRows!=1||len(q.Rows)!=1{t.Fatal("raw remainder lost")}}
func TestOPS818StopCacheCloseReadErrorFails(t *testing.T){q:=&scpQuery{Started:time.Now()};e:=scpDrainClose(q,func(v []driver.Value)error{return fmt.Errorf("actual driver timeout")},1);if e==nil||q.EOF||q.Error==""{t.Fatal("partial error accepted")}}
func TestOPS818StopCacheRawBusyCount(t *testing.T){l:=&dbFlowLoader{labels:map[string]AddressLabel{}};memo:=&scpQuery{SQL:"SELECT te.from_addr, t.memo",Columns:[]string{"from_addr","memo"},EOF:true,Closed:true};count:=&scpQuery{SQL:"SELECT message_event_attributes block_event_attributes",Rows:[][]driver.Value{{int64(500001)}},EOF:true,Closed:true};if scpVerifyStopRows("cosmos1abcdefghijklmnopqrstuvwxy",l,flowStop{Class:flowBusyWallet},[]*scpQuery{memo,count})!=nil{t.Fatal("actual heavy count rejected")};if scpVerifyStopRows("cosmos1abcdefghijklmnopqrstuvwxy",l,flowStop{},[]*scpQuery{memo,count})==nil{t.Fatal("fake empty stop accepted")}}
