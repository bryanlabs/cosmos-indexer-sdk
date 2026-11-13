package taxapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type packetIdentityQueryTrace struct {
	logger.Interface
	queries []string
}

func (l *packetIdentityQueryTrace) Trace(_ context.Context, _ time.Time, query func() (string, int64), _ error) {
	sql, _ := query()
	if strings.Contains(sql, "FROM message_events me") {
		l.queries = append(l.queries, sql)
	}
}

// This dedicated-database proof preserves its schema and all fixture rows.
func TestOutboundPacketIdentityIndexedBatchPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_VERSIONS_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_VERSIONS_TEST_DSN to a dedicated test database")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	schema := fmt.Sprintf("outbound_index_proof_%d", time.Now().UnixNano())
	require.NoError(t, db.Exec("CREATE SCHEMA "+schema).Error)
	bootstrap, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, bootstrap.Close())
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		require.NoError(t, err)
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	conn, err := db.DB()
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	for _, sql := range []string{
		"CREATE TABLE message_event_types (id bigint PRIMARY KEY,type text)",
		"CREATE TABLE message_events (id bigint PRIMARY KEY,message_id bigint,message_event_type_id bigint,index integer)",
		"CREATE UNIQUE INDEX message_event_lookup ON message_events(message_id,index)",
		"CREATE TABLE message_event_attribute_keys (id bigint PRIMARY KEY,key text)",
		"CREATE TABLE message_event_attributes (id bigint PRIMARY KEY,message_event_id bigint,message_event_attribute_key_id bigint,index integer,value text)",
		"CREATE UNIQUE INDEX attribute_event_lookup ON message_event_attributes(message_event_id,index)",
		"INSERT INTO message_event_types VALUES (1,'send_packet'),(2,'transfer')",
		"INSERT INTO message_event_attribute_keys VALUES (1,'packet_sequence'),(2,'packet_src_port'),(3,'packet_src_channel'),(4,'packet_dst_port'),(5,'packet_dst_channel')",
		"INSERT INTO message_events SELECT g*10,g,1,0 FROM generate_series(1,503) g ORDER BY g DESC",
		"INSERT INTO message_events SELECT g*10+1,g,2,1 FROM generate_series(1,502) g",
		"INSERT INTO message_events VALUES (5031,503,1,1)",
		`INSERT INTO message_event_attributes SELECT g*10+k,g*10,k,k-1,CASE k WHEN 1 THEN g::text WHEN 2 THEN 'transfer' WHEN 3 THEN 'channel-1' WHEN 4 THEN 'transfer' WHEN 5 THEN 'channel-2' END FROM generate_series(1,503) g CROSS JOIN generate_series(1,5) k ORDER BY g DESC,k DESC`,
		"INSERT INTO message_event_attributes SELECT 10000+g,g*10+1,1,0,'999999' FROM generate_series(1,502) g",
		`INSERT INTO message_event_attributes SELECT 20000+k,5031,k,k-1,CASE k WHEN 1 THEN '9999' WHEN 2 THEN 'transfer' WHEN 3 THEN 'channel-1' WHEN 4 THEN 'transfer' WHEN 5 THEN 'channel-2' END FROM generate_series(1,5) k`,
		"SET default_transaction_read_only=on",
		"SET statement_timeout='10s'",
	} {
		require.NoError(t, db.Exec(sql).Error)
	}
	events := []tax.TaxableEvent{{ID: 9000, Category: string(tax.CategoryProtocolWithdrawal), ToAddr: flowTestReceiver, RewardTrigger: ibcTimeoutRefund}}
	for id := 1; id <= 504; id++ {
		events = append(events, tax.TaxableEvent{ID: uint(id), MessageID: uint(id), FromAddr: flowTestReceiver, Category: string(tax.CategoryIBCOut)})
	}
	// Repeated economic legs of one message must not duplicate its packet
	// attributes when its identity is reconstructed across batch boundaries.
	events = append(events, tax.TaxableEvent{ID: 10000, MessageID: 1, FromAddr: flowTestReceiver, Category: string(tax.CategoryIBCOut)})
	trace := &packetIdentityQueryTrace{Interface: logger.Default.LogMode(logger.Silent)}
	s := &Server{db: db.Session(&gorm.Session{Logger: trace}), readOnlyLedger: true}
	require.NoError(t, s.withReadSnapshot(func(reader *Server) error {
		require.True(t, reader.readOnlyLedger, "read-only facade must retain the private write guard")
		deadline, bounded := reader.db.Statement.Context.Deadline()
		require.True(t, bounded)
		require.LessOrEqual(t, time.Until(deadline), 30*time.Second, "private snapshot deadline must not be raised")
		var readOnly string
		require.NoError(t, reader.db.Raw("SHOW transaction_read_only").Scan(&readOnly).Error)
		require.Equal(t, "on", readOnly)
		return reader.retainLegacyOutboundIdentities(events)
	}))
	require.Len(t, trace.queries, 16, "504 unique messages use sixteen bounded batches")
	inList := regexp.MustCompile(`IN \(([^)]*)\)`)
	for _, query := range trace.queries {
		ids := inList.FindStringSubmatch(query)
		require.Len(t, ids, 2)
		require.LessOrEqual(t, len(strings.Split(ids[1], ",")), 32)
		require.Contains(t, query, "JOIN LATERAL")
	}
	for id := 1; id <= 502; id++ {
		var key []string
		require.NoError(t, json.Unmarshal([]byte(tax.IBCPacketKey(events[id].Detail)), &key))
		require.Equal(t, []string{"v1", "transfer", "channel-1", strconv.Itoa(id)}, key)
	}
	require.Empty(t, events[503].Detail, "ambiguous packets must remain unresolved")
	require.Empty(t, events[504].Detail, "missing evidence must remain unresolved")
	require.Equal(t, events[1].Detail, events[505].Detail, "duplicate message legs retain exactly one packet identity")
	failed := append([]tax.TaxableEvent(nil), events...)
	for i := range failed {
		failed[i].Detail = ""
	}
	packetQueries := 0
	require.NoError(t, s.db.Callback().Row().Before("gorm:row").Register("ops818_fail_second_packet_batch", func(tx *gorm.DB) {
		if tx.Statement.TableExpr != nil && tx.Statement.TableExpr.SQL == "message_events me" {
			packetQueries++
			if packetQueries == 2 {
				tx.AddError(errors.New("injected packet read failure"))
			}
		}
	}))
	err = s.withReadSnapshot(func(reader *Server) error { return reader.retainLegacyOutboundIdentities(failed) })
	require.ErrorContains(t, err, "batch 2/16")
	require.ErrorContains(t, err, "injected packet read failure")
	for _, event := range failed {
		require.Empty(t, event.Detail, "a failed batch cannot publish partial evidence")
	}
	t.Logf("preserved proof schema: %s; exact packet identities: 502", schema)
}
