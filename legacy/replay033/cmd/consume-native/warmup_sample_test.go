package main
import (
 "bytes"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "os"
 "path/filepath"
 "testing"
 "time"
 "syscall"
 gaia "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
 abci "github.com/tendermint/tendermint/abci/types"
 amino "github.com/tendermint/go-amino"
 dbm "github.com/tendermint/tendermint/libs/db"
 ctypes "github.com/tendermint/tendermint/rpc/core/types"
 tm "github.com/tendermint/tendermint/types"
)
func TestBoundedNativeWarmupSample(t *testing.T){
 root:=os.Getenv("HUB1_WARMUP_SAMPLE_ROOT");if root==""{t.Skip("exclusive scratch sample not selected")}
 source:="/Users/danb/.cache/cosmos-tax-tmp/hub1-range2-retry-20261115T1555Z/profile-200001"
 cache:=filepath.Join(source,"source/cache");history:=filepath.Join(filepath.Dir(root),"hub1-validator-history.private.jsonl");body,e:=os.ReadFile(history);must(e);sum:=sha256.Sum256(body);pin:=hex.EncodeToString(sum[:])
 copyStart:=time.Now();home:=filepath.Join(root,"native-copy");copyCheckpoint(filepath.Join(source,"proof/native-home"),home);copySeconds:=time.Since(copyStart).Seconds()
 db,e:=dbm.NewGoLevelDB("application",home);must(e);defer db.Close();app:=gaia.NewGaiaApp(__diagnosticLogger(),db,nil,true)
 c:=amino.NewCodec();ctypes.RegisterAmino(c)
 var previous ctypes.ResultBlock;must(c.UnmarshalJSON(rawCache(cache,219357,"-block.raw.gz"),&previous))
 prior:=app.LastCommitID().Hash;start:=time.Now();count:=0
 for h:=int64(219358);h<=219457;h++{
  var block,next ctypes.ResultBlock;var expected ctypes.ResultBlockResults
  must(c.UnmarshalJSON(rawCache(cache,h,"-block.raw.gz"),&block));must(c.UnmarshalJSON(rawCache(cache,h+1,"-block.raw.gz"),&next));must(c.UnmarshalJSON(rawCache(cache,h,"-block_results.raw.gz"),&expected))
  need(block.Block!=nil&&next.Block!=nil&&block.Block.ValidateBasic()==nil&&next.Block.ValidateBasic()==nil&&bytes.Equal(block.Block.AppHash,prior)&&bytes.Equal(block.Block.LastBlockID.Hash,previous.Block.Hash()),"sample before root or parent differs")
  need(len(block.Block.Evidence.Evidence)==0,"bounded sample expected actual no-evidence source")
  set:=historicalValidators(history,pin,h-1);need(bytes.Equal(set.Hash(),previous.Block.ValidatorsHash)&&len(block.Block.LastCommit.Precommits)==set.Size(),"sample vote set differs")
  votes:=[]abci.VoteInfo{};for i,v:=range set.Validators{vote:=block.Block.LastCommit.Precommits[i];need(vote==nil||bytes.Equal(vote.ValidatorAddress,v.Address),"sample vote slot differs");votes=append(votes,abci.VoteInfo{Validator:tm.TM2PB.Validator(v),SignedLastBlock:vote!=nil})}
  begin:=app.BeginBlock(abci.RequestBeginBlock{Hash:block.Block.Hash(),Header:tm.TM2PB.Header(&block.Block.Header),LastCommitInfo:abci.LastCommitInfo{Round:int32(block.Block.LastCommit.Round()),Votes:votes}})
  deliver:=[]*abci.ResponseDeliverTx{};for _,tx:=range block.Block.Data.Txs{res:=app.DeliverTx(tx);deliver=append(deliver,&res)}
  end:=app.EndBlock(abci.RequestEndBlock{Height:h});id:=app.Commit()
  need(expected.Results!=nil&&sameABCIResponses(begin,deliver,end,expected.Results.BeginBlock,expected.Results.DeliverTx,expected.Results.EndBlock),"sample full ABCI differs")
  need(bytes.Equal(id.Data,next.Block.AppHash)&&bytes.Equal(next.Block.LastResultsHash,tm.NewResults(deliver).Hash()),"sample next-header root/result differs")
  prior=id.Data;previous=block;count++
 }
 seconds:=time.Since(start).Seconds();var usage syscall.Rusage;must(syscall.Getrusage(syscall.RUSAGE_SELF,&usage))
 safe:=map[string]interface{}{"first":219358,"through":219457,"heights":count,"sdk_full_execution_seconds":seconds,"height_per_second":float64(count)/seconds,"checkpoint_copy_seconds":copySeconds,"maxrss_kib_test_process":usage.Maxrss,"all_application_roots_match":true,"all_abci_results_match":true,"financial_checker_sample":false,"sample_is_100_not_50000_heights":true,"original_actors_untouched":true}
 out,e:=json.MarshalIndent(safe,"","  ");must(e);must(os.WriteFile(filepath.Join(root,"sample.safe.json"),out,0600))
}
