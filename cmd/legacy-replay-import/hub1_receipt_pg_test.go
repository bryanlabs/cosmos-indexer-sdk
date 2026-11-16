package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/legacy/genesis"
	"github.com/DefiantLabs/cosmos-indexer/legacy/hub2promotion"
	rr "github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema/pgtest"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
)

const hub1TestGenesis = `{"chain_id":"cosmoshub-1","genesis_time":"2019-03-13T23:00:00Z","app_state":{"accounts":[{"address":"cosmos1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqaa","coins":[{"amount":"1000","denom":"uatom"}],"sequence_number":"0","account_number":"0","original_vesting":null,"delegated_free":null,"delegated_vesting":null,"start_time":"0","end_time":"1600000000"}],"staking":{"validators":null,"delegations":null,"unbonding_delegations":null},"distr":{"delegator_withdraw_infos":null,"outstanding_rewards":null,"validator_accumulated_commissions":null},"gentxs":null}}`

func hub1TestPin(t *testing.T, path, body string) hub1Pin {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	sum := sha256.Sum256([]byte(body))
	return hub1Pin{Path: path, SHA256: hex.EncodeToString(sum[:])}
}

// A receipt is written only for rows that equal what the sealed part gives,
// names that part's seal, and is refused once a seal or a stored row changes.
func TestHub1PGStagingReceiptBindsStoredRowsToTheSealedPart(t *testing.T) {
	db := pgtest.OpenRetained(t)
	var name string
	require.NoError(t, db.Owner.Raw("SELECT current_database()").Scan(&name).Error)
	t.Setenv("LEGACY_REPLAY_IMPORT_DSN", db.Importer.Dialector.(*postgres.Dialector).Config.DSN)
	old := hub1RehearsalName
	hub1RehearsalName = regexp.MustCompile(`^legacy_replay_rehearsal_[a-z0-9_]+$`)
	defer func() { hub1RehearsalName = old }()

	root := hub1Fixture(t, nil)
	dir := t.TempDir()
	capture, err := os.ReadFile(filepath.Join(root, "capture.private.json"))
	require.NoError(t, err)
	var receipt struct {
		Streams struct {
			Movements struct {
				SHA string `json:"raw_sha256"`
			} `json:"movements"`
		} `json:"streams"`
	}
	require.NoError(t, json.Unmarshal(capture, &receipt))
	seal := hub1TestPin(t, filepath.Join(dir, "seal.json"), fmt.Sprintf(`{"accepted":true,"source_records_sha256":%q}`, receipt.Streams.Movements.SHA))
	gen := hub1TestPin(t, filepath.Join(dir, "genesis.json"), hub1TestGenesis)
	list := hub1Parts{Schema: hub1PartsSchema, DecoderCommit: strings.Repeat("ab", 20), Genesis: gen, Parts: []hub1Part{{First: 9, Last: 9, Source: root, Seal: seal}}}
	body, err := json.Marshal(list)
	require.NoError(t, err)
	parts := filepath.Join(dir, "parts.json")
	require.NoError(t, os.WriteFile(parts, body, 0o600))

	require.NoError(t, hub1Invoke("--hub1-parts", parts, "--rehearsal-db", name, "--write"))
	out := filepath.Join(dir, "receipt.json")
	require.Error(t, hub1Invoke("--hub1-parts", parts, "--rehearsal-db", name, "--hub1-receipt", out), "no receipt without the stored genesis")
	file, err := genesis.Read(strings.NewReader(hub1TestGenesis))
	require.NoError(t, err)
	_, err = genesis.Load(db.Importer, file)
	require.NoError(t, err)

	before := pgtest.Snapshot(t, db.Owner, "legacy")
	require.NoError(t, hub1Invoke("--hub1-parts", parts, "--rehearsal-db", name, "--hub1-receipt", out))
	pgtest.RequireUntouched(t, before, pgtest.Snapshot(t, db.Owner, "legacy"))
	raw, err := os.ReadFile(out)
	require.NoError(t, err)
	var got hub1Receipt
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, "hub1-staging-receipt-v1", got.Schema)
	require.Equal(t, "cosmoshub-1", got.Chain)
	require.False(t, got.CompleteChain, "one part short of the export height is not the complete chain")
	require.Zero(t, got.UnboundRows)
	require.True(t, got.Genesis.StoredEqual)
	require.Equal(t, gen.SHA256, got.Genesis.SHA256)
	require.Len(t, got.Components, 1)
	c := got.Components[0]
	require.True(t, c.Rederived)
	require.True(t, c.SealNamesSource)
	require.Equal(t, seal.SHA256, c.SealSHA)
	require.Equal(t, receipt.Streams.Movements.SHA, c.RecordsSHA)
	for table, rows := range map[string]int64{"blocks": 1, "tx_inclusions": 1, "taxable_events": 1} {
		require.Equal(t, rows, c.Tables[table].Rows, table)
		require.Equal(t, got.Projection[table], c.Tables[table], table)
	}
	require.Equal(t, got.Projection["block_events"], c.Tables["block_events"])
	require.Nil(t, got.Payout)
	require.Nil(t, got.Tail)

	// The loader's own export of this database must give, table for table,
	// the row count and sha256 the receipt states. This is the link the
	// finalizer relies on, checked here with both tools on one database.
	source, err := db.Owner.DB()
	require.NoError(t, err)
	index, _, err := hub2promotion.ExportStaging(context.Background(), source, filepath.Join(dir, "loader-export"), "cosmoshub-1")
	require.NoError(t, err)
	require.Len(t, index.Tables, len(got.Projection))
	for _, table := range index.Tables {
		require.Equal(t, hub1TableHash{Rows: table.Rows, SHA256: table.File.SHA256}, got.Projection[table.Table], table.Table)
	}

	require.Error(t, hub1Invoke("--hub1-parts", parts, "--rehearsal-db", name, "--hub1-receipt", out), "a receipt is never overwritten")
	require.NoError(t, hub1Invoke("--hub1-parts", parts, "--rehearsal-db", name, "--hub1-verify-receipt", out))

	// A stored row that no longer equals the sealed source ends the receipt.
	require.NoError(t, db.Owner.Exec(`UPDATE legacy.taxable_events SET amount = '54' WHERE chain_id = 'cosmoshub-1'`).Error)
	require.Error(t, hub1Invoke("--hub1-parts", parts, "--rehearsal-db", name, "--hub1-verify-receipt", out))
	require.Error(t, hub1Invoke("--hub1-parts", parts, "--rehearsal-db", name, "--hub1-receipt", filepath.Join(dir, "second.json")))
	require.NoError(t, db.Owner.Exec(`UPDATE legacy.taxable_events SET amount = '53' WHERE chain_id = 'cosmoshub-1'`).Error)
	require.NoError(t, hub1Invoke("--hub1-parts", parts, "--rehearsal-db", name, "--hub1-verify-receipt", out))

	// So does a seal that changed after it was pinned.
	sealed, err := os.ReadFile(seal.Path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(seal.Path, []byte(`{"accepted":false}`), 0o600))
	require.Error(t, hub1Invoke("--hub1-parts", parts, "--rehearsal-db", name, "--hub1-verify-receipt", out))
	require.NoError(t, os.WriteFile(seal.Path, sealed, 0o600))
	require.NoError(t, hub1Invoke("--hub1-parts", parts, "--rehearsal-db", name, "--hub1-verify-receipt", out))

	// A row of no listed part, here a block above the part, ends it too. Blocks are written once, so this stays.
	require.NoError(t, db.Owner.Exec(`INSERT INTO legacy.blocks SELECT (jsonb_populate_record(b, '{"height":10}')).* FROM legacy.blocks b`).Error)
	require.Error(t, hub1Invoke("--hub1-parts", parts, "--rehearsal-db", name, "--hub1-verify-receipt", out))
}

func TestHub1PartsListRefusals(t *testing.T) {
	dir := t.TempDir()
	seal := hub1TestPin(t, filepath.Join(dir, "seal.json"), "{}")
	gen := hub1TestPin(t, filepath.Join(dir, "genesis.json"), hub1TestGenesis)
	good := hub1Parts{Schema: hub1PartsSchema, DecoderCommit: strings.Repeat("ab", 20), Genesis: gen, Parts: []hub1Part{{First: 1, Last: 500000, Source: dir, Seal: seal}}, Tail: &hub1TailSpec{First: 500001, Last: 500015, Source: dir, Seal: seal}}
	write := func(p hub1Parts) string {
		body, err := json.Marshal(p)
		require.NoError(t, err)
		path := filepath.Join(t.TempDir(), "parts.json")
		require.NoError(t, os.WriteFile(path, body, 0o600))
		return path
	}
	_, err := readHub1Parts(write(good))
	require.NoError(t, err)
	for name, edit := range map[string]func(*hub1Parts){
		"a gap between parts": func(p *hub1Parts) {
			p.Parts = append(p.Parts[:0:0], hub1Part{First: 1, Last: 10, Source: dir, Seal: seal}, hub1Part{First: 12, Last: 500000, Source: dir, Seal: seal})
		},
		"a part past the export":     func(p *hub1Parts) { p.Parts = []hub1Part{{First: 1, Last: 500001, Source: dir, Seal: seal}} },
		"a tail after a short chain": func(p *hub1Parts) { p.Parts = []hub1Part{{First: 1, Last: 400000, Source: dir, Seal: seal}} },
		"a tail of another range":    func(p *hub1Parts) { p.Tail = &hub1TailSpec{First: 500001, Last: 500014, Source: dir, Seal: seal} },
		"a seal that is not pinned":  func(p *hub1Parts) { p.Parts[0].Seal.SHA256 = strings.Repeat("0", 64) },
		"another genesis":            func(p *hub1Parts) { p.Genesis.SHA256 = strings.Repeat("0", 64) },
		"no decoder commit":          func(p *hub1Parts) { p.DecoderCommit = "" },
	} {
		p := good
		p.Parts = append([]hub1Part(nil), good.Parts...)
		edit(&p)
		_, err := readHub1Parts(write(p))
		require.Error(t, err, name)
	}
}

// Parts may share one sealed source, but together they must cover that
// source's own range exactly once.
func TestHub1PartsSharingASourceCoverItExactlyOnce(t *testing.T) {
	source := func(dir string, from, through int64) hub1Source {
		return hub1Source{dir: dir, proof: rr.Proof{From: from, Through: through}}
	}
	part := func(dir string, first, last int64) hub1Part { return hub1Part{First: first, Last: last, Source: dir} }
	sources := []hub1Source{source("/a", 1, 200), source("/b", 201, 500)}
	good := hub1Parts{Parts: []hub1Part{part("/a", 1, 120), part("/a", 121, 200), part("/b", 201, 300), part("/b", 301, 400), part("/b", 401, 500)}}
	require.Equal(t, []string{"/a", "/b"}, good.dirs())
	require.NoError(t, good.bound(sources))
	require.Len(t, good.slices(), 5)
	for name, parts := range map[string][]hub1Part{
		"a part reaching into the next source":  {part("/a", 1, 210), part("/b", 211, 500)},
		"a source left partly uncovered":        {part("/a", 1, 190), part("/b", 191, 500)},
		"a source named again later":            {part("/a", 1, 200), part("/b", 201, 400), part("/a", 401, 500)},
		"a part of a source that was not read":  {part("/a", 1, 200), part("/b", 201, 500), part("/c", 501, 600)},
		"a part starting before its own source": {part("/a", 1, 150), part("/b", 151, 500)},
	} {
		require.Error(t, hub1Parts{Parts: parts}.bound(sources), name)
	}
}
