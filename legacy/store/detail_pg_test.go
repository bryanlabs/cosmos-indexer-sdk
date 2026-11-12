package store

import (
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedHubFourTaxRow gives the cosmoshub-4 transaction a tax row, so the proof
// covers the table people's reports are read from.
func seedHubFourTaxRow(t *testing.T, db *gorm.DB, txID uint, hash string) {
	t.Helper()
	require.NoError(t, db.AutoMigrate(&tax.TaxableEvent{}))
	var messageID uint
	require.NoError(t, db.Raw(`SELECT id FROM messages WHERE tx_id = ?`, txID).Scan(&messageID).Error)
	require.NoError(t, db.Exec(`INSERT INTO taxable_events (message_id, sub_index, category, amount, denom, asset, from_addr, to_addr, validator_address, reward_trigger, detail, block_height, timestamp, tx_hash)
VALUES (?, 0, 'transfer', '11', 'uatom', '', 'synthetic-from', 'synthetic-to', '', '', '', 5200900, ?, ?)`, messageID, time.Date(2021, 2, 18, 6, 0, 0, 0, time.UTC), hash).Error)
}

func detailBlock(shared string, when time.Time, signer string) Block {
	return Block{Chain: "cosmoshub-3", Height: 100, Time: when, Inclusions: []Inclusion{
		{Index: 0, Hash: shared, Code: 0, CodeEvidence: "explicit", Detail: &Detail{
			Signers: []string{signer}, FeePayer: signer, Fee: []Coin{{"uatom", "7"}},
			Messages: []Message{
				{TypeURL: "/cosmos.bank.v1beta1.MsgSend", Rows: []Row{{Category: "transfer", Amount: "53", Denom: "uatom", From: signer, To: "synthetic-legacy-to"}}},
				{TypeURL: "/cosmos.staking.v1beta1.MsgDelegate", Rows: []Row{
					{Category: "reward", Amount: "3", Denom: "uatom", To: signer, Validator: "synthetic-validator", Trigger: "delegate"},
					{Category: "delegate", Amount: "40", Denom: "uatom", From: signer, Validator: "synthetic-validator"}}},
			}}},
		// The same bytes again in the same block, failed with nothing charged.
		{Index: 1, Hash: shared, Code: 12, Codespace: "sdk", CodeEvidence: "explicit", Detail: &Detail{
			Signers: []string{signer}, Messages: []Message{{TypeURL: "/cosmos.bank.v1beta1.MsgSend"}, {TypeURL: "/cosmos.staking.v1beta1.MsgDelegate"}}}},
	}}
}

func TestLegacyDetailImportLeavesHubFourRowsUntouched(t *testing.T) {
	db := open(t)
	shared := syntheticHash(t)
	original := seedHubFour(t, db, shared)
	seedHubFourTaxRow(t, db, original.ID, shared)
	before := snapshotExceptNamingLocks(t, db)

	// The legacy transaction has the same hash, the same signer address, the
	// same denom and the same message type as the stored cosmoshub-4 one.
	signer := "synthetic-signer-" + shared[:8]
	when := time.Date(2020, 3, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, ImportBlock(db, detailBlock(shared, when, signer)))
	requireUntouched(t, before, snapshotExceptNamingLocks(t, db))

	count := func(query string, args ...any) int64 {
		var n int64
		require.NoError(t, db.Raw(query, args...).Scan(&n).Error)
		return n
	}
	legacyRows := `FROM taxable_events e JOIN messages m ON m.id = e.message_id JOIN txes x ON x.id = m.tx_id JOIN blocks b ON b.id = x.block_id JOIN chains c ON c.id = b.chain_id WHERE c.chain_id = ?`
	require.Equal(t, int64(3), count(`SELECT count(*) `+legacyRows, "cosmoshub-3"))
	require.Equal(t, int64(1), count(`SELECT count(*) `+legacyRows, "cosmoshub-4"), "a chain scoped read still sees exactly the cosmoshub-4 row")
	require.Equal(t, int64(4), count(`SELECT count(*) FROM taxable_events WHERE tx_hash = ?`, shared), "the true hash stays searchable across chains")
	require.Equal(t, int64(4), count(`SELECT count(*) FROM messages m JOIN txes x ON x.id = m.tx_id WHERE x.hash LIKE 'cosmoshub-3/%'`))
	require.Equal(t, int64(1), count(`SELECT count(*) FROM fees f JOIN txes x ON x.id = f.tx_id WHERE x.hash LIKE 'cosmoshub-3/%'`), "only the charged inclusion has a fee row")
	require.Equal(t, int64(1), count(`SELECT count(*) FROM addresses WHERE address = ?`, signer), "the shared address row is reused, not duplicated")
	require.Equal(t, int64(1), count(`SELECT count(*) FROM denoms WHERE base = 'uatom'`))
	require.Equal(t, int64(1), count(`SELECT count(*) FROM message_types WHERE message_type = '/cosmos.bank.v1beta1.MsgSend'`))

	// A rerun rewrites nothing at all, legacy rows included.
	stored := snapshot(t, db)
	require.NoError(t, ImportBlock(db, detailBlock(shared, when, signer)))
	after := snapshot(t, db)
	requireUntouched(t, stored, after)
	require.Len(t, after, len(stored))
	stored = snapshotStored(t, db)

	// A rerun that disagrees on any stored detail fails and changes nothing.
	for name, change := range map[string]func(*Detail){
		"fee amount":  func(d *Detail) { d.Fee[0].Amount = "8" },
		"fee removed": func(d *Detail) { d.Fee = nil },
		"fee denom":   func(d *Detail) { d.Fee[0].Denom = "synthetic-denom" },
		"row amount":  func(d *Detail) { d.Messages[1].Rows[0].Amount = "4" },
		"row added": func(d *Detail) {
			d.Messages[0].Rows = append(d.Messages[0].Rows, Row{Category: "transfer", Amount: "1", Denom: "uatom"})
		},
		"message type":    func(d *Detail) { d.Messages[0].TypeURL = "/cosmos.bank.v1beta1.MsgMultiSend" },
		"message missing": func(d *Detail) { d.Messages = d.Messages[:1] },
		"another signer":  func(d *Detail) { d.Signers = []string{"synthetic-other-signer"} },
	} {
		block := detailBlock(shared, when, signer)
		change(block.Inclusions[0].Detail)
		require.ErrorIs(t, ImportBlock(db, block), ErrStoredDiffers, name)
		final := snapshotStored(t, db)
		requireUntouched(t, stored, final)
		require.Len(t, final, len(stored), name)
	}
}

// snapshotExceptNamingLocks is the strict snapshot (tuple location, inserting
// transaction, xmax and every column) for every table except the three naming
// tables a legacy row may point at: addresses, denoms and message_types. A new
// row that references one of those takes a foreign key lock on it, which
// Postgres records in that row's xmax. No new row version is written, so for
// those tables the proof is the tuple location, its inserting transaction and
// every column. Transactions, messages, fees, signer links, tax rows, blocks
// and chains are held to the strict form.
func snapshotExceptNamingLocks(t *testing.T, db *gorm.DB) map[string]string {
	t.Helper()
	strict, stored := snapshot(t, db), snapshotStored(t, db)
	for key := range strict {
		switch strings.Fields(key)[0] {
		case "addresses", "denoms", "message_types":
			strict[key] = stored[key]
		}
	}
	return strict
}
