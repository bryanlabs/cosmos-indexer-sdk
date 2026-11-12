// Package genesis reads the opening state of cosmoshub-1, 2, 3 and 4 from
// their genesis files and loads it into the legacy schema.
//
// Each later file is the state export of the chain before it, so the four
// files are the fixed points the whole ledger is checked against: the
// opening balances of one chain plus everything indexed on it must equal
// the opening balances of the next.
package genesis

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Denom is the only coin the four files hold.
const Denom = "uatom"

var one = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

// Account is one account as the file states it, with the totals of its
// delegations and unbonding entries from the same file.
type Account struct {
	Address          string
	Position         int
	Number, Sequence uint64
	// Kind is base, continuous_vesting, delayed_vesting or module.
	Kind, Module string
	// FileCoins is the balance the file lists. Liquid is the balance at the
	// first block: FileCoins less a delegation made by a genesis transaction.
	FileCoins, Liquid, Delegated, Unbonding          *big.Int
	OriginalVesting, DelegatedFree, DelegatedVesting *big.Int
	VestingStart, VestingEnd                         int64
}

// Delegation is one delegator and validator pair. Shares is scaled by
// 10^18. Tokens is what the shares were worth at the validator's rate in
// the file, rounded down.
type Delegation struct {
	Delegator, Validator string
	Shares, Tokens       *big.Int
	FromGenesisTx        bool
}

// Unbonding is one entry of an unbonding delegation.
type Unbonding struct {
	Delegator, Validator    string
	Entry                   int
	CreationHeight          int64
	CompletionTime          time.Time
	InitialBalance, Balance *big.Int
}

// Validator is one validator. Shares and the two unpaid amounts are scaled
// by 10^18.
type Validator struct {
	Operator                                  string
	Tokens, Shares                            *big.Int
	Status                                    string
	Jailed                                    bool
	OutstandingRewards, AccumulatedCommission *big.Int
}

// Withdraw is a delegator that directed its rewards to another address.
type Withdraw struct{ Delegator, Address string }

// File is everything the loader takes from one genesis file.
type File struct {
	Chain       string
	SHA256      string
	Time        time.Time
	Accounts    []Account
	Delegations []Delegation
	Unbondings  []Unbonding
	Validators  []Validator
	Withdraws   []Withdraw
	GenesisTxs  int
	// Checks names each consistency check the file passed.
	Checks []string
}

type coin struct {
	Amount string `json:"amount"`
	Denom  string `json:"denom"`
}

type rawAccount struct {
	Type             string      `json:"@type"`
	Address          string      `json:"address"`
	Coins            []coin      `json:"coins"`
	SequenceNumber   *string     `json:"sequence_number"`
	Sequence         *string     `json:"sequence"`
	AccountNumber    string      `json:"account_number"`
	OriginalVesting  []coin      `json:"original_vesting"`
	DelegatedFree    []coin      `json:"delegated_free"`
	DelegatedVesting []coin      `json:"delegated_vesting"`
	StartTime        string      `json:"start_time"`
	EndTime          string      `json:"end_time"`
	ModuleName       string      `json:"module_name"`
	Name             string      `json:"name"`
	BaseAccount      *rawAccount `json:"base_account"`
	BaseVesting      *rawAccount `json:"base_vesting_account"`
}

type rawDistribution struct {
	Withdraws []struct {
		Delegator string `json:"delegator_address"`
		Address   string `json:"withdraw_address"`
	} `json:"delegator_withdraw_infos"`
	Outstanding []struct {
		Validator string `json:"validator_address"`
		Rewards   []coin `json:"outstanding_rewards"`
	} `json:"outstanding_rewards"`
	Commissions []struct {
		Validator   string          `json:"validator_address"`
		Accumulated json.RawMessage `json:"accumulated"`
	} `json:"validator_accumulated_commissions"`
}

type document struct {
	ChainID  string    `json:"chain_id"`
	Time     time.Time `json:"genesis_time"`
	AppState struct {
		Accounts []rawAccount `json:"accounts"`
		Auth     struct {
			Accounts []rawAccount `json:"accounts"`
		} `json:"auth"`
		Bank struct {
			Balances []struct {
				Address string `json:"address"`
				Coins   []coin `json:"coins"`
			} `json:"balances"`
			Supply []coin `json:"supply"`
		} `json:"bank"`
		Supply struct {
			Supply json.RawMessage `json:"supply"`
		} `json:"supply"`
		Staking struct {
			Validators []struct {
				Operator string          `json:"operator_address"`
				Tokens   string          `json:"tokens"`
				Shares   string          `json:"delegator_shares"`
				Status   json.RawMessage `json:"status"`
				Jailed   bool            `json:"jailed"`
			} `json:"validators"`
			Delegations []struct {
				Delegator string `json:"delegator_address"`
				Validator string `json:"validator_address"`
				Shares    string `json:"shares"`
			} `json:"delegations"`
			Unbondings []struct {
				Delegator string `json:"delegator_address"`
				Validator string `json:"validator_address"`
				Entries   []struct {
					CreationHeight string    `json:"creation_height"`
					CompletionTime time.Time `json:"completion_time"`
					InitialBalance string    `json:"initial_balance"`
					Balance        string    `json:"balance"`
				} `json:"entries"`
			} `json:"unbonding_delegations"`
		} `json:"staking"`
		Distr        *rawDistribution `json:"distr"`
		Distribution *rawDistribution `json:"distribution"`
		Gentxs       []struct {
			Value struct {
				Msg []struct {
					Type  string `json:"type"`
					Value struct {
						Delegator string `json:"delegator_address"`
						Validator string `json:"validator_address"`
						Value     coin   `json:"value"`
					} `json:"value"`
				} `json:"msg"`
			} `json:"value"`
		} `json:"gentxs"`
	} `json:"app_state"`
}

var (
	wholeNumber = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	decimal     = regexp.MustCompile(`^(0|[1-9][0-9]*)\.([0-9]{1,18})$`)
	bech32Text  = regexp.MustCompile(`^cosmos(valoper)?1[02-9ac-hj-np-z]{38,58}$`)
)

func whole(text string) (*big.Int, error) {
	if !wholeNumber.MatchString(text) {
		return nil, fmt.Errorf("genesis amount is not a whole number")
	}
	n, _ := new(big.Int).SetString(text, 10)
	return n, nil
}

// scaled reads a decimal with up to 18 places as an integer scaled by 10^18.
func scaled(text string) (*big.Int, error) {
	if wholeNumber.MatchString(text) {
		n, _ := new(big.Int).SetString(text, 10)
		return n.Mul(n, one), nil
	}
	m := decimal.FindStringSubmatch(text)
	if m == nil {
		return nil, fmt.Errorf("genesis decimal is not a number with at most 18 places")
	}
	n, _ := new(big.Int).SetString(m[1]+m[2]+strings.Repeat("0", 18-len(m[2])), 10)
	return n, nil
}

// atoms sums a coin list that may hold only uatom.
func atoms(coins []coin, parse func(string) (*big.Int, error)) (*big.Int, error) {
	total := new(big.Int)
	for _, c := range coins {
		if c.Denom != Denom {
			return nil, fmt.Errorf("genesis holds a coin that is not uatom")
		}
		n, err := parse(c.Amount)
		if err != nil {
			return nil, err
		}
		total.Add(total, n)
	}
	return total, nil
}

func seconds(text string) (int64, error) {
	if text == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("genesis vesting time is not a number")
	}
	return n, nil
}

func status(raw json.RawMessage) (string, error) {
	switch strings.Trim(string(raw), `"`) {
	case "0", "BOND_STATUS_UNBONDED":
		return "unbonded", nil
	case "1", "BOND_STATUS_UNBONDING":
		return "unbonding", nil
	case "2", "BOND_STATUS_BONDED":
		return "bonded", nil
	}
	return "", fmt.Errorf("genesis validator status not recognized")
}

// Checksum passes a file through while hashing every byte of it.
type Checksum struct {
	source io.Reader
	sum    hash.Hash
}

// NewChecksum wraps a file as stored on disk.
func NewChecksum(source io.Reader) *Checksum {
	return &Checksum{source: source, sum: sha256.New()}
}

func (c *Checksum) Read(p []byte) (int, error) {
	n, err := c.source.Read(p)
	c.sum.Write(p[:n])
	return n, err
}

// Sum reads whatever the parser left unread and returns the SHA-256 of the
// whole file in hex.
func (c *Checksum) Sum() string {
	_, _ = io.Copy(c.sum, c.source)
	return hex.EncodeToString(c.sum.Sum(nil))
}

// Read parses a cosmoshub-1, 2, 3 or 4 genesis file. It refuses a file whose
// parts disagree with each other, so what it returns has passed every check
// named in File.Checks.
func Read(r io.Reader) (*File, error) {
	digest := sha256.New()
	var doc document
	if err := json.NewDecoder(io.TeeReader(r, digest)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("genesis JSON invalid")
	}
	// The checksum covers the whole file, including anything after the document.
	if _, err := io.Copy(digest, r); err != nil {
		return nil, fmt.Errorf("genesis file unreadable")
	}
	switch doc.ChainID {
	case "cosmoshub-1", "cosmoshub-2", "cosmoshub-3", "cosmoshub-4":
	default:
		return nil, fmt.Errorf("genesis is not for cosmoshub-1 to 4")
	}
	if doc.Time.IsZero() {
		return nil, fmt.Errorf("genesis has no time")
	}
	app := &doc.AppState
	file := &File{Chain: doc.ChainID, SHA256: hex.EncodeToString(digest.Sum(nil)), Time: doc.Time.UTC()}

	raw := app.Accounts
	if len(app.Auth.Accounts) > 0 {
		if len(raw) > 0 {
			return nil, fmt.Errorf("genesis lists accounts in two places")
		}
		raw = app.Auth.Accounts
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("genesis has no accounts")
	}
	// cosmoshub-4 keeps balances apart from accounts.
	var balances map[string][]coin
	if len(app.Bank.Balances) > 0 {
		balances = make(map[string][]coin, len(app.Bank.Balances))
		for _, b := range app.Bank.Balances {
			if _, twice := balances[b.Address]; twice {
				return nil, fmt.Errorf("genesis lists a balance twice")
			}
			balances[b.Address] = b.Coins
		}
		if len(balances) != len(raw) {
			return nil, fmt.Errorf("genesis balances and accounts differ in number")
		}
	}
	index := make(map[string]int, len(raw))
	supply := new(big.Int)
	for position, a := range raw {
		account := Account{Position: position, Kind: "base", Delegated: new(big.Int), Unbonding: new(big.Int)}
		vesting, inner := a, a
		continuous := false
		switch {
		case a.BaseVesting != nil:
			if a.BaseVesting.BaseAccount == nil {
				return nil, fmt.Errorf("genesis vesting account has no base account")
			}
			vesting, inner = *a.BaseVesting, *a.BaseVesting.BaseAccount
			vesting.StartTime = a.StartTime
			continuous = strings.HasSuffix(a.Type, ".ContinuousVestingAccount")
			if !continuous && !strings.HasSuffix(a.Type, ".DelayedVestingAccount") {
				return nil, fmt.Errorf("genesis vesting account type not recognized")
			}
		case a.BaseAccount != nil:
			inner = *a.BaseAccount
			account.Module = a.Name
			if a.Name == "" {
				return nil, fmt.Errorf("genesis module account has no name")
			}
		default:
			account.Module = a.ModuleName
		}
		account.Address = inner.Address
		if !bech32Text.MatchString(account.Address) || strings.HasPrefix(account.Address, "cosmosvaloper") {
			return nil, fmt.Errorf("genesis account address invalid")
		}
		if _, twice := index[account.Address]; twice {
			return nil, fmt.Errorf("genesis lists an account twice")
		}
		sequence := inner.SequenceNumber
		if sequence == nil {
			sequence = inner.Sequence
		}
		if sequence == nil {
			return nil, fmt.Errorf("genesis account has no sequence")
		}
		var err error
		if account.Sequence, err = strconv.ParseUint(*sequence, 10, 64); err != nil {
			return nil, fmt.Errorf("genesis account sequence is not a number")
		}
		if account.Number, err = strconv.ParseUint(inner.AccountNumber, 10, 64); err != nil {
			return nil, fmt.Errorf("genesis account number is not a number")
		}
		coins := inner.Coins
		if balances != nil {
			if len(coins) > 0 {
				return nil, fmt.Errorf("genesis account holds coins beside its balance entry")
			}
			var found bool
			if coins, found = balances[account.Address]; !found {
				return nil, fmt.Errorf("genesis account has no balance entry")
			}
		}
		if account.FileCoins, err = atoms(coins, whole); err != nil {
			return nil, err
		}
		account.Liquid = new(big.Int).Set(account.FileCoins)
		supply.Add(supply, account.FileCoins)
		if account.OriginalVesting, err = atoms(vesting.OriginalVesting, whole); err != nil {
			return nil, err
		}
		if account.DelegatedFree, err = atoms(vesting.DelegatedFree, whole); err != nil {
			return nil, err
		}
		if account.DelegatedVesting, err = atoms(vesting.DelegatedVesting, whole); err != nil {
			return nil, err
		}
		if account.VestingStart, err = seconds(vesting.StartTime); err != nil {
			return nil, err
		}
		if account.VestingEnd, err = seconds(vesting.EndTime); err != nil {
			return nil, err
		}
		switch {
		case account.Module != "":
			account.Kind = "module"
		case account.OriginalVesting.Sign() > 0:
			if a.BaseVesting == nil {
				continuous = account.VestingStart != 0
			}
			account.Kind = "delayed_vesting"
			if continuous {
				account.Kind = "continuous_vesting"
			}
		case a.BaseVesting != nil:
			return nil, fmt.Errorf("genesis vesting account vests nothing")
		}
		index[account.Address] = position
		file.Accounts = append(file.Accounts, account)
	}
	file.Checks = append(file.Checks, "every account once, every coin uatom")

	stated := app.Bank.Supply
	if len(app.Supply.Supply) > 0 {
		if err := json.Unmarshal(app.Supply.Supply, &stated); err != nil {
			var nested struct {
				Total []coin `json:"total"`
			}
			if json.Unmarshal(app.Supply.Supply, &nested) != nil {
				return nil, fmt.Errorf("genesis supply not readable")
			}
			stated = nested.Total
		}
	}
	if len(stated) > 0 {
		total, err := atoms(stated, whole)
		if err != nil {
			return nil, err
		}
		if total.Cmp(supply) != 0 {
			return nil, fmt.Errorf("genesis account balances do not add up to the stated supply")
		}
		file.Checks = append(file.Checks, "account balances add up to the stated supply")
	}

	validators := map[string]int{}
	for _, v := range app.Staking.Validators {
		tokens, err := whole(v.Tokens)
		if err != nil {
			return nil, err
		}
		shares, err := scaled(v.Shares)
		if err != nil {
			return nil, err
		}
		state, err := status(v.Status)
		if err != nil {
			return nil, err
		}
		if !bech32Text.MatchString(v.Operator) {
			return nil, fmt.Errorf("genesis validator address invalid")
		}
		if _, twice := validators[v.Operator]; twice {
			return nil, fmt.Errorf("genesis lists a validator twice")
		}
		validators[v.Operator] = len(file.Validators)
		file.Validators = append(file.Validators, Validator{Operator: v.Operator, Tokens: tokens, Shares: shares, Status: state,
			Jailed: v.Jailed, OutstandingRewards: new(big.Int), AccumulatedCommission: new(big.Int)})
	}

	delegated := map[string]*big.Int{}
	pairs := map[string]bool{}
	for _, d := range app.Staking.Delegations {
		at, known := validators[d.Validator]
		if !known {
			return nil, fmt.Errorf("genesis delegation names a validator the file does not list")
		}
		who, has := index[d.Delegator]
		if !has {
			return nil, fmt.Errorf("genesis delegation names a delegator with no account")
		}
		shares, err := scaled(d.Shares)
		if err != nil {
			return nil, err
		}
		if shares.Sign() <= 0 || pairs[d.Delegator+"/"+d.Validator] {
			return nil, fmt.Errorf("genesis delegation is empty or listed twice")
		}
		pairs[d.Delegator+"/"+d.Validator] = true
		validator := file.Validators[at]
		if validator.Shares.Sign() <= 0 {
			return nil, fmt.Errorf("genesis delegation to a validator with no shares")
		}
		// The chain's own rule: shares times tokens over all shares, rounded down.
		tokens := new(big.Int).Mul(shares, validator.Tokens)
		tokens.Quo(tokens, validator.Shares)
		file.Delegations = append(file.Delegations, Delegation{Delegator: d.Delegator, Validator: d.Validator, Shares: shares, Tokens: tokens})
		if delegated[d.Validator] == nil {
			delegated[d.Validator] = new(big.Int)
		}
		delegated[d.Validator].Add(delegated[d.Validator], shares)
		file.Accounts[who].Delegated.Add(file.Accounts[who].Delegated, tokens)
	}
	for _, v := range file.Validators {
		sum := delegated[v.Operator]
		if sum == nil {
			sum = new(big.Int)
		}
		if sum.Cmp(v.Shares) != 0 {
			return nil, fmt.Errorf("genesis delegations to a validator do not add up to its shares")
		}
	}
	if len(file.Validators) > 0 {
		file.Checks = append(file.Checks, "delegations to every validator add up to its shares exactly")
	}

	for _, u := range app.Staking.Unbondings {
		who, has := index[u.Delegator]
		if _, known := validators[u.Validator]; !known || !has || len(u.Entries) == 0 {
			return nil, fmt.Errorf("genesis unbonding names an unknown validator or delegator, or has no entry")
		}
		for n, e := range u.Entries {
			height, err := strconv.ParseInt(e.CreationHeight, 10, 64)
			if err != nil || e.CompletionTime.IsZero() {
				return nil, fmt.Errorf("genesis unbonding entry has no height or completion time")
			}
			initial, err := whole(e.InitialBalance)
			if err != nil {
				return nil, err
			}
			balance, err := whole(e.Balance)
			if err != nil {
				return nil, err
			}
			file.Unbondings = append(file.Unbondings, Unbonding{Delegator: u.Delegator, Validator: u.Validator, Entry: n,
				CreationHeight: height, CompletionTime: e.CompletionTime.UTC(), InitialBalance: initial, Balance: balance})
			file.Accounts[who].Unbonding.Add(file.Accounts[who].Unbonding, balance)
		}
	}

	distribution := app.Distr
	if app.Distribution != nil {
		if distribution != nil {
			return nil, fmt.Errorf("genesis holds distribution state in two places")
		}
		distribution = app.Distribution
	}
	if distribution != nil {
		seen := map[string]bool{}
		for _, w := range distribution.Withdraws {
			if !bech32Text.MatchString(w.Delegator) || !bech32Text.MatchString(w.Address) || seen[w.Delegator] {
				return nil, fmt.Errorf("genesis withdraw address invalid or listed twice")
			}
			seen[w.Delegator] = true
			file.Withdraws = append(file.Withdraws, Withdraw{Delegator: w.Delegator, Address: w.Address})
		}
		for _, o := range distribution.Outstanding {
			at, known := validators[o.Validator]
			if !known {
				continue
			}
			amount, err := atoms(o.Rewards, scaled)
			if err != nil {
				return nil, err
			}
			file.Validators[at].OutstandingRewards = amount
		}
		for _, c := range distribution.Commissions {
			at, known := validators[c.Validator]
			if !known {
				continue
			}
			var coins []coin
			if len(c.Accumulated) > 0 && json.Unmarshal(c.Accumulated, &coins) != nil {
				var nested struct {
					Commission []coin `json:"commission"`
				}
				if json.Unmarshal(c.Accumulated, &nested) != nil {
					return nil, fmt.Errorf("genesis accumulated commission not readable")
				}
				coins = nested.Commission
			}
			amount, err := atoms(coins, scaled)
			if err != nil {
				return nil, err
			}
			file.Validators[at].AccumulatedCommission = amount
		}
	}

	// cosmoshub-1 began with no staking state. Its validators were created by
	// genesis transactions that ran before the first block, each delegating
	// part of the signer's balance to a new validator at one share a token.
	for _, tx := range app.Gentxs {
		if len(tx.Value.Msg) != 1 || tx.Value.Msg[0].Type != "cosmos-sdk/MsgCreateValidator" {
			return nil, fmt.Errorf("genesis transaction is not one validator creation")
		}
		m := tx.Value.Msg[0].Value
		who, has := index[m.Delegator]
		amount, err := atoms([]coin{m.Value}, whole)
		if err != nil {
			return nil, err
		}
		if _, twice := validators[m.Validator]; twice || !has || !bech32Text.MatchString(m.Validator) || amount.Sign() <= 0 {
			return nil, fmt.Errorf("genesis transaction creates a known validator, has no account or delegates nothing")
		}
		account := &file.Accounts[who]
		if account.Liquid.Cmp(amount) < 0 {
			return nil, fmt.Errorf("genesis transaction delegates more than the account holds")
		}
		account.Liquid.Sub(account.Liquid, amount)
		account.Delegated.Add(account.Delegated, amount)
		shares := new(big.Int).Mul(amount, one)
		validators[m.Validator] = len(file.Validators)
		file.Validators = append(file.Validators, Validator{Operator: m.Validator, Tokens: amount, Shares: shares,
			Status: "created by genesis transaction", OutstandingRewards: new(big.Int), AccumulatedCommission: new(big.Int)})
		file.Delegations = append(file.Delegations, Delegation{Delegator: m.Delegator, Validator: m.Validator,
			Shares: shares, Tokens: new(big.Int).Set(amount), FromGenesisTx: true})
		file.GenesisTxs++
	}
	if file.GenesisTxs > 0 {
		file.Checks = append(file.Checks, "every genesis transaction is one validator creation its signer could fund")
	}

	sort.Slice(file.Delegations, func(a, b int) bool {
		x, y := file.Delegations[a], file.Delegations[b]
		if x.Delegator != y.Delegator {
			return x.Delegator < y.Delegator
		}
		return x.Validator < y.Validator
	})
	return file, nil
}
