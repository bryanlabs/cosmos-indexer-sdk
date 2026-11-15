//go:build ops818harness && ops818cohorts0607 && ops818recovered07

package taxapi

import (
 "os"
 "path/filepath"
 "testing"
)

func TestRecovered07CacheGuard(t *testing.T) {
 path:=os.Getenv("H_RECOVERED07_CONFIG");if path=="" {t.Skip("offline config required")}
 var c struct {Window xcWindow `json:"window"`;Events string `json:"events"`;Binding string `json:"binding"`}
 if xcRead(path,&c)!=nil {t.Fatal("config rejected")}
 key:=xcID(c.Window.Address,c.Window.Since,c.Window.End)
 dir,e:=os.MkdirTemp(os.Getenv("TMPDIR"),"cache-guard-");if e!=nil {t.Fatal("private fixture failed")}
 if os.Mkdir(filepath.Join(dir,"windows"),0700)!=nil {t.Fatal("fixture directory failed")}
 if os.Link(c.Events,filepath.Join(dir,"windows",key+".events.private.json"))!=nil||os.Link(c.Binding,filepath.Join(dir,"windows",key+".binding.private.json"))!=nil {t.Fatal("exclusive fixture reference failed")}
 l:=&xcLoader{manifest:xcManifest{Capture:dir},windows:map[string]xcWindow{key:c.Window},used:map[string]string{}}
 events,e:=l.cached(c.Window.Address,c.Window.Since,c.Window.End);if e!=nil||len(events)==0 {t.Fatal("actual unchanged cached guard rejected conversion")}
 bad:=c.Window;bad.InputSHA="corrupt";l.windows[key]=bad
 if _,e=l.cached(c.Window.Address,c.Window.Since,c.Window.End);e==nil {t.Fatal("cached input corruption accepted")}
 t.Logf("phase original_cached_guard_passed count %d",len(events))
}
