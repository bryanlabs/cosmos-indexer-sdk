package main

// Block results parsing, the same reading as the extraction (extract_v.py):
//
//   - Events come from begin_block_events and end_block_events (CometBFT
//     0.34 and 0.37) or finalize_block_events split by their mode attribute
//     (0.38: mode EndBlock is the end block, every other mode the begin block).
//   - Attribute keys and values may be base64 (0.34). A known key is decoded
//     with its value; other attributes are kept as they are.
//   - The begin block must hold at most one mint event. Its attributes are
//     read last value wins; every other attribute is read first match wins.
//   - rewards and commission events add the uatom part of their amount
//     ("…uatom" between commas) to the block total and to their validator
//     ("?" when the event names none); an amount without uatom adds nothing.
//   - Successful transactions (code 0) add their withdraw_commission,
//     withdraw_rewards and unbond uatom amounts; unbonds are keyed by the
//     date of their completion_time.
//   - active_proposal end block events give (proposal_id, proposal_result).

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"unicode/utf8"
)

var knownKeys = map[string]bool{"amount": true, "bonded_ratio": true, "inflation": true, "annual_provisions": true,
	"validator": true, "mode": true, "proposal_id": true, "proposal_result": true, "proposal_log": true, "completion_time": true}

var knownB64 = func() map[string]string {
	m := map[string]string{}
	for k := range knownKeys {
		m[base64.StdEncoding.EncodeToString([]byte(k))] = k
	}
	return m
}()

var uatomRe = regexp.MustCompile(`(?:^|,)([0-9.]+)uatom(?:,|$)`)

type rpcAttr struct {
	Key   *string `json:"key"`
	Value *string `json:"value"`
}

type rpcEvent struct {
	Type       string    `json:"type"`
	Attributes []rpcAttr `json:"attributes"`
}

type rpcTx struct {
	Code   int64      `json:"code"`
	Events []rpcEvent `json:"events"`
}

type rpcBlockResults struct {
	Height     string          `json:"height"`
	TxsResults json.RawMessage `json:"txs_results"`
	Begin      json.RawMessage `json:"begin_block_events"`
	End        json.RawMessage `json:"end_block_events"`
	Finalize   json.RawMessage `json:"finalize_block_events"`
}

type proposal struct {
	Height int64
	ID     *string
	Result *string
}

func (p proposal) passed() bool {
	return p.Result != nil && (*p.Result == "proposal_passed" || *p.Result == "passed")
}

// blockData is what one block contributes.
type blockData struct {
	Height     int64
	Mint       map[string]string
	Rewards    *big.Int // 1e18-scaled
	Commission *big.Int
	PerVal     map[string]*[2]*big.Int
	Proposals  []proposal
	WithdrawC  *big.Int
	WithdrawR  *big.Int
	Unbond     map[string]*big.Int
}

func b64Text(v string) (string, bool) {
	b, err := base64.StdEncoding.Strict().DecodeString(v)
	if err != nil || !utf8.Valid(b) {
		return "", false
	}
	return string(b), true
}

// eventAttrs reads every attribute, last value wins (extract_v.attrs).
func eventAttrs(ev rpcEvent) map[string]string {
	out := map[string]string{}
	for _, a := range ev.Attributes {
		k, v := "", ""
		if a.Key != nil {
			k = *a.Key
		}
		if a.Value != nil {
			v = *a.Value
		}
		if !knownKeys[k] {
			if kd, ok := b64Text(k); ok && knownKeys[kd] {
				k = kd
				if v != "" {
					if vd, ok := b64Text(v); ok {
						v = vd
					}
				}
			}
		}
		out[k] = v
	}
	return out
}

// eventAttr reads one attribute, first match wins (extract_v.attr).
func eventAttr(ev rpcEvent, name string) (string, bool) {
	for _, a := range ev.Attributes {
		if a.Key == nil {
			continue
		}
		v := ""
		if a.Value != nil {
			v = *a.Value
		}
		if *a.Key == name {
			return v, true
		}
		if knownB64[*a.Key] == name {
			if v == "" {
				return "", true
			}
			if vd, ok := b64Text(v); ok {
				return vd, true
			}
			return v, true
		}
	}
	return "", false
}

func uatomAmount(ev rpcEvent) (*big.Int, bool, error) {
	a, _ := eventAttr(ev, "amount")
	m := uatomRe.FindStringSubmatch(a)
	if m == nil {
		return nil, false, nil
	}
	v, err := parseFix(m[1])
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

func decodeEvents(raw json.RawMessage) ([]rpcEvent, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var evs []rpcEvent
	err := json.Unmarshal(raw, &evs)
	return evs, err
}

var errNoResult = errors.New("no block results")

// parseBlockResults reads one /block_results JSON-RPC response body.
func parseBlockResults(body []byte, height int64) (*blockData, error) {
	var resp struct {
		Result *rpcBlockResults `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode: %v", err)
	}
	r := resp.Result
	if r == nil {
		return nil, errNoResult
	}
	if r.Height != strconv.FormatInt(height, 10) {
		return nil, errors.New("height mismatch")
	}
	if r.Begin == nil && r.End == nil && r.Finalize == nil {
		return nil, errors.New("no block events key")
	}
	begin, err := decodeEvents(r.Begin)
	if err != nil {
		return nil, err
	}
	end, err := decodeEvents(r.End)
	if err != nil {
		return nil, err
	}
	fin, err := decodeEvents(r.Finalize)
	if err != nil {
		return nil, err
	}
	for _, ev := range fin {
		if m, _ := eventAttr(ev, "mode"); m == "EndBlock" {
			end = append(end, ev)
		} else {
			begin = append(begin, ev)
		}
	}
	b := &blockData{Height: height, Rewards: new(big.Int), Commission: new(big.Int), PerVal: map[string]*[2]*big.Int{},
		WithdrawC: new(big.Int), WithdrawR: new(big.Int), Unbond: map[string]*big.Int{}}
	for _, ev := range begin {
		switch ev.Type {
		case "mint":
			if b.Mint != nil {
				return nil, errors.New("two mint events")
			}
			b.Mint = eventAttrs(ev)
		case "rewards", "commission":
			v, ok, err := uatomAmount(ev)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			i := 0
			if ev.Type == "rewards" {
				b.Rewards.Add(b.Rewards, v)
			} else {
				b.Commission.Add(b.Commission, v)
				i = 1
			}
			op, found := eventAttr(ev, "validator")
			if !found || op == "" {
				op = "?"
			}
			pv := b.PerVal[op]
			if pv == nil {
				pv = &[2]*big.Int{new(big.Int), new(big.Int)}
				b.PerVal[op] = pv
			}
			pv[i].Add(pv[i], v)
		}
	}
	for _, ev := range end {
		if ev.Type != "active_proposal" {
			continue
		}
		a := eventAttrs(ev)
		p := proposal{Height: height}
		if v, ok := a["proposal_id"]; ok {
			p.ID = &v
		}
		if v, ok := a["proposal_result"]; ok {
			p.Result = &v
		}
		b.Proposals = append(b.Proposals, p)
	}
	// Like the extraction, transactions are decoded only when the block
	// holds one of the events counted from them.
	var txs []rpcTx
	if len(r.TxsResults) > 0 && (bytes.Contains(r.TxsResults, []byte("withdraw_")) || bytes.Contains(r.TxsResults, []byte(`"unbond"`))) {
		if err := json.Unmarshal(r.TxsResults, &txs); err != nil {
			return nil, fmt.Errorf("txs_results: %v", err)
		}
	}
	for _, tx := range txs {
		if tx.Code != 0 {
			continue
		}
		for _, ev := range tx.Events {
			if ev.Type != "withdraw_commission" && ev.Type != "withdraw_rewards" && ev.Type != "unbond" {
				continue
			}
			v, ok, err := uatomAmount(ev)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			switch ev.Type {
			case "withdraw_commission":
				b.WithdrawC.Add(b.WithdrawC, v)
			case "withdraw_rewards":
				b.WithdrawR.Add(b.WithdrawR, v)
			default:
				ct, found := eventAttr(ev, "completion_time")
				k := "?"
				if found && ct != "" {
					k = ct
					if len(k) > 10 {
						k = k[:10]
					}
				}
				if b.Unbond[k] == nil {
					b.Unbond[k] = new(big.Int)
				}
				b.Unbond[k].Add(b.Unbond[k], v)
			}
		}
	}
	return b, nil
}
