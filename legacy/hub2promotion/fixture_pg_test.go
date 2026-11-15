package hub2promotion

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/legacy/schema/pgtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

type fixture struct {
	owner, loader *sql.DB
	m             Manifest
	runtime       Identity
	dir           string
	publicWriter  string
}

func randomID() string { b := make([]byte, 6); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func artifactDir(t *testing.T) string {
	t.Helper()
	root := os.Getenv("HUB2_PROMOTION_ARTIFACT_ROOT")
	require.NotEmpty(t, root, "retained private artifact root required, tests never skip")
	p := filepath.Join(root, "fixture-"+randomID())
	require.NoError(t, os.MkdirAll(p, 0700))
	return p
}
func savePin(t *testing.T, dir, name string, data []byte) Pin {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, data, 0600))
	h := sha256.Sum256(data)
	return Pin{p, hex.EncodeToString(h[:])}
}
func digest(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func (f fixture) full(t *testing.T) Snapshot {
	s, e := snapshot(context.Background(), f.owner, false, true)
	require.NoError(t, e)
	return s
}
func canonicalRow(t *testing.T, db *sql.DB, tb table, row map[string]any) string {
	t.Helper()
	b, e := json.Marshal(row)
	require.NoError(t, e)
	var s string
	require.NoError(t, db.QueryRow("SELECT pg_catalog.to_jsonb(r)::text FROM pg_catalog.jsonb_populate_record(NULL::legacy."+tb.name+",$1::jsonb) r", string(b)).Scan(&s))
	return s
}
func fixtureRow(name, chain, genSHA string) map[string]any {
	tm := "2020-01-01T00:00:00+00:00"
	switch name {
	case "genesis_files":
		return map[string]any{"chain_id": chain, "sha256": genSHA, "genesis_time": tm, "accounts": 1, "delegations": 1, "unbonding_entries": 1, "validators": 1, "withdraw_addresses": 1, "genesis_txs": 0, "loaded_at": tm}
	case "genesis_accounts":
		return map[string]any{"chain_id": chain, "address": "synthetic-account", "file_position": 0, "account_number": 0, "sequence": 0, "kind": "base", "module_name": "", "file_coins": 20, "liquid": 10, "delegated": 9, "unbonding": 1, "original_vesting": 0, "delegated_free": 0, "delegated_vesting": 0, "vesting_start": 0, "vesting_end": 0}
	case "genesis_delegations":
		return map[string]any{"chain_id": chain, "delegator": "synthetic-account", "validator": "synthetic-validator", "shares": "9.000000000000000000", "tokens": 9, "from_genesis_tx": false}
	case "genesis_unbondings":
		return map[string]any{"chain_id": chain, "delegator": "synthetic-account", "validator": "synthetic-validator", "entry": 0, "creation_height": 0, "completion_time": tm, "initial_balance": 1, "balance": 1}
	case "genesis_validators":
		return map[string]any{"chain_id": chain, "operator": "synthetic-validator", "tokens": 9, "delegator_shares": "9.000000000000000000", "status": "bonded", "jailed": false, "outstanding_rewards": "0.000000000000000000", "accumulated_commission": "0.000000000000000000"}
	case "genesis_withdraw_addresses":
		return map[string]any{"chain_id": chain, "delegator": "synthetic-account", "withdraw_address": "synthetic-withdraw"}
	case "blocks":
		return map[string]any{"chain_id": chain, "height": 1, "time": tm, "block_hash": strings.Repeat("AB", 32), "proposer": "synthetic-proposer", "tx_count": 1}
	case "tx_inclusions":
		return map[string]any{"chain_id": chain, "height": 1, "tx_index": 0, "hash": strings.Repeat("CD", 32), "code": 0, "codespace": "", "code_evidence": "native", "gas_wanted": 100, "gas_used": 90, "fee": "1uatom", "fee_payer": "synthetic-account", "fee_charged": true, "fee_evidence": "native", "memo": "synthetic", "signers": []string{"synthetic-account"}, "raw": "\\x0102"}
	case "block_events":
		return map[string]any{"chain_id": chain, "height": 1, "phase": 1, "event_index": 0, "type": "synthetic", "attributes": [][]string{{"source", "synthetic"}}}
	case "taxable_events":
		return map[string]any{"chain_id": chain, "height": 1, "phase": 1, "tx_index": 0, "msg_index": 0, "sub_index": 0, "category": "reward", "amount": "1", "denom": "uatom", "asset": "", "from_addr": "", "to_addr": "synthetic-account", "validator_address": "synthetic-validator", "reward_trigger": "", "detail": "synthetic-state", "timestamp": tm, "tx_hash": "", "amount_source": "state"}
	}
	panic(name)
}
func newFixture(t *testing.T) fixture {
	t.Helper()
	require.NotEmpty(t, os.Getenv("LEGACY_STORE_TEST_ADMIN_DSN"), "private fixture endpoint required, no skipped PG groups")
	d := pgtest.OpenRetained(t)
	owner, e := d.Owner.DB()
	require.NoError(t, e)
	dir := artifactDir(t)
	id := randomID()
	s := Scope{"h2p_s_" + id, "h2p_l_" + id, "h2p_v_" + id, "h2p_k_" + id}
	require.NoError(t, InstallScope(context.Background(), owner, s))
	_, e = owner.Exec("ALTER ROLE " + s.Loader + " PASSWORD '${PRESERVATION_REDACTED}'")
	require.NoError(t, e)
	var dbname string
	require.NoError(t, owner.QueryRow("SELECT current_database()").Scan(&dbname))
	c, e := pgx.ParseConfig(os.Getenv("LEGACY_STORE_TEST_ADMIN_DSN"))
	require.NoError(t, e)
	c.Database = dbname
	c.User = s.Loader
	c.Password = "${PRESERVATION_REDACTED}"
	loader := stdlib.OpenDB(*c)
	t.Cleanup(func() { loader.Close() })
	for _, chain := range []string{"cosmoshub-3", "cosmoshub-4"} {
		for _, tb := range inserts {
			if chain == "cosmoshub-4" && (tb.name == "blocks" || tb.name == "tx_inclusions" || tb.name == "block_events" || tb.name == "taxable_events") {
				continue
			}
			raw := canonicalRow(t, owner, tb, fixtureRow(tb.name, chain, digest([]byte(chain))))
			_, e = owner.Exec("INSERT INTO legacy."+tb.name+" SELECT * FROM pg_catalog.jsonb_populate_record(NULL::legacy."+tb.name+",$1::jsonb)", raw)
			require.NoError(t, e)
		}
	}
	_, e = owner.Exec(`INSERT INTO legacy.taxable_events SELECT chain_id,height,phase,tx_index,msg_index,sub_index+1,category,amount,denom,asset,from_addr,to_addr,validator_address,reward_trigger,'synthetic-window',timestamp,tx_hash,'window_allocated' FROM legacy.taxable_events WHERE chain_id='cosmoshub-3';
 INSERT INTO legacy.balance_days VALUES ('cosmoshub-3','synthetic-account','2020-01-01',1,'{"uatom":"10"}');
 INSERT INTO legacy.reconciliation VALUES ('cosmoshub-3','synthetic-account','liquid',10,10,0,'exact');
 INSERT INTO legacy.import_ranges(chain_id,from_height,to_height,status,next_height) VALUES ('cosmoshub-3',1,5200790,'done',5200791);
 CREATE TABLE public.protected_fixture(id integer PRIMARY KEY,payload text NOT NULL);
 INSERT INTO public.protected_fixture VALUES (1,'synthetic-protected');`)
	require.NoError(t, e)
	writer := "h2p_p_" + id
	_, e = owner.Exec("CREATE ROLE " + writer + " LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS; GRANT USAGE ON SCHEMA public TO " + writer + "; GRANT SELECT,INSERT ON public.protected_fixture TO " + writer)
	require.NoError(t, e)
	generic := savePin(t, dir, "source-receipt.json", []byte(`{"synthetic":true}`))
	gen := savePin(t, dir, "genesis.json", []byte(`{"synthetic_genesis":true}`))
	m := Manifest{Version: 1, Profile: FixtureProfile, Fixture: true, First: 1, Last: 5, Canonical: 3, MigrationLast: 3, TailFirst: 4, TailLast: 5, BuildSHA: digest([]byte("synthetic-source")), BinarySHA: digest([]byte("synthetic-binary")), BuildIdentity: generic, Source: generic, Financial: generic, Genesis: gen, Hub3GenesisSHA: digest([]byte("cosmoshub-3")), Hub4GenesisSHA: digest([]byte("cosmoshub-4")), Scope: s, Components: []Component{{1, 5, generic, digest([]byte("opening")), digest([]byte("closing")), true}}}
	for _, tb := range inserts {
		var out strings.Builder
		row := fixtureRow(tb.name, Chain, gen.SHA256)
		if tb.name == "blocks" {
			for h := int64(1); h <= 5; h++ {
				row["height"] = h
				if h > 1 {
					row["tx_count"] = 0
				}
				out.WriteString(canonicalRow(t, owner, tb, row) + "\n")
			}
		} else {
			out.WriteString(canonicalRow(t, owner, tb, row) + "\n")
			if tb.name == "taxable_events" {
				row["height"] = 3
				row["phase"] = 4
				row["amount_source"] = "export_payout"
				row["detail"] = "synthetic-canonical-payout"
				raw := canonicalRow(t, owner, tb, row) + "\n"
				out.WriteString(raw)
				m.CanonicalRows = 1
				m.CanonicalProjectionSHA = digest([]byte(raw))
			}
		}
		data := []byte(out.String())
		m.Tables = append(m.Tables, Projection{tb.name, projectionColumns(tb), int64(strings.Count(string(data), "\n")), savePin(t, dir, tb.name+".ndjson", data)})
	}
	runtime := Identity{DeploymentUID: "synthetic-deployment-" + id, PodUID: "synthetic-pod-" + id, PVCUID: "synthetic-pvc-" + id, Endpoint: "127.0.0.1:25671"}
	tx, e := owner.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, e)
	m.Destination, e = ProbeIdentity(context.Background(), tx, s, runtime)
	require.NoError(t, e)
	require.NoError(t, tx.Rollback())
	m.Protected, e = snapshot(context.Background(), owner, true, true)
	require.NoError(t, e)
	require.NoError(t, m.ValidateFixture())
	b, e := json.MarshalIndent(m, "", "  ")
	require.NoError(t, e)
	savePin(t, dir, "manifest.json", b)
	t.Logf("retained fixture=%s artifact=%s", dbname, dir)
	return fixture{owner, loader, m, runtime, dir, writer}
}
func refreshIdentity(t *testing.T, f *fixture) {
	t.Helper()
	tx, e := f.owner.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, e)
	f.m.Destination, e = ProbeIdentity(context.Background(), tx, f.m.Scope, f.runtime)
	require.NoError(t, e)
	require.NoError(t, tx.Rollback())
}
func fixtureInsert(t *testing.T, f fixture, name string) {
	t.Helper()
	if name == "balance_days" {
		_, e := f.owner.Exec(`INSERT INTO legacy.balance_days VALUES ('cosmoshub-2','synthetic-account','2020-01-01',1,'{}')`)
		require.NoError(t, e)
		return
	}
	if name == "reconciliation" {
		_, e := f.owner.Exec(`INSERT INTO legacy.reconciliation VALUES ('cosmoshub-2','synthetic-account','liquid',0,0,0,'exact')`)
		require.NoError(t, e)
		return
	}
	if name == "import_ranges" {
		_, e := f.owner.Exec(`INSERT INTO legacy.import_ranges(chain_id,from_height,to_height,status,next_height) VALUES('cosmoshub-2',1,5,'done',6)`)
		require.NoError(t, e)
		return
	}
	for _, tb := range inserts {
		if tb.name != name {
			continue
		}
		if strings.HasPrefix(name, "genesis_") && name != "genesis_files" {
			fixtureInsert(t, f, "genesis_files")
		}
		if name == "tx_inclusions" || name == "block_events" {
			fixtureInsert(t, f, "blocks")
		}
		raw := canonicalRow(t, f.owner, tb, fixtureRow(name, Chain, f.m.Genesis.SHA256))
		_, e := f.owner.Exec("INSERT INTO legacy."+tb.name+" SELECT * FROM pg_catalog.jsonb_populate_record(NULL::legacy."+tb.name+",$1::jsonb)", raw)
		require.NoError(t, e)
		return
	}
	panic(name)
}
func requireEmptyUnchanged(t *testing.T, f fixture, before Snapshot) {
	t.Helper()
	require.NoError(t, emptyHub2(context.Background(), f.owner))
	require.Equal(t, before, f.full(t))
}
func cloneManifest(m Manifest) Manifest {
	b, _ := json.Marshal(m)
	var v Manifest
	_ = json.Unmarshal(b, &v)
	return v
}
func rewriteTable(t *testing.T, f fixture, m *Manifest, name string, change func([]string) []string) {
	t.Helper()
	for i, p := range m.Tables {
		if p.Table != name {
			continue
		}
		b, e := os.ReadFile(p.File.Path)
		require.NoError(t, e)
		lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		lines = change(lines)
		data := []byte(strings.Join(lines, "\n") + "\n")
		m.Tables[i].File = savePin(t, f.dir, name+"-negative-"+randomID()+".ndjson", data)
		m.Tables[i].Rows = int64(len(lines))
		return
	}
	t.Fatal("unknown table")
}
func deadline(t *testing.T) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}
func mustExec(t *testing.T, db *sql.DB, text string, args ...any) {
	t.Helper()
	_, e := db.Exec(text, args...)
	require.NoError(t, e)
}
func fixtureDB(t *testing.T, f fixture, user string) *sql.DB {
	c, e := pgx.ParseConfig(os.Getenv("LEGACY_STORE_TEST_ADMIN_DSN"))
	require.NoError(t, e)
	c.Database = f.m.Destination.Database
	c.User = user
	c.Password = "${PRESERVATION_REDACTED}"
	db := stdlib.OpenDB(*c)
	t.Cleanup(func() { db.Close() })
	return db
}
func projectionTable(name string) table {
	for _, t := range inserts {
		if t.name == name {
			return t
		}
	}
	panic(fmt.Sprint("not projected ", name))
}
