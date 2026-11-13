package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/cosmos/cosmos-sdk/store/rootmulti"
	"github.com/tendermint/iavl"
	"github.com/tendermint/tendermint/crypto/merkle"
	"strconv"
)

// verifiedStakingRoot checks membership of the LastTotalPower key through
// both IAVL and multistore proof operators. It returns distinct commitments:
// the staking root and the whole application root. Neither is a residual.
func verifiedStakingRoot(raw []byte, height int64) ([]byte, []byte, error) {
	bad := func() ([]byte, []byte, error) { return nil, nil, fmt.Errorf("staking root proof invalid") }
	var r struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			Response struct {
				Code   uint32       `json:"code"`
				Height string       `json:"height"`
				Key    string       `json:"key"`
				Value  string       `json:"value"`
				Proof  merkle.Proof `json:"proof"`
			} `json:"response"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &r) != nil || len(r.Error) > 0 && string(r.Error) != "null" {
		return bad()
	}
	a := r.Result.Response
	p := a.Proof
	key, e := base64.StdEncoding.DecodeString(a.Key)
	if e != nil || !bytes.Equal(key, []byte{0x12}) || a.Code != 0 || a.Height != strconv.FormatInt(height, 10) {
		return bad()
	}
	if len(p.Ops) != 2 || p.Ops[0].Type != "iavl:v" || p.Ops[1].Type != "multistore" || !bytes.Equal(p.Ops[0].Key, key) || !bytes.Equal(p.Ops[1].Key, []byte("staking")) {
		return bad()
	}
	v, e := base64.StdEncoding.DecodeString(a.Value)
	if e != nil {
		return bad()
	}
	op, e := iavl.IAVLValueOpDecoder(p.Ops[0])
	if e != nil {
		return bad()
	}
	roots, e := op.Run([][]byte{v})
	if e != nil || len(roots) != 1 {
		return bad()
	}
	multi, e := rootmulti.MultiStoreProofOpDecoder(p.Ops[1])
	if e != nil {
		return bad()
	}
	apps, e := multi.Run(roots)
	if e != nil || len(apps) != 1 {
		return bad()
	}
	return roots[0], apps[0], nil
}
func stakingRoot(raw []byte) ([]byte, error) {
	var r struct {
		Result struct {
			Response struct {
				Height string `json:"height"`
			} `json:"response"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil, fmt.Errorf("proof malformed")
	}
	h, e := strconv.ParseInt(r.Result.Response.Height, 10, 64)
	if e != nil {
		return nil, e
	}
	root, _, e := verifiedStakingRoot(raw, h)
	return root, e
}
