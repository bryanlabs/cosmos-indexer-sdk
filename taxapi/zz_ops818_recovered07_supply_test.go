//go:build ops818harness && ops818cohorts0607 && ops818recovered07

package taxapi

import (
 "os"
 "testing"
 "time"
)

// Offline conversion only. Publication is separate and exclusively additive.
func TestRecovered07Convert(t *testing.T) {
 path:=os.Getenv("H_RECOVERED07_CONFIG");if path=="" {t.Skip("offline configuration required")}
 var c struct {Window xcWindow `json:"window"`; Verified string `json:"verified"`; Events string `json:"events"`; Binding string `json:"binding"`}
 if e:=xcRead(path,&c);e!=nil {t.Fatal("config rejected")}
 var verified struct {Verified bool `json:"verified_against_actual_bank_attributes"`; Credits struct {Address string `json:"address"`; Since time.Time `json:"since"`;End time.Time `json:"asof_exclusive"`;InputSHA string `json:"current_input_sha256"`;Records []struct {Message uint `json:"message"`;Coin string `json:"coin"`} `json:"credits"`} `json:"credits"`}
 if e:=xcRead(c.Verified,&verified);e!=nil {t.Fatal("verified source rejected")}
 w:=c.Window
 if !verified.Verified||verified.Credits.Address!=w.Address||!verified.Credits.Since.Equal(w.Since)||!verified.Credits.End.Equal(w.End)||verified.Credits.InputSHA!=w.InputSHA {t.Fatal("credit identity rejected")}
 h,e:=xcHash(w.Input);if e!=nil||h!=w.InputSHA {t.Fatal("input hash rejected")}
 h,e=xcHash(w.Receipt);if e!=nil||h!=w.ReceiptSHA {t.Fatal("receipt hash rejected")}
 var input reportInput;if xcRead(w.Input,&input)!=nil||len(input.Wallets)!=1 {t.Fatal("input rejected")}
 credit:=map[flowCredit]bool{};for _,r:=range verified.Credits.Records {credit[flowCredit{r.Message,r.Coin}]=true}
 events,e:=xcConvert(input,w.Address,w.Since,w.End,credit);if e!=nil {t.Fatal("conversion rejected")}
 // xcConvert is the exact unchanged original cohort converter, including stable
 // ties, duplicate occurrences, native message scope and original fee rows.
 if xcSave(c.Events,events)!=nil {t.Fatal("exclusive events save failed")}
 eh,e:=xcHash(c.Events);if e!=nil {t.Fatal("events hash failed")}
 var provenance map[string]any;if xcRead(path,&provenance)!=nil {t.Fatal("provenance rejected")}
 exe,e:=os.Executable();if e!=nil {t.Fatal("executable identity unavailable")};exeh,e:=xcHash(exe);if e!=nil {t.Fatal("executable hash unavailable")}
 binding:=map[string]any{"executed_converter_binary_sha256":exeh,"address":w.Address,"since":w.Since,"asof_exclusive":w.End,"source":xcSource,"source_input":w.Input,"source_input_sha256":w.InputSHA,"source_receipt":w.Receipt,"source_receipt_sha256":w.ReceiptSHA,"verified_credits":c.Verified,"converted_native_flow_events":len(events),"converted_events_sha256":eh,"offline_provenance":provenance,"financially_accepted":false,"closing_reconciled":false}
 if xcSave(c.Binding,binding)!=nil {t.Fatal("exclusive binding save failed")}
 t.Logf("phase converted count %d",len(events))
}
