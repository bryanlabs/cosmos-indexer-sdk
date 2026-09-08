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

var urls = []string{"/cosmos.staking.v1beta1.MsgDelegate", "/cosmos.staking.v1beta1.MsgUndelegate", "/cosmos.staking.v1beta1.MsgBeginRedelegate", "/cosmos.distribution.v1beta1.MsgWithdrawDelegatorReward", "/cosmos.distribution.v1beta1.MsgWithdrawValidatorCommission", "/cosmos.authz.v1beta1.MsgExec"}

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
type backup struct {
	MessageID uint               `json:"message_id"`
	Type      string             `json:"message_type"`
	Bytes     string             `json:"message_bytes"`
	Old       []tax.TaxableEvent `json:"old"`
	Expected  []tax.TaxableEvent `json:"expected"`
	Log       *txlog.LogMessage  `json:"log"`
}
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
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
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

func main() {
	apply := flag.Bool("apply", false, "write repaired rows (requires --backup)")
	path := flag.String("backup", "", "new JSONL backup file, never overwritten")
	addresses := flag.String("addresses", "", "optional comma-separated wallets")
	batch := flag.Int("batch", 500, "bounded messages per batch")
	from := flag.Uint("from-message-id", 0, "exclusive lower message ID for resume")
	to := flag.Uint("to-message-id", 0, "inclusive upper message ID (default current max)")
	flag.Parse()
	if *batch < 1 || *batch > 5000 {
		fatal("batch must be between 1 and 5000")
	}
	if *apply && *path == "" {
		fatal("--apply requires --backup")
	}
	db, err := dbpkg.PostgresDbConnect(env("DB_HOST"), envDefault("DB_PORT", "5432"), env("DB_NAME"), env("DB_USER"), os.Getenv("DB_PASS"), "silent")
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
	if err := db.Exec("SET statement_timeout='60s'; SET lock_timeout='3s'").Error; err != nil {
		fatal(err.Error())
	}
	codec, err := repairCodec()
	if err != nil {
		fatal(err.Error())
	}
	max := *to
	if max == 0 {
		if err := db.Table("messages").Select("COALESCE(max(id),0)").Scan(&max).Error; err != nil {
			fatal(err.Error())
		}
	}
	if max < *from {
		fatal("upper message ID precedes lower ID")
	}
	var wallets []string
	for _, a := range strings.Split(*addresses, ",") {
		if a = strings.TrimSpace(a); a != "" {
			wallets = append(wallets, a)
		}
	}
	var candidates []uint
	if len(wallets) > 0 {
		candidates, err = walletIDs(db, wallets)
		if err != nil {
			fatal(err.Error())
		}
	}
	b, err := newBackup(*path)
	if err != nil {
		fatal(err.Error())
	}
	if b != nil {
		defer b.file.Close()
	}
	scanned, changed, zeroed := 0, 0, 0
	oldTotal, newTotal := totals{}, totals{}
	last := *from
	fmt.Printf("snapshot_max_message_id=%d apply=%t wallet_candidates=%d\n", max, *apply, len(candidates))
	for len(wallets) == 0 || len(candidates) > 0 {
		rows, err := load(db, last, max, *batch, candidates)
		if err != nil {
			fatal(err.Error())
		}
		if len(rows) == 0 {
			break
		}
		plans, err := planBatch(db, codec, rows)
		if err != nil {
			fatal(err.Error())
		}
		scanned += len(rows)
		for _, p := range plans {
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
		fmt.Printf("last_message_id=%d scanned=%d changed=%d zeroed=%d\n", last, scanned, changed, zeroed)
	}
	sum, err := b.verify()
	if err != nil {
		fatal(err.Error())
	}
	if b != nil {
		fmt.Printf("backup=%s sha256=%s records=%d\n", *path, sum, b.lines)
	}
	fmt.Printf("complete=true snapshot_max_message_id=%d scanned=%d changed=%d zeroed=%d apply=%t\nold_changed_rewards=%s\nnew_changed_rewards=%s\n", max, scanned, changed, zeroed, *apply, jsonTotals(oldTotal), jsonTotals(newTotal))
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
func load(db *gorm.DB, last, max uint, n int, candidates []uint) ([]row, error) {
	q := db.Table("messages m").Select("m.*,mt.message_type type,b.height,b.time_stamp stamp,t.hash").Joins("JOIN message_types mt ON mt.id=m.message_type_id JOIN txes t ON t.id=m.tx_id JOIN blocks b ON b.id=t.block_id").Where("m.id > ? AND m.id <= ? AND mt.message_type IN ? AND t.code=0", last, max, urls).Order("m.id").Limit(n)
	if candidates != nil {
		q = q.Where("m.id IN ?", candidates)
	}
	var rows []row
	err := q.Scan(&rows).Error
	return rows, err
}
func oldRows(db *gorm.DB, ids []uint) (map[uint][]tax.TaxableEvent, error) {
	var rows []tax.TaxableEvent
	if err := db.Where("message_id IN ?", ids).Order("message_id,sub_index").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[uint][]tax.TaxableEvent{}
	for _, r := range rows {
		out[r.MessageID] = append(out[r.MessageID], r)
	}
	return out, nil
}
func planBatch(db *gorm.DB, codec codectypes.AnyUnpacker, rows []row) ([]backup, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	ids := make([]uint, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	old, err := oldRows(db, ids)
	if err != nil {
		return nil, err
	}
	var attrs []eventRow
	err = db.Table("message_events e").Select("e.message_id,e.index,et.type,k.key,a.value,a.index attr_index").Joins("JOIN message_event_types et ON et.id=e.message_event_type_id LEFT JOIN message_event_attributes a ON a.message_event_id=e.id LEFT JOIN message_event_attribute_keys k ON k.id=a.message_event_attribute_key_id").Where("e.message_id IN ?", ids).Order("e.message_id,e.index,a.index").Scan(&attrs).Error
	if err != nil {
		return nil, err
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
	for _, r := range rows {
		if logs[r.ID] == nil {
			return nil, fmt.Errorf("message %d missing event log", r.ID)
		}
		var expected []tax.TaxableEvent
		if len(r.MessageBytes) == 0 {
			expected, err = fromStoredLog(r.Type, logs[r.ID], old[r.ID])
		} else {
			var msg sdk.Msg
			if err = codec.UnpackAny(&codectypes.Any{TypeUrl: r.Type, Value: r.MessageBytes}, &msg); err == nil {
				expected, err = parse(msg, logs[r.ID])
			}
		}
		if err != nil {
			return nil, fmt.Errorf("message %d classify: %w", r.ID, err)
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
		plans = append(plans, backup{r.ID, r.Type, base64.StdEncoding.EncodeToString(r.MessageBytes), old[r.ID], expected, logs[r.ID]})
	}
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
		if err := tx.Where("message_id IN ?", ids).Delete(&tax.TaxableEvent{}).Error; err != nil {
			return err
		}
		var expected []tax.TaxableEvent
		for _, p := range plans {
			expected = append(expected, p.Expected...)
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
		return nil
	})
}
func repairCodec() (codectypes.AnyUnpacker, error) {
	m := tax.GaiaLiquidMsgTypes()
	for k, v := range tax.IBCChannelV2MsgTypes() {
		m[k] = v
	}
	for k, v := range tax.TokenFactoryMsgTypes() {
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
	for i := range a {
		x, y := a[i], b[i]
		if x.MessageID != y.MessageID || x.SubIndex != y.SubIndex || x.Category != y.Category || x.Amount != y.Amount || x.Denom != y.Denom || x.Asset != y.Asset || x.FromAddr != y.FromAddr || x.ToAddr != y.ToAddr || x.BlockHeight != y.BlockHeight || !x.Timestamp.Equal(y.Timestamp) || x.TxHash != y.TxHash || x.ValidatorAddress != y.ValidatorAddress || x.RewardTrigger != y.RewardTrigger {
			return false
		}
	}
	return true
}
func add(t totals, events []tax.TaxableEvent) {
	for _, e := range events {
		if e.Category != "reward" {
			continue
		}
		if t[e.ToAddr] == nil {
			t[e.ToAddr] = map[string]map[string]string{}
		}
		if t[e.ToAddr][e.Denom] == nil {
			t[e.ToAddr][e.Denom] = map[string]string{}
		}
		prior := new(big.Int)
		prior.SetString(t[e.ToAddr][e.Denom][e.RewardTrigger], 10)
		amount, ok := new(big.Int).SetString(e.Amount, 10)
		if !ok {
			continue
		}
		t[e.ToAddr][e.Denom][e.RewardTrigger] = prior.Add(prior, amount).String()
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
