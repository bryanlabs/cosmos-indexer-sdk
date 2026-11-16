package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	gaia "github.com/cosmos/cosmos-sdk/cmd/gaia/app"
	"github.com/cosmos/cosmos-sdk/store"
	st "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/bank"
	"github.com/cosmos/cosmos-sdk/x/distribution"
	"github.com/cosmos/cosmos-sdk/x/mint"
	"github.com/cosmos/cosmos-sdk/x/params"
	"github.com/cosmos/cosmos-sdk/x/staking"
	skt "github.com/cosmos/cosmos-sdk/x/staking/types"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
	tmtypes "github.com/tendermint/tendermint/types"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Independent native READ APIs and pure decimal math on NEW in-memory copies
// of the untouched Gaia audit. No historical keeper transition is re-executed.
func TestTerminalNativeFinancialContext(t *testing.T) {
	dir, cache := os.Getenv("HUB1_TERMINAL_AUDIT"), os.Getenv("HUB1_TERMINAL_CACHE")
	if dir == "" || cache == "" {
		t.Skip("explicit terminal raw native evidence required")
	}
	in, e := os.Open(filepath.Join(dir, "native-all-store-values.ndjson"))
	if e != nil {
		t.Fatal("native decoded evidence required")
	}
	defer in.Close()
	output := os.Getenv("HUB1_TERMINAL_CONTEXT_OUT")
	if output == "" {
		output = filepath.Join(dir, "native-financial-context.ndjson")
	}
	out, e := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal("exclusive native context required")
	}
	defer out.Close()
	codec := gaia.MakeCodec()
	reader := bufio.NewReader(in)
	count := 0
	for {
		raw, e := reader.ReadBytes('\n')
		if e == io.EOF && len(raw) == 0 {
			break
		}
		if e != nil {
			t.Fatal("incomplete native evidence")
		}
		var input struct {
			Height int64 `json:"height"`
			Stores []struct {
				Name   string       `json:"name"`
				Values []sdk.KVPair `json:"values"`
			} `json:"stores"`
		}
		if json.Unmarshal(raw, &input) != nil || input.Height != 500000+int64(count) || len(input.Stores) != 9 {
			t.Fatal("native context scope differs")
		}
		cached := load(cache, "cosmoshub-1", input.Height, false)
		if cached.err != nil {
			t.Fatal("actual native header required")
		}
		cms := store.NewCommitMultiStore(dbm.NewMemDB())
		keys := map[string]*sdk.KVStoreKey{}
		for _, n := range []string{"main", "acc", "staking", "slashing", "mint", "distr", "gov", "fee", "params"} {
			k := sdk.NewKVStoreKey(n)
			keys[n] = k
			cms.MountStoreWithDB(k, st.StoreTypeIAVL, nil)
		}
		trans := map[string]*sdk.TransientStoreKey{}
		for _, n := range []string{staking.TStoreKey, distribution.TStoreKey, params.TStoreKey} {
			k := sdk.NewTransientStoreKey(n)
			trans[n] = k
			cms.MountStoreWithDB(k, st.StoreTypeTransient, nil)
		}
		if cms.LoadLatestVersion() != nil {
			t.Fatal("in-memory native context unavailable")
		}
		seen := map[string]bool{}
		accounts, validators, delegations := 0, 0, 0
		for _, s := range input.Stores {
			k, ok := keys[s.Name]
			if !ok || seen[s.Name] {
				t.Fatal("all-nine context store identity differs")
			}
			seen[s.Name] = true
			var last []byte
			for _, pair := range s.Values {
				if len(pair.Key) == 0 || (last != nil && bytes.Compare(last, pair.Key) >= 0) {
					t.Fatal("native raw context ordering differs")
				}
				last = pair.Key
				value := pair.Value
				if value == nil {
					value = []byte{}
				}
				cms.GetKVStore(k).Set(pair.Key, value)
				if s.Name == "acc" && pair.Key[0] == 1 {
					accounts++
				}
				if s.Name == "staking" && pair.Key[0] == 0x21 {
					validators++
				}
				if s.Name == "staking" && pair.Key[0] == 0x31 {
					delegations++
				}
			}
		}
		ctx := sdk.NewContext(cms, tmtypes.TM2PB.Header(&cached.block.Header), false, log.NewNopLogger())
		pk := params.NewKeeper(codec, keys[params.StoreKey], trans[params.TStoreKey])
		ak := auth.NewAccountKeeper(codec, keys[auth.StoreKey], pk.Subspace(auth.DefaultParamspace), auth.ProtoBaseAccount)
		bk := bank.NewBaseKeeper(ak, pk.Subspace(bank.DefaultParamspace), bank.DefaultCodespace)
		fk := auth.NewFeeCollectionKeeper(codec, keys[auth.FeeStoreKey])
		sk := staking.NewKeeper(codec, keys[staking.StoreKey], trans[staking.TStoreKey], bk, pk.Subspace(staking.DefaultParamspace), staking.DefaultCodespace)
		dk := distribution.NewKeeper(codec, keys[distribution.StoreKey], pk.Subspace(distribution.DefaultParamspace), bk, &sk, fk, distribution.DefaultCodespace)
		mk := mint.NewKeeper(codec, keys[mint.StoreKey], pk.Subspace(mint.DefaultParamspace), &sk, fk)
		type financialContext struct {
			Height          int64                     `json:"height"`
			HeaderHash      []byte                    `json:"header_hash"`
			RawNativeSHA256 string                    `json:"raw_native_sha256"`
			Accounts        []json.RawMessage         `json:"accounts"`
			Bank            map[string]string         `json:"bank"`
			Validators      []skt.Validator           `json:"validators"`
			Delegations     []skt.Delegation          `json:"delegations"`
			Unbondings      []skt.UnbondingDelegation `json:"unbondings"`
			Redelegations   []skt.Redelegation        `json:"redelegations"`
			Pool            skt.Pool                  `json:"pool"`
			Fees            sdk.Coins                 `json:"fees"`
			BondDenom       string                    `json:"bond_denom"`
			Minter          mint.Minter               `json:"minter"`
			NextMinter      mint.Minter               `json:"next_minter"`
			MintProvision   sdk.Coin                  `json:"mint_provision"`
			Commissions     map[string]sdk.DecCoins   `json:"commissions"`
			Outstanding     map[string]sdk.DecCoins   `json:"outstanding"`
		}
		v := financialContext{Height: input.Height, HeaderHash: cached.metaID, Accounts: []json.RawMessage{}, Bank: map[string]string{}, Validators: sk.GetAllValidators(ctx), Delegations: sk.GetAllDelegations(ctx), Unbondings: []skt.UnbondingDelegation{}, Redelegations: []skt.Redelegation{}, Pool: sk.GetPool(ctx), Fees: fk.GetCollectedFees(ctx), BondDenom: sk.GetParams(ctx).BondDenom, Minter: mk.GetMinter(ctx), Commissions: map[string]sdk.DecCoins{}, Outstanding: map[string]sdk.DecCoins{}}
		mp := mk.GetParams(ctx)
		v.NextMinter = v.Minter
		v.NextMinter.Inflation = v.Minter.NextInflationRate(mp, sk.BondedRatio(ctx))
		v.NextMinter.AnnualProvisions = v.NextMinter.NextAnnualProvisions(mp, sk.TotalTokens(ctx))
		v.MintProvision = v.Minter.BlockProvision(mp)
		ak.IterateAccounts(ctx, func(a auth.Account) bool {
			b, e := codec.MarshalJSON(a)
			if e != nil {
				t.Fatal("native account decode refused")
			}
			v.Accounts = append(v.Accounts, b)
			for _, coin := range a.GetCoins() {
				key := a.GetAddress().String() + "/" + coin.Denom
				if _, ok := v.Bank[key]; ok {
					t.Fatal("duplicate all-coin native balance")
				}
				v.Bank[key] = coin.Amount.String()
			}
			return false
		})
		sk.IterateUnbondingDelegations(ctx, func(_ int64, u skt.UnbondingDelegation) bool { v.Unbondings = append(v.Unbondings, u); return false })
		sk.IterateRedelegations(ctx, func(_ int64, r skt.Redelegation) bool { v.Redelegations = append(v.Redelegations, r); return false })
		for _, val := range v.Validators {
			addr := val.OperatorAddress
			v.Commissions[addr.String()] = dk.GetValidatorAccumulatedCommission(ctx, addr)
			v.Outstanding[addr.String()] = dk.GetValidatorOutstandingRewards(ctx, addr)
		}
		if len(v.Accounts) != accounts || len(v.Validators) != validators || len(v.Delegations) != delegations {
			t.Fatal("complete native financial context coverage differs")
		}
		sum := sha256.Sum256(raw)
		v.RawNativeSHA256 = hex.EncodeToString(sum[:])
		if json.NewEncoder(out).Encode(v) != nil {
			t.Fatal("native financial context write failed")
		}
		count++
	}
	if count != 16 || out.Sync() != nil {
		t.Fatal("complete terminal financial context required")
	}
}
