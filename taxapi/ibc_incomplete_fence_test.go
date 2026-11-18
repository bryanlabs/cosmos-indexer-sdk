package taxapi

import (
	"strings"
	"testing"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// The incomplete-refund attribute read must stay a per-event index lookup.
// As a plain join, 64 messages made Postgres hash the whole attribute table
// and a wallet's readiness hit the 60 s private read deadline.
func TestIncompleteRefundEventLogsReadsAttributesPerEvent(t *testing.T) {
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: "host=invalid.invalid"}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	require.NoError(t, err)
	var sql []string
	capture := func(tx *gorm.DB) { sql = append(sql, tx.Statement.SQL.String()) }
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("capture_query_sql", capture))
	require.NoError(t, db.Callback().Row().After("gorm:row").Register("capture_row_sql", capture))
	s := &Server{db: db.Session(&gorm.Session{Logger: db.Logger.LogMode(1)})}
	_, err = s.incompleteRefundEventLogs([]uint{1, 2, 3})
	if err != nil {
		require.ErrorIs(t, err, gorm.ErrDryRunModeUnsupported)
	}
	require.Len(t, sql, 1)
	require.Contains(t, sql[0], "JOIN LATERAL (SELECT message_event_attribute_key_id, value, index FROM message_event_attributes WHERE message_event_id=me.id OFFSET 0) a ON true")
	require.NotContains(t, sql[0], "JOIN message_event_attributes a ON")
	require.True(t, strings.HasSuffix(strings.TrimSpace(sql[0]), "ORDER BY me.message_id, me.index, a.index"), sql[0])
}

func TestIncompleteRefundEventLogsPostgres(t *testing.T) {
	s := flowPGServer(t)
	for _, sql := range []string{
		"CREATE TABLE message_event_types (id bigint, type text)",
		"CREATE TABLE message_events (id bigint, message_id bigint, message_event_type_id bigint, index bigint)",
		"CREATE TABLE message_event_attribute_keys (id bigint, key text)",
		"CREATE TABLE message_event_attributes (id bigint, message_event_id bigint, message_event_attribute_key_id bigint, value text, index bigint)",
		"INSERT INTO message_event_types VALUES (1,'recv_packet'),(2,'write_acknowledgement'),(3,'message')",
		"INSERT INTO message_event_attribute_keys VALUES (1,'packet_sequence'),(2,'packet_ack'),(3,'action')",
		// Stored out of order: message 701 has events at index 1 then 0;
		// message 702 has one event; message 703 is not asked for.
		"INSERT INTO message_events VALUES (11,701,2,1),(10,701,1,0),(20,702,3,0),(30,703,3,0)",
		"INSERT INTO message_event_attributes VALUES " +
			"(5,11,2,'ack',0),(3,10,1,'7',1),(4,10,3,'recv',0)," +
			"(6,20,3,'exec',0),(7,30,3,'other',0)",
	} {
		require.NoError(t, s.db.Exec(sql).Error, sql)
	}
	logs, err := s.incompleteRefundEventLogs([]uint{701, 702})
	require.NoError(t, err)
	require.Equal(t, map[uint]*txlog.LogMessage{
		701: {Events: []txlog.LogMessageEvent{
			{Type: "recv_packet", Attributes: []txlog.Attribute{{Key: "action", Value: "recv"}, {Key: "packet_sequence", Value: "7"}}},
			{Type: "write_acknowledgement", Attributes: []txlog.Attribute{{Key: "packet_ack", Value: "ack"}}},
		}},
		702: {Events: []txlog.LogMessageEvent{{Type: "message", Attributes: []txlog.Attribute{{Key: "action", Value: "exec"}}}}},
	}, logs)
}
