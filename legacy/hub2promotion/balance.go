package hub2promotion

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"sort"
	"strings"
)

// BalanceComparison contains no account identifiers or amounts. Missing and
// extra count accounts; denomination differences include absent/extra coins.
type BalanceComparison struct {
	AccountsChecked         int    `json:"accounts_checked"`
	Missing                 int    `json:"missing"`
	Extra                   int    `json:"extra"`
	MismatchedDenominations int    `json:"mismatched_denominations"`
	ExportHash              string `json:"export_hash"`
	GenesisHash             string `json:"genesis_hash"`
	Pass                    bool   `json:"pass"`
}

// decodeUnique rejects duplicate keys at every nesting level, not merely the
// final decoded account map. It never includes private input in an error.
func decodeUnique(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var value func() (any, error)
	value = func() (any, error) {
		tok, e := d.Token()
		if e != nil {
			return nil, errors.New("invalid JSON")
		}
		switch tok {
		case json.Delim('{'):
			out := map[string]any{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, errors.New("invalid JSON key")
				}
				s, ok := k.(string)
				if !ok {
					return nil, errors.New("invalid JSON key")
				}
				if _, ok = out[s]; ok {
					return nil, errors.New("duplicate JSON key")
				}
				out[s], e = value()
				if e != nil {
					return nil, e
				}
			}
			_, e = d.Token()
			return out, e
		case json.Delim('['):
			out := []any{}
			for d.More() {
				v, e := value()
				if e != nil {
					return nil, e
				}
				out = append(out, v)
			}
			_, e = d.Token()
			return out, e
		default:
			return tok, nil
		}
	}
	v, e := value()
	if e != nil {
		return nil, e
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	return v, nil
}

type accountCoins map[string]map[string]string

// The accepted Hub1/2 export and next genesis formats keep the authoritative
// complete account list in app_state.accounts. No uatom-only SQL is consulted.
// Unknown alternate account layouts fail, rather than silently dropping them.
func readAllAccountCoins(p Pin) (accountCoins, map[string]any, error) {
	b, e := readPinnedBytes(p, 256<<20)
	if e != nil {
		return nil, nil, e
	}
	v, e := decodeUnique(b)
	if e != nil {
		return nil, nil, e
	}
	root, ok := v.(map[string]any)
	if !ok {
		return nil, nil, errors.New("genesis object required")
	}
	state, ok := root["app_state"].(map[string]any)
	if !ok {
		return nil, nil, errors.New("app_state object required")
	}
	raw, ok := state["accounts"].([]any)
	if !ok || len(raw) == 0 {
		return nil, nil, errors.New("complete old-SDK account list required")
	}
	out := accountCoins{}
	for _, item := range raw {
		a, ok := item.(map[string]any)
		if !ok {
			return nil, nil, errors.New("account object required")
		}
		address, ok := a["address"].(string)
		if !ok || address == "" {
			return nil, nil, errors.New("account identity missing")
		}
		if _, ok = out[address]; ok {
			return nil, nil, errors.New("duplicate account")
		}
		coins := map[string]string{}
		rawCoins, exists := a["coins"]
		if !exists {
			return nil, nil, errors.New("account coins missing")
		}
		if rawCoins != nil {
			cs, ok := rawCoins.([]any)
			if !ok {
				return nil, nil, errors.New("coin list required")
			}
			for _, item := range cs {
				c, ok := item.(map[string]any)
				if !ok {
					return nil, nil, errors.New("coin object required")
				}
				denom, ok := c["denom"].(string)
				if !ok || denom == "" {
					return nil, nil, errors.New("coin denomination missing")
				}
				amount, ok := c["amount"].(string)
				if !ok || amount == "" || strings.Trim(amount, "0123456789") != "" {
					return nil, nil, errors.New("nonnegative integer coin required")
				}
				n, ok := new(big.Int).SetString(amount, 10)
				if !ok || n.Sign() < 0 {
					return nil, nil, errors.New("invalid integer coin")
				}
				if _, ok = coins[denom]; ok {
					return nil, nil, errors.New("duplicate denomination")
				}
				coins[denom] = n.String()
			}
		}
		out[address] = coins
	}
	return out, root, nil
}
func accountHash(a accountCoins) string {
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		b, _ := json.Marshal([]any{k, a[k]})
		h.Write(b)
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}
func CompareAllAccounts(export, nextGenesis Pin) (BalanceComparison, error) {
	a, _, e := readAllAccountCoins(export)
	if e != nil {
		return BalanceComparison{}, e
	}
	b, _, e := readAllAccountCoins(nextGenesis)
	if e != nil {
		return BalanceComparison{}, e
	}
	r := BalanceComparison{ExportHash: accountHash(a), GenesisHash: accountHash(b)}
	for address, coins := range a {
		r.AccountsChecked++
		next, ok := b[address]
		if !ok {
			r.Missing++
			continue
		}
		for denom, amount := range coins {
			other, ok := next[denom]
			if !ok || amount != other {
				r.MismatchedDenominations++
			}
		}
		for denom := range next {
			if _, ok := coins[denom]; !ok {
				r.MismatchedDenominations++
			}
		}
	}
	for address := range b {
		if _, ok := a[address]; !ok {
			r.Extra++
		}
	}
	r.Pass = r.Missing == 0 && r.Extra == 0 && r.MismatchedDenominations == 0 && r.ExportHash == r.GenesisHash
	return r, nil
}
