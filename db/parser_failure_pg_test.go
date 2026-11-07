package db_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	indexdb "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/parsers"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type rollbackInvalidator struct{ *tax.Parser }

func (p *rollbackInvalidator) InvalidateMessage(database *gorm.DB, message models.Message) error {
	if err := p.Parser.InvalidateMessage(database, message); err != nil {
		return err
	}
	return errors.New("simulated invalidation failure after deletion")
}

func TestMessageParserFailureDryRunDoesNotWrite(t *testing.T) {
	require.NoError(t, indexdb.IndexCustomMessages(config.IndexConfig{}, nil, true, nil, nil))
}

// This optional native-Postgres test uses only connection-local temporary tables.
// No public schema migration or persistent user data is read or modified.
func TestMessageParserFailureInvalidationPostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for the native Postgres transaction test")
	}
	database, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	connection.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	database = database.WithContext(ctx)
	for _, sql := range []string{
		"SET search_path = pg_temp",
		"CREATE TEMP TABLE taxable_events (id bigserial PRIMARY KEY, message_id bigint NOT NULL)",
		"CREATE TEMP TABLE message_parser_errors (id bigserial PRIMARY KEY, message_id bigint NOT NULL, message_parser_id bigint NOT NULL, error text NOT NULL)",
		"INSERT INTO pg_temp.taxable_events (message_id) VALUES (42),(43)",
		"INSERT INTO pg_temp.message_parser_errors (message_id,message_parser_id,error) VALUES (42,7,'prior diagnostic')",
	} {
		require.NoError(t, database.Exec(sql).Error)
	}
	var parser parsers.MessageParser = &tax.Parser{}
	wrapper := []indexdb.TxDBWrapper{{Messages: []indexdb.MessageDBWrapper{{
		Message: models.Message{ID: 42},
		MessageParsedDatasets: []parsers.MessageParsedData{{
			Error: errors.New("classification incomplete: test unknown child"), Parser: &parser,
		}},
	}}}}
	trackers := map[string]models.MessageParser{parser.Identifier(): {ID: 7}}
	require.NoError(t, indexdb.IndexCustomMessages(config.IndexConfig{}, database, false, wrapper, trackers))
	var count int64
	require.NoError(t, database.Raw("SELECT count(*) FROM pg_temp.taxable_events WHERE message_id=42").Scan(&count).Error)
	require.Zero(t, count, "an incomplete replacement must not leave prior tax rows")
	require.NoError(t, database.Raw("SELECT count(*) FROM pg_temp.taxable_events WHERE message_id=43").Scan(&count).Error)
	require.Equal(t, int64(1), count, "other messages must remain unchanged")
	var diagnostic string
	require.NoError(t, database.Raw("SELECT error FROM pg_temp.message_parser_errors WHERE message_id=42").Scan(&diagnostic).Error)
	require.Equal(t, "classification incomplete: test unknown child", diagnostic)

	require.NoError(t, database.Exec("INSERT INTO pg_temp.taxable_events (message_id) VALUES (42)").Error)
	parser = &rollbackInvalidator{Parser: &tax.Parser{}}
	err = indexdb.IndexCustomMessages(config.IndexConfig{}, database, false, wrapper, trackers)
	require.ErrorContains(t, err, "simulated invalidation failure")
	require.NoError(t, database.Raw("SELECT count(*) FROM pg_temp.taxable_events WHERE message_id=42").Scan(&count).Error)
	require.Equal(t, int64(1), count, "failed replacement must roll back deletion")
	var after string
	require.NoError(t, database.Raw("SELECT error FROM pg_temp.message_parser_errors WHERE message_id=42").Scan(&after).Error)
	require.Equal(t, diagnostic, after, "failed replacement must also roll back diagnostic changes")
}
