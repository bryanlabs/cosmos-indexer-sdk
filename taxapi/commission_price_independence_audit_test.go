package taxapi

import (
 "encoding/json"
 "errors"
 "math/big"
 "net/http"
 "testing"
 "time"

 "github.com/shopspring/decimal"
 "github.com/stretchr/testify/require"
)

type auditFailTransport struct{}
func (auditFailTransport) RoundTrip(*http.Request) (*http.Response,error) { return nil,errors.New("synthetic oracle timeout") }

// This audit test is confined to a detached candidate worktree. No live oracle,
// database, collector, public generation or financial input is used.
func TestAuditCommissionNativeCapturePriceIndependence(t *testing.T) {
 day:=time.Date(2026,1,2,0,0,0,0,time.UTC)
 var baseline []byte
 for _,amount:=range []int64{0,37} {
  baseline=nil
  for _,mode:=range []string{"priced","found_zero","missing_cached","timeout","metadata_assumed"} {
   o:=NewOracle("http://fixture.invalid","")
   o.http=&http.Client{Transport:auditFailTransport{}}
   if mode!="timeout" && mode!="metadata_assumed" {
    price:=2.0
    if mode=="found_zero" { price=0 }
    o.prices[apiMainnet+"|"+apiUatom+"|"+day.Format(apiDateLayout)]=priceCacheEntry{usd:price,found:mode!="missing_cached",cached:nowUTC()}
   }
   s:=NewServer(nil,o,DefaultNativeAsset)
   meta:=map[string]DenomMeta{apiUatom:{Symbol:"ATOM",Decimals:18}}
   if mode=="metadata_assumed" { meta=map[string]DenomMeta{} }
   h:=ledgerHoldings{Liquid:map[string]*big.Int{apiUatom:big.NewInt(amount)},Delegations:map[string]*big.Int{"fixture":big.NewInt(amount)},Unbonding:map[string]*big.Int{"fixture":big.NewInt(amount)}}
   hb:=HistoricalBalance{Height:99}
   s.fillBalanceLines(apiMainnet,meta,&hb,h,day)
   got,source,err:=flowHistoricalHoldings(hb,apiUatom,true)
   require.NoError(t,err)
   require.Equal(t,"ledger_exact",source.Kind)
   require.Equal(t,decimal.NewFromInt(amount),got.Liquid)
   require.Equal(t,decimal.NewFromInt(amount),got.Staked["fixture"])
   require.Equal(t,decimal.NewFromInt(amount),got.Unbonding["fixture"])
   // The actual retained evidence constructor consumes base holdings only.
   w:=&flowWallet{Addr:"fixture",Known:true}
   for i:=range w.pools { w.pools[i]=newFlowPools(i) }
   evidence:=commissionFlowWalletEvidence(w,got,day)
   b,err:=json.Marshal(evidence);require.NoError(t,err)
   if baseline==nil { baseline=b } else { require.Equal(t,string(baseline),string(b),mode) }
   require.NotContains(t,string(b),"Price")
   require.NotContains(t,string(b),"USD")
  }
 }
}
