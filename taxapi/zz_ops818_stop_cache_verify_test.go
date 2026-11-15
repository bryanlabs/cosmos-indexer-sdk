//go:build ops818stopcacheprobe || ops818stopshared

package taxapi

import (
 "fmt"
 "strings"
)
func scpString(v any)(string,error){switch x:=v.(type){case string:return x,nil;case []byte:return string(x),nil;default:return "",fmt.Errorf("not a string driver value")}}
func scpVerifyStopRows(addr string,l *dbFlowLoader,stop flowStop,queries []*scpQuery)error{
 for _,q:=range queries{if !q.EOF||!q.Closed||q.Error!=""{return fmt.Errorf("incomplete actual rows")}}
 lab,ok:=l.labels[addr]
 if ok&&lab.Kind!=labelValidatorOperator&&lab.Source!=flowMemoHeuristic{if len(queries)!=0{return fmt.Errorf("labeled branch queried source")};var expected flowStop;switch{case lab.Kind==labelModule&&lab.Name=="distribution":expected=flowStop{Class:flowCommunityPool};case lab.Kind==labelModule:expected=flowStop{Class:flowModule,Detail:lab.Name};case lab.Kind==labelIBCEscrow:expected=flowStop{Class:flowIBCOut,Detail:lab.Name};case lab.Kind==labelExchangeLike:expected=flowStop{Class:flowExchangeLike,Detail:lab.Source};default:expected=flowStop{Class:flowExchange,Detail:lab.Name}};if expected!=stop{return fmt.Errorf("label branch disagrees")};return nil}
 if ok&&lab.Kind==labelExchangeLike&&lab.Source==flowMemoHeuristic&&len(queries)==0{if stop!=(flowStop{Class:flowExchangeLike,Detail:flowMemoHeuristic}){return fmt.Errorf("cached heuristic differs")};return nil}
 if len(addr)>len("cosmos1")+38+1{if len(queries)!=0||stop!=(flowStop{Class:flowContract}){return fmt.Errorf("contract branch differs")};return nil}
 if len(queries)<1||!strings.Contains(queries[0].SQL,"SELECT te.from_addr, t.memo")||len(queries[0].Columns)!=2{return fmt.Errorf("memo query not recorded")}
 senders:=map[string]bool{};for _,row:=range queries[0].Rows{if len(row)!=2{return fmt.Errorf("memo row shape")};from,e:=scpString(row[0]);if e!=nil{return e};memo,e:=scpString(row[1]);if e!=nil{return e};if from!=""&&depositMemo(memo){senders[from]=true}}
 if len(senders)>=flowMemoMinSenders{if len(queries)!=1||stop!=(flowStop{Class:flowExchangeLike,Detail:flowMemoHeuristic}){return fmt.Errorf("memo class differs")};if !ok||lab.Evidence!=fmt.Sprintf("%d distinct senders with deposit-style memos in a sample of %d receipts",len(senders),flowMemoSample){return fmt.Errorf("actual sender count differs")};return nil}
 if len(queries)!=2||!strings.Contains(queries[1].SQL,"message_event_attributes")||!strings.Contains(queries[1].SQL,"block_event_attributes")||len(queries[1].Rows)!=1||len(queries[1].Rows[0])!=1{return fmt.Errorf("wallet count query not recorded")};n,ok:=queries[1].Rows[0][0].(int64);if !ok{return fmt.Errorf("wallet count type")};expected:=flowStop{};if n>ledgerHeavyRows{expected.Class=flowBusyWallet};if expected!=stop{return fmt.Errorf("wallet count class differs")};return nil
}
