package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	dbpkg "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"gorm.io/gorm"
)

// The private commission profile hashes this message's stored rows, so the
// withdraw-address plan never touches it.
const privateProfileMessageID = 27907375

const typeURLAuthzExec = "/cosmos.authz.v1beta1.MsgExec"

// Plan classes. Only exact and rewardsEqual messages get rows.
const (
	waExact        = "exact"         // the parser reproduces every stored row; the moves follow them
	waRewardsEqual = "rewards_equal" // other stored rows differ, the booked rewards the moves are capped at do not
	waRewardsDrift = "rewards_drift" // the stored rewards differ from the parser's: no plan
	waNoMoves      = "no_moves"      // the parser adds nothing (LSM payouts, commission, nothing redirected)
	waAlready      = "already"       // the message already holds withdraw_address rows (and cannot be reparsed)
	// A message that already holds the moves is reparsed to check them: the
	// whole fresh parse equals the stored rows, only the moves do (the other
	// rows predate a parser change), or the moves differ.
	waAlreadyExact    = "already_exact"
	waAlreadyMoves    = "already_moves_equal"
	waAlreadyMismatch = "already_mismatch"
	waNoBody          = "no_body" // authz without stored bytes: the parser cannot rerun it
	waParseError      = "parse_error"
	waFailedTx        = "failed_tx"
	waPrivate         = "private_profile"
	waGap             = "sub_index_gap"
)

// waPlanRow is one row the loader inserts, with the stored state it was
// planned against. The loader refuses the message when that state changed.
type waPlanRow struct {
	tax.TaxableEvent
	Class         string
	Trigger       string
	StoredRows    int
	StoredFinger  string
	MessageTypeID uint
}

// waFingerSQL is the stored-row fingerprint the loader recomputes inside
// its transaction; both sides use this one expression.
const waFingerSQL = `md5(string_agg(concat_ws('|', te.id, te.sub_index, coalesce(te.category,''), coalesce(te.amount,''), coalesce(te.denom,''),
 coalesce(te.asset,''), coalesce(te.from_addr,''), coalesce(te.to_addr,''), coalesce(te.validator_address,''), coalesce(te.reward_trigger,''),
 coalesce(te.detail,''), coalesce(te.block_height,0), coalesce(extract(epoch FROM te.timestamp)::text,''), coalesce(te.tx_hash,'')), E'\n' ORDER BY te.sub_index))`

type waMessage struct {
	ID            uint
	MessageTypeID uint
	Type          string
	MessageBytes  []byte
	Code          int64
	Height        int64
	Stamp         time.Time
	Hash          string
}

type waFinger struct {
	MessageID uint
	N         int
	MaxSub    int
	Finger    string
}

func runWithdrawAddressPlan(args []string) int {
	fs := flag.NewFlagSet("withdraw-address-plan", flag.ExitOnError)
	idsFile := fs.String("message-ids-file", "", "file of target message ids, one per line")
	out := fs.String("out", "", "new plan file (TSV), never overwritten")
	batch := fs.Int("batch", 200, "messages per read batch")
	_ = fs.Parse(args)
	if *idsFile == "" || *out == "" || *batch < 1 || *batch > 2000 {
		fatal("withdraw-address-plan needs --message-ids-file, --out and a batch of 1 to 2000")
	}
	ids, err := readMessageIDs(*idsFile)
	if err != nil {
		fatal(err.Error())
	}
	// default_transaction_read_only is a connection parameter, so every
	// reconnect is read-only too.
	db, err := dbpkg.PostgresDbConnect(env("DB_HOST"), envDefault("DB_PORT", "5432"), env("DB_NAME"), env("DB_USER"), os.Getenv("DB_PASS"), "silent",
		"default_transaction_read_only=on", "application_name=withdraw-address-plan")
	if err != nil {
		fatal(err.Error())
	}
	sqlDB, err := db.DB()
	if err != nil {
		fatal(err.Error())
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	if err := db.Exec("SET statement_timeout='300s'; SET jit=off").Error; err != nil {
		fatal(err.Error())
	}
	codec, err := repairCodec()
	if err != nil {
		fatal(err.Error())
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fatal(err.Error())
	}
	w := bufio.NewWriter(f)
	classes := map[string]int{}
	for start := 0; start < len(ids); start += *batch {
		end := min(start+*batch, len(ids))
		plans, counts, err := planWithdrawAddressBatch(db, codec, ids[start:end])
		if err != nil {
			fatal(err.Error())
		}
		for _, p := range plans {
			if _, err := w.WriteString(waPlanLine(p)); err != nil {
				fatal(err.Error())
			}
		}
		for k, v := range counts {
			classes[k] += v
		}
		fmt.Fprintf(os.Stderr, "planned %d/%d %s\n", end, len(ids), jsonClasses(classes))
	}
	if err := w.Flush(); err != nil {
		fatal(err.Error())
	}
	if err := f.Sync(); err != nil {
		fatal(err.Error())
	}
	if err := f.Close(); err != nil {
		fatal(err.Error())
	}
	fmt.Println(jsonClasses(classes))
	return 0
}

func jsonClasses(m map[string]int) string { b, _ := json.Marshal(m); return string(b) }

func readMessageIDs(path string) ([]uint, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seen := map[uint]bool{}
	var ids []uint
	for _, line := range strings.Fields(string(b)) {
		v, err := strconv.ParseUint(line, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad message id %q", line)
		}
		if !seen[uint(v)] {
			seen[uint(v)] = true
			ids = append(ids, uint(v))
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// waLoad reads the batch's messages, stored logs, stored rows and their
// fingerprints. Every lookup is a per-id LATERAL on its indexed key.
func waLoad(db *gorm.DB, ids []uint) (map[uint]waMessage, map[uint]*txlog.LogMessage, map[uint][]tax.TaxableEvent, map[uint]waFinger, error) {
	arr := idArray(ids)
	var msgs []waMessage
	err := db.Raw(`SELECT m.id, m.message_type_id, mt.message_type AS type, m.message_bytes, t.code, b.height, b.time_stamp AS stamp, t.hash
FROM unnest(?::bigint[]) u(id)
CROSS JOIN LATERAL (SELECT m.id, m.tx_id, m.message_type_id, m.message_bytes FROM messages m WHERE m.id = u.id OFFSET 0) m
CROSS JOIN LATERAL (SELECT mt.message_type FROM message_types mt WHERE mt.id = m.message_type_id OFFSET 0) mt
CROSS JOIN LATERAL (SELECT t.code, t.hash, t.block_id FROM txes t WHERE t.id = m.tx_id OFFSET 0) t
CROSS JOIN LATERAL (SELECT b.height, b.time_stamp FROM blocks b WHERE b.id = t.block_id OFFSET 0) b`, arr).Scan(&msgs).Error
	if err != nil {
		return nil, nil, nil, nil, err
	}
	byID := map[uint]waMessage{}
	for _, m := range msgs {
		byID[m.ID] = m
	}
	var attrs []eventRow
	err = db.Raw(`SELECT e.message_id, e.index, et.type, coalesce(k.key, '') AS key, coalesce(a.value, '') AS value, coalesce(a.index, 0) AS attr_index
FROM unnest(?::bigint[]) u(id)
CROSS JOIN LATERAL (SELECT me.id, me.message_id, me.index, me.message_event_type_id FROM message_events me WHERE me.message_id = u.id OFFSET 0) e
CROSS JOIN LATERAL (SELECT et.type FROM message_event_types et WHERE et.id = e.message_event_type_id OFFSET 0) et
LEFT JOIN LATERAL (SELECT a.index, a.value, a.message_event_attribute_key_id FROM message_event_attributes a WHERE a.message_event_id = e.id OFFSET 0) a ON true
LEFT JOIN LATERAL (SELECT k.key FROM message_event_attribute_keys k WHERE k.id = a.message_event_attribute_key_id OFFSET 0) k ON true
ORDER BY e.message_id, e.index, a.index`, arr).Scan(&attrs).Error
	if err != nil {
		return nil, nil, nil, nil, err
	}
	logs := map[uint]*txlog.LogMessage{}
	var lastMsg uint
	var lastIndex uint64
	for _, a := range attrs {
		log := logs[a.MessageID]
		if log == nil {
			log = &txlog.LogMessage{}
			logs[a.MessageID] = log
		}
		if len(log.Events) == 0 || lastMsg != a.MessageID || lastIndex != a.Index {
			log.Events = append(log.Events, txlog.LogMessageEvent{Type: a.Type})
		}
		if a.Key != "" {
			i := len(log.Events) - 1
			log.Events[i].Attributes = append(log.Events[i].Attributes, txlog.Attribute{Key: a.Key, Value: a.Value})
		}
		lastMsg, lastIndex = a.MessageID, a.Index
	}
	var rows []tax.TaxableEvent
	err = db.Raw(`SELECT te.* FROM unnest(?::bigint[]) u(id)
CROSS JOIN LATERAL (SELECT * FROM taxable_events t WHERE t.message_id = u.id OFFSET 0) te
ORDER BY te.message_id, te.sub_index`, arr).Scan(&rows).Error
	if err != nil {
		return nil, nil, nil, nil, err
	}
	stored := map[uint][]tax.TaxableEvent{}
	for _, r := range rows {
		stored[r.MessageID] = append(stored[r.MessageID], r)
	}
	var fps []waFinger
	err = db.Raw(`SELECT u.id AS message_id, f.n, f.max_sub, f.finger FROM unnest(?::bigint[]) u(id)
CROSS JOIN LATERAL (SELECT count(*) AS n, coalesce(max(te.sub_index), -1) AS max_sub, coalesce(`+waFingerSQL+`, '') AS finger
  FROM (SELECT * FROM taxable_events t WHERE t.message_id = u.id OFFSET 0) te) f`, arr).Scan(&fps).Error
	if err != nil {
		return nil, nil, nil, nil, err
	}
	fingers := map[uint]waFinger{}
	for _, f := range fps {
		fingers[f.MessageID] = f
	}
	return byID, logs, stored, fingers, nil
}

func planWithdrawAddressBatch(db *gorm.DB, codec codectypes.AnyUnpacker, ids []uint) ([]waPlanRow, map[string]int, error) {
	msgs, logs, stored, fingers, err := waLoad(db, ids)
	if err != nil {
		return nil, nil, err
	}
	counts := map[string]int{}
	var out []waPlanRow
	for _, id := range ids {
		m, ok := msgs[id]
		if !ok {
			return nil, nil, fmt.Errorf("message %d not found", id)
		}
		class, rows := planWithdrawAddressMessage(codec, m, logs[id], stored[id])
		counts[class]++
		for _, r := range rows {
			out = append(out, waPlanRow{TaxableEvent: r, Class: class, Trigger: waTrigger(r), StoredRows: fingers[id].N, StoredFinger: fingers[id].Finger, MessageTypeID: m.MessageTypeID})
		}
		if len(rows) != 0 && fingers[id].MaxSub+1 != fingers[id].N {
			return nil, nil, fmt.Errorf("message %d fingerprint read disagrees with its rows", id)
		}
	}
	return out, counts, nil
}

func waTrigger(e tax.TaxableEvent) string {
	var d map[string]string
	_ = json.Unmarshal([]byte(e.Detail), &d)
	return d["reward_trigger"]
}

// planWithdrawAddressMessage runs the production parser on the stored
// message and returns the withdraw_address rows it appends after the rows
// already stored, numbered from the next free sub_index.
func planWithdrawAddressMessage(codec codectypes.AnyUnpacker, m waMessage, log *txlog.LogMessage, stored []tax.TaxableEvent) (string, []tax.TaxableEvent) {
	switch {
	case m.ID == privateProfileMessageID:
		return waPrivate, nil
	case m.Code != 0:
		return waFailedTx, nil
	case log == nil:
		return waParseError, nil
	}
	already := false
	for _, r := range stored {
		if isMove(r) {
			already = true
		}
	}
	for i, r := range stored {
		if r.SubIndex != i && !already {
			return waGap, nil
		}
	}
	var msg sdk.Msg
	var err error
	switch {
	case len(m.MessageBytes) != 0:
		err = codec.UnpackAny(&codectypes.Any{TypeUrl: m.Type, Value: m.MessageBytes}, &msg)
	case m.Type == typeURLAuthzExec && already:
		return waAlready, nil
	case m.Type == typeURLAuthzExec:
		return waNoBody, nil
	default:
		msg, err = tax.ReconstructMsg(m.Type, log)
	}
	if err != nil && already {
		return waAlready, nil
	}
	if err != nil {
		return waParseError, nil
	}
	fresh, err := parse(msg, log)
	if err != nil && already {
		return waAlready, nil
	}
	if err != nil {
		return waParseError, nil
	}
	// The parser appends the moves last; anything after them is a parser
	// change this plan was not written for.
	base, moves, ok := splitMoves(fresh)
	if !ok {
		return waParseError, nil
	}
	if already {
		_, storedMoves, ok := splitMoves(stored)
		switch {
		case !ok:
			return waAlreadyMismatch, nil
		case sameContent(fresh, stored):
			return waAlreadyExact, nil
		case sameContent(moves, storedMoves):
			return waAlreadyMoves, nil
		}
		return waAlreadyMismatch, nil
	}
	if len(moves) == 0 {
		return waNoMoves, nil
	}
	class := waExact
	if !sameContent(base, stored) {
		if !sameRewards(base, stored) {
			return waRewardsDrift, nil
		}
		class = waRewardsEqual
	}
	for i := range moves {
		tax.BoundIndexedFields(&moves[i])
		moves[i].ID = 0
		moves[i].MessageID = m.ID
		moves[i].SubIndex = len(stored) + i
		moves[i].BlockHeight = m.Height
		moves[i].Timestamp = m.Stamp
		moves[i].TxHash = m.Hash
	}
	return class, moves
}

func isMove(e tax.TaxableEvent) bool {
	return e.RewardTrigger == tax.TriggerWithdrawAddress && e.Category == string(tax.CategoryTransfer)
}

// splitMoves separates the withdraw_address moves from the other rows and
// reports whether the moves are all at the end.
func splitMoves(rows []tax.TaxableEvent) (base, moves []tax.TaxableEvent, ok bool) {
	for _, e := range rows {
		if isMove(e) {
			moves = append(moves, e)
			continue
		}
		if len(moves) != 0 {
			return nil, nil, false
		}
		base = append(base, e)
	}
	return base, moves, true
}

// sameContent compares what IndexMessage writes from the parser's output,
// leaving out what it takes from the block and transaction.
func sameContent(fresh, stored []tax.TaxableEvent) bool {
	if len(fresh) != len(stored) {
		return false
	}
	for i, y := range stored {
		x := fresh[i]
		tax.BoundIndexedFields(&x)
		if x.Category != y.Category || x.Amount != y.Amount || x.Denom != y.Denom || x.Asset != y.Asset || x.FromAddr != y.FromAddr || x.ToAddr != y.ToAddr ||
			x.ValidatorAddress != y.ValidatorAddress || x.RewardTrigger != y.RewardTrigger || x.Detail != y.Detail {
			return false
		}
	}
	return true
}

// sameRewards compares the booked rewards per receiving address and denom,
// which is all the moves are capped at.
func sameRewards(fresh, stored []tax.TaxableEvent) bool {
	sum := func(rows []tax.TaxableEvent) map[string]string {
		out := map[string]*big.Int{}
		for _, r := range rows {
			if r.Category != categoryReward {
				continue
			}
			v, ok := new(big.Int).SetString(r.Amount, 10)
			if !ok {
				return nil
			}
			k := r.ToAddr + "\x00" + r.Denom
			if out[k] == nil {
				out[k] = new(big.Int)
			}
			out[k].Add(out[k], v)
		}
		s := map[string]string{}
		for k, v := range out {
			s[k] = v.String()
		}
		return s
	}
	a, b := sum(fresh), sum(stored)
	if a == nil || b == nil || len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// waPlanLine is one COPY text line. Every text column is escaped for COPY.
func waPlanLine(p waPlanRow) string {
	esc := func(s string) string {
		r := strings.NewReplacer("\\", "\\\\", "\t", "\\t", "\n", "\\n", "\r", "\\r")
		return r.Replace(s)
	}
	cols := []string{
		strconv.FormatUint(uint64(p.MessageID), 10), strconv.Itoa(p.SubIndex), esc(p.Category), esc(p.Amount), esc(p.Denom), esc(p.Asset),
		esc(p.FromAddr), esc(p.ToAddr), strconv.FormatInt(p.BlockHeight, 10), p.Timestamp.UTC().Format(time.RFC3339Nano), esc(p.TxHash),
		esc(p.ValidatorAddress), esc(p.RewardTrigger), esc(p.Detail), p.Class, esc(p.Trigger), strconv.Itoa(p.StoredRows), p.StoredFinger,
		strconv.FormatUint(uint64(p.MessageTypeID), 10),
	}
	return strings.Join(cols, "\t") + "\n"
}
