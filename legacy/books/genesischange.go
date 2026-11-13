package books

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
)

// The Cosmos Hub 4 genesis file was not a plain copy of the Hub 3 state. The
// program that wrote it also carried out governance proposal 29: it emptied
// seven fundraiser wallets whose owners had lost their keys, credited seven
// recovery wallets with a fixed amount each, and left the rest in the
// community pool. No Hub 3 transaction exists for any of it, so the stored
// history can never explain those fourteen balances.
//
// The list is closed. It is read from the public source file that made the
// change, and only a file with the sha256 below is accepted. No wallet,
// amount or address of the list is written in this repository.
const (
	Prop29Chain    = "cosmoshub-3"
	Prop29Name     = "Proposal 29 genesis change"
	Prop29Category = "prop29_genesis_recovery"
	Prop29Repo     = "github.com/cosmos/gaia"
	Prop29Tag      = "v4.0.0"
	Prop29Path     = "app/prop29.go"
	Prop29SHA256   = "912e0dcf14808028bc09bee726eeb003216aed069ceaffee530db69ae9fc5519"
	prop29Entries  = 7
)

// The public program moves the seven source balances into one sum, pays the
// seven listed amounts out of it and leaves the rest in the community pool.
// It gives no share per source, so the remainder is pinned as one number: the
// community pool of the Hub 4 genesis file minus the community pool of Hub 3
// after its export height, which two archives answered byte for byte alike.
// The close refuses the whole list unless the source balances it computes,
// less the listed amounts, come to exactly this.
const (
	Prop29Pool = "131859276000"
	// Prop29PoolStateSHA256 is the sha256 of the archives' answer for the
	// distribution fee pool after height 5200790.
	Prop29PoolStateSHA256 = "f598818f717ce3ab2b616220da5348d43ae2be11df88ad0c3c678e16b3768afe"
	// Prop29NextGenesisSHA256 is the sha256 of the Hub 4 genesis file the
	// pool was read from, as legacy.genesis_files records it.
	Prop29NextGenesisSHA256 = "7fe946e6bb3c378da546767f4d078585c38f256c8ec17888d71aeee3b7edd5c7"
)

// StatusChange is the status a line of legacy.reconciliation carries when
// the address closes only with the genesis change.
const StatusChange = "closed_with_prop29_genesis_change"

// Recovery is one entry of the list: the wallet the genesis program emptied,
// the wallet it credited and the amount it credited.
type Recovery struct {
	Source, Recovery string
	Amount           *big.Int
}

// GenesisChange is the whole list with where it came from. Only LoadProp29
// makes one, and nothing of it can be set or changed from outside this
// package: not the list, not the chain it belongs to, not the community pool
// remainder and not the genesis file the remainder was proven against.
type GenesisChange struct {
	chain, name             string
	repo, tag, path, sha256 string
	// poolState and nextGenesis name the evidence of the community pool
	// remainder by sha256.
	poolState, nextGenesis string
	recoveries             []Recovery
	pool                   *big.Int
}

// Name is the name the change is reported under.
func (c *GenesisChange) Name() string { return c.name }

// Source says where the list was read from: repository, tag, path and the
// sha256 of the file.
func (c *GenesisChange) Source() (repo, tag, path, sha256 string) {
	return c.repo, c.tag, c.path, c.sha256
}

// PoolEvidence gives the sha256 of the fee pool answer after the export
// height and of the next genesis file the remainder was read from.
func (c *GenesisChange) PoolEvidence() (poolState, nextGenesis string) {
	return c.poolState, c.nextGenesis
}

// Listed is the number of recoveries in the list.
func (c *GenesisChange) Listed() int { return len(c.recoveries) }

// Pool is the community pool remainder the list must leave, in uatom.
func (c *GenesisChange) Pool() string { return c.pool.String() }

var (
	donorBlock  = regexp.MustCompile(`(?s)\nfunc (?:btc|eth)Donor[0-9]+\(\) FundRecoveryMessage \{\n(.*?)\n\}\n`)
	donorSource = regexp.MustCompile(`\bsourceAddress, _ := sdk\.AccAddressFromBech32\("([^"]+)"\)`)
	donorDest   = regexp.MustCompile(`\bdestAddress, _ := sdk\.AccAddressFromBech32\("([^"]+)"\)`)
	donorAmount = regexp.MustCompile(`\bsdk\.NewInt64Coin\("uatom", ([0-9]{1,18})\)\}`)
	hubAddress  = regexp.MustCompile(`^cosmos1[02-9ac-hj-np-z]{38}$`)
)

// LoadProp29 reads the list from the bytes of the public source file. It
// refuses any file but the one pinned above.
func LoadProp29(source []byte) (*GenesisChange, error) {
	sum := sha256.Sum256(source)
	if hex.EncodeToString(sum[:]) != Prop29SHA256 {
		return nil, fmt.Errorf("the genesis change file is not %s %s %s with the recorded sha256", Prop29Repo, Prop29Tag, Prop29Path)
	}
	recoveries, err := parseRecoveries(source)
	if err != nil {
		return nil, err
	}
	if len(recoveries) != prop29Entries {
		return nil, fmt.Errorf("the genesis change file lists %d recoveries, not %d", len(recoveries), prop29Entries)
	}
	pool, ok := new(big.Int).SetString(Prop29Pool, 10)
	if !ok {
		return nil, fmt.Errorf("the recorded community pool remainder is not a number")
	}
	return &GenesisChange{chain: Prop29Chain, name: Prop29Name,
		repo: Prop29Repo, tag: Prop29Tag, path: Prop29Path, sha256: Prop29SHA256,
		poolState: Prop29PoolStateSHA256, nextGenesis: Prop29NextGenesisSHA256, recoveries: recoveries, pool: pool}, nil
}

// parseRecoveries reads every donor function of the file. Each must name one
// source wallet, one recovery wallet and one amount, and no wallet may
// appear twice in the file.
func parseRecoveries(source []byte) ([]Recovery, error) {
	var out []Recovery
	seen := map[string]bool{}
	for _, block := range donorBlock.FindAllSubmatch(source, -1) {
		body := block[1]
		from, to, amount := donorSource.FindAllSubmatch(body, -1), donorDest.FindAllSubmatch(body, -1), donorAmount.FindAllSubmatch(body, -1)
		if len(from) != 1 || len(to) != 1 || len(amount) != 1 {
			return nil, fmt.Errorf("a recovery in the genesis change file does not name one source, one recovery wallet and one amount")
		}
		entry := Recovery{Source: string(from[0][1]), Recovery: string(to[0][1])}
		value, ok := new(big.Int).SetString(string(amount[0][1]), 10)
		if !ok || value.Sign() <= 0 {
			return nil, fmt.Errorf("a recovery in the genesis change file has no positive amount")
		}
		entry.Amount = value
		for _, address := range []string{entry.Source, entry.Recovery} {
			if !hubAddress.MatchString(address) {
				return nil, fmt.Errorf("a wallet in the genesis change file is not a Cosmos Hub address")
			}
			if seen[address] {
				return nil, fmt.Errorf("a wallet appears twice in the genesis change file")
			}
			seen[address] = true
		}
		out = append(out, entry)
	}
	if bytes.Count(source, []byte("FundRecoveryMessage {\n")) != len(out) {
		return nil, fmt.Errorf("the genesis change file has a recovery this reader did not understand")
	}
	return out, nil
}

// listed says which entry of the change names the address and on which side.
func (c *GenesisChange) listed(address string) (entry *Recovery, source bool) {
	if c == nil {
		return nil, false
	}
	for i := range c.recoveries {
		switch address {
		case c.recoveries[i].Source:
			return &c.recoveries[i], true
		case c.recoveries[i].Recovery:
			return &c.recoveries[i], false
		}
	}
	return nil, false
}

type changeDetail struct {
	Change          string         `json:"change"`
	MadeBy          string         `json:"made_by"`
	EffectiveChain  string         `json:"effective_chain_id"`
	DatedAt         string         `json:"dated_at"`
	RecoveryAddress string         `json:"recovery_address,omitempty"`
	Recovered       string         `json:"recovered,omitempty"`
	SourceAddress   string         `json:"source_address,omitempty"`
	SourceBalance   string         `json:"source_balance,omitempty"`
	CommunityPool   string         `json:"community_pool"`
	AmountBasis     string         `json:"amount_basis"`
	Review          string         `json:"review"`
	Evidence        changeEvidence `json:"evidence"`
}

type changeEvidence struct {
	Repo              string `json:"repo"`
	Tag               string `json:"tag"`
	Path              string `json:"path"`
	SHA256            string `json:"sha256"`
	PoolTotal         string `json:"community_pool_total"`
	PoolStateSHA256   string `json:"community_pool_state_sha256"`
	NextGenesisSHA256 string `json:"next_genesis_sha256"`
}

type changeRow struct {
	sub                      int
	amount, from, to, detail string
}

// changeRows builds the two tax rows of every recovery: one out of the
// source wallet for its whole balance and one into the recovery wallet for
// the amount the file states. Neither is income or a sale by itself; both
// say they need review.
func changeRows(result *Result) ([]changeRow, error) {
	change := result.Change
	var rows []changeRow
	for i, entry := range change.recoveries {
		whole := result.changeSourceBalance[entry.Source]
		if whole == nil {
			return nil, fmt.Errorf("a source wallet of the genesis change was not accepted by the close")
		}
		pool := new(big.Int).Sub(whole, entry.Amount)
		evidence := changeEvidence{Repo: change.repo, Tag: change.tag, Path: change.path, SHA256: change.sha256,
			PoolTotal: change.pool.String(), PoolStateSHA256: change.poolState, NextGenesisSHA256: change.nextGenesis}
		madeBy := "the genesis file of " + result.Next + ", not a transaction"
		datedAt := fmt.Sprintf("the last block of %s, height %d", result.Chain, result.Through)
		out, err := json.Marshal(changeDetail{Change: change.name, MadeBy: madeBy, EffectiveChain: result.Next, DatedAt: datedAt,
			RecoveryAddress: entry.Recovery, Recovered: entry.Amount.String(),
			CommunityPool: pool.String(), AmountBasis: "the whole balance the stored history gives at the export height; the next genesis file holds zero",
			Review: "needs review", Evidence: evidence})
		if err != nil {
			return nil, err
		}
		in, err := json.Marshal(changeDetail{Change: change.name, MadeBy: madeBy, EffectiveChain: result.Next, DatedAt: datedAt,
			SourceAddress: entry.Source, SourceBalance: whole.String(),
			CommunityPool: pool.String(), AmountBasis: "the amount the genesis change file states", Review: "needs review", Evidence: evidence})
		if err != nil {
			return nil, err
		}
		rows = append(rows,
			changeRow{sub: 2 * i, amount: whole.String(), from: entry.Source, detail: string(out)},
			changeRow{sub: 2*i + 1, amount: entry.Amount.String(), to: entry.Recovery, detail: string(in)})
	}
	return rows, nil
}

// RecordChange stores the tax rows of the genesis change, once, in
// legacy.taxable_events: at the export height, in the phase of the state
// export, dated at the time of the block at the export height (the last block
// of the chain; the next genesis file carries no start time of its own), with
// the genesis file as the source of the amount. It refuses unless the close
// covered every height and accepted the whole list. Stored rows are never replaced: a second
// call reads them back and fails if one differs. It returns how many rows it
// added.
func RecordChange(ctx context.Context, db *sql.DB, result *Result) (int, error) {
	if result == nil || result.Change == nil {
		return 0, fmt.Errorf("the close ran without a genesis change")
	}
	if !result.Complete {
		return 0, fmt.Errorf("the genesis change is recorded only when every height up to the export height is stored")
	}
	if !result.ChangeWhole {
		return 0, fmt.Errorf("the genesis change is recorded only when the close accepted the whole list")
	}
	want, err := changeRows(result)
	if err != nil {
		return 0, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	stored, err := tx.QueryContext(ctx, `SELECT sub_index, category, amount, denom, from_addr, to_addr, detail, amount_source,
"timestamp" = (SELECT "time" FROM legacy.blocks WHERE chain_id = $1 AND height = $2)
FROM legacy.taxable_events WHERE chain_id = $1 AND height = $2 AND phase = 4 ORDER BY sub_index`, result.Chain, result.Through)
	if err != nil {
		return 0, err
	}
	have := 0
	for stored.Next() {
		var sub int
		var category, amount, unit, from, to, detail, source string
		var dated bool
		if err := stored.Scan(&sub, &category, &amount, &unit, &from, &to, &detail, &source, &dated); err != nil {
			stored.Close()
			return 0, err
		}
		if have >= len(want) || want[have].sub != sub || category != Prop29Category || amount != want[have].amount || unit != denom ||
			from != want[have].from || to != want[have].to || detail != want[have].detail || source != "genesis" || !dated {
			stored.Close()
			return 0, fmt.Errorf("the rows stored at the export height are not the genesis change this close accepted")
		}
		have++
	}
	if err := finish(stored); err != nil {
		return 0, err
	}
	if have == len(want) {
		return 0, nil
	}
	if have != 0 {
		return 0, fmt.Errorf("only part of the genesis change is stored")
	}
	for _, row := range want {
		if _, err := tx.ExecContext(ctx, `INSERT INTO legacy.taxable_events (chain_id, height, phase, tx_index, msg_index, sub_index, category,
amount, denom, from_addr, to_addr, detail, "timestamp", amount_source)
SELECT $1, $2, 4, 0, 0, $3, $4, $5, $6, $7, $8, $9, "time", 'genesis' FROM legacy.blocks WHERE chain_id = $1 AND height = $2`,
			result.Chain, result.Through, row.sub, Prop29Category, row.amount, denom, row.from, row.to, row.detail); err != nil {
			return 0, err
		}
	}
	var added int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM legacy.taxable_events WHERE chain_id = $1 AND height = $2 AND phase = 4 AND category = $3`,
		result.Chain, result.Through, Prop29Category).Scan(&added); err != nil {
		return 0, err
	}
	if added != len(want) {
		return 0, fmt.Errorf("the genesis change rows were not all stored")
	}
	return added, tx.Commit()
}
