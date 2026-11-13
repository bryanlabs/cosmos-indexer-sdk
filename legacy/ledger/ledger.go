// Package ledger writes Cosmos Hub 1 to 3 heights into the legacy schema and
// reads them back. It touches no table outside that schema, so it works as
// the importer role, which can reach nothing else.
//
// A height is identified by its chain and its number, never by the number
// alone: heights restart on each of the three chains. A transaction is
// identified by (chain, height, index); its hash is evidence and may repeat.
//
// The writer only inserts. A height that is already stored is read back and
// compared with what this import would write; any difference is an error and
// nothing is rewritten.
package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Phases order the rows of one height.
const (
	PhaseBegin  = 1
	PhaseTx     = 2
	PhaseEnd    = 3
	PhaseExport = 4
)

// Block is one height with everything the ledger keeps for it.
type Block struct {
	Chain    string
	Height   int64
	Time     time.Time
	Hash     string
	Proposer string
	// Inclusions holds every transaction of the block, in block order.
	Inclusions []Inclusion
	// Rows holds the tax rows of the height ordered by phase, transaction,
	// message and position.
	Rows []Row
	// Events holds the begin and end block events that are kept.
	Events []Event
}

// Inclusion is one appearance of a transaction in a block.
type Inclusion struct {
	Index                   int
	Hash                    string
	Code                    uint32
	Codespace, CodeEvidence string
	GasWanted, GasUsed      int64
	// Fee is the fee the transaction signed. FeeCharged says whether the
	// chain kept it and FeeEvidence how that is known.
	Fee, FeePayer string
	FeeCharged    bool
	FeeEvidence   string
	Memo          string
	Signers       []string
	Raw           []byte
}

// Row is one tax row. Source says where its amount came from.
type Row struct {
	Phase, TxIndex, MsgIndex, SubIndex                                   int
	Category, Amount, Denom, Asset, From, To, Validator, Trigger, Detail string
	TxHash, Source                                                       string
}

// Event is one begin or end block event. Attributes keep the chain's order.
type Event struct {
	Phase, Index int
	Type         string
	Attributes   [][2]string
}

// ErrStoredDiffers reports a height whose stored rows disagree with what
// this import would write for it.
var ErrStoredDiffers = errors.New("a stored legacy height differs from this import")

var (
	upperHash   = regexp.MustCompile(`^[0-9A-F]{64}$`)
	wholeNumber = regexp.MustCompile(`^[0-9]{1,78}$`)
	// importerSources are the sources this writer can own. State and export
	// sources are admitted and compared only for Hub2 replay. Later Hub3 state
	// rows and window allocations remain outside this writer's ownership.
	importerSources = map[string]bool{"message": true, "event": true, "tag": true, "state": true, "export_payout": true}
)

// Text makes a byte string safe for a text column: invalid UTF-8 becomes the
// replacement character and NUL bytes are dropped. The original bytes stay
// in the stored transaction.
func Text(raw []byte) string {
	return strings.ReplaceAll(strings.ToValidUTF8(string(raw), "�"), "\x00", "")
}

func (b *Block) prepare() error {
	if b.Chain != "cosmoshub-1" && b.Chain != "cosmoshub-2" && b.Chain != "cosmoshub-3" {
		return fmt.Errorf("the ledger holds cosmoshub-1, 2 and 3 only")
	}
	if b.Height <= 0 || b.Time.IsZero() {
		return fmt.Errorf("legacy height or block time missing")
	}
	// Postgres keeps microseconds. Truncate first so a rerun compares equal.
	b.Time = b.Time.UTC().Truncate(time.Microsecond)
	if !upperHash.MatchString(b.Hash) || b.Proposer == "" {
		return fmt.Errorf("legacy block hash or proposer missing")
	}
	for i := range b.Inclusions {
		inc := &b.Inclusions[i]
		if inc.Index != i {
			return fmt.Errorf("legacy inclusions must be complete and in block order")
		}
		if !upperHash.MatchString(inc.Hash) || len(inc.Raw) == 0 {
			return fmt.Errorf("legacy inclusion hash or bytes missing")
		}
		if inc.CodeEvidence == "" || inc.FeeEvidence == "" || inc.FeePayer == "" || len(inc.Signers) == 0 {
			return fmt.Errorf("legacy inclusion without code evidence, fee evidence or signers")
		}
		for _, signer := range inc.Signers {
			if signer == "" || strings.Contains(signer, ",") {
				return fmt.Errorf("legacy signer is not an address")
			}
		}
		if !utf8.ValidString(inc.Memo) || strings.Contains(inc.Memo, "\x00") {
			return fmt.Errorf("legacy memo is not storable text")
		}
	}
	last := [4]int{-1, -1, -1, -1}
	for _, row := range b.Rows {
		key := [4]int{row.Phase, row.TxIndex, row.MsgIndex, row.SubIndex}
		if !less(last, key) {
			return fmt.Errorf("legacy tax rows must be in order and distinct")
		}
		last = key
		switch {
		case row.Phase == PhaseTx:
			if row.TxIndex < 0 || row.TxIndex >= len(b.Inclusions) || row.MsgIndex < 0 || row.SubIndex < 0 {
				return fmt.Errorf("legacy tax row names a transaction the block does not hold")
			}
		case row.Phase == PhaseBegin || row.Phase == PhaseEnd || row.Phase == PhaseExport:
			if row.TxIndex != 0 || row.MsgIndex != 0 || row.SubIndex < 0 {
				return fmt.Errorf("legacy block tax row carries a transaction position")
			}
		default:
			return fmt.Errorf("legacy tax row phase is not one the importer writes")
		}
		if (row.Source == "state" || row.Source == "export_payout" || row.Phase == PhaseExport) && b.Chain != "cosmoshub-2" {
			return fmt.Errorf("replay sources and export phase require Hub2")
		}
		if (row.Phase == PhaseExport) != (row.Source == "export_payout") {
			return fmt.Errorf("export payout source requires export phase exclusively")
		}
		if row.Source == "state" && row.Phase != PhaseTx && row.Phase != PhaseEnd {
			return fmt.Errorf("replay state source requires transaction or end phase")
		}
		if row.Source == "export_payout" && (row.Category != "reward" && row.Category != "commission" || row.Validator == "" || row.To == "" || row.Trigger != "zero_height_export") {
			return fmt.Errorf("export payout requires actual reward or commission identity")
		}
		if row.Category == "" || row.Denom == "" || !wholeNumber.MatchString(row.Amount) || !importerSources[row.Source] {
			return fmt.Errorf("legacy tax row without a category, denom, whole amount or importer source")
		}
	}
	lastEvent := [2]int{-1, -1}
	for _, event := range b.Events {
		if event.Phase != PhaseBegin && event.Phase != PhaseEnd {
			return fmt.Errorf("legacy block event phase must be begin or end block")
		}
		key := [2]int{event.Phase, event.Index}
		if key[0] < lastEvent[0] || (key[0] == lastEvent[0] && key[1] <= lastEvent[1]) || event.Index < 0 || event.Type == "" {
			return fmt.Errorf("legacy block events must be in order, distinct and typed")
		}
		lastEvent = key
	}
	return nil
}

func less(a, b [4]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// same compares two heights by what the ledger stores for them.
func same(a, b Block) bool {
	if a.Chain != b.Chain || a.Height != b.Height || !a.Time.Equal(b.Time) || a.Hash != b.Hash || a.Proposer != b.Proposer ||
		len(a.Inclusions) != len(b.Inclusions) || len(a.Rows) != len(b.Rows) || len(a.Events) != len(b.Events) {
		return false
	}
	for i := range a.Inclusions {
		if !reflect.DeepEqual(a.Inclusions[i], b.Inclusions[i]) {
			return false
		}
	}
	for i := range a.Rows {
		if a.Rows[i] != b.Rows[i] {
			return false
		}
	}
	for i := range a.Events {
		x, y := a.Events[i], b.Events[i]
		if x.Phase != y.Phase || x.Index != y.Index || x.Type != y.Type || len(x.Attributes) != len(y.Attributes) {
			return false
		}
		for j := range x.Attributes {
			if x.Attributes[j] != y.Attributes[j] {
				return false
			}
		}
	}
	return true
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Load reads the named heights of one chain back in height order. Heights
// that are not stored are absent from the result. Only rows this writer
// stores are returned.
func Load(ctx context.Context, db querier, chain string, heights []int64) ([]Block, error) {
	if len(heights) == 0 {
		return nil, nil
	}
	found := map[int64]*Block{}
	var order []int64
	rows, err := db.QueryContext(ctx, `SELECT height, time, block_hash, proposer, tx_count FROM legacy.blocks
WHERE chain_id = $1 AND height = ANY($2::bigint[]) ORDER BY height`, chain, heights)
	if err != nil {
		return nil, err
	}
	counts := map[int64]int{}
	for rows.Next() {
		b := &Block{Chain: chain}
		var count int
		if err := rows.Scan(&b.Height, &b.Time, &b.Hash, &b.Proposer, &count); err != nil {
			rows.Close()
			return nil, err
		}
		b.Time = b.Time.UTC()
		found[b.Height] = b
		counts[b.Height] = count
		order = append(order, b.Height)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	rows, err = db.QueryContext(ctx, `SELECT height, tx_index, hash, code, codespace, code_evidence, gas_wanted, gas_used, fee, fee_payer,
fee_charged, fee_evidence, memo, array_to_string(signers, ','), raw FROM legacy.tx_inclusions
WHERE chain_id = $1 AND height = ANY($2::bigint[]) ORDER BY height, tx_index`, chain, heights)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var height int64
		var inc Inclusion
		var code int64
		var signers string
		if err := rows.Scan(&height, &inc.Index, &inc.Hash, &code, &inc.Codespace, &inc.CodeEvidence, &inc.GasWanted, &inc.GasUsed, &inc.Fee,
			&inc.FeePayer, &inc.FeeCharged, &inc.FeeEvidence, &inc.Memo, &signers, &inc.Raw); err != nil {
			rows.Close()
			return nil, err
		}
		inc.Code = uint32(code)
		inc.Signers = strings.Split(signers, ",")
		if b := found[height]; b != nil {
			b.Inclusions = append(b.Inclusions, inc)
		}
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	rows, err = db.QueryContext(ctx, `SELECT height, phase, tx_index, msg_index, sub_index, category, amount, denom, asset, from_addr, to_addr,
validator_address, reward_trigger, detail, tx_hash, amount_source FROM legacy.taxable_events
WHERE chain_id = $1 AND height = ANY($2::bigint[]) AND (amount_source IN ('message', 'event', 'tag') OR (chain_id = 'cosmoshub-2' AND amount_source IN ('state', 'export_payout')))
ORDER BY height, phase, tx_index, msg_index, sub_index`, chain, heights)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var height int64
		var row Row
		if err := rows.Scan(&height, &row.Phase, &row.TxIndex, &row.MsgIndex, &row.SubIndex, &row.Category, &row.Amount, &row.Denom, &row.Asset,
			&row.From, &row.To, &row.Validator, &row.Trigger, &row.Detail, &row.TxHash, &row.Source); err != nil {
			rows.Close()
			return nil, err
		}
		if b := found[height]; b != nil {
			b.Rows = append(b.Rows, row)
		}
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	rows, err = db.QueryContext(ctx, `SELECT height, phase, event_index, type, attributes::text FROM legacy.block_events
WHERE chain_id = $1 AND height = ANY($2::bigint[]) ORDER BY height, phase, event_index`, chain, heights)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var height int64
		var event Event
		var attributes string
		if err := rows.Scan(&height, &event.Phase, &event.Index, &event.Type, &attributes); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(attributes), &event.Attributes); err != nil {
			rows.Close()
			return nil, fmt.Errorf("stored block event attributes are not pairs")
		}
		if b := found[height]; b != nil {
			b.Events = append(b.Events, event)
		}
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	out := make([]Block, 0, len(order))
	for _, height := range order {
		b := found[height]
		if counts[height] != len(b.Inclusions) {
			return nil, fmt.Errorf("stored legacy height %d of %s holds fewer transactions than its block row says", height, chain)
		}
		out = append(out, *b)
	}
	return out, nil
}

func closeRows(rows *sql.Rows) error {
	err := rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	return err
}

// TransactionBeginner accepts either a pool or a dedicated session. Callers
// relying on session-scoped admission must supply their pinned *sql.Conn.
type TransactionBeginner interface {
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

// Write stores a run of heights of one chain in a single database
// transaction and returns how many of them were new. A height that is
// already stored is compared instead; ErrStoredDiffers names the first one
// that disagrees and nothing in the run is stored.
func Write(ctx context.Context, db TransactionBeginner, blocks []Block) (int, error) {
	if len(blocks) == 0 {
		return 0, nil
	}
	chain := blocks[0].Chain
	heights := make([]int64, len(blocks))
	for i := range blocks {
		if err := blocks[i].prepare(); err != nil {
			return 0, fmt.Errorf("height %d: %w", blocks[i].Height, err)
		}
		if blocks[i].Chain != chain || (i > 0 && blocks[i].Height <= blocks[i-1].Height) {
			return 0, fmt.Errorf("a run of legacy heights must be one chain in rising order")
		}
		heights[i] = blocks[i].Height
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	stored, err := Load(ctx, tx, chain, heights)
	if err != nil {
		return 0, err
	}
	have := map[int64]Block{}
	for _, b := range stored {
		have[b.Height] = b
	}
	var fresh []Block
	for _, b := range blocks {
		old, ok := have[b.Height]
		if !ok {
			fresh = append(fresh, b)
			continue
		}
		if !same(old, b) {
			return 0, fmt.Errorf("%w: %s height %d", ErrStoredDiffers, chain, b.Height)
		}
	}
	if len(fresh) == 0 {
		return 0, tx.Commit()
	}
	if err := insert(ctx, tx, chain, fresh); err != nil {
		return 0, err
	}
	return len(fresh), tx.Commit()
}

func insert(ctx context.Context, tx *sql.Tx, chain string, blocks []Block) error {
	var (
		bHeight                              []int64
		bTime                                []time.Time
		bHash, bProposer                     []string
		bCount                               []int32
		iHeight, iCode, iWanted, iUsed       []int64
		iIndex                               []int32
		iHash, iSpace, iCodeEv, iFee, iPayer []string
		iFeeEv, iMemo, iSigners              []string
		iCharged                             []bool
		iRaw                                 [][]byte
		rHeight                              []int64
		rPhase, rTx, rMsg, rSub              []int32
		rCat, rAmount, rDenom, rAsset, rFrom []string
		rTo, rVal, rTrigger, rDetail, rHash  []string
		rSource                              []string
		rTime                                []time.Time
		eHeight                              []int64
		ePhase, eIndex                       []int32
		eType, eAttributes                   []string
	)
	for _, b := range blocks {
		bHeight = append(bHeight, b.Height)
		bTime = append(bTime, b.Time)
		bHash = append(bHash, b.Hash)
		bProposer = append(bProposer, b.Proposer)
		bCount = append(bCount, int32(len(b.Inclusions)))
		for _, inc := range b.Inclusions {
			iHeight = append(iHeight, b.Height)
			iIndex = append(iIndex, int32(inc.Index))
			iHash = append(iHash, inc.Hash)
			iCode = append(iCode, int64(inc.Code))
			iSpace = append(iSpace, inc.Codespace)
			iCodeEv = append(iCodeEv, inc.CodeEvidence)
			iWanted = append(iWanted, inc.GasWanted)
			iUsed = append(iUsed, inc.GasUsed)
			iFee = append(iFee, inc.Fee)
			iPayer = append(iPayer, inc.FeePayer)
			iCharged = append(iCharged, inc.FeeCharged)
			iFeeEv = append(iFeeEv, inc.FeeEvidence)
			iMemo = append(iMemo, inc.Memo)
			iSigners = append(iSigners, strings.Join(inc.Signers, ","))
			iRaw = append(iRaw, inc.Raw)
		}
		for _, row := range b.Rows {
			rHeight = append(rHeight, b.Height)
			rPhase = append(rPhase, int32(row.Phase))
			rTx = append(rTx, int32(row.TxIndex))
			rMsg = append(rMsg, int32(row.MsgIndex))
			rSub = append(rSub, int32(row.SubIndex))
			rCat = append(rCat, row.Category)
			rAmount = append(rAmount, row.Amount)
			rDenom = append(rDenom, row.Denom)
			rAsset = append(rAsset, row.Asset)
			rFrom = append(rFrom, row.From)
			rTo = append(rTo, row.To)
			rVal = append(rVal, row.Validator)
			rTrigger = append(rTrigger, row.Trigger)
			rDetail = append(rDetail, row.Detail)
			rTime = append(rTime, b.Time)
			rHash = append(rHash, row.TxHash)
			rSource = append(rSource, row.Source)
		}
		for _, event := range b.Events {
			attributes := event.Attributes
			if attributes == nil {
				attributes = [][2]string{}
			}
			encoded, err := json.Marshal(attributes)
			if err != nil {
				return err
			}
			eHeight = append(eHeight, b.Height)
			ePhase = append(ePhase, int32(event.Phase))
			eIndex = append(eIndex, int32(event.Index))
			eType = append(eType, event.Type)
			eAttributes = append(eAttributes, string(encoded))
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO legacy.blocks (chain_id, height, time, block_hash, proposer, tx_count)
SELECT $1, * FROM unnest($2::bigint[], $3::timestamptz[], $4::text[], $5::text[], $6::integer[])`,
		chain, bHeight, bTime, bHash, bProposer, bCount); err != nil {
		return err
	}
	if len(iHeight) > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO legacy.tx_inclusions (chain_id, height, tx_index, hash, code, codespace, code_evidence,
gas_wanted, gas_used, fee, fee_payer, fee_charged, fee_evidence, memo, signers, raw)
SELECT $1, h, i, hash, code, space, code_evidence, wanted, used, fee, payer, charged, fee_evidence, memo, string_to_array(signers, ','), raw
FROM unnest($2::bigint[], $3::integer[], $4::text[], $5::bigint[], $6::text[], $7::text[], $8::bigint[], $9::bigint[], $10::text[],
$11::text[], $12::boolean[], $13::text[], $14::text[], $15::text[], $16::bytea[])
AS t(h, i, hash, code, space, code_evidence, wanted, used, fee, payer, charged, fee_evidence, memo, signers, raw)`,
			chain, iHeight, iIndex, iHash, iCode, iSpace, iCodeEv, iWanted, iUsed, iFee, iPayer, iCharged, iFeeEv, iMemo, iSigners, iRaw); err != nil {
			return err
		}
	}
	if len(rHeight) > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO legacy.taxable_events (chain_id, height, phase, tx_index, msg_index, sub_index, category,
amount, denom, asset, from_addr, to_addr, validator_address, reward_trigger, detail, "timestamp", tx_hash, amount_source)
SELECT $1, * FROM unnest($2::bigint[], $3::smallint[], $4::integer[], $5::integer[], $6::integer[], $7::text[], $8::text[], $9::text[],
$10::text[], $11::text[], $12::text[], $13::text[], $14::text[], $15::text[], $16::timestamptz[], $17::text[], $18::text[])`,
			chain, rHeight, rPhase, rTx, rMsg, rSub, rCat, rAmount, rDenom, rAsset, rFrom, rTo, rVal, rTrigger, rDetail, rTime, rHash, rSource); err != nil {
			return err
		}
	}
	if len(eHeight) > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO legacy.block_events (chain_id, height, phase, event_index, type, attributes)
SELECT $1, h, phase, i, type, attributes::jsonb
FROM unnest($2::bigint[], $3::smallint[], $4::integer[], $5::text[], $6::text[]) AS t(h, phase, i, type, attributes)`,
			chain, eHeight, ePhase, eIndex, eType, eAttributes); err != nil {
			return err
		}
	}
	return nil
}
