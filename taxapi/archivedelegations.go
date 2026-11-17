package taxapi

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strconv"
	"strings"

	"cosmossdk.io/math"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/shopspring/decimal"

	"github.com/DefiantLabs/cosmos-indexer/taxapi/internal/servicehttp"
)

// delegationKeyPrefix is the staking store prefix of delegation records,
// keyed by the length-prefixed delegator address since SDK v0.43 (Gaia v6).
const delegationKeyPrefix = 0x31

// rpcHeightParam is the RPC query parameter naming the height to answer at.
const rpcHeightParam = "height"

// lostBondDenom is the delegation query failing on an archive node whose
// newer binary reads staking params where the queried height never kept
// them, so it prices each delegation in an empty denom.
func lostBondDenom(err error) bool {
	var se *servicehttp.StatusError
	return errors.As(err, &se) && se.Code == 500 && strings.Contains(se.Message, "invalid denom")
}

// storeDelegations reads addr's delegations at height without the delegation
// query: the delegator's validators, with their tokens and shares, from the
// validators query at height, and each delegation record from the staking
// store by its key at height over RPC (the store's prefix read answers from
// the latest state whatever height it is asked for, so it is never used).
// Each record is priced with the SDK's own truncation, as the delegation
// query does. A store read that does not answer at height returns nothing.
func (n *ledgerNode) storeDelegations(addr string, height int64) (map[string]*big.Int, map[string]decimal.Decimal, error) {
	if n.rpc == nil {
		return nil, nil, errors.New("no archive RPC to read the staking store from")
	}
	_, del, err := bech32.DecodeAndConvert(addr)
	if err != nil {
		return nil, nil, err
	}
	var vals []stakingtypes.Validator
	err = n.paged("/cosmos/staking/v1beta1/delegators/"+addr+"/validators", height, func(get func(any) error) (pageResp, error) {
		var page struct {
			Validators []struct {
				OperatorAddress string `json:"operator_address"`
				Tokens          string `json:"tokens"`
				DelegatorShares string `json:"delegator_shares"`
			} `json:"validators"`
			Pagination pageResp `json:"pagination"`
		}
		if err := get(&page); err != nil {
			return pageResp{}, err
		}
		for _, v := range page.Validators {
			tokens, ok := math.NewIntFromString(v.Tokens)
			if !ok {
				return pageResp{}, fmt.Errorf("validator tokens %q", v.Tokens)
			}
			shares, err := math.LegacyNewDecFromStr(v.DelegatorShares)
			if err != nil {
				return pageResp{}, err
			}
			vals = append(vals, stakingtypes.Validator{OperatorAddress: v.OperatorAddress, Tokens: tokens, DelegatorShares: shares})
		}
		return page.Pagination, nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("delegator validators: %w", err)
	}
	amounts, shares := map[string]*big.Int{}, map[string]decimal.Decimal{}
	for _, v := range vals {
		if amounts[v.OperatorAddress] != nil {
			return nil, nil, errors.New("the delegator's validators list a validator twice")
		}
		_, val, err := bech32.DecodeAndConvert(v.OperatorAddress)
		if err != nil {
			return nil, nil, err
		}
		if len(del) > 255 || len(val) > 255 {
			return nil, nil, errors.New("an address is too long for a store key")
		}
		key := append([]byte{delegationKeyPrefix, byte(len(del))}, del...) //nolint:gosec // length checked above
		key = append(append(key, byte(len(val))), val...)                  //nolint:gosec // length checked above
		bz, err := n.storeKey("staking", key, height)
		if err != nil {
			return nil, nil, err
		}
		if len(bz) == 0 {
			return nil, nil, errors.New("a validator the delegator has has no delegation record at the height")
		}
		var d stakingtypes.Delegation
		if err := d.Unmarshal(bz); err != nil {
			return nil, nil, fmt.Errorf("delegation record: %w", err)
		}
		if d.DelegatorAddress != addr || d.ValidatorAddress != v.OperatorAddress {
			return nil, nil, errors.New("a delegation record names another delegator or validator")
		}
		amounts[v.OperatorAddress] = v.TokensFromShares(d.Shares).TruncateInt().BigInt()
		sh, err := decimal.NewFromString(d.Shares.String())
		if err != nil {
			return nil, nil, err
		}
		shares[v.OperatorAddress] = sh
	}
	return amounts, shares, nil
}

// storeKey reads one key of a module store at height over RPC.
func (n *ledgerNode) storeKey(store string, key []byte, height int64) ([]byte, error) {
	var body struct {
		Result struct {
			Response struct {
				Code   uint32 `json:"code"`
				Value  string `json:"value"`
				Height string `json:"height"`
			} `json:"response"`
		} `json:"result"`
	}
	q := url.Values{"path": {`"/store/` + store + `/key"`}, "data": {"0x" + hex.EncodeToString(key)}, rpcHeightParam: {strconv.FormatInt(height, 10)}}
	if err := n.rpc.GetJSON("/abci_query", q, &body); err != nil {
		return nil, fmt.Errorf("%s store: %w", store, err)
	}
	r := body.Result.Response
	if r.Code != 0 {
		return nil, fmt.Errorf("%s store: code %d", store, r.Code)
	}
	if r.Height != strconv.FormatInt(height, 10) {
		return nil, fmt.Errorf("%s store answered at height %q, not %d", store, r.Height, height)
	}
	return base64.StdEncoding.DecodeString(r.Value)
}
