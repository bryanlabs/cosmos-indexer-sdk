// Package sequence checks a Cosmos Hub 1 to 3 index against account sequence
// numbers, and settles failed transactions whose fee charge the block results
// cannot prove.
//
// In all three eras the ante handler raises the sequence of every signer by
// one and deducts the fee in the same step, and that step is kept exactly when
// the ante handler finishes. So an account's sequence counts the transactions
// it signed that paid their fee. Each hub's genesis file records every
// account's sequence at the start, and the next hub's genesis file records it
// at the end. For every account:
//
//	closing - opening - genesis transactions = transactions that paid a fee
//
// An account with no unproven failure must match exactly, which proves no
// fee paying transaction of that account is missing or counted twice. For an
// account with unproven failures the difference says how many of them paid.
// When that is none or all of them, each one is settled.
package sequence

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
)

// Genesis is what one hub's genesis file says about account sequences.
type Genesis struct {
	Chain string
	// Sequence is each account's sequence number as written in the file.
	Sequence map[string]uint64
	// GenesisSigned counts the genesis transactions an account signed. They
	// ran before height 1 and each raised its signer's sequence by one.
	GenesisSigned map[string]uint64
}

// ReadGenesis reads the account sequences of a cosmoshub-1, 2, 3 or 4
// genesis file.
func ReadGenesis(r io.Reader) (*Genesis, error) {
	var doc struct {
		ChainID  string `json:"chain_id"`
		AppState struct {
			Accounts []map[string]json.RawMessage `json:"accounts"`
			Auth     struct {
				Accounts []map[string]json.RawMessage `json:"accounts"`
			} `json:"auth"`
			Gentxs []struct {
				Value struct {
					Msg []struct {
						Type  string `json:"type"`
						Value struct {
							Delegator string `json:"delegator_address"`
						} `json:"value"`
					} `json:"msg"`
					Signatures []json.RawMessage `json:"signatures"`
				} `json:"value"`
			} `json:"gentxs"`
		} `json:"app_state"`
	}
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("genesis JSON invalid")
	}
	accounts := doc.AppState.Accounts
	if len(doc.AppState.Auth.Accounts) > 0 {
		if len(accounts) > 0 {
			return nil, fmt.Errorf("genesis lists accounts in two places")
		}
		accounts = doc.AppState.Auth.Accounts
	}
	if doc.ChainID == "" || len(accounts) == 0 {
		return nil, fmt.Errorf("genesis has no chain id or no accounts")
	}
	g := &Genesis{Chain: doc.ChainID, Sequence: make(map[string]uint64, len(accounts)), GenesisSigned: map[string]uint64{}}
	for _, account := range accounts {
		// Vesting and module accounts nest the base account one or two levels down.
		for depth := 0; depth < 3; depth++ {
			inner, ok := account["base_vesting_account"]
			if !ok {
				if inner, ok = account["base_account"]; !ok {
					break
				}
			}
			account = nil
			if json.Unmarshal(inner, &account) != nil {
				return nil, fmt.Errorf("genesis account invalid")
			}
		}
		var address, text string
		field := account["sequence_number"]
		if field == nil {
			field = account["sequence"]
		}
		if json.Unmarshal(account["address"], &address) != nil || address == "" || json.Unmarshal(field, &text) != nil {
			return nil, fmt.Errorf("genesis account has no address or sequence")
		}
		sequence, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("genesis account sequence is not a number")
		}
		if _, twice := g.Sequence[address]; twice {
			return nil, fmt.Errorf("genesis lists an account twice")
		}
		g.Sequence[address] = sequence
	}
	for _, tx := range doc.AppState.Gentxs {
		if len(tx.Value.Msg) != 1 || tx.Value.Msg[0].Type != "cosmos-sdk/MsgCreateValidator" || tx.Value.Msg[0].Value.Delegator == "" || len(tx.Value.Signatures) != 1 {
			return nil, fmt.Errorf("genesis transaction is not one signed validator creation")
		}
		g.GenesisSigned[tx.Value.Msg[0].Value.Delegator]++
	}
	return g, nil
}

// Tally counts, per signer, the transactions of a height range by what is
// known about their fee.
type Tally struct {
	// Charged counts transactions known to have paid their fee.
	Charged map[string]uint64
	// Unknown lists the inclusion keys of failures whose fee is unproven.
	Unknown map[string][]string
	// Transactions counts what was added, by fee charge.
	Transactions map[string]uint64
}

func NewTally() *Tally {
	return &Tally{Charged: map[string]uint64{}, Unknown: map[string][]string{}, Transactions: map[string]uint64{}}
}

// Add records one transaction. Signers are the distinct accounts that signed
// it; charge is "charged", "not charged" or "unknown".
func (t *Tally) Add(key string, signers []string, charge string) error {
	if len(signers) == 0 {
		return fmt.Errorf("transaction has no signer")
	}
	seen := map[string]bool{}
	for _, signer := range signers {
		if signer == "" || seen[signer] {
			return fmt.Errorf("transaction signers are not distinct")
		}
		seen[signer] = true
	}
	switch charge {
	case "charged":
		for _, signer := range signers {
			t.Charged[signer]++
		}
	case "unknown":
		for _, signer := range signers {
			t.Unknown[signer] = append(t.Unknown[signer], key)
		}
	case "not charged":
	default:
		return fmt.Errorf("fee charge not recognized")
	}
	t.Transactions[charge]++
	return nil
}

// Merge adds another tally of different transactions.
func (t *Tally) Merge(other *Tally) {
	for signer, n := range other.Charged {
		t.Charged[signer] += n
	}
	for signer, keys := range other.Unknown {
		t.Unknown[signer] = append(t.Unknown[signer], keys...)
	}
	for charge, n := range other.Transactions {
		t.Transactions[charge] += n
	}
}

// Report is the outcome of comparing a whole hub with its two genesis files.
// Every field is a count; Resolutions is keyed by chain, height and index.
type Report struct {
	// Accounts is every account in either file or the tally.
	Accounts int
	// Exact accounts have no unproven failure and match to the transaction.
	Exact int
	// Mismatched accounts have no unproven failure and do not match: the
	// index misses or double counts a fee paying transaction of theirs.
	Mismatched int
	// Settled accounts had unproven failures that all paid or all did not.
	Settled int
	// Partial accounts had some but not all of their unproven failures paid.
	Partial int
	// Impossible accounts moved by less than their proven transactions or by
	// more than proven and unproven together.
	Impossible int
	// ChargedAbsent accounts paid a fee in the tally yet are not in the
	// closing file.
	ChargedAbsent int
	// SettledCharged, SettledNotCharged and Unsettled count transactions.
	SettledCharged, SettledNotCharged, Unsettled int
	// Resolutions maps the inclusion key of each settled failure to
	// "charged" or "not charged".
	Resolutions map[string]string
}

// Reconcile compares a tally of every transaction of a hub with the hub's own
// genesis file and the next hub's. It is only meaningful when the tally
// covers every height of the hub.
func Reconcile(opening, closing *Genesis, tally *Tally) Report {
	report := Report{Resolutions: map[string]string{}}
	accounts := map[string]bool{}
	for _, set := range []map[string]uint64{opening.Sequence, opening.GenesisSigned, closing.Sequence, tally.Charged} {
		for account := range set {
			accounts[account] = true
		}
	}
	for account := range tally.Unknown {
		accounts[account] = true
	}
	verdicts := map[string]map[string]bool{}
	note := func(key, verdict string) {
		if verdicts[key] == nil {
			verdicts[key] = map[string]bool{}
		}
		verdicts[key][verdict] = true
	}
	report.Accounts = len(accounts)
	for account := range accounts {
		closed, present := closing.Sequence[account]
		proven := tally.Charged[account]
		if !present && proven > 0 {
			report.ChargedAbsent++
		}
		moved := int64(closed) - int64(opening.Sequence[account]) - int64(opening.GenesisSigned[account]) - int64(proven)
		unknown := tally.Unknown[account]
		switch {
		case len(unknown) == 0 && moved == 0:
			report.Exact++
		case len(unknown) == 0:
			report.Mismatched++
		case moved == 0 || moved == int64(len(unknown)):
			report.Settled++
			verdict := "not charged"
			if moved != 0 {
				verdict = "charged"
			}
			for _, key := range unknown {
				note(key, verdict)
			}
		default:
			if moved < 0 || moved > int64(len(unknown)) {
				report.Impossible++
			} else {
				report.Partial++
			}
			for _, key := range unknown {
				note(key, "unsettled")
			}
		}
	}
	// A failure signed by several accounts is settled only when they agree.
	for key, seen := range verdicts {
		switch {
		case len(seen) == 1 && seen["charged"]:
			report.SettledCharged++
			report.Resolutions[key] = "charged"
		case len(seen) == 1 && seen["not charged"]:
			report.SettledNotCharged++
			report.Resolutions[key] = "not charged"
		default:
			report.Unsettled++
		}
	}
	return report
}

// Ahead counts the accounts whose proven fee paying transactions in a tally
// already exceed what the two genesis files allow. A tally of part of a hub
// can never be ahead, so any account counted here shows an index or decoder
// fault. It returns the accounts checked and the accounts ahead.
func Ahead(opening, closing *Genesis, tally *Tally) (checked, ahead int) {
	for account, proven := range tally.Charged {
		checked++
		if opening.Sequence[account]+opening.GenesisSigned[account]+proven > closing.Sequence[account] {
			ahead++
		}
	}
	return checked, ahead
}

// SortedResolutions returns the settled inclusion keys in a stable order.
func (r Report) SortedResolutions() []string {
	keys := make([]string, 0, len(r.Resolutions))
	for key := range r.Resolutions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
