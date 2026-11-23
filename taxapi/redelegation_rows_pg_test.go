package taxapi

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// oldRedelegationRowsQuery is the single join redelegationRows ran before its
// messages were dated first; the new read must return exactly its rows.
func oldRedelegationRowsQuery(rel string) string {
	return `SELECT * FROM (` + datedBranches(`
SELECT me.id AS event_id, me.message_id, b.height, b.time_stamp, 1 AS phase, t.type, k.key, a.value, me.index AS event_index, a.index AS attr_index
FROM message_events me
JOIN message_event_types t ON t.id = me.message_event_type_id
JOIN messages m ON m.id = me.message_id
JOIN txes x ON x.id = m.tx_id
{{BLOCK}}
JOIN message_event_attributes a ON a.message_event_id = me.id
JOIN message_event_attribute_keys k ON k.id = a.message_event_attribute_key_id
WHERE me.message_id IN @msgs AND t.type IN ('redelegate', 'delegate', 'unbond', 'withdraw_rewards', 'message', 'transfer') {{OWN}}`, rel) + `) u
ORDER BY height, message_id, event_index, attr_index`
}

// Messages dated at their own block, moved by an effective inclusion to a
// later block, or left at their own block by a non-effective one, read with
// events of other types, events without attributes, attributes stored out of
// order, messages without events, repeated ids and an unknown id.
func TestRedelegationRowsMatchSingleJoinPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres redelegation read verification")
	}
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	connection.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	database = database.WithContext(ctx)
	for _, sql := range []string{
		"SET search_path = pg_temp",
		"CREATE TEMP TABLE blocks (id bigint PRIMARY KEY, chain_id bigint, height bigint, time_stamp timestamptz)",
		"CREATE TEMP TABLE txes (id bigint PRIMARY KEY, block_id bigint, hash text, code bigint)",
		"CREATE TEMP TABLE messages (id bigint PRIMARY KEY, tx_id bigint, message_index bigint)",
		"CREATE TEMP TABLE message_event_types (id bigint PRIMARY KEY, type text)",
		"CREATE TEMP TABLE message_event_attribute_keys (id bigint PRIMARY KEY, key text)",
		"CREATE TEMP TABLE message_events (id bigint PRIMARY KEY, message_id bigint, index bigint, message_event_type_id bigint, UNIQUE (message_id, index))",
		"CREATE TEMP TABLE message_event_attributes (id bigserial PRIMARY KEY, message_event_id bigint, value text, index bigint, message_event_attribute_key_id bigint, UNIQUE (message_event_id, index))",
		tempInclusionsTable,
		"CREATE UNIQUE INDEX ON tax_tx_inclusions (tx_id) WHERE effective",
		// Block ids run against height.
		"INSERT INTO blocks SELECT g, 1, 1000 - 10 * g, timestamptz '2026-03-01T00:00:00Z' - g * interval '6 seconds' FROM generate_series(1, 6) g",
		"INSERT INTO txes SELECT g, g, 'RRTX' || g, 0 FROM generate_series(1, 6) g",
		// tx 2 is effective at block 6, tx 3 at its own block, tx 4 only
		// non-effectively at block 1.
		`INSERT INTO tax_tx_inclusions (chain, height, tx_index, hash, tx_id, block_id, code, effective, recorded_at) VALUES
('cosmoshub-4', 940, 0, 'RRTX2', 2, 6, 0, true, now()),
('cosmoshub-4', 970, 0, 'RRTX3', 3, 3, 0, true, now()),
('cosmoshub-4', 990, 0, 'RRTX4', 4, 1, 0, false, now())`,
		// Messages 1 and 2 share tx 1, 3 and 8 share tx 2; 9 has no events.
		"INSERT INTO messages VALUES (1,1,0),(2,1,1),(3,2,0),(4,3,0),(5,4,0),(6,5,0),(7,6,0),(8,2,1),(9,5,1),(10,6,1)",
		"INSERT INTO message_event_types VALUES (1,'redelegate'),(2,'withdraw_rewards'),(3,'message'),(4,'transfer'),(5,'delegate'),(6,'unbond'),(7,'coin_received')",
		"INSERT INTO message_event_attribute_keys VALUES (1,'source_validator'),(2,'amount'),(3,'delegator'),(4,'sender')",
		`INSERT INTO message_events SELECT 10 * m + i, m, i, (m + i) % 7 + 1
FROM unnest(ARRAY[1,2,3,4,5,6,7,8,10]) m, generate_series(0, 3) i`,
		// Attributes go in last index first, and every event with
		// (message * index) % 5 = 3 has none.
		`INSERT INTO message_event_attributes (message_event_id, value, index, message_event_attribute_key_id)
SELECT me.id, 'v' || me.id || '/' || i, i, i % 4 + 1 FROM message_events me, generate_series(2, 0, -1) i
WHERE (me.message_id * me.index) % 5 <> 3 ORDER BY me.id, i DESC`,
	} {
		require.NoError(t, database.Exec(sql).Error, sql)
	}
	msgs := []uint64{7, 3, 1, 8, 4, 2, 5, 6, 9, 3, 1, 999}

	equal := func(t *testing.T, s *Server, placed map[uint64]int64) {
		t.Helper()
		rel, rp := s.effectiveRel()
		var want []ledgerAttrRow
		require.NoError(t, s.db.Raw(oldRedelegationRowsQuery(rel), withParams(rp, map[string]any{"msgs": msgs})).Scan(&want).Error)
		got, err := s.redelegationRows(msgs, rel, rp)
		require.NoError(t, err)
		require.NotEmpty(t, want)
		require.Equal(t, want, got)
		seen := map[uint64]bool{}
		for _, r := range got {
			seen[r.MessageID] = true
			if h, ok := placed[r.MessageID]; ok {
				require.Equal(t, h, r.Height, "message %d is dated by its effective inclusion", r.MessageID)
			}
		}
		require.False(t, seen[9] || seen[10] || seen[999])
		for m := range placed {
			require.True(t, seen[m], "message %d is read", m)
		}
	}

	t.Run("inclusion table", func(t *testing.T) {
		equal(t, &Server{db: database}, map[uint64]int64{3: 940, 8: 940, 4: 970, 5: 960})
	})
	t.Run("preview rows", func(t *testing.T) {
		equal(t, &Server{db: database, preview: &inclusionPreview{effective: []TxInclusion{
			{TxID: 5, BlockID: 1, Effective: true},
			{TxID: 1, BlockID: 1, Effective: true},
		}}}, map[uint64]int64{6: 990, 1: 990, 3: 980})
	})
	t.Run("empty preview", func(t *testing.T) {
		equal(t, &Server{db: database, preview: &inclusionPreview{}}, map[uint64]int64{3: 980, 6: 950})
	})

	t.Run("plan reads by id", func(t *testing.T) {
		// Enough other messages that a scan of the event or attribute table
		// costs more than reading the requested ones by id.
		for _, sql := range []string{
			"INSERT INTO txes SELECT g, g % 6 + 1, 'RRFILL' || g, 0 FROM generate_series(7, 2006) g",
			"INSERT INTO messages SELECT g, g % 2000 + 7, 0 FROM generate_series(1001, 21000) g",
			"INSERT INTO message_events SELECT 10 * m + i, m, i, i % 7 + 1 FROM generate_series(1001, 21000) m, generate_series(0, 2) i",
			`INSERT INTO message_event_attributes (message_event_id, value, index, message_event_attribute_key_id)
SELECT me.id, 'f' || me.id || '/' || i, i, i + 1 FROM message_events me, generate_series(0, 2) i WHERE me.message_id > 1000`,
			"ANALYZE blocks, txes, messages, message_events, message_event_attributes, message_event_types, message_event_attribute_keys, tax_tx_inclusions",
		} {
			require.NoError(t, database.Exec(sql).Error, sql)
		}
		s := &Server{db: database}
		equal(t, s, map[uint64]int64{3: 940, 8: 940, 4: 970, 5: 960})
		rel, rp := s.effectiveRel()
		var plan []string
		require.NoError(t, s.db.Raw("EXPLAIN (COSTS OFF) "+redelegationRowsQuery(rel), withParams(rp, map[string]any{"msgs": msgs})).Scan(&plan).Error)
		text := strings.Join(plan, "\n")
		t.Log("\n" + text)
		for _, line := range plan {
			if strings.Contains(line, "Seq Scan on message_events ") || strings.Contains(line, "Seq Scan on message_event_attributes ") {
				t.Fatalf("event or attribute table scanned:\n%s", text)
			}
		}
		require.Contains(t, text, "message_event_attributes_message_event_id_index_key")
		require.Contains(t, text, "message_events_message_id_index_key")
	})
}
