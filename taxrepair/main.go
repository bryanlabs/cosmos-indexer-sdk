// taxrepair reclassifies stored staking messages with the production parser.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"hash"
	"io"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	dbpkg "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/probe"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// urls are every message type the production parser classifies and that can
// be replayed from the stored event log: the event-only types ReconstructMsg
// rebuilds, authz (handled event by event), validator commission (kept
// from the stored rows with the operator address verified) and the pre-v24
// SDK fork's tokenize-share reward messages, which are always stored with
// their bytes.
var urls = append(append([]string{}, tax.ReplayableTypeURLs...), "/cosmos.distribution.v1beta1.MsgWithdrawValidatorCommission", "/cosmos.authz.v1beta1.MsgExec",
	tax.LegacyTypeURLRedeemTokensForShares, tax.LegacyTypeURLWithdrawTokenizeShareReward, tax.LegacyTypeURLWithdrawAllTokenizeShareReward)

type row struct {
	models.Message
	Type   string
	Height int64
	Stamp  time.Time
	Hash   string
}
type eventRow struct {
	MessageID uint
	Index     uint64
	Type      string
	Key       string
	Value     string
	AttrIndex uint64
}

// backup holds what a restore needs: the rows as they were and the rows
// written. The event log is not copied; it is immutable in the database.
type backup struct {
	MessageID uint               `json:"message_id"`
	Type      string             `json:"message_type"`
	Bytes     string             `json:"message_bytes,omitempty"`
	Old       []tax.TaxableEvent `json:"old"`
	Expected  []tax.TaxableEvent `json:"expected"`
	// ClearedParserErrors are the message's parser error rows that a
	// --clear-parser-errors run deletes because the production parser now
	// classifies the stored body. A record whose expected rows equal its old
	// rows only clears errors and leaves the tax rows alone.
	ClearedParserErrors []parserErrorRow `json:"cleared_parser_errors,omitempty"`
}

// parserErrorRow is one message_parser_errors row, kept whole in the backup so
// a cleared error can be restored as it was.
type parserErrorRow struct {
	ID              uint   `json:"id"`
	MessageID       uint   `json:"message_id"`
	MessageParserID uint   `json:"message_parser_id"`
	Error           string `json:"error"`
}

// totals sums amounts by category, denom and trigger so the before and after
// pictures of a replay can be compared.
type totals map[string]map[string]map[string]string

type backupWriter struct {
	file   *os.File
	digest hash.Hash
	lines  int
}

func newBackup(path string) (*backupWriter, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &backupWriter{file: f, digest: sha256.New()}, nil
}

// persist checks a byte-for-byte durable readback BEFORE database mutation.
func (b *backupWriter) persist(plans []backup) error {
	if b == nil || len(plans) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, p := range plans {
		if err := enc.Encode(p); err != nil {
			return err
		}
	}
	offset, err := b.file.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err = b.file.Write(buf.Bytes()); err != nil {
		return err
	}
	if err = b.file.Sync(); err != nil {
		return err
	}
	check := make([]byte, buf.Len())
	if _, err = b.file.ReadAt(check, offset); err != nil {
		return err
	}
	if !bytes.Equal(check, buf.Bytes()) {
		return fmt.Errorf("backup readback mismatch")
	}
	_, _ = b.digest.Write(check)
	b.lines += len(plans)
	return nil
}

func (b *backupWriter) verify() (string, error) {
	if b == nil {
		return "", nil
	}
	if _, err := b.file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, b.file); err != nil {
		return "", err
	}
	if !bytes.Equal(h.Sum(nil), b.digest.Sum(nil)) {
		return "", fmt.Errorf("backup checksum mismatch")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// defaultSupplementBoundary is the message after the cutoff the wasm
// supplement plan was taken at. The rollout passes the first message the
// new indexer image wrote.
const defaultSupplementBoundary = 138666819

// replayUpper is the inclusive upper message ID of a replay: the one asked
// for, or the message before the boundary. Messages from the boundary on
// were written by an indexer that has the supplement, so a replay that
// reached them would write its rows a second time.
func replayUpper(to, boundary uint) (uint, error) {
	if boundary == 0 {
		return 0, fmt.Errorf("--supplement-boundary is required")
	}
	if to == 0 {
		return boundary - 1, nil
	}
	if to >= boundary {
		return 0, fmt.Errorf("--to-message-id %d reaches the supplement boundary %d", to, boundary)
	}
	return to, nil
}

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) > 1 && os.Args[1] == "withdraw-address-plan" {
		return runWithdrawAddressPlan(os.Args[2:])
	}
	if len(os.Args) > 1 && os.Args[1] == "withdraw-address-load" {
		return runWithdrawAddressLoad(os.Args[2:])
	}
	if len(os.Args) > 1 && os.Args[1] == "message-bodies-load" {
		return runMessageBodiesLoad(os.Args[2:])
	}
	apply := flag.Bool("apply", false, "write repaired rows (requires --backup)")
	path := flag.String("backup", "", "new JSONL backup file, never overwritten")
	addresses := flag.String("addresses", "", "optional comma-separated wallets")
	batch := flag.Int("batch", 500, "bounded messages per batch")
	from := flag.Uint("from-message-id", 0, "exclusive lower message ID for resume")
	to := flag.Uint("to-message-id", 0, "inclusive upper message ID (default the message before --supplement-boundary)")
	boundary := flag.Uint("supplement-boundary", defaultSupplementBoundary, "first message ID an indexer with the wasm supplement wrote; no replay reaches it")
	only := flag.String("categories", "", "optional comma-separated categories; replay only messages holding a stored row in one of them")
	kinds := flag.String("message-types", "", "optional comma-separated message type URLs; replay only messages of those types")
	idsFile := flag.String("message-ids-file", "", "optional file of message ids, one per line; replay only those messages")
	readOnly := flag.Bool("read-only", false, "open the connection with default_transaction_read_only=on (plan only)")
	keepSeqscan := flag.Bool("keep-seqscan", false, "leave enable_seqscan at the server default")
	clearErrors := flag.Bool("clear-parser-errors", false, "also delete the tax parser error of each message the production parser now classifies from its stored body, as the indexer does on a clean parse (backed up)")
	flag.Parse()
	if *batch < 1 || *batch > 5000 {
		fatal("batch must be between 1 and 5000")
	}
	if *apply && *path == "" {
		fatal("--apply requires --backup")
	}
	if *apply && *readOnly {
		fatal("--apply and --read-only conflict")
	}
	var runtime []string
	if *readOnly {
		// A connection parameter, so a reconnect is read-only too.
		runtime = append(runtime, "default_transaction_read_only=on")
	}
	db, err := dbpkg.PostgresDbConnect(env("DB_HOST"), envDefault("DB_PORT", "5432"), env("DB_NAME"), env("DB_USER"), os.Getenv("DB_PASS"), "silent", runtime...)
	if err != nil {
		fatal(err.Error())
	}
	sqlDB, err := db.DB()
	if err != nil {
		fatal(err.Error())
	}
	defer sqlDB.Close()
	// Keep timeout settings on the one connection used for this repair.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	// enable_seqscan=off keeps the planner on the (message_id,index) indexes
	// for the per-batch event loads; with stale statistics it otherwise picks
	// a sequential scan of the attributes table for every batch.
	session := "SET statement_timeout='120s'; SET lock_timeout='3s'"
	if !*keepSeqscan {
		session += "; SET enable_seqscan=off"
	}
	lookups = *keepSeqscan
	if err := db.Exec(session).Error; err != nil {
		fatal(err.Error())
	}
	codec, err := repairCodec()
	if err != nil {
		fatal(err.Error())
	}
	maxID, err := replayUpper(*to, *boundary)
	if err != nil {
		fatal(err.Error())
	}
	if maxID < *from {
		fatal("upper message ID precedes lower ID")
	}
	var wallets []string
	for _, a := range strings.Split(*addresses, ",") {
		if a = strings.TrimSpace(a); a != "" {
			wallets = append(wallets, a)
		}
	}
	var categories []string
	for _, c := range strings.Split(*only, ",") {
		if c = strings.TrimSpace(c); c != "" {
			categories = append(categories, c)
		}
	}
	var types []string
	for _, t := range strings.Split(*kinds, ",") {
		if t = strings.TrimSpace(t); t != "" {
			types = append(types, t)
		}
	}
	var candidates []uint
	if len(wallets) > 0 {
		candidates, err = walletIDs(db, wallets)
		if err != nil {
			fatal(err.Error())
		}
	}
	if len(categories) > 0 {
		ids, err := categoryIDs(db, categories, *from, maxID)
		if err != nil {
			fatal(err.Error())
		}
		if len(wallets) > 0 {
			ids = intersect(candidates, ids)
		}
		candidates = ids
	}
	if len(types) > 0 {
		ids, err := typeIDs(db, types, *from, maxID)
		if err != nil {
			fatal(err.Error())
		}
		if len(wallets) > 0 || len(categories) > 0 {
			ids = intersect(candidates, ids)
		}
		candidates = ids
	}
	if *idsFile != "" {
		ids, err := readIDs(*idsFile, *from, maxID)
		if err != nil {
			fatal(err.Error())
		}
		if len(wallets) > 0 || len(categories) > 0 || len(types) > 0 {
			ids = intersect(candidates, ids)
		}
		candidates = ids
	}
	restricted := len(wallets) > 0 || len(categories) > 0 || len(types) > 0 || *idsFile != ""
	b, err := newBackup(*path)
	if err != nil {
		fatal(err.Error())
	}
	if b != nil {
		defer b.file.Close()
	}
	scanned, changed, zeroed, skipped, cleared := 0, 0, 0, 0, 0
	oldTotal, newTotal := totals{}, totals{}
	last := *from
	fmt.Printf("snapshot_max_message_id=%d apply=%t candidates=%d categories=%s message_types=%s\n", maxID, *apply, len(candidates), strings.Join(categories, ","), strings.Join(types, ","))
	for {
		// A restricted run walks its candidate ids a batch at a time instead of
		// every message id, so sparse candidates never make one load scan
		// millions of messages.
		var chunk []uint
		if restricted {
			for len(candidates) > 0 && candidates[0] <= last {
				candidates = candidates[1:]
			}
			if len(candidates) == 0 {
				break
			}
			chunk = candidates[:min(*batch, len(candidates))]
		}
		rows, err := load(db, last, maxID, *batch, chunk)
		if err != nil {
			fatal(err.Error())
		}
		if len(rows) == 0 {
			if restricted {
				last = chunk[len(chunk)-1]
				continue
			}
			break
		}
		plans, skips, parsed, err := planBatchParsed(db, codec, rows)
		if err != nil {
			fatal(err.Error())
		}
		if *clearErrors {
			if plans, err = withResolvedErrors(db, plans, parsed, rows); err != nil {
				fatal(err.Error())
			}
		}
		for _, sk := range skips {
			fmt.Printf("skip message_id=%d type=%s reason=%s\n", sk.MessageID, sk.Type, sk.Reason)
		}
		skipped += len(skips)
		scanned += len(rows)
		for _, p := range plans {
			for _, e := range p.ClearedParserErrors {
				cleared++
				fmt.Printf("clear message_id=%d parser_error_id=%d\n", e.MessageID, e.ID)
			}
			if same(p.Old, p.Expected) {
				continue
			}
			changed++
			if len(p.Expected) == 0 {
				zeroed++
			}
			add(oldTotal, p.Old)
			add(newTotal, p.Expected)
		}
		if err = b.persist(plans); err != nil {
			fatal(err.Error())
		}
		if *apply {
			if err = applyPlans(db, plans); err != nil {
				fatal(err.Error())
			}
		}
		last = rows[len(rows)-1].ID
		if restricted {
			last = chunk[len(chunk)-1]
		}
		fmt.Printf("last_message_id=%d scanned=%d changed=%d zeroed=%d skipped=%d cleared_parser_errors=%d\n", last, scanned, changed, zeroed, skipped, cleared)
	}
	sum, err := b.verify()
	if err != nil {
		fatal(err.Error())
	}
	if b != nil {
		fmt.Printf("backup=%s sha256=%s records=%d\n", *path, sum, b.lines)
	}
	fmt.Printf("complete=true snapshot_max_message_id=%d scanned=%d changed=%d zeroed=%d skipped=%d cleared_parser_errors=%d apply=%t\nold_changed_totals=%s\nnew_changed_totals=%s\n", maxID, scanned, changed, zeroed, skipped, cleared, *apply, jsonTotals(oldTotal), jsonTotals(newTotal))
	if skipped > 0 {
		// Every skipped message is printed above; the run is incomplete until
		// each one is explained or replayed from an archive node.
		return 3
	}
	return 0
}

// skip records a message the replay could not classify from its stored
// events. It is left untouched and reported, never silently dropped.
type skip struct {
	MessageID uint
	Type      string
	Reason    string
}

// trivialLog reports whether a message emitted nothing beyond the generic
// message and tx events, meaning no funds, shares or tokens moved. A
// no-op IBC receive of an already relayed packet looks like this.
func trivialLog(log *txlog.LogMessage) bool {
	for _, e := range log.Events {
		if e.Type != "message" && e.Type != "tx" {
			return false
		}
	}
	return true
}

func walletIDs(db *gorm.DB, addresses []string) ([]uint, error) {
	// Materialize this once; do not correlate two wallet subqueries against every
	// message in the entire database. Existing events also cover authz bot signers.
	var ids []uint
	err := db.Raw(`SELECT message_id FROM taxable_events WHERE from_addr IN ? OR to_addr IN ?
 UNION SELECT m.id FROM messages m JOIN tx_signer_addresses tsa ON tsa.tx_id=m.tx_id
 JOIN addresses a ON a.id=tsa.address_id WHERE a.address IN ? ORDER BY 1`, addresses, addresses, addresses).Scan(&ids).Error
	return ids, err
}

// categoryIDs lists the messages in (from, maxID] that hold a stored row in
// one of categories, so a repair of one classification leaves every other
// message alone.
func categoryIDs(db *gorm.DB, categories []string, from, maxID uint) ([]uint, error) {
	var ids []uint
	err := db.Raw("SELECT DISTINCT message_id FROM taxable_events WHERE category IN ? AND message_id > ? AND message_id <= ? ORDER BY 1", categories, from, maxID).Scan(&ids).Error
	return ids, err
}

// typeIDs lists the messages in (from, maxID] of one of the given type URLs,
// so a repair of one message kind leaves every other message alone.
func typeIDs(db *gorm.DB, types []string, from, maxID uint) ([]uint, error) {
	var ids []uint
	err := db.Raw("SELECT m.id FROM messages m JOIN message_types mt ON mt.id=m.message_type_id WHERE mt.message_type IN ? AND m.id > ? AND m.id <= ? ORDER BY 1", types, from, maxID).Scan(&ids).Error
	return ids, err
}

// readIDs reads the message ids in (from, maxID] from a file of one id per
// line, sorted and without repeats.
func readIDs(path string, from, maxID uint) ([]uint, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seen := map[uint]bool{}
	var ids []uint
	for _, line := range strings.Fields(string(raw)) {
		v, err := strconv.ParseUint(line, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("message id %q: %w", line, err)
		}
		if id := uint(v); id > from && id <= maxID && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// intersect keeps the ids present in both sorted lists.
func intersect(a, b []uint) []uint {
	var out []uint
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}

// idArray renders ids as one Postgres array literal. Passed as a single
// parameter it keeps the planner from treating hundreds of ids as constants.
func idArray(ids []uint) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// lookups makes every per-batch read walk its ids one index lookup at a
// time, for a session that leaves the planner at its defaults.
var lookups bool

// load reads the next batch of replayable messages after last, only from
// candidates when they are given.
func load(db *gorm.DB, last, maxID uint, n int, candidates []uint) ([]row, error) {
	if lookups && candidates != nil {
		var rows []row
		err := db.Raw(`SELECT m.*,mt.message_type type,b.height,b.time_stamp stamp,t.hash
 FROM unnest(?::bigint[]) c(id)
 CROSS JOIN LATERAL (SELECT * FROM messages WHERE id=c.id OFFSET 0) m
 CROSS JOIN LATERAL (SELECT message_type FROM message_types WHERE id=m.message_type_id OFFSET 0) mt
 CROSS JOIN LATERAL (SELECT hash,code,block_id FROM txes WHERE id=m.tx_id OFFSET 0) t
 CROSS JOIN LATERAL (SELECT height,time_stamp FROM blocks WHERE id=t.block_id OFFSET 0) b
 WHERE m.id > ? AND m.id <= ? AND mt.message_type IN ? AND t.code=0 ORDER BY m.id LIMIT ?`, idArray(candidates), last, maxID, urls, n).Scan(&rows).Error
		return rows, err
	}
	q := db.Table("messages m").Select("m.*,mt.message_type type,b.height,b.time_stamp stamp,t.hash").Joins("JOIN message_types mt ON mt.id=m.message_type_id JOIN txes t ON t.id=m.tx_id JOIN blocks b ON b.id=t.block_id").Where("m.id > ? AND m.id <= ? AND mt.message_type IN ? AND t.code=0", last, maxID, urls).Order("m.id").Limit(n)
	if candidates != nil {
		q = q.Where("m.id = ANY(ARRAY(SELECT unnest(?::bigint[])))", idArray(candidates))
	}
	var rows []row
	err := q.Scan(&rows).Error
	return rows, err
}

func oldRows(db *gorm.DB, ids []uint) (map[uint][]tax.TaxableEvent, error) {
	var rows []tax.TaxableEvent
	q := db.Where("message_id IN ?", ids)
	if lookups {
		q = db.Where("id IN (SELECT te.id FROM unnest(?::bigint[]) c(id) CROSS JOIN LATERAL (SELECT id FROM taxable_events WHERE message_id=c.id OFFSET 0) te)", idArray(ids))
	}
	if err := q.Order("message_id,sub_index").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[uint][]tax.TaxableEvent{}
	for _, r := range rows {
		out[r.MessageID] = append(out[r.MessageID], r)
	}
	return out, nil
}

func planBatch(db *gorm.DB, codec codectypes.AnyUnpacker, rows []row) ([]backup, []skip, error) {
	plans, skips, _, err := planBatchParsed(db, codec, rows)
	return plans, skips, err
}

// planBatchParsed also returns the messages the production parser classified
// from their stored body without an error. Only those can have a parser error
// cleared: a body-free reconstruction or a trivial-log fallback is not the
// parse the indexer ran.
func planBatchParsed(db *gorm.DB, codec codectypes.AnyUnpacker, rows []row) ([]backup, []skip, []uint, error) {
	if len(rows) == 0 {
		return nil, nil, nil, nil
	}
	ids := make([]uint, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	old, err := oldRows(db, ids)
	if err != nil {
		return nil, nil, nil, err
	}
	// The ids go in as one opaque array: with a literal list of hundreds the
	// planner estimates enough events to merge join against the whole
	// attribute index (measured 35 s for 500 messages, 0.35 s this way).
	var attrs []eventRow
	if lookups {
		err = db.Raw(`SELECT e.message_id,e.index,et.type,a.key,a.value,a.index attr_index
 FROM unnest(?::bigint[]) c(id)
 CROSS JOIN LATERAL (SELECT id,message_id,index,message_event_type_id FROM message_events WHERE message_id=c.id OFFSET 0) e
 CROSS JOIN LATERAL (SELECT type FROM message_event_types WHERE id=e.message_event_type_id OFFSET 0) et
 LEFT JOIN LATERAL (SELECT x.index,x.value,k.key FROM message_event_attributes x
   LEFT JOIN LATERAL (SELECT key FROM message_event_attribute_keys WHERE id=x.message_event_attribute_key_id OFFSET 0) k ON true
   WHERE x.message_event_id=e.id OFFSET 0) a ON true
 ORDER BY e.message_id,e.index,a.index`, idArray(ids)).Scan(&attrs).Error
	} else {
		err = db.Table("message_events e").Select("e.message_id,e.index,et.type,k.key,a.value,a.index attr_index").Joins("JOIN message_event_types et ON et.id=e.message_event_type_id LEFT JOIN message_event_attributes a ON a.message_event_id=e.id LEFT JOIN message_event_attribute_keys k ON k.id=a.message_event_attribute_key_id").Where("e.message_id = ANY(ARRAY(SELECT unnest(?::bigint[])))", idArray(ids)).Order("e.message_id,e.index,a.index").Scan(&attrs).Error
	}
	if err != nil {
		return nil, nil, nil, err
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
	plans := []backup{}
	var skips []skip
	var parsed []uint
	for _, r := range rows {
		if logs[r.ID] == nil {
			return nil, nil, nil, fmt.Errorf("message %d missing event log", r.ID)
		}
		var expected []tax.TaxableEvent
		fromBody := false
		if len(r.MessageBytes) == 0 {
			expected, err = fromStoredLog(r.Type, logs[r.ID], old[r.ID])
		} else {
			var msg sdk.Msg
			if err = codec.UnpackAny(&codectypes.Any{TypeUrl: r.Type, Value: r.MessageBytes}, &msg); err == nil {
				expected, err = parse(msg, logs[r.ID])
				fromBody = err == nil
			}
		}
		if err != nil {
			if trivialLog(logs[r.ID]) {
				expected, err = nil, nil
			} else {
				skips = append(skips, skip{r.ID, r.Type, err.Error()})
				continue
			}
		}
		if fromBody {
			parsed = append(parsed, r.ID)
		}
		for i := range expected {
			expected[i].MessageID = r.ID
			expected[i].SubIndex = i
			expected[i].BlockHeight = r.Height
			expected[i].Timestamp = r.Stamp
			expected[i].TxHash = r.Hash
		}
		if same(old[r.ID], expected) {
			continue
		}
		plans = append(plans, backup{r.ID, r.Type, base64.StdEncoding.EncodeToString(r.MessageBytes), old[r.ID], expected, nil})
	}
	return plans, skips, parsed, nil
}

// withResolvedErrors attaches to the plans the tax parser errors of the
// messages in parsed: the rows the indexer itself deletes when the same
// parser now succeeds. A message whose tax rows do not change gets a record
// with its current rows as both old and expected, so the backup still holds
// the error and apply still checks the rows have not moved.
func withResolvedErrors(db *gorm.DB, plans []backup, parsed []uint, rows []row) ([]backup, error) {
	if len(parsed) == 0 {
		return plans, nil
	}
	var errs []parserErrorRow
	if err := db.Raw(`SELECT e.id,e.message_id,e.message_parser_id,e.error FROM message_parser_errors e
 JOIN messages m ON m.id=e.message_id JOIN message_types mt ON mt.id=m.message_type_id
 JOIN message_parsers p ON p.id=e.message_parser_id
 WHERE e.message_id = ANY(?::bigint[]) AND p.identifier = 'tax:' || mt.message_type ORDER BY e.id`, idArray(parsed)).Scan(&errs).Error; err != nil {
		return nil, err
	}
	if len(errs) == 0 {
		return plans, nil
	}
	byMessage := map[uint][]parserErrorRow{}
	for _, e := range errs {
		byMessage[e.MessageID] = append(byMessage[e.MessageID], e)
	}
	planned := map[uint]bool{}
	for i := range plans {
		planned[plans[i].MessageID] = true
		plans[i].ClearedParserErrors = byMessage[plans[i].MessageID]
	}
	var ids []uint
	for id := range byMessage {
		if !planned[id] {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return plans, nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	current, err := oldRows(db, ids)
	if err != nil {
		return nil, err
	}
	byID := map[uint]row{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	for _, id := range ids {
		r := byID[id]
		plans = append(plans, backup{id, r.Type, base64.StdEncoding.EncodeToString(r.MessageBytes), current[id], current[id], byMessage[id]})
	}
	sort.SliceStable(plans, func(i, j int) bool { return plans[i].MessageID < plans[j].MessageID })
	return plans, nil
}

func applyPlans(db *gorm.DB, plans []backup) error {
	if len(plans) == 0 {
		return nil
	}
	ids := make([]uint, len(plans))
	for i, p := range plans {
		ids[i] = p.MessageID
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL statement_timeout='60s'; SET LOCAL lock_timeout='3s'").Error; err != nil {
			return err
		}
		var locked []uint
		if err := tx.Raw("SELECT id FROM messages WHERE id IN ? ORDER BY id FOR UPDATE", ids).Scan(&locked).Error; err != nil {
			return err
		}
		if len(locked) != len(ids) {
			return fmt.Errorf("candidate messages changed")
		}
		current, err := oldRows(tx, ids)
		if err != nil {
			return err
		}
		for _, p := range plans {
			if !same(current[p.MessageID], p.Old) {
				return fmt.Errorf("message %d changed since backup; retry with a new backup", p.MessageID)
			}
		}
		// A record that only clears parser errors keeps its rows as they are.
		var rewrite []uint
		var expected []tax.TaxableEvent
		for _, p := range plans {
			if same(p.Old, p.Expected) {
				continue
			}
			rewrite = append(rewrite, p.MessageID)
			expected = append(expected, p.Expected...)
		}
		if len(rewrite) > 0 {
			if err := tx.Where("message_id IN ?", rewrite).Delete(&tax.TaxableEvent{}).Error; err != nil {
				return err
			}
		}
		// Tax row IDs are not referenced by any table; canonical event identity is
		// (message_id,sub_index). Bulk replacement is atomic for this bounded batch.
		if len(expected) > 0 {
			if err := tx.Omit(clause.Associations).CreateInBatches(&expected, 1000).Error; err != nil {
				return err
			}
		}
		got, err := oldRows(tx, ids)
		if err != nil {
			return err
		}
		for _, p := range plans {
			if !same(got[p.MessageID], p.Expected) {
				return fmt.Errorf("post-write verification failed for %d", p.MessageID)
			}
		}
		return clearParserErrors(tx, plans)
	})
}

// clearParserErrors deletes exactly the backed-up parser error rows and
// rolls the batch back unless every one was still there unchanged.
func clearParserErrors(tx *gorm.DB, plans []backup) error {
	want := map[uint]parserErrorRow{}
	var ids []uint
	for _, p := range plans {
		for _, e := range p.ClearedParserErrors {
			if e.MessageID != p.MessageID {
				return fmt.Errorf("parser error %d belongs to message %d, not %d", e.ID, e.MessageID, p.MessageID)
			}
			want[e.ID] = e
			ids = append(ids, e.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var gone []parserErrorRow
	if err := tx.Raw("DELETE FROM message_parser_errors WHERE id = ANY(?::bigint[]) RETURNING id,message_id,message_parser_id,error", idArray(ids)).Scan(&gone).Error; err != nil {
		return err
	}
	if len(gone) != len(want) {
		return fmt.Errorf("cleared %d parser errors, backup holds %d", len(gone), len(want))
	}
	for _, g := range gone {
		if want[g.ID] != g {
			return fmt.Errorf("parser error %d changed since backup; retry with a new backup", g.ID)
		}
	}
	return nil
}

func repairCodec() (codectypes.AnyUnpacker, error) {
	m := tax.GaiaLiquidMsgTypes()
	for k, v := range tax.LegacyLSMMsgTypes() {
		m[k] = v
	}
	for k, v := range tax.IBCChannelV2MsgTypes() {
		m[k] = v
	}
	for k, v := range tax.TokenFactoryMsgTypes() {
		m[k] = v
	}
	for k, v := range tax.ProviderMsgTypes() {
		m[k] = v
	}
	for k, v := range tax.LiquidityMsgTypes() {
		m[k] = v
	}
	for k, v := range tax.IBCClientV2MsgTypes() {
		m[k] = v
	}
	for k, v := range tax.IBCWasmMsgTypes() {
		m[k] = v
	}
	c, err := probe.GetProbeClient(config.Probe{RPC: "http://127.0.0.1:1"}, nil, m)
	if err != nil {
		return nil, err
	}
	return c.Codec.InterfaceRegistry, nil
}

func same(a, b []tax.TaxableEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		x, y := a[i], b[i]
		if x.MessageID != y.MessageID || x.SubIndex != y.SubIndex || x.Category != y.Category || x.Amount != y.Amount || x.Denom != y.Denom || x.Asset != y.Asset || x.FromAddr != y.FromAddr || x.ToAddr != y.ToAddr || x.BlockHeight != y.BlockHeight || !x.Timestamp.Equal(y.Timestamp) || x.TxHash != y.TxHash || x.ValidatorAddress != y.ValidatorAddress || x.RewardTrigger != y.RewardTrigger || x.Detail != y.Detail {
			return false
		}
	}
	return true
}

func add(t totals, events []tax.TaxableEvent) {
	for _, e := range events {
		denom := e.Denom
		if denom == "" {
			denom = "rows"
		}
		if t[e.Category] == nil {
			t[e.Category] = map[string]map[string]string{}
		}
		if t[e.Category][denom] == nil {
			t[e.Category][denom] = map[string]string{}
		}
		prior := new(big.Int)
		prior.SetString(t[e.Category][denom][e.RewardTrigger], 10)
		amount, ok := new(big.Int).SetString(e.Amount, 10)
		if !ok {
			amount = big.NewInt(1)
		}
		t[e.Category][denom][e.RewardTrigger] = prior.Add(prior, amount).String()
	}
}
func jsonTotals(t totals) string { b, _ := json.Marshal(t); return string(b) }
func env(k string) string {
	v := os.Getenv(k)
	if v == "" {
		fatal("missing " + k)
	}
	return v
}

func envDefault(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func fatal(s string) { fmt.Fprintln(os.Stderr, "taxrepair:", s); os.Exit(1) }
