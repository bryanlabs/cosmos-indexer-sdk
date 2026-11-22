package taxapi

import (
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/stretchr/testify/require"
)

func TestPrivateAddressFirstEventsPostgres(t *testing.T) {
	db := effectiveFeesProofDB(t)
	for _, sql := range []string{
		"CREATE TABLE messages(id bigint PRIMARY KEY,tx_id bigint)",
		"CREATE TABLE taxable_events(id bigint PRIMARY KEY,message_id bigint,sub_index int,category text,amount text,denom text,asset text,from_addr text,to_addr text,validator_address text,reward_trigger text,detail text,block_height bigint,timestamp timestamptz,tx_hash text)",
		"CREATE INDEX idx_tax_from ON taxable_events(from_addr)",
		"CREATE INDEX idx_tax_to ON taxable_events(to_addr)",
		"CREATE INDEX idx_tax_time ON taxable_events(timestamp)",
		"INSERT INTO chains VALUES(4,'cosmoshub')",
		"INSERT INTO blocks VALUES(1,1,1,'2026-01-01 00:00Z'),(2,2,2,'2026-01-01 00:00Z'),(3,3,3,'2026-01-01 00:00Z'),(4,4,4,'2026-01-01 00:00Z')",
		"INSERT INTO txes VALUES(1,1,'hub',0),(2,2,'alias',32),(3,3,'foreign',0),(4,4,'alias2',0)",
		"INSERT INTO blocks VALUES(5,5,NULL,'2026-01-01 00:00Z'),(6,6,999,'2026-01-01 00:00Z')",
		"INSERT INTO txes VALUES(5,NULL,'null-block',0),(6,999,'missing-block',0),(7,5,'null-chain',0),(8,6,'missing-chain',0)",
		"INSERT INTO messages VALUES(1,1),(2,2),(3,3),(4,4),(5,NULL),(6,999),(7,5),(8,6),(9,7),(10,8)",
	} {
		require.NoError(t, db.Exec(sql).Error)
	}
	parse := func(value string) time.Time { x, e := time.Parse(time.RFC3339, value); require.NoError(t, e); return x }
	start, end := parse("2026-01-02T00:00:00Z"), parse("2026-01-03T00:00:00Z")
	events := []tax.TaxableEvent{}
	add := func(id, msg uint, from, to, denom string, at time.Time) {
		events = append(events, tax.TaxableEvent{ID: id, MessageID: msg, SubIndex: int(id), Category: "transfer", Amount: "123", Denom: denom, Asset: "asset-evidence", FromAddr: from, ToAddr: to, Detail: "retained detail", BlockHeight: 99, Timestamp: at, TxHash: "retained hash"})
	}
	add(1, 1, "proof-owner", "other", "uatom", start.Add(-time.Microsecond))
	add(4, 1, "other", "proof-owner", "voucher", start)
	add(3, 1, "proof-owner", "proof-owner", "uatom", start)
	add(2, 1, "proof-owner", "other", "uatom", start)
	add(5, 3, "proof-owner", "other", "uatom", start)
	add(6, 1, "proof-owner", "other", "uatom", end)
	add(7, 2, "proof-owner", "other", "non-native", start.Add(time.Second))
	add(8, 4, "proof-owner", "other", "uatom", start.Add(time.Second))
	add(9, 1, "proof-owner", "", "uatom", start.Add(time.Second))
	add(10, 999, "proof-owner", "other", "uatom", start)
	add(11, 0, "proof-owner", "other", "uatom", start)
	for msg := uint(5); msg <= 10; msg++ {
		add(msg+7, msg, "proof-owner", "other", "uatom", start)
	}
	require.NoError(t, db.Omit("Message").Create(&events).Error)
	require.NoError(t, db.Exec("UPDATE taxable_events SET to_addr=NULL WHERE id=9").Error)
	require.NoError(t, db.Exec("UPDATE taxable_events SET message_id=NULL WHERE id=11").Error)
	for _, c := range []struct {
		name, address string
		start, end    time.Time
		ids           []uint
	}{
		{"exact_midnight_all_coins_aliases_failed_tx_self_once", "proof-owner", start, end, []uint{2, 3, 4, 7, 8, 9}},
		{"utc_equivalent_offset", "proof-owner", parse("2026-01-02T01:00:00+01:00"), parse("2026-01-02T19:00:00-05:00"), []uint{2, 3, 4, 7, 8, 9}},
		{"end_exclusive", "proof-owner", start, start.Add(time.Second), []uint{2, 3, 4}},
		{"empty", "absent", start, end, []uint{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tx := db.Begin()
			require.NoError(t, tx.Error)
			defer tx.Rollback()
			require.NoError(t, tx.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY").Error)
			got, want := make([]tax.TaxableEvent, 0), make([]tax.TaxableEvent, 0)
			require.NoError(t, tx.Raw(privateWalletTaxEventsSQL, c.address, c.address, c.start, c.end, chainIDs(apiMainnet)).Scan(&got).Error)
			s := &Server{db: tx}
			require.NoError(t, s.scopedTaxEvents(apiMainnet).Select("taxable_events.*").Where("(from_addr=? OR to_addr=?)", c.address, c.address).Where("timestamp>=? AND timestamp<?", c.start, c.end).Order("timestamp asc,taxable_events.id asc").Find(&want).Error)
			require.Equal(t, want, got)
			ids := make([]uint, 0, len(got))
			for _, event := range got {
				ids = append(ids, event.ID)
			}
			require.Equal(t, c.ids, ids)
			var unassigned bool
			require.NoError(t, tx.Raw("SELECT txid_current_if_assigned() IS NULL").Scan(&unassigned).Error)
			require.True(t, unassigned)
		})
	}
}
