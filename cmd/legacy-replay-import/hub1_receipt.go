package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/DefiantLabs/cosmos-indexer/core"
	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	"github.com/DefiantLabs/cosmos-indexer/legacy/genesis"
	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The Hub 1 staging receipt says which sealed part every stored row came
// from. Its name, fields and row hashes are those of the Hub 2 receipt, so
// the finalizer reads both the same way. It is written only after every
// listed part has been read again from its sealed files and found equal, row
// for row, to what is stored. It holds no clock time, host or database name.
const (
	hub1ReceiptSchema = "hub1-staging-receipt-v1"
	hub1PartsSchema   = "hub1-staging-parts-v1"
	hub1TailLast      = 500015
	hub1RowFormat     = "one line per row: (pg_catalog.to_jsonb(r) - 'loaded_at')::text plus newline, timezone UTC, datestyle ISO, in primary key order; sha256 of the lines"
)

var hub1SHA = regexp.MustCompile(`^[0-9a-f]{64}$`)
var hub1Commit = regexp.MustCompile(`^[0-9a-f]{40}$`)

var hub1TableKeys = [][2]string{{"balance_days", "chain_id,address,day"}, {"block_events", "chain_id,height,phase,event_index"}, {"blocks", "chain_id,height"}, {"eras", "chain_id"}, {"genesis_accounts", "chain_id,address"}, {"genesis_delegations", "chain_id,delegator,validator"}, {"genesis_files", "chain_id"}, {"genesis_unbondings", "chain_id,delegator,validator,entry"}, {"genesis_validators", "chain_id,operator"}, {"genesis_withdraw_addresses", "chain_id,delegator"}, {"import_ranges", "chain_id,from_height"}, {"reconciliation", "chain_id,address,component"}, {"taxable_events", "chain_id,height,phase,tx_index,msg_index,sub_index"}, {"tx_inclusions", "chain_id,height,tx_index"}}

// hub1LoaderTables are the ten tables the loader takes.
var hub1LoaderTables = map[string]bool{"genesis_files": true, "genesis_accounts": true, "genesis_delegations": true, "genesis_unbondings": true, "genesis_validators": true, "genesis_withdraw_addresses": true, "blocks": true, "tx_inclusions": true, "block_events": true, "taxable_events": true}

type hub1Pin struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func (p hub1Pin) holds() bool {
	sum, _, err := fileSHA256(p.Path)
	return err == nil && filepath.IsAbs(p.Path) && hub1SHA.MatchString(p.SHA256) && sum == p.SHA256
}

type hub1Part struct {
	First  int64   `json:"first"`
	Last   int64   `json:"last"`
	Source string  `json:"source"`
	Seal   hub1Pin `json:"seal"`
}

// hub1TailSpec names the captures of the blocks after the export height and
// the acceptance file that seals them.
type hub1TailSpec struct {
	First  int64   `json:"first"`
	Last   int64   `json:"last"`
	Source string  `json:"source"`
	Seal   hub1Pin `json:"seal"`
}

// hub1Parts is the ordered list of sealed parts a staging database is built
// from, each with the acceptance file that seals it.
type hub1Parts struct {
	Schema        string        `json:"schema"`
	DecoderCommit string        `json:"decoder_source_commit"`
	Genesis       hub1Pin       `json:"genesis"`
	Parts         []hub1Part    `json:"parts"`
	Tail          *hub1TailSpec `json:"tail,omitempty"`
	sha256        string
}

func readHub1Parts(path string) (hub1Parts, error) {
	var p hub1Parts
	raw, err := os.ReadFile(path)
	if err != nil {
		return p, errors.New("parts list unreadable")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || p.Schema != hub1PartsSchema || !hub1Commit.MatchString(p.DecoderCommit) || len(p.Parts) == 0 {
		return p, errors.New("parts list malformed")
	}
	sum := sha256.Sum256(raw)
	p.sha256 = hex.EncodeToString(sum[:])
	for i, part := range p.Parts {
		if part.Last < part.First || part.Last > hub1Export || (i > 0 && part.First != p.Parts[i-1].Last+1) || !filepath.IsAbs(part.Source) {
			return p, fmt.Errorf("part %d: ranges must be consecutive and end by the export height", i+1)
		}
	}
	if t := p.Tail; t != nil && (p.Parts[len(p.Parts)-1].Last != hub1Export || t.First != hub1Export+1 || t.Last != hub1TailLast || !filepath.IsAbs(t.Source)) {
		return p, errors.New("the tail is heights 500001 to 500015 after a complete history")
	}
	return p, p.pins()
}

// pins rehashes every acceptance file and the genesis file.
func (p hub1Parts) pins() error {
	if !p.Genesis.holds() {
		return errors.New("genesis file missing or changed since it was pinned")
	}
	for i, part := range p.Parts {
		if !part.Seal.holds() {
			return fmt.Errorf("part %d (heights %d to %d): seal missing or changed since it was pinned", i+1, part.First, part.Last)
		}
	}
	if p.Tail != nil && !p.Tail.Seal.holds() {
		return errors.New("tail seal missing or changed since it was pinned")
	}
	return nil
}

// dirs lists each source once. Consecutive parts may share one source; each
// then takes only its own heights from it.
func (p hub1Parts) dirs() []string {
	var dirs []string
	for i, part := range p.Parts {
		if i == 0 || part.Source != p.Parts[i-1].Source {
			dirs = append(dirs, part.Source)
		}
	}
	return dirs
}

func (p hub1Parts) slices() []*hub1Slice {
	var slices []*hub1Slice
	for _, part := range p.Parts {
		slices = append(slices, &hub1Slice{first: part.First, last: part.Last, captures: sha256.New()})
	}
	return slices
}

// bound refuses unless the parts that name a source cover that source's own
// sealed range exactly once: the first begins where its capture receipt
// begins, the last ends where it ends, and no source is named again later.
func (p hub1Parts) bound(sources []hub1Source) error {
	at := 0
	seen := map[string]bool{}
	for i, s := range sources {
		if at == len(p.Parts) || p.Parts[at].Source != s.dir || seen[s.dir] {
			return fmt.Errorf("source %d is not the source the parts list names next, or it is named twice", i+1)
		}
		seen[s.dir] = true
		first := p.Parts[at].First
		for at+1 < len(p.Parts) && p.Parts[at+1].Source == s.dir {
			at++
		}
		if first != s.proof.From || p.Parts[at].Last != s.proof.Through {
			return fmt.Errorf("source %d is sealed for heights %d to %d, the parts that name it cover %d to %d", i+1, s.proof.From, s.proof.Through, first, p.Parts[at].Last)
		}
		at++
	}
	if at != len(p.Parts) {
		return errors.New("not every listed part was read")
	}
	return nil
}

// hub1TailBlocks reads the blocks after the export height from their
// captures. They are stored as history: header, transactions as included, and
// one receipt event. No replay ran past the export, so no tax row exists.
func hub1TailBlocks(t hub1TailSpec, parent string, maxBody int64) ([]ledger.Block, string, error) {
	list := &hub1Read{captures: sha256.New()}
	for n := t.First; n <= t.Last; n++ {
		if err := list.capture(t.Source, n); err != nil {
			return nil, "", fmt.Errorf("tail height %d: block capture missing", n)
		}
	}
	listed := hex.EncodeToString(list.captures.Sum(nil))
	receipt, _ := json.Marshal(map[string]any{"historical_tail": fmt.Sprintf("%d..%d", t.First, t.Last), "tax_rows": "none_tail_is_after_the_canonical_export", "capture_list_sha256": listed, "tail_seal_sha256": t.Seal.SHA256})
	var blocks []ledger.Block
	for n := t.First; n <= t.Last; n++ {
		h, err := capture.LoadHeight(filepath.Join(t.Source, "cache"), "cosmoshub-1", n, maxBody)
		if err != nil || h.Previous != parent || h.Block.Block.ChainID != "cosmoshub-1" || h.Block.Block.Height != n {
			return nil, "", fmt.Errorf("tail height %d: capture unreadable or it does not follow the block before it", n)
		}
		items, err := core.DecodeLegacyBlock(h.Block, h.Results)
		if err != nil || len(items) != len(h.Block.Block.Txs) {
			return nil, "", fmt.Errorf("tail height %d does not decode", n)
		}
		b := ledger.Block{Chain: "cosmoshub-1", Height: n, Time: h.Block.Block.Time, Hash: h.Hash, Proposer: h.Proposer}
		for i, item := range items {
			if item.Tx == nil || item.Result == nil || len(item.Tx.Issues) != 0 || !item.Tx.SignersComplete || len(item.Tx.Signers) == 0 || (item.Fee.Charge != "charged" && item.Fee.Charge != "not charged") {
				return nil, "", fmt.Errorf("tail height %d transaction %d cannot be stored without a replay", n, i)
			}
			inc := ledger.Inclusion{Index: i, Hash: item.Hash, Code: item.Result.Code, Codespace: item.Result.Codespace, CodeEvidence: item.CodeEvidence, GasWanted: item.Result.GasWanted, GasUsed: item.Result.GasUsed, Fee: item.Fee.Declared.String(), FeePayer: item.Fee.Payer.String(), FeeCharged: item.Fee.Charge == "charged", FeeEvidence: item.Fee.Evidence, Memo: ledger.Text(item.Tx.Memo), Raw: bytes.Clone(h.Block.Block.Txs[i])}
			for _, s := range item.Tx.Signers {
				inc.Signers = append(inc.Signers, s.String())
			}
			b.Inclusions = append(b.Inclusions, inc)
		}
		b.Events = []ledger.Event{{Phase: ledger.PhaseBegin, Index: 2, Type: "pinned_tail_receipt", Attributes: [][2]string{{"json", string(receipt)}}}}
		parent = h.Hash
		blocks = append(blocks, b)
	}
	return blocks, listed, nil
}

type hub1TableHash struct {
	Rows   int64  `json:"rows"`
	SHA256 string `json:"sha256"`
}

type hub1ReceiptPart struct {
	First           int64                    `json:"first"`
	Last            int64                    `json:"last"`
	SealPath        string                   `json:"seal_path"`
	SealSHA         string                   `json:"seal_sha256"`
	SealNamesSource bool                     `json:"seal_text_names_source_digest"`
	RecordsSHA      string                   `json:"source_records_sha256"`
	StatusSHA       string                   `json:"source_status_sha256"`
	CacheListSHA    string                   `json:"source_cache_list_sha256"`
	Rederived       bool                     `json:"stored_rows_equal_rows_rederived_from_sealed_source"`
	Tables          map[string]hub1TableHash `json:"tables"`
}

type hub1ReceiptPayout struct {
	Height     int64         `json:"height"`
	ExportSHA  string        `json:"export_records_sha256"`
	Rederived  bool          `json:"stored_rows_equal_rows_rederived_from_export"`
	Rows       hub1TableHash `json:"taxable_events_phase4"`
	IndexZero  bool          `json:"every_row_tx_index_0_msg_index_0"`
	ExportPath string        `json:"export_records_path"`
}

type hub1ReceiptTail struct {
	First        int64                    `json:"first"`
	Last         int64                    `json:"last"`
	SealPath     string                   `json:"tail_seal_path"`
	SealSHA      string                   `json:"tail_seal_sha256"`
	CacheListSHA string                   `json:"capture_list_sha256"`
	Rederived    bool                     `json:"stored_rows_equal_rows_rederived_from_captures"`
	TaxRows      int64                    `json:"tax_rows"`
	Tables       map[string]hub1TableHash `json:"tables"`
}

type hub1ReceiptGenesis struct {
	SHA256      string                   `json:"sha256"`
	StoredEqual bool                     `json:"stored_rows_equal_genesis_file"`
	Tables      map[string]hub1TableHash `json:"tables"`
}

type hub1Receipt struct {
	Schema           string                   `json:"schema"`
	Chain            string                   `json:"chain"`
	First            int64                    `json:"first"`
	Last             int64                    `json:"last"`
	CanonicalThrough int64                    `json:"canonical_through"`
	CanonicalHeight  int64                    `json:"canonical_height"`
	CompleteChain    bool                     `json:"complete_chain"`
	ManifestSHA      string                   `json:"manifest_sha256"`
	SourceCommit     string                   `json:"decoder_source_commit"`
	RowFormat        string                   `json:"row_format"`
	Genesis          hub1ReceiptGenesis       `json:"genesis"`
	Components       []hub1ReceiptPart        `json:"components"`
	Payout           *hub1ReceiptPayout       `json:"restart_payout"`
	Tail             *hub1ReceiptTail         `json:"historical_tail"`
	Projection       map[string]hub1TableHash `json:"projection_tables"`
	ProjectionNote   string                   `json:"projection_tables_note"`
	Fingerprint      map[string]hub1TableHash `json:"fingerprint_tables"`
	FingerprintAll   string                   `json:"fingerprint_all_tables_sha256"`
	UnboundRows      int64                    `json:"rows_bound_to_no_listed_part"`
}

type hub1Bucket struct {
	h hash.Hash
	w *bufio.Writer
	n int64
}

func newHub1Bucket() *hub1Bucket {
	h := sha256.New()
	return &hub1Bucket{h: h, w: bufio.NewWriterSize(h, 64<<10)}
}
func (b *hub1Bucket) add(line string) { b.w.WriteString(line); b.w.WriteByte('\n'); b.n++ }
func (b *hub1Bucket) sum() hub1TableHash {
	b.w.Flush()
	return hub1TableHash{b.n, hex.EncodeToString(b.h.Sum(nil))}
}

// hub1HashStored reads every table once in key order inside one read-only
// snapshot and files each row under the part its height belongs to. A row
// of another chain, a payout row away from the export height, or a row at a
// height no listed part covers is counted as bound to no part.
func hub1HashStored(ctx context.Context, conn *sql.Conn, r *hub1Receipt) error {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO, YMD'`); err != nil {
		return err
	}
	split := []string{"blocks", "tx_inclusions", "block_events", "taxable_events"}
	parts := make([]map[string]*hub1Bucket, len(r.Components))
	for i := range parts {
		parts[i] = map[string]*hub1Bucket{}
		for _, t := range split {
			parts[i][t] = newHub1Bucket()
		}
	}
	tail := map[string]*hub1Bucket{"blocks": newHub1Bucket(), "tx_inclusions": newHub1Bucket(), "block_events": newHub1Bucket()}
	payout, payoutZero := newHub1Bucket(), true
	r.Genesis.Tables, r.Projection, r.Fingerprint = map[string]hub1TableHash{}, map[string]hub1TableHash{}, map[string]hub1TableHash{}
	all := sha256.New()
	for _, k := range hub1TableKeys {
		name := k[0]
		keyed := strings.Contains(k[1], ",height")
		extra := "0::bigint,0::int,true"
		switch {
		case name == "taxable_events":
			extra = "height,phase::int,(tx_index=0 AND msg_index=0)"
		case name == "block_events":
			extra = "height,phase::int,true"
		case keyed:
			extra = "height,0::int,true"
		}
		rows, err := tx.QueryContext(ctx, "SELECT (chain_id=$1),"+extra+",(pg_catalog.to_jsonb(r)-'loaded_at')::pg_catalog.text FROM legacy."+name+" r ORDER BY "+k[1], "cosmoshub-1")
		if err != nil {
			return err
		}
		whole := newHub1Bucket()
		at := 0
		for rows.Next() {
			var height int64
			var phase int
			var ours, zero bool
			var line string
			if err = rows.Scan(&ours, &height, &phase, &zero, &line); err != nil {
				rows.Close()
				return err
			}
			whole.add(line)
			if !ours && name != "eras" {
				r.UnboundRows++
				continue
			}
			if !keyed {
				continue
			}
			switch {
			case name == "taxable_events" && phase == 4:
				payout.add(line)
				payoutZero = payoutZero && zero
				if height != hub1Export {
					r.UnboundRows++
				}
			case height > hub1Export:
				if b := tail[name]; b != nil && r.Tail != nil && height <= r.Tail.Last {
					b.add(line)
				} else {
					r.UnboundRows++
				}
			default:
				for at < len(r.Components) && height > r.Components[at].Last {
					at++
				}
				if at == len(r.Components) || height < r.Components[at].First {
					r.UnboundRows++
					at = 0
				} else {
					parts[at][name].add(line)
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		sum := whole.sum()
		fmt.Fprintf(all, "%s %d %s\n", name, sum.Rows, sum.SHA256)
		r.Fingerprint[name] = sum
		if strings.HasPrefix(name, "genesis_") {
			r.Genesis.Tables[name] = sum
		}
		if hub1LoaderTables[name] {
			r.Projection[name] = sum
		}
	}
	// The loader's export keeps loaded_at, so its genesis_files line is hashed
	// as the export writes it.
	var line string
	if err = tx.QueryRowContext(ctx, `SELECT pg_catalog.to_jsonb(r)::pg_catalog.text FROM legacy.genesis_files r WHERE chain_id=$1`, "cosmoshub-1").Scan(&line); err != nil {
		return errors.New("no stored Hub 1 genesis")
	}
	g := newHub1Bucket()
	g.add(line)
	r.Projection["genesis_files"] = g.sum()
	r.FingerprintAll = hex.EncodeToString(all.Sum(nil))
	for i := range r.Components {
		r.Components[i].Tables = map[string]hub1TableHash{}
		for t, b := range parts[i] {
			r.Components[i].Tables[t] = b.sum()
		}
	}
	if r.Payout != nil {
		r.Payout.Rows, r.Payout.IndexZero = payout.sum(), payoutZero
	} else {
		r.UnboundRows += payout.n
	}
	if r.Tail != nil {
		r.Tail.Tables = map[string]hub1TableHash{}
		for t, b := range tail {
			r.Tail.Tables[t] = b.sum()
		}
	}
	return nil
}

// openHub1Stored opens a populated staging database for reading only, on one
// session that holds the importer lock.
func openHub1Stored(ctx context.Context, dsn, target string) (*rehearsalDestination, *gorm.DB, error) {
	if guardDSN(dsn, target) != nil || !hub1RehearsalName.MatchString(target) {
		return nil, nil, refused
	}
	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, refused
	}
	d := &rehearsalDestination{pool: pool}
	fail := func() (*rehearsalDestination, *gorm.DB, error) { d.close(); return nil, nil, refused }
	if d.conn, err = pool.Conn(ctx); err != nil {
		return fail()
	}
	guard, err := gorm.Open(postgres.New(postgres.Config{Conn: d.conn}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return fail()
	}
	var name string
	var locked bool
	if d.conn.QueryRowContext(ctx, "SELECT current_database()").Scan(&name) != nil || name != target {
		return fail()
	}
	if findings, err := schema.Confined(guard.WithContext(ctx)); err != nil || len(findings) != 0 {
		return fail()
	}
	if d.conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(340310)").Scan(&locked) != nil || !locked {
		return fail()
	}
	if _, err = d.conn.ExecContext(ctx, `SET default_transaction_read_only=on`); err != nil {
		return fail()
	}
	return d, guard, nil
}

// buildHub1Receipt recomputes the whole receipt from the database and the
// sealed files. The session is read-only: nothing is written to the database.
func buildHub1Receipt(p hub1Parts, exportRecords, exportProof, target string, maxBody int64, manifest string) (hub1Receipt, error) {
	last := p.Parts[len(p.Parts)-1].Last
	r := hub1Receipt{Schema: hub1ReceiptSchema, Chain: "cosmoshub-1", First: p.Parts[0].First, Last: last, CanonicalThrough: last, CanonicalHeight: hub1Export, CompleteChain: p.Parts[0].First == 1 && last == hub1Export && p.Tail != nil,
		ManifestSHA: p.sha256, SourceCommit: p.DecoderCommit, RowFormat: hub1RowFormat, ProjectionNote: "equal to the sha256 of the loader export's ndjson file of the same table; genesis_files there keeps loaded_at"}
	fail := func(format string, a ...any) (hub1Receipt, error) {
		fmt.Fprintln(os.Stderr, "hub1 receipt:", fmt.Sprintf(format, a...))
		return r, refused
	}
	if (last == hub1Export) != (exportRecords != "") {
		return fail("the restart payout records are required exactly when the parts reach height %d", hub1Export)
	}
	for _, part := range p.Parts {
		r.Components = append(r.Components, hub1ReceiptPart{First: part.First, Last: part.Last, SealPath: part.Seal.Path, SealSHA: part.Seal.SHA256})
	}
	if exportRecords != "" {
		r.Payout = &hub1ReceiptPayout{Height: hub1Export, ExportPath: exportRecords}
	}
	if t := p.Tail; t != nil {
		r.Last = t.Last
		r.Tail = &hub1ReceiptTail{First: t.First, Last: t.Last, SealPath: t.Seal.Path, SealSHA: t.Seal.SHA256}
	}
	ctx := context.Background()
	d, guard, err := openHub1Stored(ctx, os.Getenv("LEGACY_REPLAY_IMPORT_DSN"), target)
	if err != nil {
		return fail("staging database identity, confinement or lock")
	}
	defer d.close()
	gf, err := os.Open(p.Genesis.Path)
	if err != nil {
		return fail("genesis file unreadable")
	}
	opening, err := genesis.Read(gf)
	gf.Close()
	if err != nil || opening.Chain != "cosmoshub-1" || opening.SHA256 != p.Genesis.SHA256 {
		return fail("the pinned genesis is not a readable Hub 1 genesis")
	}
	// Load on a read-only session can only compare. A stored genesis that
	// differs from the file, or a missing one, fails here.
	gr, err := genesis.Load(guard.WithContext(ctx), opening)
	if err != nil || !gr.AlreadyLoaded || !reflect.DeepEqual(gr.File, gr.Stored) {
		return fail("stored genesis rows differ from the genesis file")
	}
	r.Genesis.SHA256, r.Genesis.StoredEqual = opening.SHA256, true
	if err = hub1HashStored(ctx, d.conn, &r); err != nil {
		return fail("stored rows could not be hashed: %v", err)
	}
	fmt.Printf("hub1 stored rows hashed all_tables_sha256=%s\n", r.FingerprintAll)
	if r.UnboundRows != 0 {
		return fail("%d stored rows belong to no listed part", r.UnboundRows)
	}
	slices := p.slices()
	done, err := hub1Options{dirs: p.dirs(), exportRecords: exportRecords, exportProof: exportProof, target: target, maxBody: maxBody, manifest: manifest, compare: d.conn, tail: p.Tail, bound: p.bound, slices: slices}.run()
	if err != nil {
		return fail("stored rows are not the rows the sealed parts give")
	}
	at := 0
	for i, part := range p.Parts {
		for done.sources[at].dir != part.Source {
			at++
		}
		s, cut := done.sources[at], slices[i]
		seal, err := os.ReadFile(part.Seal.Path)
		if err != nil {
			return fail("part %d: seal unreadable", i+1)
		}
		c := &r.Components[i]
		c.SealNamesSource, c.RecordsSHA, c.StatusSHA, c.CacheListSHA, c.Rederived = bytes.Contains(seal, []byte(s.proof.SHA256)), s.proof.SHA256, s.read.CaptureReceiptSHA256, hex.EncodeToString(cut.captures.Sum(nil)), true
		if cut.heights != part.Last-part.First+1 || c.Tables["blocks"].Rows != cut.heights || c.Tables["tx_inclusions"].Rows != cut.transactions {
			return fail("part %d: stored block or transaction count differs from what its heights of the sealed source give", i+1)
		}
	}
	if x := r.Payout; x != nil {
		sum, _, err := fileSHA256(exportRecords)
		if err != nil || done.export == nil || sum != done.export.SHA256 {
			return fail("the payout records file is not the file its proof pins")
		}
		x.ExportSHA, x.Rederived = sum, true
		if !x.IndexZero || x.Rows.Rows != done.payoutRows || x.Rows.Rows == 0 {
			return fail("restart payout rows are not all transaction 0 message 0, or their count differs from the payout records")
		}
	}
	if t := r.Tail; t != nil {
		t.CacheListSHA, t.Rederived = done.tailList, true
		if t.Tables["blocks"].Rows != t.Last-t.First+1 {
			return fail("the stored tail is not complete")
		}
	}
	// Tax rows above the export height were already counted as unbound, and
	// the row counts are taken again so a change during the read is caught.
	again := hub1Receipt{Components: append([]hub1ReceiptPart(nil), r.Components...), Tail: nil}
	if r.Payout != nil {
		again.Payout = &hub1ReceiptPayout{}
	}
	if r.Tail != nil {
		again.Tail = &hub1ReceiptTail{Last: r.Tail.Last}
	}
	for i := range again.Components {
		again.Components[i].Tables = nil
	}
	if err = hub1HashStored(ctx, d.conn, &again); err != nil || again.FingerprintAll != r.FingerprintAll {
		return fail("stored rows changed while the sealed parts were read")
	}
	if err = p.pins(); err != nil {
		return fail("%v", err)
	}
	return r, nil
}

func hub1ReceiptBytes(r hub1Receipt) []byte {
	raw, _ := json.MarshalIndent(r, "", " ")
	return append(raw, '\n')
}

func writeHub1Receipt(p hub1Parts, exportRecords, exportProof, target string, maxBody int64, manifest, out string) error {
	if !filepath.IsAbs(out) {
		return refused
	}
	if _, err := os.Lstat(out); err == nil {
		fmt.Fprintln(os.Stderr, "hub1 receipt: path exists; a receipt is never overwritten")
		return refused
	}
	r, err := buildHub1Receipt(p, exportRecords, exportProof, target, maxBody, manifest)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return refused
	}
	if _, err = f.Write(hub1ReceiptBytes(r)); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err != nil || closeErr != nil {
		return refused
	}
	sum, _, _ := fileSHA256(out)
	fmt.Printf("hub1 staging receipt written sha256=%s through=%d components=%d complete_chain=%t\n", sum, r.Last, len(r.Components), r.CompleteChain)
	return nil
}

// verifyHub1Receipt recomputes the receipt and refuses on any difference,
// naming the first part whose rows differ.
func verifyHub1Receipt(p hub1Parts, exportRecords, exportProof, target string, maxBody int64, path string) error {
	var given hub1Receipt
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &given) != nil || given.Schema != hub1ReceiptSchema {
		fmt.Fprintln(os.Stderr, "hub1 receipt: unreadable or of another schema")
		return refused
	}
	now, err := buildHub1Receipt(p, exportRecords, exportProof, target, maxBody, "")
	if err != nil {
		return err
	}
	if bytes.Equal(hub1ReceiptBytes(now), raw) {
		sum, _, _ := fileSHA256(path)
		fmt.Printf("hub1 staging receipt verified sha256=%s through=%d components=%d\n", sum, now.Last, len(now.Components))
		return nil
	}
	what := "recomputed receipt differs from the given receipt"
	for i := range now.Components {
		if i >= len(given.Components) || !reflect.DeepEqual(given.Components[i], now.Components[i]) {
			what = fmt.Sprintf("part %d (heights %d to %d) differs from the receipt", i+1, now.Components[i].First, now.Components[i].Last)
			break
		}
	}
	fmt.Fprintln(os.Stderr, "hub1 receipt:", what)
	return refused
}
