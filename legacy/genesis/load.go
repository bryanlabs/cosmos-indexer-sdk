package genesis

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/legacy/schema"
	"gorm.io/gorm"
)

// Counts is how many rows of each kind a file holds or the database stores.
type Counts struct {
	Accounts, Delegations, Unbondings, Validators, Withdraws, GenesisTxs int
}

// Counts returns the row counts of a parsed file.
func (f *File) Counts() Counts {
	return Counts{len(f.Accounts), len(f.Delegations), len(f.Unbondings), len(f.Validators), len(f.Withdraws), f.GenesisTxs}
}

// Result says what Load did. Stored is counted from the database after the
// load, not taken from the file.
type Result struct {
	// AlreadyLoaded is true when the same file was loaded before and nothing
	// was written.
	AlreadyLoaded bool
	File, Stored  Counts
}

func fixed(n *big.Int) string {
	whole, part := new(big.Int).QuoRem(n, one, new(big.Int))
	return fmt.Sprintf("%s.%018s", whole, part)
}

// table is one legacy table with the rows the file gives it. Each row is
// kept twice: as a JSON object to insert, and as the text the same row must
// read back as.
type table struct {
	name    string
	columns string
	record  string
	text    string
	rows    []map[string]any
	lines   []string
}

func (t *table) add(row map[string]any, line ...string) {
	t.rows = append(t.rows, row)
	t.lines = append(t.lines, strings.Join(line, "|"))
}

func tables(f *File) []*table {
	const stamp = "2006-01-02T15:04:05.000000"
	b := func(v bool) string {
		if v {
			return "true"
		}
		return "false"
	}
	accounts := &table{name: "genesis_accounts",
		columns: "address, file_position, account_number, sequence, kind, module_name, file_coins, liquid, delegated, unbonding, original_vesting, delegated_free, delegated_vesting, vesting_start, vesting_end",
		record:  "address text, file_position integer, account_number bigint, sequence bigint, kind text, module_name text, file_coins numeric, liquid numeric, delegated numeric, unbonding numeric, original_vesting numeric, delegated_free numeric, delegated_vesting numeric, vesting_start bigint, vesting_end bigint",
		text:    "concat_ws('|', address, file_position, account_number, sequence, kind, module_name, file_coins, liquid, delegated, unbonding, original_vesting, delegated_free, delegated_vesting, vesting_start, vesting_end)"}
	for _, a := range f.Accounts {
		accounts.add(map[string]any{"address": a.Address, "file_position": a.Position, "account_number": a.Number, "sequence": a.Sequence,
			"kind": a.Kind, "module_name": a.Module, "file_coins": a.FileCoins.String(), "liquid": a.Liquid.String(),
			"delegated": a.Delegated.String(), "unbonding": a.Unbonding.String(), "original_vesting": a.OriginalVesting.String(),
			"delegated_free": a.DelegatedFree.String(), "delegated_vesting": a.DelegatedVesting.String(),
			"vesting_start": a.VestingStart, "vesting_end": a.VestingEnd},
			a.Address, fmt.Sprint(a.Position), fmt.Sprint(a.Number), fmt.Sprint(a.Sequence), a.Kind, a.Module, a.FileCoins.String(),
			a.Liquid.String(), a.Delegated.String(), a.Unbonding.String(), a.OriginalVesting.String(), a.DelegatedFree.String(),
			a.DelegatedVesting.String(), fmt.Sprint(a.VestingStart), fmt.Sprint(a.VestingEnd))
	}
	delegations := &table{name: "genesis_delegations",
		columns: "delegator, validator, shares, tokens, from_genesis_tx",
		record:  "delegator text, validator text, shares numeric, tokens numeric, from_genesis_tx boolean",
		text:    "concat_ws('|', delegator, validator, shares, tokens, from_genesis_tx::text)"}
	for _, d := range f.Delegations {
		delegations.add(map[string]any{"delegator": d.Delegator, "validator": d.Validator, "shares": fixed(d.Shares),
			"tokens": d.Tokens.String(), "from_genesis_tx": d.FromGenesisTx},
			d.Delegator, d.Validator, fixed(d.Shares), d.Tokens.String(), b(d.FromGenesisTx))
	}
	unbondings := &table{name: "genesis_unbondings",
		columns: "delegator, validator, entry, creation_height, completion_time, initial_balance, balance",
		record:  "delegator text, validator text, entry integer, creation_height bigint, completion_time timestamptz, initial_balance numeric, balance numeric",
		text:    `concat_ws('|', delegator, validator, entry, creation_height, to_char(completion_time AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US'), initial_balance, balance)`}
	for _, u := range f.Unbondings {
		// Postgres keeps microseconds.
		when := u.CompletionTime.UTC().Truncate(time.Microsecond)
		unbondings.add(map[string]any{"delegator": u.Delegator, "validator": u.Validator, "entry": u.Entry,
			"creation_height": u.CreationHeight, "completion_time": when.Format(stamp) + "Z",
			"initial_balance": u.InitialBalance.String(), "balance": u.Balance.String()},
			u.Delegator, u.Validator, fmt.Sprint(u.Entry), fmt.Sprint(u.CreationHeight), when.Format(stamp),
			u.InitialBalance.String(), u.Balance.String())
	}
	validators := &table{name: "genesis_validators",
		columns: "operator, tokens, delegator_shares, status, jailed, outstanding_rewards, accumulated_commission",
		record:  "operator text, tokens numeric, delegator_shares numeric, status text, jailed boolean, outstanding_rewards numeric, accumulated_commission numeric",
		text:    "concat_ws('|', operator, tokens, delegator_shares, status, jailed::text, outstanding_rewards, accumulated_commission)"}
	for _, v := range f.Validators {
		validators.add(map[string]any{"operator": v.Operator, "tokens": v.Tokens.String(), "delegator_shares": fixed(v.Shares),
			"status": v.Status, "jailed": v.Jailed, "outstanding_rewards": fixed(v.OutstandingRewards),
			"accumulated_commission": fixed(v.AccumulatedCommission)},
			v.Operator, v.Tokens.String(), fixed(v.Shares), v.Status, b(v.Jailed), fixed(v.OutstandingRewards), fixed(v.AccumulatedCommission))
	}
	withdraws := &table{name: "genesis_withdraw_addresses",
		columns: "delegator, withdraw_address",
		record:  "delegator text, withdraw_address text",
		text:    "concat_ws('|', delegator, withdraw_address)"}
	for _, w := range f.Withdraws {
		withdraws.add(map[string]any{"delegator": w.Delegator, "withdraw_address": w.Address}, w.Delegator, w.Address)
	}
	return []*table{accounts, delegations, unbondings, validators, withdraws}
}

func digest(lines []string) string {
	sorted := append([]string(nil), lines...)
	sort.Strings(sorted)
	sum := sha256.New()
	for _, line := range sorted {
		sum.Write([]byte(line))
		sum.Write([]byte{'\n'})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// verify reads every stored row of the chain back and compares it, field
// for field, with the parsed file.
func verify(tx *gorm.DB, f *File, all []*table) (Counts, error) {
	var stored Counts
	var header struct {
		Sha256                                                                             string
		GenesisTime                                                                        time.Time
		Accounts, Delegations, UnbondingEntries, Validators, WithdrawAddresses, GenesisTxs int
	}
	if err := tx.Raw(`SELECT sha256, genesis_time, accounts, delegations, unbonding_entries, validators, withdraw_addresses, genesis_txs
		FROM legacy.genesis_files WHERE chain_id = ?`, f.Chain).Scan(&header).Error; err != nil {
		return stored, err
	}
	want := f.Counts()
	if header.Sha256 != f.SHA256 || !header.GenesisTime.Equal(f.Time.Truncate(time.Microsecond)) || (Counts{header.Accounts, header.Delegations, header.UnbondingEntries, header.Validators,
		header.WithdrawAddresses, header.GenesisTxs}) != want {
		return stored, fmt.Errorf("the database holds another genesis file for this chain")
	}
	counts := []*int{&stored.Accounts, &stored.Delegations, &stored.Unbondings, &stored.Validators, &stored.Withdraws}
	for n, t := range all {
		var lines []string
		if err := tx.Raw("SELECT "+t.text+" FROM legacy."+t.name+" WHERE chain_id = ?", f.Chain).Scan(&lines).Error; err != nil {
			return stored, err
		}
		*counts[n] = len(lines)
		if len(lines) != len(t.lines) || digest(lines) != digest(t.lines) {
			return stored, fmt.Errorf("stored rows of %s differ from the genesis file", t.name)
		}
	}
	stored.GenesisTxs = header.GenesisTxs
	return stored, nil
}

// Load stores one parsed genesis file in the legacy schema, in a single
// database transaction, then reads every row back and compares it with the
// file. Loading the same file again writes nothing and repeats the
// comparison. A different file for a chain already loaded is refused.
//
// It refuses to run unless the connection is confined to the legacy schema.
func Load(db *gorm.DB, f *File) (Result, error) {
	result := Result{File: f.Counts()}
	findings, err := schema.Confined(db)
	if err != nil {
		return result, err
	}
	if len(findings) > 0 {
		return result, fmt.Errorf("the database role is not confined to the legacy schema: %s", strings.Join(findings, "; "))
	}
	all := tables(f)
	err = db.Transaction(func(tx *gorm.DB) error {
		var loaded int64
		if err := tx.Raw(`SELECT count(*) FROM legacy.genesis_files WHERE chain_id = ?`, f.Chain).Scan(&loaded).Error; err != nil {
			return err
		}
		if loaded == 0 {
			c := result.File
			if err := tx.Exec(`INSERT INTO legacy.genesis_files
				(chain_id, sha256, genesis_time, accounts, delegations, unbonding_entries, validators, withdraw_addresses, genesis_txs)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, f.Chain, f.SHA256, f.Time, c.Accounts, c.Delegations, c.Unbondings,
				c.Validators, c.Withdraws, c.GenesisTxs).Error; err != nil {
				return err
			}
			for _, t := range all {
				const batch = 10000
				for start := 0; start < len(t.rows); start += batch {
					end := min(start+batch, len(t.rows))
					payload, err := json.Marshal(t.rows[start:end])
					if err != nil {
						return err
					}
					if err := tx.Exec("INSERT INTO legacy."+t.name+" (chain_id, "+t.columns+") SELECT ?, "+t.columns+
						" FROM jsonb_to_recordset(CAST(? AS jsonb)) AS x("+t.record+")", f.Chain, string(payload)).Error; err != nil {
						return err
					}
				}
			}
		} else {
			result.AlreadyLoaded = true
		}
		result.Stored, err = verify(tx, f, all)
		return err
	})
	return result, err
}
