// Package books checks that a legacy chain's stored history explains the
// balances the next chain started with. For every address it takes the
// spendable balance in the chain's own genesis file, applies every stored
// coin movement and every charged fee up to the export height, and compares
// the result with the spendable balance in the next chain's genesis file.
//
// It reads only the legacy schema and prints nothing itself. It writes
// legacy.reconciliation, and once, when a close accepted a genesis change in
// full, the rows of that change in legacy.taxable_events.
package books

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
)

// Component is the name the spendable balance check has in
// legacy.reconciliation.
const Component = "liquid"

const denom = "uatom"

// One coin of a stored fee: a whole amount and its denomination.
var feeCoin = regexp.MustCompile(`^([0-9]{1,78})([a-z][a-z0-9/]{0,127})?$`)

// What a stored row does to the spendable balance of the address it was
// paid from and of the address it was paid to. Bonding, unbonding and moving
// stake change no spendable balance until the unbonding completes.
var liquidRule = map[string][2]int{
	"transfer":           {-1, 1},
	"reward":             {0, 1},
	"commission":         {0, 1},
	"income":             {0, 1},
	"delegate":           {-1, 0},
	"undelegate":         {0, 0},
	"redelegate":         {0, 0},
	"gov_deposit":        {-1, 0},
	"gov_deposit_refund": {0, 1},
	"unbonding_complete": {0, 1},
}

// Result is the outcome of a close. Lines hold addresses and are for
// recording only; everything else is a count or a total.
type Result struct {
	Chain, Next string
	// Through is the export height: the last height the next chain's genesis
	// file reflects.
	Through int64
	// HeightsStored counts the stored heights up to Through. The close is
	// complete only when every one is stored.
	HeightsStored int64
	Complete      bool
	Rows, Fees    int64
	// OtherDenomRows counts stored rows and fees in a denomination other
	// than uatom. They are outside this check.
	OtherDenomRows int64
	ModuleAccounts int
	Addresses      int
	Closed, Open   int
	// Short is the total the next genesis file holds above what the history
	// explains, over the addresses that hold more. Over is the total it
	// holds below, over the addresses that hold less.
	Short, Over *big.Int
	// OpenByCause groups the addresses that do not close by which side holds
	// more and by what kinds of movement touched the address.
	OpenByCause map[string]int
	// Touched and TouchedOpen count, per kind of movement, the addresses it
	// touched and how many of those do not close.
	Touched, TouchedOpen map[string]int
	// OpenBySize groups the addresses that do not close by the size of the
	// difference in uatom.
	OpenBySize map[string]int
	Lines      []Line

	// Change is the genesis change the close was given, if any. The fields
	// below are all zero without one.
	Change *GenesisChange
	// ClosedWithChange counts the addresses that close only with the genesis
	// change. They are in neither Closed nor Open, and in neither total.
	ClosedWithChange int
	// ChangeSources and ChangeRecoveries count the listed wallets of each
	// side that pass their own test. ChangePoolExact says the remainder is
	// the recorded community pool change. ChangeWhole says the list was
	// accepted: every wallet passed, no source held less than its recovery
	// was given, and the remainder is exact.
	ChangeSources, ChangeRecoveries int
	ChangePoolExact, ChangeWhole    bool
	// ChangeOut is what the history gives the source wallets that pass,
	// ChangeIn what the recovery wallets that pass were given, ChangePool
	// the rest.
	ChangeOut, ChangeIn, ChangePool *big.Int
	// ChangeRows counts the stored rows of the genesis change. They move no
	// balance in this check.
	ChangeRows          int64
	changeSourceBalance map[string]*big.Int
}

// Line is one address of the close.
type Line struct {
	Address            string
	Expected, Computed *big.Int
	// Status is closed, open, or StatusChange.
	Status string
}

const (
	statusClosed = "closed"
	statusOpen   = "open"
)

// listedWallet is a wallet of the genesis change as the close found it.
type listedWallet struct {
	address    string
	line       int
	account    *account
	kinds      []string
	difference *big.Int
	entry      *Recovery
	source     bool
}

type account struct {
	opening, closing *big.Int
	balance          *big.Int
	module           bool
	inOpening        bool
	inClosing        bool
	vesting          bool
	touched          map[string]bool
}

type book map[string]*account

func (b book) at(address string) *account {
	a := b[address]
	if a == nil {
		a = &account{opening: new(big.Int), closing: new(big.Int), balance: new(big.Int), touched: map[string]bool{}}
		b[address] = a
	}
	return a
}

func (b book) move(address string, sign int, amount *big.Int, kind string) {
	if address == "" || sign == 0 {
		return
	}
	a := b.at(address)
	if sign > 0 {
		a.balance.Add(a.balance, amount)
		a.touched[kind+" in"] = true
	} else {
		a.balance.Sub(a.balance, amount)
		a.touched[kind+" out"] = true
	}
}

// Close runs the check for one chain against the genesis file of the chain
// after it. It supports a chain whose state export paid nothing into
// balances; one whose export paid out pending rewards needs those payout
// rows stored first.
func Close(ctx context.Context, db *sql.DB, chain string) (*Result, error) {
	return CloseWith(ctx, db, chain, nil)
}

// CloseWith is Close with the balances the next genesis file changed in
// code. The list is taken whole or not at all. It is accepted only when
// every source wallet is empty in the next genesis and has a balance in the
// history, every recovery wallet holds in the next genesis exactly the
// listed amount more than the history gives, no source held less than its
// recovery was given, and the source balances less the listed amounts come
// to exactly the community pool remainder recorded with the list. Accepted,
// its wallets are counted as closing with the change, never as closing
// exactly. Refused, the result is the one a close without the list gives.
// No wallet outside the list is affected either way.
func CloseWith(ctx context.Context, db *sql.DB, chain string, change *GenesisChange) (*Result, error) {
	if change != nil && change.Chain != chain {
		return nil, fmt.Errorf("the genesis change belongs to another chain")
	}
	out := &Result{Chain: chain, Short: new(big.Int), Over: new(big.Int), OpenByCause: map[string]int{},
		Touched: map[string]int{}, TouchedOpen: map[string]int{}, OpenBySize: map[string]int{},
		Change: change, ChangeOut: new(big.Int), ChangeIn: new(big.Int), ChangePool: new(big.Int), changeSourceBalance: map[string]*big.Int{}}
	var era int
	var through sql.NullInt64
	var paid bool
	if err := db.QueryRowContext(ctx, `SELECT era, export_height, export_paid_rewards FROM legacy.eras WHERE chain_id = $1`, chain).
		Scan(&era, &through, &paid); err != nil {
		return nil, fmt.Errorf("the chain is not one of the legacy ledger")
	}
	if !through.Valid {
		return nil, fmt.Errorf("the chain has no export height")
	}
	if paid {
		return nil, fmt.Errorf("the export of this chain paid pending rewards into balances, so its close needs those payout rows first")
	}
	out.Through = through.Int64
	if err := db.QueryRowContext(ctx, `SELECT chain_id FROM legacy.eras WHERE era = $1`, era+1).Scan(&out.Next); err != nil {
		return nil, fmt.Errorf("the chain after this one is not known")
	}
	var loaded int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM legacy.genesis_files WHERE chain_id IN ($1, $2)`, chain, out.Next).Scan(&loaded); err != nil {
		return nil, err
	}
	if loaded != 2 {
		return nil, fmt.Errorf("the genesis files of the chain and of the chain after it must both be loaded")
	}
	if change != nil {
		var next string
		if err := db.QueryRowContext(ctx, `SELECT sha256 FROM legacy.genesis_files WHERE chain_id = $1`, out.Next).Scan(&next); err != nil {
			return nil, err
		}
		if next != change.NextGenesisSHA256 || change.pool == nil {
			return nil, fmt.Errorf("the genesis file loaded for the chain after this one is not the one the genesis change was proven against")
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM legacy.blocks WHERE chain_id = $1 AND height <= $2`, chain, out.Through).
		Scan(&out.HeightsStored); err != nil {
		return nil, err
	}
	var first int64
	if err := db.QueryRowContext(ctx, `SELECT first_height FROM legacy.eras WHERE chain_id = $1`, chain).Scan(&first); err != nil {
		return nil, err
	}
	out.Complete = out.HeightsStored == out.Through-first+1

	accounts := book{}
	for _, side := range []string{chain, out.Next} {
		rows, err := db.QueryContext(ctx, `SELECT address, kind, liquid::text FROM legacy.genesis_accounts WHERE chain_id = $1`, side)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var address, kind, liquid string
			if err := rows.Scan(&address, &kind, &liquid); err != nil {
				rows.Close()
				return nil, err
			}
			amount, ok := new(big.Int).SetString(liquid, 10)
			if !ok {
				rows.Close()
				return nil, fmt.Errorf("a genesis balance is not a whole number")
			}
			a := accounts.at(address)
			a.module = a.module || kind == "module"
			a.vesting = a.vesting || strings.HasSuffix(kind, "_vesting")
			if side == chain {
				a.opening, a.inOpening = amount, true
				a.balance.Add(a.balance, amount)
			} else {
				a.closing, a.inClosing = amount, true
			}
		}
		if err := finish(rows); err != nil {
			return nil, err
		}
	}

	rows, err := db.QueryContext(ctx, `SELECT category, amount, denom, from_addr, to_addr, detail FROM legacy.taxable_events
WHERE chain_id = $1 AND height <= $2`, chain, out.Through)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var category, amount, unit, from, to, detail string
		if err := rows.Scan(&category, &amount, &unit, &from, &to, &detail); err != nil {
			rows.Close()
			return nil, err
		}
		// The rows of the genesis change record what the next genesis did.
		// They move nothing here, so they can never make a wallet close.
		if category == Prop29Category {
			out.ChangeRows++
			continue
		}
		rule, known := liquidRule[category]
		if !known {
			rows.Close()
			return nil, fmt.Errorf("a stored row has a category this check has no rule for")
		}
		if unit != denom {
			out.OtherDenomRows++
			continue
		}
		value, ok := new(big.Int).SetString(amount, 10)
		if !ok {
			rows.Close()
			return nil, fmt.Errorf("a stored amount is not a whole number")
		}
		out.Rows++
		// A reward names the delegator who earned it. The coins went to the
		// address the row records as paid, when it records one.
		if category == "reward" && detail != "" {
			var paid struct {
				To string `json:"paid_to"`
			}
			if json.Unmarshal([]byte(detail), &paid) != nil || paid.To == "" {
				rows.Close()
				return nil, fmt.Errorf("a reward row carries a detail that names no paid address")
			}
			accounts.at(to).touched["reward paid to another address"] = true
			accounts.move(paid.To, 1, value, "reward of another address")
			continue
		}
		accounts.move(from, rule[0], value, category)
		accounts.move(to, rule[1], value, category)
	}
	if err := finish(rows); err != nil {
		return nil, err
	}

	fees, err := db.QueryContext(ctx, `SELECT fee, fee_payer, count(*) FROM legacy.tx_inclusions
WHERE chain_id = $1 AND height <= $2 AND fee_charged AND fee <> '' GROUP BY fee, fee_payer`, chain, out.Through)
	if err != nil {
		return nil, err
	}
	for fees.Next() {
		var fee, payer string
		var times int64
		if err := fees.Scan(&fee, &payer, &times); err != nil {
			fees.Close()
			return nil, err
		}
		for _, part := range strings.Split(fee, ",") {
			found := feeCoin.FindStringSubmatch(part)
			if found == nil {
				fees.Close()
				return nil, fmt.Errorf("a stored fee is not readable")
			}
			amount, _ := new(big.Int).SetString(found[1], 10)
			switch {
			case amount.Sign() == 0:
				// Hub 3 accepted a fee of zero with no denomination.
			case found[2] != denom:
				out.OtherDenomRows += times
			default:
				out.Fees += times
				accounts.move(payer, -1, amount.Mul(amount, big.NewInt(times)), "fee")
			}
		}
	}
	if err := finish(fees); err != nil {
		return nil, err
	}

	addresses := make([]string, 0, len(accounts))
	for address := range accounts {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	open := func(kinds []string, difference *big.Int) {
		out.Open++
		side := "the next genesis holds more"
		if difference.Sign() < 0 {
			side = "the next genesis holds less"
			out.Over.Sub(out.Over, difference)
		} else {
			out.Short.Add(out.Short, difference)
		}
		for _, kind := range kinds {
			out.TouchedOpen[kind]++
		}
		out.OpenByCause[side+": "+strings.Join(kinds, ", ")]++
		out.OpenBySize[size(difference)]++
	}
	var listed []listedWallet
	for _, address := range addresses {
		a := accounts[address]
		if a.module {
			out.ModuleAccounts++
			continue
		}
		out.Addresses++
		out.Lines = append(out.Lines, Line{Address: address, Expected: a.closing, Computed: a.balance, Status: statusOpen})
		line := &out.Lines[len(out.Lines)-1]
		kinds := make([]string, 0, len(a.touched)+3)
		for kind := range a.touched {
			kinds = append(kinds, kind)
		}
		if !a.inOpening {
			kinds = append(kinds, "not in the opening genesis")
		}
		if !a.inClosing {
			kinds = append(kinds, "not in the closing genesis")
		}
		if a.vesting {
			kinds = append(kinds, "vesting account")
		}
		if len(a.touched) == 0 {
			kinds = append(kinds, "no stored movement")
		}
		sort.Strings(kinds)
		for _, kind := range kinds {
			out.Touched[kind]++
		}
		difference := new(big.Int).Sub(a.closing, a.balance)
		entry, source := change.listed(address)
		if entry != nil {
			listed = append(listed, listedWallet{address: address, line: len(out.Lines) - 1, account: a, kinds: kinds,
				difference: difference, entry: entry, source: source})
		}
		if difference.Sign() == 0 {
			out.Closed++
			line.Status = statusClosed
			continue
		}
		// A listed wallet is decided below, once every one has been read.
		if entry != nil {
			continue
		}
		open(kinds, difference)
	}
	if change != nil {
		held := map[string]*big.Int{}
		for _, w := range listed {
			switch {
			case w.source && w.account.closing.Sign() == 0 && w.account.balance.Sign() > 0:
				out.ChangeSources++
				out.ChangeOut.Add(out.ChangeOut, w.account.balance)
				held[w.address] = w.account.balance
			case !w.source && w.difference.Cmp(w.entry.Amount) == 0:
				out.ChangeRecoveries++
				out.ChangeIn.Add(out.ChangeIn, w.entry.Amount)
			}
		}
		out.ChangePool.Sub(out.ChangeOut, out.ChangeIn)
		out.ChangePoolExact = out.ChangePool.Cmp(change.pool) == 0
		out.ChangeWhole = out.ChangeSources == change.Listed() && out.ChangeRecoveries == change.Listed() && out.ChangePoolExact
		for _, entry := range change.recoveries {
			if whole := held[entry.Source]; whole != nil && whole.Cmp(entry.Amount) < 0 {
				out.ChangeWhole = false
			}
		}
		// The list is taken whole or not at all. Refused, every listed
		// wallet is counted exactly as a close without the list counts it.
		for _, w := range listed {
			switch {
			case w.difference.Sign() == 0:
			case !out.ChangeWhole:
				open(w.kinds, w.difference)
			default:
				out.Lines[w.line].Status = StatusChange
				out.ClosedWithChange++
				if w.source {
					out.changeSourceBalance[w.address] = new(big.Int).Set(w.account.balance)
				}
			}
		}
	}
	return out, nil
}

func size(difference *big.Int) string {
	magnitude := new(big.Int).Abs(difference)
	for _, step := range []struct {
		below int64
		label string
	}{{10, "under 10 uatom"}, {1000, "10 to 999 uatom"}, {1000000, "1,000 uatom to under 1 atom"}, {1000000000, "1 to under 1,000 atom"}} {
		if magnitude.Cmp(big.NewInt(step.below)) < 0 {
			return step.label
		}
	}
	return "1,000 atom or more"
}

func finish(rows *sql.Rows) error {
	err := rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	return err
}

// Record replaces the chain's lines of this check in legacy.reconciliation
// with the result: one line per address, closed or open. It refuses a close
// that did not cover every height, because its open addresses would mean
// nothing.
func Record(ctx context.Context, db *sql.DB, result *Result) error {
	if result == nil || !result.Complete {
		return fmt.Errorf("a close is recorded only when every height up to the export height is stored")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM legacy.reconciliation WHERE chain_id = $1 AND component = $2`, result.Chain, Component); err != nil {
		return err
	}
	const batch = 5000
	for start := 0; start < len(result.Lines); start += batch {
		end := start + batch
		if end > len(result.Lines) {
			end = len(result.Lines)
		}
		addresses, expected, computed, statuses := make([]string, 0, batch), make([]string, 0, batch), make([]string, 0, batch), make([]string, 0, batch)
		for _, line := range result.Lines[start:end] {
			equal := line.Expected.Cmp(line.Computed) == 0
			if (line.Status == statusClosed) != equal || (line.Status != statusClosed && line.Status != statusOpen && line.Status != StatusChange) {
				return fmt.Errorf("a line of the close carries a status its amounts do not support")
			}
			addresses = append(addresses, line.Address)
			expected = append(expected, line.Expected.String())
			computed = append(computed, line.Computed.String())
			statuses = append(statuses, line.Status)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO legacy.reconciliation (chain_id, address, component, expected, computed, difference, status)
SELECT $1, a, $2, e::numeric, c::numeric, e::numeric - c::numeric, s
FROM unnest(string_to_array($3, ','), string_to_array($4, ','), string_to_array($5, ','), string_to_array($6, ',')) AS t(a, e, c, s)`,
			result.Chain, Component, strings.Join(addresses, ","), strings.Join(expected, ","), strings.Join(computed, ","), strings.Join(statuses, ",")); err != nil {
			return err
		}
	}
	return tx.Commit()
}
