// Command consume-native independently decodes complete native033 phase states.
// It never executes a historical block, imports rows, or changes default admission.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"

	gaia "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
	"github.com/cosmos/cosmos-sdk/store"
	st "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/bank"
	"github.com/cosmos/cosmos-sdk/x/distribution"
	dt "github.com/cosmos/cosmos-sdk/x/distribution/types"
	"github.com/cosmos/cosmos-sdk/x/mint"
	"github.com/cosmos/cosmos-sdk/x/params"
	"github.com/cosmos/cosmos-sdk/x/staking"
	skt "github.com/cosmos/cosmos-sdk/x/staking/types"
	amino "github.com/tendermint/go-amino"
	abci "github.com/tendermint/tendermint/abci/types"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	tm "github.com/tendermint/tendermint/types"
)

var names = []string{"main", "acc", "staking", "slashing", "mint", "distr", "gov", "fee", "params"}
var codec = gaia.MakeCodec()

type refusal string

func need(ok bool, why string) {
	if !ok {
		panic(refusal(why))
	}
}
func must(e error) {
	if e != nil {
		panic(refusal("private native operation refused"))
	}
}
func equal(a, b interface{}) bool {
	x, e := json.Marshal(a)
	must(e)
	y, e := json.Marshal(b)
	must(e)
	return bytes.Equal(x, y)
}

type pair struct {
	Key    []byte          `json:"key"`
	Before json.RawMessage `json:"before"`
	Value  json.RawMessage `json:"value"`
}
type patch map[string][]pair
type record struct {
	Kind, Phase, Label, Status, Address, Denom, Amount, Validator, Delegator, Unit, Classification string
	Code                                                                                           uint32
	Height                                                                                         int64
	Tx, Message                                                                                    int
	Before, After                                                                                  json.RawMessage
}
type roots struct {
	Version    int64
	StoreInfos []struct {
		Name string
		Core struct{ CommitID sdk.CommitID }
	}
}
type history struct {
	Kind, Chain, Release string
	Height               int64
	Header               tm.Header
	Hash                 []byte `json:"block_hash"`
	Commit               sdk.CommitID
	Roots                roots `json:"native_commit_info"`
	Transactions         [][]byte
	Begin                abci.ResponseBeginBlock
	Deliver              []*abci.ResponseDeliverTx
	End                  abci.ResponseEndBlock
	Genesis              string `json:"genesis_sha256"`
}
type request struct {
	Kind             string
	Height, Through  int64
	Tx, Message      int
	Phase, Status    string
	Restore, Changes patch
	Rows             []record
	Header           tm.Header
	Transactions     [][]byte
	History          history
	NextHeader       tm.Header `json:"next_header"`
	Anchor           history
}
type native struct {
	cms   sdk.CommitMultiStore
	keys  map[string]*sdk.KVStoreKey
	trans map[string]*sdk.TransientStoreKey
}

func newNative() *native {
	n := &native{cms: store.NewCommitMultiStore(dbm.NewMemDB()), keys: map[string]*sdk.KVStoreKey{}, trans: map[string]*sdk.TransientStoreKey{}}
	for _, name := range names {
		k := sdk.NewKVStoreKey(name)
		n.keys[name] = k
		n.cms.MountStoreWithDB(k, st.StoreTypeIAVL, nil)
	}
	for _, name := range []string{staking.TStoreKey, distribution.TStoreKey, params.TStoreKey} {
		k := sdk.NewTransientStoreKey(name)
		n.trans[name] = k
		n.cms.MountStoreWithDB(k, st.StoreTypeTransient, nil)
	}
	must(n.cms.LoadLatestVersion())
	return n
}
func (n *native) apply(p patch) {
	need(len(p) == 9, "all nine patch scopes required")
	for _, name := range names {
		pairs, ok := p[name]
		need(ok, "unknown or missing patch store")
		kv := n.cms.GetKVStore(n.keys[name])
		var previous []byte
		for _, v := range pairs {
			need(len(v.Key) > 0 && (previous == nil || bytes.Compare(previous, v.Key) < 0), "unordered duplicate native patch")
			previous = v.Key
			need(len(v.Before) > 0 && len(v.Value) > 0, "missing explicit native before/after image")
			if bytes.Equal(v.Before, []byte("null")) {
				need(!kv.Has(v.Key), "native absent before-image differs")
			} else {
				var before []byte
				must(json.Unmarshal(v.Before, &before))
				need(kv.Has(v.Key) && bytes.Equal(kv.Get(v.Key), before), "native before-image bytes differ")
			}
			if bytes.Equal(v.Value, []byte("null")) {
				kv.Delete(v.Key)
			} else {
				var b []byte
				must(json.Unmarshal(v.Value, &b))
				if b == nil {
					b = []byte{}
				}
				kv.Set(v.Key, b)
			}
		}
	}
}

type financial struct {
	Bank          map[string]sdk.Int
	Fees          sdk.Coins
	Pool          skt.Pool
	Minter, Next  mint.Minter
	Provision     sdk.Coin
	Liabilities   sdk.DecCoins
	Validators    map[string]skt.Validator
	Delegations   map[string]skt.Delegation
	Accounts      int
	Unbondings    []skt.UnbondingDelegation
	Redelegations []skt.Redelegation
	Stake         staking.Keeper
	Context       sdk.Context
}

func (n *native) decode(header tm.Header) financial {
	ctx := sdk.NewContext(n.cms, tm.TM2PB.Header(&header), false, log.NewNopLogger())
	pk := params.NewKeeper(codec, n.keys[params.StoreKey], n.trans[params.TStoreKey])
	ak := auth.NewAccountKeeper(codec, n.keys[auth.StoreKey], pk.Subspace(auth.DefaultParamspace), auth.ProtoBaseAccount)
	bk := bank.NewBaseKeeper(ak, pk.Subspace(bank.DefaultParamspace), bank.DefaultCodespace)
	fk := auth.NewFeeCollectionKeeper(codec, n.keys[auth.FeeStoreKey])
	sk := staking.NewKeeper(codec, n.keys[staking.StoreKey], n.trans[staking.TStoreKey], bk, pk.Subspace(staking.DefaultParamspace), staking.DefaultCodespace)
	dk := distribution.NewKeeper(codec, n.keys[distribution.StoreKey], pk.Subspace(distribution.DefaultParamspace), bk, &sk, fk, distribution.DefaultCodespace)
	mk := mint.NewKeeper(codec, n.keys[mint.StoreKey], pk.Subspace(mint.DefaultParamspace), &sk, fk)
	v := financial{Bank: map[string]sdk.Int{}, Fees: fk.GetCollectedFees(ctx), Pool: sk.GetPool(ctx), Minter: mk.GetMinter(ctx), Validators: map[string]skt.Validator{}, Delegations: map[string]skt.Delegation{}, Stake: sk, Context: ctx}
	v.Next = v.Minter
	mp := mk.GetParams(ctx)
	v.Next.Inflation = v.Minter.NextInflationRate(mp, sk.BondedRatio(ctx))
	v.Next.AnnualProvisions = v.Next.NextAnnualProvisions(mp, sk.TotalTokens(ctx))
	v.Provision = v.Next.BlockProvision(mp)
	v.Liabilities = dk.GetFeePool(ctx).CommunityPool
	outstanding := 0
	dk.IterateValidatorOutstandingRewards(ctx, func(_ sdk.ValAddress, rewards dt.ValidatorOutstandingRewards) bool {
		v.Liabilities = v.Liabilities.Add(rewards)
		outstanding++
		return false
	})
	ak.IterateAccounts(ctx, func(a auth.Account) bool {
		v.Accounts++
		for _, c := range a.GetCoins() {
			key := a.GetAddress().String() + "/" + c.Denom
			_, exists := v.Bank[key]
			need(!exists, "duplicate native coin")
			v.Bank[key] = c.Amount
		}
		return false
	})
	for _, x := range sk.GetAllValidators(ctx) {
		v.Validators[x.OperatorAddress.String()] = x
	}
	for _, x := range sk.GetAllDelegations(ctx) {
		v.Delegations[x.DelegatorAddress.String()+"/"+x.ValidatorAddress.String()] = x
	}
	sk.IterateUnbondingDelegations(ctx, func(_ int64, x skt.UnbondingDelegation) bool { v.Unbondings = append(v.Unbondings, x); return false })
	sk.IterateRedelegations(ctx, func(_ int64, x skt.Redelegation) bool { v.Redelegations = append(v.Redelegations, x); return false })
	// Independent native iterators must cover every typed prefix, not a projection.
	counts := func(name string, prefix byte) int {
		it := n.cms.GetKVStore(n.keys[name]).Iterator([]byte{prefix}, []byte{prefix + 1})
		defer it.Close()
		count := 0
		for ; it.Valid(); it.Next() {
			count++
		}
		return count
	}
	need(v.Accounts == counts("acc", 1) && len(v.Validators) == counts("staking", 0x21) && len(v.Delegations) == counts("staking", 0x31), "incomplete typed native financial coverage")
	return v
}
func delta(a, b map[string]sdk.Int) map[string]sdk.Int {
	o := map[string]sdk.Int{}
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	for k := range keys {
		x, y := sdk.ZeroInt(), sdk.ZeroInt()
		if n, ok := a[k]; ok {
			x = n
		}
		if n, ok := b[k]; ok {
			y = n
		}
		if !x.Equal(y) {
			o[k] = y.Sub(x)
		}
	}
	return o
}
func sameInts(a, b map[string]sdk.Int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w, ok := b[k]
		if !ok || !v.Equal(w) {
			return false
		}
	}
	return true
}
func add(m map[string]sdk.Int, k string, v sdk.Int) {
	if old, ok := m[k]; ok {
		v = v.Add(old)
	}
	if v.IsZero() {
		delete(m, k)
	} else {
		m[k] = v
	}
}
func signed(s string) sdk.Int {
	x, ok := sdk.NewIntFromString(s)
	need(ok && x.String() == s, "noncanonical token integer")
	return x
}
func stakeObject(v financial, r record) interface{} {
	a, e := sdk.ValAddressFromBech32(r.Validator)
	must(e)
	d, e := sdk.AccAddressFromBech32(r.Delegator)
	must(e)
	var result struct {
		Validator  *skt.Validator  `json:"validator"`
		Delegation *skt.Delegation `json:"delegation"`
	}
	if x, ok := v.Validators[a.String()]; ok {
		result.Validator = &x
	}
	if x, ok := v.Delegations[d.String()+"/"+a.String()]; ok {
		result.Delegation = &x
	}
	return result
}
func financialCheck(r request, a, b financial) {
	if r.Phase == "block" || r.Phase == "transaction" {
		return
	}
	expected := map[string]sdk.Int{}
	for _, row := range r.Rows {
		need(row.Height == r.Height && row.Tx == r.Tx && row.Message == r.Message && row.Phase == r.Phase && row.Status == r.Status, "financial row scope differs")
		switch row.Kind {
		case "movement":
			need(row.Unit == "native_bank_tokens", "movement units differ")
			n := signed(row.Amount)
			add(expected, row.Address+"/"+row.Denom, n)
			if row.Label == "ante_fee" {
				need(row.Phase == "ante" && n.IsNegative() || row.Phase == "ante" && n.IsZero(), "signed fee is not paid income")
			}
		case "principal", "staking_shares":
			var before, after interface{}
			must(json.Unmarshal(row.Before, &before))
			must(json.Unmarshal(row.After, &after))
			x, y := stakeObject(a, row), stakeObject(b, row)
			var sx, sy interface{}
			bx, e := json.Marshal(x)
			must(e)
			by, e := json.Marshal(y)
			must(e)
			must(json.Unmarshal(bx, &sx))
			must(json.Unmarshal(by, &sy))
			need(reflect.DeepEqual(before, sx) && reflect.DeepEqual(after, sy), "principal/share full native before/after differs")
			var bs, as struct {
				Validator  *skt.Validator  `json:"validator"`
				Delegation *skt.Delegation `json:"delegation"`
			}
			must(json.Unmarshal(row.Before, &bs))
			must(json.Unmarshal(row.After, &as))
			if row.Kind == "principal" {
				need(bs.Validator != nil && row.Unit == "native_bond_tokens", "principal native unit/source missing")
				end := sdk.ZeroInt()
				if as.Validator != nil {
					end = as.Validator.Tokens
				}
				need(signed(row.Amount).Equal(bs.Validator.Tokens.Sub(end)), "native principal differs")
			} else {
				start, end := sdk.ZeroDec(), sdk.ZeroDec()
				if bs.Delegation != nil {
					start = bs.Delegation.Shares
				}
				if as.Delegation != nil {
					end = as.Delegation.Shares
				}
				need(row.Unit == "delegation_shares" && row.Denom == "" && end.Sub(start).String() == row.Amount, "native shares differ")
			}
		case "principal_request", "share_request", "accounting", "native_execution":
		default:
			need(false, "unknown typed financial record")
		}
	}
	need(sameInts(delta(a.Bank, b.Bank), expected), "all-account all-coin native bank effects unexplained")
	if r.Phase == "ante" {
		fee := b.Fees.Sub(a.Fees)
		paid := map[string]sdk.Int{}
		for _, c := range fee {
			if !c.Amount.IsZero() {
				paid[c.Denom] = c.Amount
			}
		}
		debits := map[string]sdk.Int{}
		for k, v := range expected {
			for i := len(k) - 1; i >= 0; i-- {
				if k[i] == '/' {
					add(debits, k[i+1:], v.Neg())
					break
				}
			}
		}
		need(sameInts(paid, debits), "signed fee differs from native paid collection")
		need(equal(a.Pool, b.Pool) && equal(a.Minter, b.Minter) && equal(a.Validators, b.Validators) && equal(a.Delegations, b.Delegations), "ante changed staking principal/shares/mint")
	}
	if r.Phase == "begin_block" {
		need(equal(b.Minter, a.Next), "native exact minter transition differs")
		need(b.Pool.NotBondedTokens.Sub(a.Pool.NotBondedTokens).Equal(a.Provision.Amount) && b.Pool.BondedTokens.Equal(a.Pool.BondedTokens), "native mint/pool supply differs")
		inflow := sdk.NewDecCoins(sdk.Coins{a.Provision})
		beforeLiability := a.Liabilities.Add(sdk.NewDecCoins(a.Fees))
		afterLiability := b.Liabilities.Add(sdk.NewDecCoins(b.Fees))
		need(afterLiability.Sub(beforeLiability).IsEqual(inflow), "mint/fee/community/outstanding reward conservation differs")
		if r.Height == 1 {
			// Native distribution/abci_app.go allocates only above height one.
			need(b.Liabilities.IsEqual(a.Liabilities), "genesis block invented reward allocation")
		} else {
			need(len(b.Fees) == 0, "native allocation left unexplained collected fees")
		}
	}
}

// InitChain only, never BeginBlock, DeliverTx, EndBlock, or an export. The
// private diagnostic commit makes the genuine genesis bytes queryable and is
// not a historical height, a payout, or a next-chain opening.
func genesisOpening(path string, n *native) {
	doc, e := tm.GenesisDocFromFile(path)
	must(e)
	need(doc.ChainID == "cosmoshub-1", "native genesis chain differs")
	app := gaia.NewGaiaApp(log.NewNopLogger(), dbm.NewMemDB(), nil, true)
	validators := make([]*tm.Validator, len(doc.Validators))
	for i, v := range doc.Validators {
		validators[i] = tm.NewValidator(v.PubKey, v.Power)
	}
	app.InitChain(abci.RequestInitChain{Time: doc.GenesisTime, ChainId: doc.ChainID, ConsensusParams: tm.TM2PB.ConsensusParams(doc.ConsensusParams), Validators: tm.TM2PB.ValidatorUpdates(tm.NewValidatorSet(validators)), AppStateBytes: doc.AppState})
	app.Commit()
	for _, name := range names {
		expected := map[string][]byte{}
		for prefix := 0; prefix < 256; prefix++ {
			res := app.Query(abci.RequestQuery{Path: "/store/" + name + "/subspace", Height: 1, Data: []byte{byte(prefix)}})
			need(res.Code == 0, "native genesis prefix query failed")
			var pairs []sdk.KVPair
			must(codec.UnmarshalBinaryLengthPrefixed(res.Value, &pairs))
			for _, pair := range pairs {
				need(len(pair.Key) > 0 && int(pair.Key[0]) == prefix, "native genesis key scope differs")
				_, duplicate := expected[string(pair.Key)]
				need(!duplicate, "duplicate genesis key")
				expected[string(pair.Key)] = pair.Value
			}
		}
		it := n.cms.GetKVStore(n.keys[name]).Iterator(nil, nil)
		count := 0
		for ; it.Valid(); it.Next() {
			value, ok := expected[string(it.Key())]
			need(ok && bytes.Equal(value, it.Value()), "genuine native InitChain opening differs from complete source baseline")
			count++
		}
		it.Close()
		need(count == len(expected), "missing genuine native genesis key")
	}
}

func signedStakingBinding(r request, row record) {
	need(r.Tx >= 0 && r.Tx < len(r.Transactions) && r.Message >= 0, "staking record lacks original native transaction")
	var tx auth.StdTx
	must(codec.UnmarshalBinaryLengthPrefixed(r.Transactions[r.Tx], &tx))
	messages := tx.GetMsgs()
	need(r.Message < len(messages), "staking record original message index differs")
	var label string
	var src, dst sdk.ValAddress
	var del sdk.AccAddress
	var coin *sdk.Coin
	var shares *sdk.Dec
	switch message := messages[r.Message].(type) {
	case staking.MsgDelegate:
		label = "delegate"
		src = message.ValidatorAddress
		dst = src
		del = message.DelegatorAddress
		coin = &message.Value
	case staking.MsgCreateValidator:
		label = "create_validator"
		src = message.ValidatorAddress
		dst = src
		del = message.DelegatorAddress
		coin = &message.Value
	case staking.MsgUndelegate:
		label = "undelegate"
		src = message.ValidatorAddress
		del = message.DelegatorAddress
		shares = &message.SharesAmount
	case staking.MsgBeginRedelegate:
		label = "redelegate"
		src = message.ValidatorSrcAddress
		dst = message.ValidatorDstAddress
		del = message.DelegatorAddress
		shares = &message.SharesAmount
	default:
		need(false, "staking classification not bound to original native message")
	}
	need(row.Delegator == del.String() && row.Address == del.String(), "staking owner differs from original native signer message")
	switch row.Kind {
	case "principal_request":
		need(coin != nil && row.Validator == src.String() && row.Label == label && row.Unit == "native_bond_tokens" && row.Denom == coin.Denom && row.Amount == coin.Amount.String(), "native token request differs from original signed message")
		need(equal(json.RawMessage(row.Before), coin), "native token request original coin differs")
	case "share_request":
		need(shares != nil && row.Validator == src.String() && row.Label == label+":requested_source_shares" && row.Unit == "delegation_shares" && row.Denom == "" && row.Amount == shares.String(), "native share request differs from original signed message")
		need(equal(json.RawMessage(row.Before), shares), "native requested shares original scalar differs")
	case "principal":
		need(shares != nil && row.Validator == src.String() && row.Label == label, "native principal source differs from original signed message")
	case "staking_shares":
		expected := src
		expectedLabel := label + ":source"
		if coin != nil || row.Label == label+":destination" {
			expected = dst
			expectedLabel = label + ":destination"
		}
		need(len(expected) > 0 && row.Validator == expected.String() && row.Label == expectedLabel, "native shares direction differs from original signed message")
	default:
		need(false, "unknown original staking binding")
	}
}

func signedAnte(r request, a, b financial) {
	need(r.Tx >= 0 && r.Tx < len(r.Transactions), "native ante missing original signed transaction")
	var tx auth.StdTx
	must(codec.UnmarshalBinaryLengthPrefixed(r.Transactions[r.Tx], &tx))
	signers := tx.GetSigners()
	need(len(signers) > 0, "native fee payer absent")
	actual := delta(a.Bank, b.Bank)
	if r.Status == "committed" {
		expected := map[string]sdk.Int{}
		for _, coin := range tx.Fee.Amount {
			add(expected, signers[0].String()+"/"+coin.Denom, coin.Amount.Neg())
		}
		need(sameInts(actual, expected) && b.Fees.Sub(a.Fees).IsEqual(tx.Fee.Amount), "native charged fee not bound to original signer and complete signed fee vector")
	}
}

func rawCache(cache string, h int64, suffix string) []byte {
	p := filepath.Join(cache, "cosmoshub-1", strconv.FormatInt(h/10000, 10), strconv.FormatInt(h, 10)+suffix)
	f, e := os.Open(p)
	must(e)
	defer f.Close()
	z, e := gzip.NewReader(f)
	must(e)
	defer z.Close()
	raw, e := io.ReadAll(io.LimitReader(z, 64<<20))
	must(e)
	var env struct{ Result json.RawMessage }
	must(json.Unmarshal(raw, &env))
	return env.Result
}
func checkHistory(cache string, h history, next tm.Header, prior []byte) {
	c := amino.NewCodec()
	ctypes.RegisterAmino(c)
	var block ctypes.ResultBlock
	must(c.UnmarshalJSON(rawCache(cache, h.Height, "-block.raw.gz"), &block))
	need(block.Block != nil && block.BlockMeta != nil && block.Block.ValidateBasic() == nil, "invalid raw native block")
	need(equal(h.Header, block.Block.Header) && bytes.Equal(h.Header.Hash(), h.Hash) && bytes.Equal(h.Hash, block.BlockMeta.BlockID.Hash), "native header/time/block hash differs")
	need(bytes.Equal(prior, h.Header.AppHash), "native previous application root differs")
	var results ctypes.ResultBlockResults
	must(c.UnmarshalJSON(rawCache(cache, h.Height, "-block_results.raw.gz"), &results))
	need(results.Height == h.Height && results.Results != nil, "raw native result identity differs")
	need(equal(h.Deliver, results.Results.DeliverTx) && equal(h.Begin, results.Results.BeginBlock) && equal(h.End, results.Results.EndBlock), "complete native ABCI result differs")
	need(len(h.Transactions) == len(block.Block.Data.Txs), "signed transaction coverage differs")
	for i, tx := range h.Transactions {
		need(bytes.Equal(tx, block.Block.Data.Txs[i]), "signed bytes differ")
		var signed auth.StdTx
		must(codec.UnmarshalBinaryLengthPrefixed(tx, &signed))
		need(len(signed.GetMsgs()) > 0, "native signed tx empty")
	}
	var following ctypes.ResultBlock
	must(c.UnmarshalJSON(rawCache(cache, h.Height+1, "-block.raw.gz"), &following))
	need(following.Block != nil && equal(next, following.Block.Header) && bytes.Equal(next.AppHash, h.Commit.Hash) && bytes.Equal(next.LastResultsHash, tm.NewResults(h.Deliver).Hash()) && bytes.Equal(next.LastBlockID.Hash, h.Hash), "independent next-header root/result binding differs")
}
func run() {
	cache := flag.String("cache", "", "bounded immutable native raw cache")
	receipt := flag.String("receipt", "", "exclusive private receipt")
	flag.Parse()
	need(*cache != "" && *receipt != "", "private paths required")
	phase, committed := newNative(), newNative()
	decoder := json.NewDecoder(bufio.NewReader(os.Stdin))
	var height, through, phases, transactions int64
	var prior []byte
	anchor := false
	openingChecked := false
	done := false
	for {
		var r request
		e := decoder.Decode(&r)
		if e == io.EOF {
			break
		}
		must(e)
		need(!done, "trailing native data")
		switch r.Kind {
		case "anchor":
			need(!anchor && r.Anchor.Height == 0 && r.Anchor.Kind == "source_anchor" && r.Anchor.Chain == "cosmoshub-1" && r.Through > 0, "genuine genesis anchor required")
			raw, e := os.ReadFile(filepath.Join(*cache, "genesis/genesis.cosmoshub-1.json"))
			must(e)
			sum := sha256.Sum256(raw)
			need(hex.EncodeToString(sum[:]) == r.Anchor.Genesis, "source genesis differs")
			anchor = true
			through = r.Through
			prior = r.Anchor.Commit.Hash
		case "phase":
			need(anchor && r.Height == height+1, "native phase height gap")
			phase.apply(r.Restore)
			if !openingChecked {
				need(r.Height == 1 && r.Phase == "begin_block", "genuine opening must precede first phase")
				genesisOpening(filepath.Join(*cache, "genesis/genesis.cosmoshub-1.json"), phase)
				openingChecked = true
			}
			a := phase.decode(r.Header)
			phase.apply(r.Changes)
			b := phase.decode(r.Header)
			financialCheck(r, a, b)
			for _, row := range r.Rows {
				if row.Kind == "principal_request" || row.Kind == "share_request" || row.Kind == "principal" || row.Kind == "staking_shares" {
					signedStakingBinding(r, row)
				}
			}
			if r.Phase == "ante" {
				signedAnte(r, a, b)
			}
			phases++
		case "commit":
			need(anchor && r.Height == height+1 && r.Height <= through, "native commit gap/overlap")
			committed.apply(r.Changes)
			id := committed.cms.Commit()
			need(id.Version == r.Height && bytes.Equal(id.Hash, r.History.Commit.Hash), "raw source journals do not reproduce native IAVL application commitment")
			need(r.History.Roots.Version == r.Height && len(r.History.Roots.StoreInfos) == 9, "all-nine native roots required")
			seen := map[string]bool{}
			for _, s := range r.History.Roots.StoreInfos {
				k, ok := committed.keys[s.Name]
				need(ok && !seen[s.Name], "duplicate root store")
				seen[s.Name] = true
				actual := committed.cms.GetCommitKVStore(k).LastCommitID()
				need(actual.Version == s.Core.CommitID.Version && bytes.Equal(actual.Hash, s.Core.CommitID.Hash), "complete raw store does not reproduce native IAVL commitment")
			}
			checkHistory(*cache, r.History, r.NextHeader, prior)
			prior = id.Hash
			height = r.Height
			if height%100 == 0 {
				fmt.Printf("bounded_native_verified_height=%d typed_phases=%d\n", height, phases)
			}
			transactions += int64(len(r.History.Transactions))
		case "bounded_end":
			need(anchor && height == through && r.Height == height, "incomplete bounded native proof")
			done = true
		default:
			need(false, "unknown native consumer instruction")
		}
	}
	need(done, "EOF is not bounded completion")
	f, e := os.OpenFile(*receipt, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	must(e)
	defer f.Close()
	must(json.NewEncoder(f).Encode(map[string]interface{}{"schema": "native033-independent-bounded-financial-v1", "first": 1, "through": height, "native_iavl_heights": height, "typed_financial_phases": phases, "transactions": transactions, "source_whole_complete": false, "financial_admission": false, "global_books_tax_acceptance": false}))
	must(f.Sync())
}
func main() {
	defer func() {
		if x := recover(); x != nil {
			if reason, ok := x.(refusal); ok {
				fmt.Fprintln(os.Stderr, "native bounded financial proof refused:", reason)
			} else {
				fmt.Fprintln(os.Stderr, "native bounded financial proof refused: native SDK internal refusal")
			}
			os.Exit(1)
		}
	}()
	run()
}
