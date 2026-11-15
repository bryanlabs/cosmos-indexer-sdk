//go:build ops818astrabatchv1 && ops818stopshared

package taxapi

import (
 "encoding/json"
 "os"
 "testing"
)
func TestOPS818AstraActualCompositeAdmission(t *testing.T){
 path:=os.Getenv("OPS818_ASTRA_COMPOSITE_WINDOW");if path==""{t.Skip("explicit existing composite only")};b,e:=os.ReadFile(path);var w sqpWindow;if e!=nil||json.Unmarshal(b,&w)!=nil{t.Fatal("actual composite fixture")};check:=func(p,h string)bool{got,e:=sqpDigest(p);return e==nil&&got==h};if e=avNative(w,check,avRetainedProof{});e!=nil{t.Fatal(e)};w.Address="invalid-fixture-actor";if avNative(w,check,avRetainedProof{})==nil{t.Fatal("wrong composite actor accepted")}
}
