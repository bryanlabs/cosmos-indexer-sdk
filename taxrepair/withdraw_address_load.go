package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	dbpkg "github.com/DefiantLabs/cosmos-indexer/db"
	"gorm.io/gorm"
)

// The withdraw-address plan is loaded insert-only, in parts of whole
// messages by message id, each part in its own transaction. A part locks its
// messages rows FOR UPDATE in id order exactly as applyPlans does, and that
// row lock is what keeps a taxrepair run (which deletes and reinserts a
// message's rows) and a load of the same message from interleaving.
// applyPlans does not take the advisory lock; waLoadLockKey only keeps two
// loader runs (this one and message-bodies-load) apart.
//
// Every part is checked inside Postgres against the message's own stored
// data before anything is written: the message still holds exactly the rows
// the plan was made against (count and fingerprint), holds no move yet, the
// delegator's booked reward in that denom covers each planned move, and the
// distribution module paid at least that much to the recipient in the
// message's stored transfer events. A message that already holds a move is
// skipped, so a rerun inserts nothing.
//
// --check runs only those reads on a read-only connection (the production
// dry run): no temporary table, no row lock, nothing written.

const waLoadLockKey = "taxable_events withdraw_address insert"

// waPlanParam names the part's plan json in every query.
const waPlanParam = "plan"

// waLoadPart is one transaction's worth of plan: whole messages, in message
// id order.
type waLoadPart struct {
	N     int
	First uint
	Last  uint
	Lines [][]byte
	Rows  []waLoadRow
	Sum   string
}

type waLoadRow struct {
	MessageID        uint      `json:"message_id"`
	SubIndex         int       `json:"sub_index"`
	Category         string    `json:"category"`
	Amount           string    `json:"amount"`
	Denom            string    `json:"denom"`
	Asset            string    `json:"asset"`
	FromAddr         string    `json:"from_addr"`
	ToAddr           string    `json:"to_addr"`
	BlockHeight      int64     `json:"block_height"`
	Timestamp        time.Time `json:"timestamp"`
	TxHash           string    `json:"tx_hash"`
	ValidatorAddress string    `json:"validator_address"`
	RewardTrigger    string    `json:"reward_trigger"`
	Detail           string    `json:"detail"`
	Class            string    `json:"class"`
	Trigger          string    `json:"trigger"`
	StoredRows       int       `json:"stored_rows"`
	StoredFinger     string    `json:"stored_finger"`
}

// waIDs are the event type and attribute key ids the paid check reads,
// resolved by name on the connection that runs it.
type waIDs struct {
	Transfer  uint
	Recipient uint
	Sender    uint
	Amount    uint
}

// waVerdict is what the check query found for one part.
type waVerdict struct {
	Messages  int
	Rows      int
	Shape     int
	Numbering int
	Moved     int
	State     int
	Booked    int
	Paid      int
	MovedIDs  string
}

func (v waVerdict) failures() int { return v.Shape + v.Numbering + v.State + v.Booked + v.Paid }

var waCopyUnescape = strings.NewReplacer(`\\`, `\`, `\t`, "\t", `\n`, "\n", `\r`, "\r")

// readWithdrawAddressPlan reads a plan file into parts of at most perPart
// messages. sum is the sha256 of the whole file as read, which is the bytes
// that get loaded.
func readWithdrawAddressPlan(path string, perPart int) (sum string, parts []waLoadPart, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	whole := sha256.Sum256(raw)
	type msg struct {
		lines [][]byte
		rows  []waLoadRow
	}
	byID := map[uint]*msg{}
	for i, line := range bytes.Split(raw, []byte("\n")) {
		if len(line) == 0 {
			if i == len(bytes.Split(raw, []byte("\n")))-1 {
				continue
			}
			return "", nil, fmt.Errorf("plan line %d is empty", i+1)
		}
		r, err := parseWaPlanLine(string(line))
		if err != nil {
			return "", nil, fmt.Errorf("plan line %d: %w", i+1, err)
		}
		m := byID[r.MessageID]
		if m == nil {
			m = &msg{}
			byID[r.MessageID] = m
		}
		m.lines = append(m.lines, line)
		m.rows = append(m.rows, r)
	}
	if len(byID) == 0 {
		return "", nil, errors.New("empty plan")
	}
	ids := make([]uint, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for start := 0; start < len(ids); start += perPart {
		end := min(start+perPart, len(ids))
		p := waLoadPart{N: len(parts) + 1, First: ids[start], Last: ids[end-1]}
		h := sha256.New()
		for _, id := range ids[start:end] {
			m := byID[id]
			for _, l := range m.lines {
				h.Write(l)
				h.Write([]byte("\n"))
			}
			p.Lines = append(p.Lines, m.lines...)
			p.Rows = append(p.Rows, m.rows...)
		}
		p.Sum = hex.EncodeToString(h.Sum(nil))
		parts = append(parts, p)
	}
	return hex.EncodeToString(whole[:]), parts, nil
}

func parseWaPlanLine(line string) (waLoadRow, error) {
	f := strings.Split(line, "\t")
	if len(f) != 19 {
		return waLoadRow{}, fmt.Errorf("%d columns, want 19", len(f))
	}
	for i := range f {
		f[i] = waCopyUnescape.Replace(f[i])
	}
	var r waLoadRow
	id, err := strconv.ParseUint(f[0], 10, 64)
	if err != nil {
		return r, err
	}
	r.MessageID = uint(id)
	if r.SubIndex, err = strconv.Atoi(f[1]); err != nil {
		return r, err
	}
	r.Category, r.Amount, r.Denom, r.Asset, r.FromAddr, r.ToAddr = f[2], f[3], f[4], f[5], f[6], f[7]
	if r.BlockHeight, err = strconv.ParseInt(f[8], 10, 64); err != nil {
		return r, err
	}
	if r.Timestamp, err = time.Parse(time.RFC3339Nano, f[9]); err != nil {
		return r, err
	}
	r.TxHash, r.ValidatorAddress, r.RewardTrigger, r.Detail, r.Class, r.Trigger = f[10], f[11], f[12], f[13], f[14], f[15]
	if r.StoredRows, err = strconv.Atoi(f[16]); err != nil {
		return r, err
	}
	r.StoredFinger = f[17]
	return r, nil
}

// resolveWaIDs reads the ids by name. Each name is unique in its table.
func resolveWaIDs(tx *gorm.DB) (waIDs, error) {
	var got []struct {
		Kind string
		Name string
		ID   uint
	}
	err := tx.Raw(`SELECT 'type' AS kind, type AS name, id FROM message_event_types WHERE type = 'transfer'
UNION ALL SELECT 'key', key, id FROM message_event_attribute_keys WHERE key IN ('recipient', 'sender', 'amount')`).Scan(&got).Error
	if err != nil {
		return waIDs{}, err
	}
	seen := map[string]uint{}
	for _, g := range got {
		k := g.Kind + ":" + g.Name
		if _, dup := seen[k]; dup {
			return waIDs{}, fmt.Errorf("%s names more than one id", k)
		}
		seen[k] = g.ID
	}
	ids := waIDs{Transfer: seen["type:transfer"], Recipient: seen["key:recipient"], Sender: seen["key:sender"], Amount: seen["key:amount"]}
	if ids.Transfer == 0 || ids.Recipient == 0 || ids.Sender == 0 || ids.Amount == 0 {
		return waIDs{}, fmt.Errorf("event ids not all found: %v", seen)
	}
	return ids, nil
}

// waPlanRecords is the part's plan as a jsonb recordset; the check, insert
// and post check all read it.
const waPlanRecords = `jsonb_to_recordset(CAST(@plan AS jsonb)) AS p(message_id bigint, sub_index bigint, category text, amount text, denom text,
 asset text, from_addr text, to_addr text, block_height bigint, "timestamp" timestamptz, tx_hash text, validator_address text,
 reward_trigger text, detail text, class text, stored_rows bigint, stored_finger text)`

// waCheckSQL finds, for one part, every reason not to load it. Every lookup
// is a per-message LATERAL on an indexed key behind OFFSET 0.
var waCheckSQL = `WITH p AS MATERIALIZED (SELECT * FROM ` + waPlanRecords + `),
w AS MATERIALIZED (
  SELECT message_id, count(*) AS n, min(sub_index) AS lo, max(sub_index) AS hi,
         min(stored_rows) AS stored_rows, max(stored_rows) AS stored_rows2, min(stored_finger) AS finger, max(stored_finger) AS finger2,
         min(block_height) AS h, max(block_height) AS h2, min(tx_hash) AS hash, max(tx_hash) AS hash2, min("timestamp") AS ts, max("timestamp") AS ts2
    FROM p GROUP BY message_id),
now AS MATERIALIZED (
  SELECT w.message_id, s.*
    FROM w CROSS JOIN LATERAL (
      SELECT count(*) AS n_now, coalesce(max(te.sub_index), -1) AS max_sub, coalesce(` + waFingerSQL + `, '') AS finger_now,
             count(*) FILTER (WHERE te.reward_trigger = 'withdraw_address') AS moved,
             min(te.block_height) AS block_height, min(te.tx_hash) AS tx_hash, min(te."timestamp") AS stamp
        FROM (SELECT * FROM taxable_events t WHERE t.message_id = w.message_id OFFSET 0) te) s),
todo AS MATERIALIZED (SELECT p.* FROM p JOIN now n USING (message_id) WHERE n.moved = 0),
c AS MATERIALIZED (SELECT message_id, from_addr, to_addr, denom, sum(amount::numeric) AS planned FROM todo GROUP BY 1, 2, 3, 4),
booked AS (
  SELECT c.message_id, c.from_addr, c.denom, max(b.booked) AS booked, sum(c.planned) AS planned
    FROM c CROSS JOIN LATERAL (
      SELECT coalesce(sum(te.amount::numeric), 0) AS booked
        FROM (SELECT * FROM taxable_events t WHERE t.message_id = c.message_id OFFSET 0) te
       WHERE te.category = 'reward' AND te.to_addr = c.from_addr AND te.denom = c.denom) b
   GROUP BY 1, 2, 3),
paid AS (
  SELECT c.message_id, c.to_addr, c.denom, c.planned, coalesce(pd.paid, 0) AS paid, pd.triples
    FROM (SELECT message_id, to_addr, denom, sum(planned) AS planned FROM c GROUP BY 1, 2, 3) c
    CROSS JOIN LATERAL (
      SELECT sum(substring(coin FROM '^([0-9]+)')::numeric) FILTER (WHERE substring(coin FROM '^[0-9]+(.*)$') = c.denom) AS paid,
             count(DISTINCT r.id) AS triples
        FROM (SELECT e.id FROM message_events e WHERE e.message_id = c.message_id AND e.message_event_type_id = @transfer OFFSET 0) e
        CROSS JOIN LATERAL (SELECT a.index FROM message_event_attributes a
                             WHERE a.message_event_id = e.id AND a.message_event_attribute_key_id = @recipient AND a.value = c.to_addr OFFSET 0) rcpt
        CROSS JOIN LATERAL (SELECT a.id FROM message_event_attributes a
                             WHERE a.message_event_id = e.id AND a.index = rcpt.index + 1 AND a.message_event_attribute_key_id = @sender
                               AND a.value = @module OFFSET 0) r
        CROSS JOIN LATERAL (SELECT a.value FROM message_event_attributes a
                             WHERE a.message_event_id = e.id AND a.index = rcpt.index + 2 AND a.message_event_attribute_key_id = @amount OFFSET 0) amt
        CROSS JOIN LATERAL string_to_table(amt.value, ',') AS coin) pd)
SELECT (SELECT count(*) FROM w) AS messages, (SELECT count(*) FROM p) AS rows,
  (SELECT count(*) FROM p
    WHERE category IS DISTINCT FROM 'transfer' OR reward_trigger IS DISTINCT FROM 'withdraw_address' OR amount !~ '^[1-9][0-9]*$'
       OR coalesce(denom, '') = '' OR coalesce(asset, '') <> '' OR coalesce(validator_address, '') <> ''
       OR from_addr !~ '^cosmos1[0-9a-z]{38,}$' OR to_addr !~ '^cosmos1[0-9a-z]{38,}$' OR from_addr = to_addr
       OR detail !~ '^\{"protocol":"distribution","reward_trigger":"[a-z_]+"\}$'
       OR class NOT IN ('exact', 'rewards_equal') OR message_id = @private) AS shape,
  (SELECT count(*) FROM w
    WHERE stored_rows <> stored_rows2 OR finger <> finger2 OR lo <> stored_rows OR hi <> stored_rows + n - 1
       OR h <> h2 OR hash <> hash2 OR ts <> ts2) AS numbering,
  (SELECT count(*) FROM now WHERE moved <> 0) AS moved,
  (SELECT count(*) FROM now n JOIN w USING (message_id)
    WHERE n.moved = 0 AND (n.n_now <> w.stored_rows OR n.max_sub <> w.stored_rows - 1 OR n.finger_now <> w.finger
       OR n.block_height IS DISTINCT FROM w.h OR n.tx_hash IS DISTINCT FROM w.hash OR n.stamp IS DISTINCT FROM w.ts)) AS state,
  (SELECT count(*) FROM booked WHERE planned > booked) AS booked,
  (SELECT count(*) FROM paid WHERE triples = 0 OR planned > paid) AS paid,
  (SELECT coalesce(string_agg(message_id::text, ',' ORDER BY message_id), '') FROM now WHERE moved <> 0) AS moved_ids`

const waInsertSQL = `INSERT INTO taxable_events (message_id, sub_index, category, amount, denom, asset, from_addr, to_addr, block_height,
 "timestamp", tx_hash, validator_address, reward_trigger, detail)
SELECT p.message_id, p.sub_index, p.category, p.amount, p.denom, coalesce(p.asset, ''), p.from_addr, p.to_addr, p.block_height,
       p."timestamp", p.tx_hash, coalesce(p.validator_address, ''), p.reward_trigger, p.detail
  FROM ` + waPlanRecords + ` ORDER BY p.message_id, p.sub_index`

// waPostSQL counts the inserted rows that read back equal and the messages
// whose earlier rows changed.
var waPostSQL = `WITH p AS MATERIALIZED (SELECT * FROM ` + waPlanRecords + `),
w AS MATERIALIZED (SELECT message_id, min(stored_rows) AS stored_rows, min(stored_finger) AS finger FROM p GROUP BY message_id)
SELECT (SELECT count(*) FROM p CROSS JOIN LATERAL (
          SELECT * FROM taxable_events t WHERE t.message_id = p.message_id AND t.sub_index = p.sub_index OFFSET 0) te
         WHERE (te.category, te.amount, te.denom, te.asset, te.from_addr, te.to_addr, te.block_height, te."timestamp", te.tx_hash,
                te.validator_address, te.reward_trigger, te.detail)
               IS NOT DISTINCT FROM (p.category, p.amount, p.denom, coalesce(p.asset, ''), p.from_addr, p.to_addr, p.block_height, p."timestamp",
                p.tx_hash, coalesce(p.validator_address, ''), p.reward_trigger, p.detail)) AS equal,
       (SELECT count(*) FROM w CROSS JOIN LATERAL (
          SELECT count(*) AS k, coalesce(` + waFingerSQL + `, '') AS finger
            FROM (SELECT * FROM taxable_events t WHERE t.message_id = w.message_id OFFSET 0) te WHERE te.sub_index < w.stored_rows) s
         WHERE s.k <> w.stored_rows OR s.finger <> w.finger) AS changed,
       (SELECT count(*) FROM w CROSS JOIN LATERAL (
          SELECT count(*) AS k FROM taxable_events t WHERE t.message_id = w.message_id) s
         WHERE s.k <> w.stored_rows + (SELECT count(*) FROM p WHERE p.message_id = w.message_id)) AS extra`

func waArgs(planJSON []byte, ids waIDs) map[string]any {
	return map[string]any{
		waPlanParam: string(planJSON), "transfer": ids.Transfer, "recipient": ids.Recipient, "sender": ids.Sender, "amount": ids.Amount,
		"module": distributionModule, "private": privateProfileMessageID,
	}
}

// distributionModule is the Cosmos Hub distribution module account.
const distributionModule = "cosmos1jv65s3grqf6v6jl3dp4t6c9t9rk99cd88lyufl"

func checkWaPart(tx *gorm.DB, p waLoadPart, ids waIDs) (waVerdict, error) {
	planJSON, err := json.Marshal(p.Rows)
	if err != nil {
		return waVerdict{}, err
	}
	var v waVerdict
	err = tx.Raw(waCheckSQL, waArgs(planJSON, ids)).Scan(&v).Error
	return v, err
}

// waSeqScan matches a planned sequential scan on a table too big to read.
var waSeqScan = regexp.MustCompile(`Seq Scan on (taxable_events|message_events|message_event_attributes|messages|txes|blocks)\b`)

// explainWaPart returns the check's plan text and refuses a sequential scan
// on a big table.
func explainWaPart(tx *gorm.DB, p waLoadPart, ids waIDs) (string, error) {
	planJSON, err := json.Marshal(p.Rows)
	if err != nil {
		return "", err
	}
	rows, err := tx.Raw("EXPLAIN "+waCheckSQL, waArgs(planJSON, ids)).Rows()
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return "", err
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	text := strings.Join(lines, "\n")
	if m := waSeqScan.FindString(text); m != "" {
		return text, fmt.Errorf("check plans a %s", m)
	}
	return text, nil
}

// loadWaPart loads one part in its own transaction. With apply false it
// rolls back after every check and the post check.
func loadWaPart(db *gorm.DB, p waLoadPart, apply bool) (v waVerdict, inserted int, err error) {
	errRollback := errors.New("dry run")
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL statement_timeout = '900s'; SET LOCAL lock_timeout = '10s'; SET LOCAL TimeZone = 'UTC'; SET LOCAL jit = off").Error; err != nil {
			return err
		}
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", waLoadLockKey).Error; err != nil {
			return err
		}
		msgIDs := waMessageIDs(p.Rows)
		var locked []uint
		if err := tx.Raw("SELECT id FROM messages WHERE id IN ? ORDER BY id FOR UPDATE", msgIDs).Scan(&locked).Error; err != nil {
			return err
		}
		if len(locked) != len(msgIDs) {
			return fmt.Errorf("part %d: %d of %d planned messages exist", p.N, len(locked), len(msgIDs))
		}
		var n int
		if err := tx.Raw(`SELECT count(*) FROM unnest(?::bigint[]) u(id)
 CROSS JOIN LATERAL (SELECT t.id FROM taxable_events t WHERE t.message_id = u.id OFFSET 0 FOR UPDATE) l`, idArray(msgIDs)).Scan(&n).Error; err != nil {
			return err
		}
		ids, err := resolveWaIDs(tx)
		if err != nil {
			return err
		}
		if v, err = checkWaPart(tx, p, ids); err != nil {
			return err
		}
		if v.failures() != 0 {
			return fmt.Errorf("part %d refused: %+v", p.N, v)
		}
		todo := waWithoutMoved(p.Rows, v.MovedIDs)
		if len(todo) != 0 {
			planJSON, err := json.Marshal(todo)
			if err != nil {
				return err
			}
			res := tx.Exec(waInsertSQL, map[string]any{waPlanParam: string(planJSON)})
			if res.Error != nil {
				return res.Error
			}
			inserted = int(res.RowsAffected)
			var post struct{ Equal, Changed, Extra int }
			if err := tx.Raw(waPostSQL, map[string]any{waPlanParam: string(planJSON)}).Scan(&post).Error; err != nil {
				return err
			}
			if inserted != len(todo) || post.Equal != len(todo) || post.Changed != 0 || post.Extra != 0 {
				return fmt.Errorf("part %d post check: inserted %d of %d, %d read back equal, %d messages changed, %d with other rows", p.N, inserted, len(todo), post.Equal, post.Changed, post.Extra)
			}
		}
		if !apply {
			return errRollback
		}
		return nil
	})
	if errors.Is(err, errRollback) {
		err = nil
	}
	return v, inserted, err
}

func waMessageIDs(rows []waLoadRow) []uint {
	var ids []uint
	for i, r := range rows {
		if i == 0 || r.MessageID != rows[i-1].MessageID {
			ids = append(ids, r.MessageID)
		}
	}
	return ids
}

func waWithoutMoved(rows []waLoadRow, movedIDs string) []waLoadRow {
	moved := map[string]bool{}
	for _, id := range strings.Split(movedIDs, ",") {
		if id != "" {
			moved[id] = true
		}
	}
	var out []waLoadRow
	for _, r := range rows {
		if !moved[strconv.FormatUint(uint64(r.MessageID), 10)] {
			out = append(out, r)
		}
	}
	return out
}

func runWithdrawAddressLoad(args []string) int {
	fs := flag.NewFlagSet("withdraw-address-load", flag.ExitOnError)
	planPath := fs.String("plan", "", "plan file written by withdraw-address-plan")
	perPart := fs.Int("part-messages", 2000, "messages per part, each loaded in its own transaction")
	check := fs.Bool("check", false, "read-only check of every part on a read-only connection; nothing is locked or written")
	explain := fs.Bool("explain", false, "with --check, print the first part's query plan and refuse a sequential scan on a big table first")
	apply := fs.Bool("apply", false, "commit each part (requires --sha)")
	want := fs.String("sha", "", "sha256 of the plan file the dry run reported; apply refuses any other file")
	_ = fs.Parse(args)
	if *planPath == "" || *perPart < 1 || *perPart > 20000 || (*apply && (*check || *want == "")) {
		fatal("withdraw-address-load needs --plan, 1 to 20000 --part-messages, and --apply only with --sha and without --check")
	}
	sum, parts, err := readWithdrawAddressPlan(*planPath, *perPart)
	if err != nil {
		fatal(err.Error())
	}
	fmt.Printf("plan sha256 %s, %d parts\n", sum, len(parts))
	if *apply && sum != *want {
		fatal("the plan file's sha256 is not the one --sha names")
	}
	params := []string{"application_name=withdraw-address-load"}
	if *check {
		params = append(params, "default_transaction_read_only=on")
	}
	db, err := dbpkg.PostgresDbConnect(env("DB_HOST"), envDefault("DB_PORT", "5432"), env("DB_NAME"), env("DB_USER"), os.Getenv("DB_PASS"), "silent", params...)
	if err != nil {
		fatal(err.Error())
	}
	sqlDB, err := db.DB()
	if err != nil {
		fatal(err.Error())
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	if *check {
		return runWaCheck(db, parts, *explain)
	}
	total := 0
	for _, p := range parts {
		start := time.Now()
		v, n, err := loadWaPart(db, p, *apply)
		if err != nil {
			fatal(err.Error())
		}
		total += n
		fmt.Printf("part %d messages %d..%d sha256 %s: %d messages, %d rows, %d already moved, %d inserted, %.1fs\n",
			p.N, p.First, p.Last, p.Sum, v.Messages, v.Rows, v.Moved, n, time.Since(start).Seconds())
	}
	if *apply {
		fmt.Printf("committed %d rows\n", total)
	} else {
		fmt.Printf("dry run: %d rows would be inserted, every part rolled back\n", total)
	}
	return 0
}

func runWaCheck(db *gorm.DB, parts []waLoadPart, explain bool) int {
	failed := 0
	for i, p := range parts {
		start := time.Now()
		var v waVerdict
		err := db.Transaction(func(tx *gorm.DB) error {
			var ro string
			if err := tx.Raw("SHOW transaction_read_only").Scan(&ro).Error; err != nil || ro != "on" {
				return fmt.Errorf("check transaction is not read only (%q, %v)", ro, err)
			}
			if err := tx.Exec("SET LOCAL statement_timeout = '900s'; SET LOCAL TimeZone = 'UTC'; SET LOCAL jit = off").Error; err != nil {
				return err
			}
			ids, err := resolveWaIDs(tx)
			if err != nil {
				return err
			}
			if explain && i == 0 {
				text, err := explainWaPart(tx, p, ids)
				fmt.Println(text)
				if err != nil {
					return err
				}
			}
			v, err = checkWaPart(tx, p, ids)
			return err
		})
		if err != nil {
			fatal(err.Error())
		}
		if v.failures() != 0 {
			failed++
		}
		fmt.Printf("part %d messages %d..%d sha256 %s: %d messages, %d rows, %d already moved, refused shape %d numbering %d state %d booked %d paid %d, %.1fs\n",
			p.N, p.First, p.Last, p.Sum, v.Messages, v.Rows, v.Moved, v.Shape, v.Numbering, v.State, v.Booked, v.Paid, time.Since(start).Seconds())
	}
	if failed != 0 {
		fmt.Printf("%d parts would be refused\n", failed)
		return 1
	}
	return 0
}
