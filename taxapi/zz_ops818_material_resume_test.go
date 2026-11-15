//go:build ops818harness

package taxapi

import (
 "encoding/json"
 "os"
 "path/filepath"
 "testing"
 "time"
 "github.com/stretchr/testify/require"
)

func TestOPS818RetainedMaterialExactResume(t *testing.T) {
 old:=t.TempDir(); next:=t.TempDir(); start:=time.Date(2024,3,21,0,0,0,0,time.UTC);end:=start.AddDate(0,0,3);address:="test-wallet"
 events:=[]flowEvent{{Key:flowKey{Time:start.Add(time.Hour)}}};raw,err:=json.Marshal(events);require.NoError(t,err)
 name:=filepath.Join(old,walletFile(address));require.NoError(t,os.WriteFile(name+".events.private.json",raw,0600));require.NoError(t,os.WriteFile(name+".input.private.json",[]byte(`{"Wallets":[]}`),0600));require.NoError(t,os.WriteFile(name+".native-cash.private.json",[]byte(`{"source_error":"<nil>"}`),0600))
 loader:=&materialCaptureLoader{dir:next,resumeDir:old,resumeStarts:map[string]time.Time{address:start},resumeEnd:end,resumeOpenings:map[string]retainedMaterialOpening{address:{Address:address,Day:start.AddDate(0,0,-1),Known:true,Error:"<nil>"}}}
 got,err:=loader.events(address,start,end);require.NoError(t,err);require.Len(t,got,len(events));require.Equal(t,events[0].Key,got[0].Key);require.True(t,events[0].Amount.Equal(got[0].Amount))
 before,err:=os.Stat(name+".events.private.json");require.NoError(t,err);after,err:=os.Stat(filepath.Join(next,walletFile(address))+".events.private.json");require.NoError(t,err);require.True(t,os.SameFile(before,after))
 require.Error(t,privateJSON(filepath.Join(next,walletFile(address))+".events.private.json",[]flowEvent{}))
 unchanged,err:=os.ReadFile(name+".events.private.json");require.NoError(t,err);require.Equal(t,raw,unchanged)
 _,known,err:=loader.opening(address,start.AddDate(0,0,-1));require.NoError(t,err);require.True(t,known)
 _,_,err=loader.opening(address,start);require.Error(t,err)
 _,err=loader.events(address,start.Add(time.Second),end);require.Error(t,err)
 _,err=loader.events(address,start,end.Add(time.Second));require.Error(t,err)
}
