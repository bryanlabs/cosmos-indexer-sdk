package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	indexerdb "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// These tests need a private throwaway Postgres. LEGACY_STORE_TEST_ADMIN_DSN
// must name a loopback server; each test creates and drops its own database
// and touches no other. Port 15432 is refused because it is the customary
// forward to the production database.
func open(t *testing.T) *gorm.DB {
	t.Helper()
	admin := os.Getenv("LEGACY_STORE_TEST_ADMIN_DSN")
	if admin == "" {
		t.Skip("private Postgres not supplied")
	}
	fields := map[string]string{}
	for _, part := range strings.Fields(admin) {
		k, v, _ := strings.Cut(part, "=")
		fields[k] = v
	}
	ip := net.ParseIP(fields["host"])
	if ip == nil || !ip.IsLoopback() || fields["port"] == "15432" || fields["port"] == "" || fields["dbname"] == "" {
		t.Fatal("refusing a DSN that is not an explicit loopback throwaway server")
	}
	quiet := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	root, err := gorm.Open(postgres.Open(admin), quiet)
	require.NoError(t, err)
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "legacy_store_test_" + hex.EncodeToString(suffix)
	require.NoError(t, root.Exec("CREATE DATABASE "+name).Error)
	fields["dbname"] = name
	var parts []string
	for k, v := range fields {
		parts = append(parts, k+"="+v)
	}
	db, err := gorm.Open(postgres.Open(strings.Join(parts, " ")), quiet)
	require.NoError(t, err)
	t.Cleanup(func() {
		if sql, err := db.DB(); err == nil {
			_ = sql.Close()
		}
		_ = root.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)").Error
		if sql, err := root.DB(); err == nil {
			_ = sql.Close()
		}
	})
	// The serving schema exactly as the indexer creates it, then the addition.
	require.NoError(t, indexerdb.MigrateModels(db))
	require.NoError(t, Migrate(db))
	return db
}

func syntheticHash(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 96)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// seedHubFour stores a cosmoshub-4 transaction the way the serving indexer
// does: hash-keyed, with a message, a fee and a signer. Values are synthetic.
func seedHubFour(t *testing.T, db *gorm.DB, hash string) models.Tx {
	t.Helper()
	chain := models.Chain{ChainID: "cosmoshub-4", Name: "cosmoshub-4"}
	require.NoError(t, db.Where("chain_id = ?", chain.ChainID).FirstOrCreate(&chain).Error)
	signer := models.Address{Address: "synthetic-signer-" + hash[:8]}
	require.NoError(t, db.Create(&signer).Error)
	proposer := models.Address{Address: "synthetic-proposer-" + hash[:8]}
	require.NoError(t, db.Create(&proposer).Error)
	block := models.Block{Height: 5200900, ChainID: chain.ID, TimeStamp: time.Date(2021, 2, 18, 6, 0, 0, 0, time.UTC), ProposerConsAddressID: proposer.ID, TxIndexed: true, BlockEventsIndexed: true}
	require.NoError(t, db.Omit(clause.Associations).Create(&block).Error)
	tx := models.Tx{Hash: hash, Code: 0, BlockID: block.ID, Memo: "synthetic memo"}
	require.NoError(t, db.Omit(clause.Associations).Create(&tx).Error)
	require.NoError(t, db.Exec(`INSERT INTO tx_signer_addresses (tx_id, address_id) VALUES (?, ?)`, tx.ID, signer.ID).Error)
	kind := models.MessageType{MessageType: "/cosmos.bank.v1beta1.MsgSend"}
	require.NoError(t, db.Create(&kind).Error)
	require.NoError(t, db.Omit(clause.Associations).Create(&models.Message{TxID: tx.ID, MessageTypeID: kind.ID, MessageIndex: 0, MessageBytes: []byte{1, 2, 3}}).Error)
	denom := models.Denom{Base: "uatom"}
	require.NoError(t, db.Create(&denom).Error)
	require.NoError(t, db.Exec(`INSERT INTO fees (tx_id, amount, denomination_id, payer_address_id) VALUES (?, 5000, ?, ?)`, tx.ID, denom.ID, signer.ID).Error)
	return tx
}

// snapshot records every row of every serving table with its physical tuple
// location and inserting transaction. An unchanged ctid and xmin mean the
// stored tuple was never rewritten, so the row is byte-identical on disk.
func snapshot(t *testing.T, db *gorm.DB) map[string]string {
	t.Helper()
	return snapshotColumns(t, db, "xmin::text || ' ' || xmax::text")
}

// snapshotStored leaves out xmax. A refused UPDATE rolls back, but Postgres
// still leaves the aborted transaction's id in xmax as a dead lock marker;
// the tuple itself (ctid, xmin and every column) is what must not change.
func snapshotStored(t *testing.T, db *gorm.DB) map[string]string {
	t.Helper()
	return snapshotColumns(t, db, "xmin::text")
}

func snapshotColumns(t *testing.T, db *gorm.DB, system string) map[string]string {
	t.Helper()
	var tables []string
	require.NoError(t, db.Raw(`SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY 1`).Scan(&tables).Error)
	rows := map[string]string{}
	for _, table := range tables {
		var lines []string
		require.NoError(t, db.Raw(fmt.Sprintf(`SELECT ctid::text || ' ' || %s || ' ' || r::text FROM %q r`, system, table)).Scan(&lines).Error)
		for _, line := range lines {
			location := strings.SplitN(line, " ", 2)
			rows[table+" "+location[0]] = location[1]
		}
	}
	return rows
}

func requireUntouched(t *testing.T, before, after map[string]string) {
	t.Helper()
	var keys []string
	for key := range before {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		got, ok := after[key]
		require.True(t, ok, "a row that existed before the import is gone: %s", strings.Fields(key)[0])
		require.Equal(t, before[key], got, "a row that existed before the import changed: %s", strings.Fields(key)[0])
	}
}

func TestLegacyImportLeavesCollidingHubFourRowUntouched(t *testing.T) {
	db := open(t)
	shared := syntheticHash(t)
	original := seedHubFour(t, db, shared)
	before := snapshot(t, db)
	require.NotEmpty(t, before)

	// The same bytes, so the same hash, included on cosmoshub-3 (failed, then
	// successful one height later) and on cosmoshub-2 at a height number that
	// also exists on cosmoshub-3.
	other := syntheticHash(t)
	when := time.Date(2020, 3, 1, 12, 0, 0, 123456789, time.UTC)
	blocks := []Block{
		{Chain: "cosmoshub-3", Height: 100, Time: when, Inclusions: []Inclusion{{Index: 0, Hash: shared, Code: 5, Codespace: "sdk", CodeEvidence: "explicit"}}},
		{Chain: "cosmoshub-3", Height: 101, Time: when.Add(7 * time.Second), Inclusions: []Inclusion{{Index: 0, Hash: shared, Code: 0, CodeEvidence: "explicit"}, {Index: 1, Hash: other, Code: 0, CodeEvidence: "explicit"}}},
		{Chain: "cosmoshub-2", Height: 100, Time: when.AddDate(-1, 0, 0), Inclusions: []Inclusion{{Index: 0, Hash: shared, Code: 0, CodeEvidence: "omitted-zero-omitempty"}, {Index: 1, Hash: shared, Code: 4, CodeEvidence: "explicit"}}},
	}
	for _, block := range blocks {
		require.NoError(t, ImportBlock(db, block))
	}

	requireUntouched(t, before, snapshot(t, db))
	var again models.Tx
	require.NoError(t, db.Where("hash = ?", shared).Find(&again).Error)
	require.Equal(t, original.ID, again.ID)
	require.Equal(t, original.BlockID, again.BlockID)
	require.Equal(t, uint32(0), again.Code)
	var hashKeyed int64
	require.NoError(t, db.Table("txes").Where("hash = ?", shared).Count(&hashKeyed).Error)
	require.Equal(t, int64(1), hashKeyed, "only the cosmoshub-4 row may be keyed by the shared hash")

	// Every inclusion kept its own row, block and result.
	var kept []struct {
		Chain   string
		Height  int64
		TxIndex int
		Code    uint32
		TxHash  string
		TxCode  uint32
		TxChain string
	}
	require.NoError(t, db.Raw(`SELECT c.chain_id AS chain, i.height, i.tx_index, i.code, x.hash AS tx_hash, x.code AS tx_code, bc.chain_id AS tx_chain
FROM tx_inclusions i JOIN chains c ON c.id = i.chain_id JOIN txes x ON x.id = i.tx_id JOIN blocks b ON b.id = x.block_id JOIN chains bc ON bc.id = b.chain_id
WHERE i.hash = ? ORDER BY c.chain_id, i.height, i.tx_index`, shared).Scan(&kept).Error)
	require.Len(t, kept, 4)
	want := []struct {
		chain  string
		height int64
		index  int
		code   uint32
	}{{"cosmoshub-2", 100, 0, 0}, {"cosmoshub-2", 100, 1, 4}, {"cosmoshub-3", 100, 0, 5}, {"cosmoshub-3", 101, 0, 0}}
	for i, w := range want {
		require.Equal(t, w.chain, kept[i].Chain)
		require.Equal(t, w.height, kept[i].Height)
		require.Equal(t, w.index, kept[i].TxIndex)
		require.Equal(t, w.code, kept[i].Code)
		require.Equal(t, w.code, kept[i].TxCode)
		require.Equal(t, w.chain, kept[i].TxChain, "an inclusion's txes row must sit on its own chain")
		require.Equal(t, fmt.Sprintf("%s/%d/%d", w.chain, w.height, w.index), kept[i].TxHash)
	}
	var covered int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM blocks b JOIN chains c ON c.id = b.chain_id WHERE c.chain_id <> 'cosmoshub-4' AND (b.tx_indexed OR b.block_events_indexed)`).Scan(&covered).Error)
	require.Zero(t, covered, "an inclusion import must not mark a legacy height as indexed")
}

func TestLegacyImportRerunChangesNothingAndDisagreementFails(t *testing.T) {
	db := open(t)
	seedHubFour(t, db, syntheticHash(t))
	block := Block{Chain: "cosmoshub-1", Height: 7, Time: time.Date(2019, 3, 14, 0, 0, 0, 0, time.UTC), Inclusions: []Inclusion{
		{Index: 0, Hash: syntheticHash(t), Code: 0, CodeEvidence: "omitted-zero-omitempty"}, {Index: 1, Hash: syntheticHash(t), Code: 12, CodeEvidence: "explicit"}}}
	require.NoError(t, ImportBlock(db, block))
	before := snapshot(t, db)

	require.NoError(t, ImportBlock(db, block))
	after := snapshot(t, db)
	requireUntouched(t, before, after)
	require.Len(t, after, len(before), "a rerun must not add rows")

	changed := block
	changed.Inclusions = append([]Inclusion(nil), block.Inclusions...)
	changed.Inclusions[1].Code = 0
	require.ErrorIs(t, ImportBlock(db, changed), ErrStoredDiffers)
	shorter := block
	shorter.Inclusions = block.Inclusions[:1]
	require.ErrorIs(t, ImportBlock(db, shorter), ErrStoredDiffers)
	moved := block
	moved.Time = block.Time.Add(time.Second)
	require.ErrorIs(t, ImportBlock(db, moved), ErrStoredDiffers)
	after = snapshot(t, db)
	requireUntouched(t, before, after)
	require.Len(t, after, len(before), "a refused rerun must leave nothing behind")

	require.Error(t, ImportBlock(db, Block{Chain: "cosmoshub-4", Height: 1, Time: block.Time}), "the legacy writer must refuse cosmoshub-4")
	require.Error(t, ImportBlock(db, Block{Chain: "cosmoshub-3", Height: 1, Time: block.Time, Inclusions: []Inclusion{{Index: 1, Hash: syntheticHash(t)}}}))
	require.Error(t, ImportBlock(db, Block{Chain: "cosmoshub-3", Height: 1, Time: block.Time, Inclusions: []Inclusion{{Index: 0, Hash: "cosmoshub-3/1/0"}}}))
}

// The control: this is the hazard the design avoids. The serving indexer's
// hash upsert, handed a legacy inclusion under its true hash, repoints the
// cosmoshub-4 row. With the guard trigger the same statement is refused.
func TestGenericHashUpsertRepointsAcrossChainsUnlessGuarded(t *testing.T) {
	upsert := func(db *gorm.DB, hash string, blockID uint) error {
		return db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "hash"}},
			DoUpdates: clause.AssignmentColumns([]string{"code", "block_id"}),
		}).Omit(clause.Associations).Create(&[]models.Tx{{Hash: hash, Code: 5, BlockID: blockID}}).Error
	}
	legacyBlock := func(t *testing.T, db *gorm.DB) uint {
		require.NoError(t, ImportBlock(db, Block{Chain: "cosmoshub-3", Height: 100, Time: time.Date(2020, 3, 1, 0, 0, 0, 0, time.UTC)}))
		var id uint
		require.NoError(t, db.Raw(`SELECT b.id FROM blocks b JOIN chains c ON c.id = b.chain_id WHERE c.chain_id = 'cosmoshub-3' AND b.height = 100`).Scan(&id).Error)
		require.NotZero(t, id)
		return id
	}

	t.Run("unguarded upsert overwrites the cosmoshub-4 row", func(t *testing.T) {
		db := open(t)
		hash := syntheticHash(t)
		original := seedHubFour(t, db, hash)
		target := legacyBlock(t, db)
		require.NoError(t, upsert(db, hash, target))
		var row models.Tx
		require.NoError(t, db.Where("hash = ?", hash).Find(&row).Error)
		require.Equal(t, original.ID, row.ID)
		require.Equal(t, target, row.BlockID, "expected the known hazard: the row now points at a cosmoshub-3 block")
		require.Equal(t, uint32(5), row.Code)
	})

	t.Run("guarded upsert is refused and the row is untouched", func(t *testing.T) {
		db := open(t)
		require.NoError(t, InstallCrossChainGuard(db))
		hash := syntheticHash(t)
		seedHubFour(t, db, hash)
		target := legacyBlock(t, db)
		before := snapshotStored(t, db)
		err := upsert(db, hash, target)
		require.Error(t, err)
		require.Contains(t, err.Error(), "may not move to a block on another chain")
		requireUntouched(t, before, snapshotStored(t, db))

		// The guard must not disturb the serving path's own same-chain update.
		var chainID uint
		require.NoError(t, db.Raw(`SELECT id FROM chains WHERE chain_id = 'cosmoshub-4'`).Scan(&chainID).Error)
		var proposer uint
		require.NoError(t, db.Raw(`SELECT proposer_cons_address_id FROM blocks WHERE chain_id = ? LIMIT 1`, chainID).Scan(&proposer).Error)
		later := models.Block{Height: 5200901, ChainID: chainID, TimeStamp: time.Date(2021, 2, 18, 6, 0, 7, 0, time.UTC), ProposerConsAddressID: proposer}
		require.NoError(t, db.Omit(clause.Associations).Create(&later).Error)
		require.NoError(t, upsert(db, hash, later.ID))
		var row models.Tx
		require.NoError(t, db.Where("hash = ?", hash).Find(&row).Error)
		require.Equal(t, later.ID, row.BlockID)
	})
}

func TestStoredDiffersIsDistinguishable(t *testing.T) {
	require.True(t, errors.Is(fmt.Errorf("wrapped: %w", ErrStoredDiffers), ErrStoredDiffers))
}
