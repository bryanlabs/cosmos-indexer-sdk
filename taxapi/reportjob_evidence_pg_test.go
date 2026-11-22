package taxapi

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The evidence tables as the live database has them: the same columns,
// primary keys and foreign keys.
var evidenceTestSchema = []string{
	`CREATE TABLE chains (id bigserial PRIMARY KEY, chain_id text, name text)`,
	`CREATE TABLE addresses (id bigserial PRIMARY KEY, address text)`,
	`CREATE TABLE denoms (id bigserial PRIMARY KEY, base text)`,
	`CREATE TABLE message_types (id bigserial PRIMARY KEY, message_type text NOT NULL)`,
	`CREATE TABLE message_event_types (id bigserial PRIMARY KEY, type text)`,
	`CREATE TABLE message_event_attribute_keys (id bigserial PRIMARY KEY, key text)`,
	`CREATE TABLE blocks (id bigserial PRIMARY KEY, time_stamp timestamptz, height bigint, chain_id bigint REFERENCES chains(id), proposer_cons_address_id bigint REFERENCES addresses(id), tx_indexed boolean, block_events_indexed boolean)`,
	`CREATE TABLE txes (id bigserial PRIMARY KEY, hash text, code bigint, block_id bigint REFERENCES blocks(id), memo text)`,
	`CREATE TABLE failed_txes (id bigserial PRIMARY KEY, hash text, block_id bigint REFERENCES blocks(id))`,
	`CREATE TABLE messages (id bigserial PRIMARY KEY, tx_id bigint REFERENCES txes(id), message_type_id bigint REFERENCES message_types(id), message_index bigint, message_bytes bytea, opaque boolean DEFAULT false NOT NULL)`,
	`CREATE TABLE failed_messages (id bigserial PRIMARY KEY, message_index bigint, tx_id bigint REFERENCES txes(id))`,
	`CREATE TABLE fees (id bigserial PRIMARY KEY, tx_id bigint REFERENCES txes(id), amount numeric(78,0), denomination_id bigint REFERENCES denoms(id), payer_address_id bigint REFERENCES addresses(id))`,
	`CREATE TABLE tx_signer_addresses (tx_id bigint NOT NULL REFERENCES txes(id), address_id bigint NOT NULL REFERENCES addresses(id), PRIMARY KEY (tx_id, address_id))`,
	`CREATE TABLE message_events (id bigserial PRIMARY KEY, index bigint, message_id bigint REFERENCES messages(id), message_event_type_id bigint REFERENCES message_event_types(id))`,
	`CREATE TABLE message_event_attributes (id bigserial PRIMARY KEY, message_event_id bigint REFERENCES message_events(id), value text, index bigint, message_event_attribute_key_id bigint REFERENCES message_event_attribute_keys(id))`,
	`CREATE TABLE message_parser_errors (id bigserial PRIMARY KEY, message_parser_id bigint, message_id bigint REFERENCES messages(id), error text)`,
	`CREATE TABLE taxable_events (id bigserial PRIMARY KEY, message_id bigint REFERENCES messages(id), sub_index bigint, category text, amount text, denom text, from_addr text, to_addr text, block_height bigint, "timestamp" timestamptz, tx_hash text, asset text, validator_address text, reward_trigger text, detail text)`,
	`CREATE TABLE block_event_types (id bigserial PRIMARY KEY, type text)`,
	`CREATE TABLE block_event_attribute_keys (id bigserial PRIMARY KEY, key text)`,
	`CREATE TABLE block_events (id bigserial PRIMARY KEY, index bigint, lifecycle_position bigint, block_id bigint REFERENCES blocks(id), block_event_type_id bigint REFERENCES block_event_types(id))`,
	`CREATE TABLE block_event_attributes (id bigserial PRIMARY KEY, block_event_id bigint REFERENCES block_events(id), value text, index bigint, block_event_attribute_key_id bigint REFERENCES block_event_attribute_keys(id))`,
}

const (
	evWallet  = "cosmos1evidencewalletqqqqqqqqqqqqqqqqqqqqqqqq"
	evOther   = "cosmos1evidenceotherqqqqqqqqqqqqqqqqqqqqqqqqq"
	evValoper = "cosmosvaloper1evidencevalidatorqqqqqqqqqqqqqqqq"
)

var evidenceTestFixture = []string{
	`INSERT INTO chains (id, chain_id) VALUES (1, 'cosmoshub-4')`,
	`INSERT INTO addresses (id, address) VALUES (1, '` + evWallet + `'), (2, '` + evOther + `')`,
	`INSERT INTO denoms (id, base) VALUES (1, 'uatom')`,
	`INSERT INTO message_types (id, message_type) VALUES (1, '/cosmos.bank.v1beta1.MsgSend'), (2, '/cosmos.staking.v1beta1.MsgDelegate')`,
	`INSERT INTO message_event_types (id, type) VALUES (1, 'transfer')`,
	`INSERT INTO message_event_attribute_keys (id, key) VALUES (1, 'recipient'), (2, 'sender')`,
	`INSERT INTO block_event_types (id, type) VALUES (1, 'complete_unbonding')`,
	`INSERT INTO block_event_attribute_keys (id, key) VALUES (1, 'delegator'), (2, 'amount'), (3, 'validator')`,
	`INSERT INTO blocks (id, time_stamp, height, chain_id, tx_indexed, block_events_indexed) VALUES
	 (1, '2025-03-01T00:00:00Z', 100, 1, true, true),
	 (2, '2025-04-01T00:00:00Z', 200, 1, true, true),
	 (3, '2025-05-01T00:00:00Z', 300, 1, true, true)`,
	`INSERT INTO txes (id, hash, code, block_id, memo) VALUES (1, 'AA01', 0, 1, ''), (2, 'AA02', 0, 2, 'm'), (3, 'BB03', 0, 3, '')`,
	`INSERT INTO messages (id, tx_id, message_type_id, message_index, message_bytes) VALUES (1, 1, 1, 0, '\x01'), (2, 2, 2, 0, '\x02'), (3, 3, 1, 0, '\x03')`,
	`INSERT INTO tx_signer_addresses VALUES (1, 1), (2, 1), (3, 2)`,
	`INSERT INTO fees (tx_id, amount, denomination_id, payer_address_id) VALUES (1, 5000, 1, 1), (2, 4000, 1, 1), (3, 3000, 1, 2)`,
	`INSERT INTO message_events (id, index, message_id, message_event_type_id) VALUES (1, 0, 1, 1), (2, 0, 2, 1), (3, 0, 3, 1)`,
	`INSERT INTO message_event_attributes (message_event_id, value, index, message_event_attribute_key_id) VALUES
	 (1, '` + evOther + `', 0, 1), (1, '` + evWallet + `', 1, 2), (3, '` + evOther + `', 0, 1)`,
	`INSERT INTO taxable_events (message_id, sub_index, category, amount, denom, from_addr, to_addr, block_height, "timestamp", tx_hash, validator_address) VALUES
	 (1, 0, 'transfer', '10', 'uatom', '` + evWallet + `', '` + evOther + `', 100, '2025-03-01T00:00:00Z', 'AA01', NULL),
	 (2, 0, 'delegate', '20', 'uatom', '` + evWallet + `', NULL, 200, '2025-04-01T00:00:00Z', 'AA02', '` + evValoper + `'),
	 (3, 0, 'transfer', '30', 'uatom', '` + evOther + `', NULL, 300, '2025-05-01T00:00:00Z', 'BB03', NULL)`,
	`DO $$ DECLARE r record; BEGIN
	  FOR r IN SELECT c.relname FROM pg_class c WHERE c.relkind = 'S' AND c.relnamespace = (SELECT relnamespace FROM pg_class WHERE oid = 'txes'::regclass) LOOP
	    PERFORM setval(quote_ident(r.relname), 100);
	  END LOOP;
	END $$`,
}

type evidenceHarness struct {
	t      *testing.T
	db     *gorm.DB
	other  *gorm.DB
	server *Server
	key    string
	addrs  []string
}

func newEvidenceHarness(t *testing.T) *evidenceHarness {
	t.Helper()
	dsn := os.Getenv("COSMOS_TAX_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_TEST_DSN for native Postgres evidence journal verification")
	}
	open := func(dsn string) *gorm.DB {
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		require.NoError(t, err)
		conn, err := db.DB()
		require.NoError(t, err)
		conn.SetMaxOpenConns(4)
		t.Cleanup(func() { _ = conn.Close() })
		return db
	}
	admin := open(dsn)
	schema := fmt.Sprintf("ops817_%d", time.Now().UnixNano())
	require.NoError(t, admin.Exec("CREATE SCHEMA "+schema).Error)
	t.Cleanup(func() { _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error })
	scoped := dsn
	if strings.Contains(dsn, "://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		scoped += sep + "search_path=" + schema
	} else {
		scoped += " search_path=" + schema
	}
	db := open(scoped)
	other := open(scoped)
	for _, sql := range append(append([]string{}, evidenceTestSchema...), evidenceTestFixture...) {
		require.NoError(t, db.Exec(sql).Error, sql)
	}
	require.NoError(t, db.AutoMigrate(&ReportJob{}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	require.NoError(t, InstallEvidenceJournal(ctx, db, 3*time.Second, 1, t.Logf))
	return &evidenceHarness{t: t, db: db, other: other, server: &Server{db: db}, key: "ops817-test", addrs: []string{evWallet}}
}

func (h *evidenceHarness) exec(sql string, args ...any) {
	h.t.Helper()
	require.NoError(h.t, h.db.Exec(sql, args...).Error, sql)
}

// fingerprint is the full check's digest now.
func (h *evidenceHarness) fingerprint() string {
	h.t.Helper()
	var fp string
	require.NoError(h.t, h.server.withReadSnapshot(func(r *Server) error {
		var err error
		fp, err = r.reviewFingerprintInSnapshot(apiMainnet, h.addrs)
		return err
	}))
	return fp
}

// check runs the full check and stores the job as a ready report with its
// evidence check, the way a passing full check does.
func (h *evidenceHarness) check(end time.Time) string {
	h.t.Helper()
	var fp string
	var rec *evidenceRecord
	require.NoError(h.t, h.server.withReadSnapshot(func(r *Server) error {
		var err error
		fp, rec, err = r.reportEvidenceInSnapshot(apiMainnet, h.addrs, end)
		return err
	}))
	require.NotNil(h.t, rec, "journal should vouch for the report")
	require.Equal(h.t, h.fingerprint(), fp, "collecting the evidence check must not change the digest")
	h.exec("DELETE FROM report_jobs WHERE key = ?", h.key)
	job := ReportJob{
		Key: h.key, Chain: apiMainnet, Status: ReportJobReady, ReviewFingerprint: fp, ComputedThrough: time.Now().UTC(),
		EvidenceCheck: rec.check, EvidenceSnapshot: rec.snapshot, ReadinessJSON: "{}",
	}
	require.NoError(h.t, h.db.Create(&job).Error)
	return fp
}

func (h *evidenceHarness) fast(end time.Time) bool {
	h.t.Helper()
	job, ok := h.server.reportJobFastPath(h.key, apiMainnet, h.addrs, end)
	if ok {
		require.Equal(h.t, ReportJobReady, job.Status)
	}
	return ok
}

// changed asserts a write the full check sees is never served from the
// journal.
func (h *evidenceHarness) changed(name string, end time.Time, stored string, write func()) {
	h.t.Helper()
	write()
	require.NotEqual(h.t, stored, h.fingerprint(), "%s: the full check should see this write", name)
	require.False(h.t, h.fast(end), "%s: a changed report must not be served from the journal", name)
}

// refused asserts a write the journal cannot rule out sends the view to the
// full check.
func (h *evidenceHarness) refused(name string, end time.Time, write func()) {
	h.t.Helper()
	write()
	require.False(h.t, h.fast(end), "%s: should run the full check", name)
}

// served asserts a write that cannot change the report leaves it served.
func (h *evidenceHarness) served(name string, end time.Time, stored string, write func()) {
	h.t.Helper()
	write()
	require.Equal(h.t, stored, h.fingerprint(), "%s: the full check should not see this write", name)
	require.True(h.t, h.fast(end), "%s: an unchanged report should be served from the journal", name)
}

func TestReportEvidenceJournalPostgres(t *testing.T) {
	h := newEvidenceHarness(t)
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	trust, err := readEvidenceTrust(h.db)
	require.NoError(t, err)
	require.True(t, trust.ok(), "%+v", trust)
	require.Equal(t, 64, trust.Triggers)
	require.Equal(t, 17, trust.ForeignKeys)

	fp := h.check(end)
	require.True(t, h.fast(end), "an untouched report is served")
	require.True(t, h.fast(end), "and again from the advanced snapshot")

	t.Run("unrelated writes at the tip", func(t *testing.T) {
		fp := h.check(end)
		h.served("new block of another wallet", end, fp, func() {
			h.exec(`INSERT INTO blocks (id, time_stamp, height, chain_id) VALUES (20, '2025-06-01T00:00:00Z', 400, 1)`)
			h.exec(`INSERT INTO txes (id, hash, code, block_id) VALUES (20, 'CC20', 0, 20)`)
			h.exec(`INSERT INTO messages (id, tx_id, message_type_id, message_index) VALUES (20, 20, 1, 0)`)
			h.exec(`INSERT INTO tx_signer_addresses VALUES (20, 2)`)
			h.exec(`INSERT INTO fees (tx_id, amount, denomination_id, payer_address_id) VALUES (20, 1, 1, 2)`)
			h.exec(`INSERT INTO message_events (id, index, message_id, message_event_type_id) VALUES (20, 0, 20, 1)`)
			h.exec(`INSERT INTO message_event_attributes (message_event_id, value, index, message_event_attribute_key_id) VALUES (20, ?, 0, 1)`, evOther)
			h.exec(`INSERT INTO taxable_events (message_id, category, from_addr, "timestamp") VALUES (20, 'transfer', ?, '2025-06-01T00:00:00Z')`, evOther)
			h.exec(`INSERT INTO addresses (address) VALUES ('cosmos1newaddressqqqqqqqqqqqqqqqqqqqqqqqqqqqqq')`)
			h.exec(`INSERT INTO message_types (message_type) VALUES ('/new.Type')`)
		})
		h.served("an indexer upsert that rewrites a wallet row unchanged", end, fp, func() {
			h.exec(`INSERT INTO txes (id, hash, code, block_id, memo) VALUES (1, 'AA01', 0, 1, '') ON CONFLICT (id) DO UPDATE SET hash = excluded.hash, code = excluded.code, block_id = excluded.block_id, memo = excluded.memo`)
			h.exec(`UPDATE blocks SET tx_indexed = true WHERE id IN (1, 2)`)
			h.exec(`UPDATE message_types SET message_type = message_type`)
		})
	})

	t.Run("replayed block", func(t *testing.T) {
		fp := h.check(end)
		h.changed("block replay changes a result code", end, fp, func() {
			h.exec(`UPDATE txes SET code = 5 WHERE id = 2`)
		})
		fp = h.check(end)
		h.changed("block replay rewrites the block row", end, fp, func() {
			h.exec(`UPDATE blocks SET block_events_indexed = false WHERE id = 1`)
		})
		fp = h.check(end)
		h.changed("replay deletes and reinserts a taxable event with a new amount", end, fp, func() {
			h.exec(`DELETE FROM taxable_events WHERE message_id = 1`)
			h.exec(`INSERT INTO taxable_events (message_id, sub_index, category, amount, denom, from_addr, to_addr, block_height, "timestamp", tx_hash) VALUES (1, 0, 'transfer', '11', 'uatom', ?, ?, 100, '2025-03-01T00:00:00Z', 'AA01')`, evWallet, evOther)
		})
	})

	t.Run("parser error added or cleared", func(t *testing.T) {
		fp := h.check(end)
		h.changed("parser error added", end, fp, func() {
			h.exec(`INSERT INTO message_parser_errors (message_id, error) VALUES (2, 'bad')`)
		})
		fp = h.check(end)
		h.changed("parser error cleared", end, fp, func() {
			h.exec(`DELETE FROM message_parser_errors WHERE message_id = 2`)
		})
		fp = h.check(end)
		h.changed("failed message recorded", end, fp, func() {
			h.exec(`INSERT INTO failed_messages (message_index, tx_id) VALUES (1, 1)`)
		})
		fp = h.check(end)
		h.changed("failed transaction recorded in a wallet block", end, fp, func() {
			h.exec(`INSERT INTO failed_txes (hash, block_id) VALUES ('AA01', 1)`)
		})
	})

	t.Run("deleted transaction", func(t *testing.T) {
		h.exec(`INSERT INTO blocks (id, time_stamp, height, chain_id) VALUES (30, '2025-07-01T00:00:00Z', 500, 1)`)
		h.exec(`INSERT INTO txes (id, hash, code, block_id) VALUES (30, 'DD30', 0, 30)`)
		h.exec(`INSERT INTO messages (id, tx_id, message_type_id, message_index) VALUES (30, 30, 1, 0)`)
		h.exec(`INSERT INTO tx_signer_addresses VALUES (30, 1)`)
		fp := h.check(end)
		h.changed("wallet transaction deleted", end, fp, func() {
			h.exec(`DELETE FROM tx_signer_addresses WHERE tx_id = 30`)
			h.exec(`DELETE FROM messages WHERE tx_id = 30`)
			h.exec(`DELETE FROM txes WHERE id = 30`)
		})
	})

	t.Run("new links to the wallet", func(t *testing.T) {
		fp := h.check(end)
		h.changed("event attribute names the wallet on another wallet's old transaction", end, fp, func() {
			h.exec(`INSERT INTO message_event_attributes (message_event_id, value, index, message_event_attribute_key_id) VALUES (3, ?, 1, 2)`, evWallet)
		})
		fp = h.check(end)
		h.changed("wallet signs a transaction in a new block", end, fp, func() {
			h.exec(`INSERT INTO blocks (id, time_stamp, height, chain_id) VALUES (40, '2025-08-01T00:00:00Z', 600, 1)`)
			h.exec(`INSERT INTO txes (id, hash, code, block_id) VALUES (40, 'EE40', 0, 40)`)
			h.exec(`INSERT INTO tx_signer_addresses VALUES (40, 1)`)
		})
		fp = h.check(end)
		h.changed("wallet pays a fee on an old transaction", end, fp, func() {
			h.exec(`INSERT INTO fees (tx_id, amount, denomination_id, payer_address_id) VALUES (3, 1, 1, 1)`)
		})
		fp = h.check(end)
		h.changed("taxable event names the wallet as recipient", end, fp, func() {
			h.exec(`UPDATE taxable_events SET to_addr = ? WHERE message_id = 3`, evWallet)
		})
	})

	t.Run("moved rows and lookup tables", func(t *testing.T) {
		fp := h.check(end)
		h.changed("message moved onto a wallet transaction", end, fp, func() {
			h.exec(`UPDATE messages SET tx_id = 1 WHERE id = 20`)
		})
		h.check(end)
		h.refused("transaction moved to another block", end, func() {
			h.exec(`UPDATE txes SET block_id = 30 WHERE id = 20`)
		})
		h.check(end)
		h.refused("message type renamed (the gates read type names)", end, func() {
			h.exec(`UPDATE message_types SET message_type = '/cosmos.bank.v1beta1.MsgMultiSend' WHERE id = 1`)
		})
		h.check(end)
		h.refused("address row rewritten", end, func() {
			h.exec(`UPDATE addresses SET address = ? WHERE id = 2`, "cosmos1renamedqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq")
		})
		h.check(end)
		h.refused("lookup table truncated", end, func() {
			h.exec(`TRUNCATE block_event_attributes, block_events, failed_messages`)
		})
	})

	t.Run("block events", func(t *testing.T) {
		fp := h.check(end)
		h.changed("block event names the wallet", end, fp, func() {
			h.exec(`INSERT INTO block_events (id, index, lifecycle_position, block_id, block_event_type_id) VALUES (1, 0, 0, 3, 1)`)
			h.exec(`INSERT INTO block_event_attributes (block_event_id, value, index, block_event_attribute_key_id) VALUES (1, ?, 0, 1), (1, '5uatom', 1, 2), (1, ?, 2, 3)`, evWallet, evValoper)
		})
	})

	t.Run("period end", func(t *testing.T) {
		h.check(end)
		require.True(t, h.fast(end.Add(48*time.Hour)), "a later default end after all evidence reads the same rows")
		require.False(t, h.fast(end.Add(-time.Hour)), "an earlier end may cut evidence")
		early := time.Date(2025, 4, 15, 0, 0, 0, 0, time.UTC)
		h.check(early)
		require.True(t, h.fast(early), "the same explicit end")
		require.False(t, h.fast(early.Add(time.Hour)), "an end inside the evidence reads different rows")
	})

	t.Run("write in flight during the check", func(t *testing.T) {
		tx := h.other.Begin()
		require.NoError(t, tx.Error)
		require.NoError(t, tx.Exec(`INSERT INTO message_event_attributes (message_event_id, value, index, message_event_attribute_key_id) VALUES (20, ?, 5, 2)`, evWallet).Error)
		fp := h.check(end)
		require.NoError(t, tx.Commit().Error)
		require.NotEqual(t, fp, h.fingerprint())
		require.False(t, h.fast(end), "a write committed after the check's snapshot is seen")
	})

	t.Run("journal trust", func(t *testing.T) {
		h.check(end)
		require.True(t, h.fast(end))
		h.refused("trigger disabled", end, func() {
			h.exec(`ALTER TABLE txes DISABLE TRIGGER report_evidence_ins`)
		})
		h.exec(`ALTER TABLE txes ENABLE ALWAYS TRIGGER report_evidence_ins`)
		require.False(t, h.fast(end), "a trigger disabled and enabled again since the check")

		h.check(end)
		h.refused("column added", end, func() {
			h.exec(`ALTER TABLE txes ADD COLUMN extra text`)
		})

		h.check(end)
		h.refused("foreign key dropped", end, func() {
			h.exec(`ALTER TABLE messages DROP CONSTRAINT messages_tx_id_fkey`)
		})
		trust, err := readEvidenceTrust(h.db)
		require.NoError(t, err)
		require.False(t, trust.ok())
		h.exec(`ALTER TABLE messages ADD CONSTRAINT messages_tx_id_fkey FOREIGN KEY (tx_id) REFERENCES txes(id)`)

		h.check(end)
		h.refused("journal rows pruned past the check", end, func() {
			h.exec(`UPDATE report_evidence_pruned SET below = pg_current_xact_id()`)
		})

		h.check(end)
		var superuser bool
		require.NoError(t, h.db.Raw("SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Row().Scan(&superuser))
		if superuser {
			h.refused("write with foreign keys off", end, func() {
				require.NoError(t, h.db.Transaction(func(tx *gorm.DB) error {
					if err := tx.Exec("SET LOCAL session_replication_role = replica").Error; err != nil {
						return err
					}
					return tx.Exec(`INSERT INTO failed_txes (hash, block_id) VALUES ('FF99', 999)`).Error
				}))
			})
			trust, err := readEvidenceTrust(h.db)
			require.NoError(t, err)
			require.False(t, trust.Trusted)
			var rec *evidenceRecord
			require.NoError(t, h.server.withReadSnapshot(func(r *Server) error {
				var err error
				_, rec, err = r.reportEvidenceInSnapshot(apiMainnet, h.addrs, end)
				return err
			}))
			require.Nil(t, rec, "an untrusted journal vouches for nothing")
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			require.NoError(t, InstallEvidenceJournal(ctx, h.db, 3*time.Second, 1, t.Logf))
		}

		h.check(end)
		require.Error(t, h.db.Exec(`TRUNCATE report_evidence_log`).Error, "the journal cannot be truncated")
		require.True(t, h.fast(end))
		h.exec(`UPDATE report_jobs SET evidence_snapshot = 'x' || evidence_snapshot WHERE key = ?`, h.key)
		require.False(t, h.fast(end), "a snapshot that does not belong to the check")
	})

	t.Run("journal off", func(t *testing.T) {
		h.check(end)
		t.Setenv("TAX_REPORT_EVIDENCE_JOURNAL", "off")
		require.False(t, h.fast(end))
	})

	t.Run("prune keeps recent rows", func(t *testing.T) {
		h.check(end)
		n, err := PruneEvidenceJournal(h.db)
		require.NoError(t, err)
		require.Zero(t, n)
		require.True(t, h.fast(end))
		h.exec(`UPDATE report_evidence_log SET logged_at = logged_at - interval '30 hours'`)
		h.exec(`UPDATE report_evidence_link SET logged_at = logged_at - interval '30 hours'`)
		n, err = PruneEvidenceJournal(h.db)
		require.NoError(t, err)
		require.Positive(t, n)
		h.check(end)
		require.True(t, h.fast(end), "a check made after the prune")
	})
	_ = fp
}

// The ready poll serves a cached report through getOrStartReportJob and
// recomputes nothing while the wallet is untouched.
func TestReportJobPollUsesEvidenceJournalPostgres(t *testing.T) {
	h := newEvidenceHarness(t)
	start := time.Time{}
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	key, err := reportJobKey(apiMainnet, h.addrs, "", "2026-01-01", apiSumm, nil, nil)
	require.NoError(t, err)
	h.key = key
	require.NoError(t, h.server.requireReportReady(apiMainnet, h.addrs, start, end))
	fp := h.fingerprint()
	require.NoError(t, h.db.Create(&ReportJob{Key: key, Chain: apiMainnet, Status: ReportJobReady, ReviewFingerprint: fp, ComputedThrough: time.Now().UTC(), CSV: "cached", ReadinessJSON: "{}"}).Error)

	// The first poll runs the full check and keeps the evidence check.
	job, err := h.server.getOrStartReportJob(apiMainnet, h.addrs, start, end, "", "2026-01-01", apiSumm, false, nil, nil)
	require.NoError(t, err)
	require.Equal(t, ReportJobReady, job.Status)
	require.Equal(t, "cached", job.CSV)
	var stored ReportJob
	require.NoError(t, h.db.Where("key = ?", key).First(&stored).Error)
	require.NotEmpty(t, stored.EvidenceCheck)

	// The next poll is served from the journal and moves the snapshot on.
	_, ok := h.server.reportJobFastPath(key, apiMainnet, h.addrs, end)
	require.True(t, ok)
	h.exec(`INSERT INTO message_types (message_type) VALUES ('/tip.Type')`)
	job, err = h.server.getOrStartReportJob(apiMainnet, h.addrs, start, end, "", "2026-01-01", apiSumm, false, nil, nil)
	require.NoError(t, err)
	require.Equal(t, ReportJobReady, job.Status)
	var advanced ReportJob
	require.NoError(t, h.db.Where("key = ?", key).First(&advanced).Error)
	require.NotEqual(t, stored.EvidenceSnapshot, advanced.EvidenceSnapshot)
	require.Equal(t, stored.EvidenceCheck, advanced.EvidenceCheck)

	// A change to the wallet sends the poll to the full check, which starts a
	// new build.
	h.exec(`UPDATE txes SET code = 7 WHERE id = 1`)
	_, ok = h.server.reportJobFastPath(key, apiMainnet, h.addrs, end)
	require.False(t, ok)
	inFlight.Store(key, struct{}{})
	defer inFlight.Delete(key)
	job, err = h.server.getOrStartReportJob(apiMainnet, h.addrs, start, end, "", "2026-01-01", apiSumm, false, nil, nil)
	require.NoError(t, err)
	require.Equal(t, ReportJobPending, job.Status)
}
