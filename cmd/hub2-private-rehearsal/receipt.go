package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/legacy/genesis"
	"github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The staging receipt says which sealed part every stored row came from. It
// is written only after every listed part has been read again from its
// sealed files and found equal, row for row, to what is stored. It holds no
// clock time, host or database name, so the same database and the same
// sealed files always give the same bytes.

const receiptSchema = "hub2-staging-receipt-v1"
const rowFormat = "one line per row: (pg_catalog.to_jsonb(r) - 'loaded_at')::text plus newline, timezone UTC, datestyle ISO, in primary key order; sha256 of the lines"

var fingerprintKeys = [][2]string{{"balance_days", "chain_id,address,day"}, {"block_events", "chain_id,height,phase,event_index"}, {"blocks", "chain_id,height"}, {"eras", "chain_id"}, {"genesis_accounts", "chain_id,address"}, {"genesis_delegations", "chain_id,delegator,validator"}, {"genesis_files", "chain_id"}, {"genesis_unbondings", "chain_id,delegator,validator,entry"}, {"genesis_validators", "chain_id,operator"}, {"genesis_withdraw_addresses", "chain_id,delegator"}, {"import_ranges", "chain_id,from_height"}, {"reconciliation", "chain_id,address,component"}, {"taxable_events", "chain_id,height,phase,tx_index,msg_index,sub_index"}, {"tx_inclusions", "chain_id,height,tx_index"}}

// projectionTables are the ten tables the loader takes, in its order.
var projectionTables = []string{"genesis_files", "genesis_accounts", "genesis_delegations", "genesis_unbondings", "genesis_validators", "genesis_withdraw_addresses", "blocks", "tx_inclusions", "block_events", "taxable_events"}

type tableHash struct {
	Rows   int64  `json:"rows"`
	SHA256 string `json:"sha256"`
}

type receiptComponent struct {
	First            int64                `json:"first"`
	Last             int64                `json:"last"`
	SealPath         string               `json:"seal_path"`
	SealSHA          string               `json:"seal_sha256"`
	SealNamesSource  bool                 `json:"seal_text_names_source_digest"`
	RecordsSHA       string               `json:"source_records_sha256"`
	StatusSHA        string               `json:"source_status_sha256"`
	CacheManifestSHA string               `json:"source_cache_list_sha256"`
	Rederived        bool                 `json:"stored_rows_equal_rows_rederived_from_sealed_source"`
	Tables           map[string]tableHash `json:"tables"`
}

type receiptPayout struct {
	Height     int64     `json:"height"`
	ExportSHA  string    `json:"export_records_sha256"`
	Rederived  bool      `json:"stored_rows_equal_rows_rederived_from_export"`
	Rows       tableHash `json:"taxable_events_phase4"`
	IndexZero  bool      `json:"every_row_tx_index_0_msg_index_0"`
	ExportPath string    `json:"export_records_path"`
}

type receiptTail struct {
	First            int64                `json:"first"`
	Last             int64                `json:"last"`
	SealPath         string               `json:"tail_seal_path"`
	SealSHA          string               `json:"tail_seal_sha256"`
	CacheManifestSHA string               `json:"capture_list_sha256"`
	Rederived        bool                 `json:"stored_rows_equal_rows_rederived_from_captures"`
	TaxRows          int64                `json:"tax_rows"`
	Tables           map[string]tableHash `json:"tables"`
}

type receiptGenesis struct {
	SHA256      string               `json:"sha256"`
	StoredEqual bool                 `json:"stored_rows_equal_genesis_file"`
	Tables      map[string]tableHash `json:"tables"`
}

type StagingReceipt struct {
	Schema           string               `json:"schema"`
	Chain            string               `json:"chain"`
	First            int64                `json:"first"`
	Last             int64                `json:"last"`
	CanonicalThrough int64                `json:"canonical_through"`
	CanonicalHeight  int64                `json:"canonical_height"`
	CompleteChain    bool                 `json:"complete_chain"`
	ManifestSHA      string               `json:"manifest_sha256"`
	SourceCommit     string               `json:"decoder_source_commit"`
	RowFormat        string               `json:"row_format"`
	Genesis          receiptGenesis       `json:"genesis"`
	Components       []receiptComponent   `json:"components"`
	Payout           *receiptPayout       `json:"restart_payout"`
	Tail             *receiptTail         `json:"historical_tail"`
	Projection       map[string]tableHash `json:"projection_tables"`
	ProjectionNote   string               `json:"projection_tables_note"`
	Fingerprint      map[string]tableHash `json:"fingerprint_tables"`
	FingerprintAll   string               `json:"fingerprint_all_tables_sha256"`
	UnboundRows      int64                `json:"rows_bound_to_no_listed_part"`
}

type bucket struct {
	h hash.Hash
	w *bufio.Writer
	n int64
}

func newBucket() *bucket {
	h := sha256.New()
	return &bucket{h: h, w: bufio.NewWriterSize(h, 64<<10)}
}
func (b *bucket) add(line string) { b.w.WriteString(line); b.w.WriteByte('\n'); b.n++ }
func (b *bucket) sum() tableHash {
	b.w.Flush()
	return tableHash{b.n, hex.EncodeToString(b.h.Sum(nil))}
}

// sealOf names the acceptance receipt of a component. It is always a file
// the manifest already pins, never one found by a folder name alone.
func sealOf(m Manifest, c Component) (string, error) {
	root := filepath.Dir(filepath.Dir(c.Records))
	candidates := []string{filepath.Join(root, "final-component-acceptance.json"), filepath.Join(root, "financial", "component-result.json"), filepath.Join(root, "financial-component-v3", "component-result.json")}
	if c.Prefix {
		candidates = nil
		for p := range m.Pins {
			if filepath.Base(p) == "final-acceptance.json" && strings.Contains(p, "genesis-prefix") {
				candidates = append(candidates, p)
			}
		}
		sort.Strings(candidates)
		if len(candidates) != 1 {
			return "", refused
		}
	}
	for _, p := range candidates {
		if len(m.Pins[p]) == 64 {
			return p, nil
		}
	}
	return "", refused
}

// hashStored reads every table once in key order inside one read-only
// snapshot and files each row under the part its height belongs to.
func hashStored(ctx context.Context, conn *sql.Conn, m Manifest, r *StagingReceipt) error {
	tx, e := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if e != nil {
		return refused
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO, YMD'`); e != nil {
		return refused
	}
	parts := make([]map[string]*bucket, len(m.Components))
	for i := range parts {
		parts[i] = map[string]*bucket{}
		for _, t := range []string{"blocks", "tx_inclusions", "block_events", "taxable_events"} {
			parts[i][t] = newBucket()
		}
	}
	tail := map[string]*bucket{"blocks": newBucket(), "tx_inclusions": newBucket(), "block_events": newBucket()}
	payout := newBucket()
	payoutIndexZero := true
	r.Genesis.Tables, r.Projection, r.Fingerprint = map[string]tableHash{}, map[string]tableHash{}, map[string]tableHash{}
	all := sha256.New()
	for _, k := range fingerprintKeys {
		name := k[0]
		keyed := strings.Contains(k[1], ",height")
		extra := "0::bigint,0::int,true"
		// Every table is read whole, as the fingerprint mode reads it. Only the
		// eras table may name another chain.
		switch {
		case name == "taxable_events":
			extra = "height,phase::int,(tx_index=0 AND msg_index=0)"
		case name == "block_events":
			extra = "height,phase::int,true"
		case keyed:
			extra = "height,0::int,true"
		}
		rows, e := tx.QueryContext(ctx, "SELECT (chain_id=$1),"+extra+",(pg_catalog.to_jsonb(r)-'loaded_at')::pg_catalog.text FROM legacy."+name+" r ORDER BY "+k[1], chainID)
		if e != nil {
			return refused
		}
		whole := newBucket()
		at := 0
		for rows.Next() {
			var height int64
			var phase int
			var ours, zero bool
			var line string
			if rows.Scan(&ours, &height, &phase, &zero, &line) != nil {
				rows.Close()
				return refused
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
				payoutIndexZero = payoutIndexZero && zero
				if height != canonicalHeight {
					r.UnboundRows++
				}
			case height > canonicalHeight:
				if b := tail[name]; b != nil && m.Tail != nil && height <= m.Tail.Last {
					b.add(line)
				} else {
					r.UnboundRows++
				}
			default:
				for at < len(m.Components) && height > m.Components[at].Last {
					at++
				}
				if at == len(m.Components) || height < m.Components[at].First {
					r.UnboundRows++
					at = 0
				} else {
					parts[at][name].add(line)
				}
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return refused
		}
		sum := whole.sum()
		fmt.Fprintf(all, "%s %d %s\n", name, sum.Rows, sum.SHA256)
		r.Fingerprint[name] = sum
		if strings.HasPrefix(name, "genesis_") {
			r.Genesis.Tables[name] = sum
		}
		for _, p := range projectionTables {
			if p == name {
				r.Projection[name] = sum
			}
		}
	}
	// The loader's export keeps loaded_at, so its genesis_files line is hashed
	// as the export writes it.
	var line string
	g := newBucket()
	if tx.QueryRowContext(ctx, `SELECT pg_catalog.to_jsonb(r)::pg_catalog.text FROM legacy.genesis_files r WHERE chain_id=$1`, chainID).Scan(&line) != nil {
		return refused
	}
	g.add(line)
	r.Projection["genesis_files"] = g.sum()
	r.FingerprintAll = hex.EncodeToString(all.Sum(nil))
	fmt.Printf("stored rows hashed all_tables_sha256=%s\n", r.FingerprintAll)
	for i := range r.Components {
		r.Components[i].Tables = map[string]tableHash{}
		for t, b := range parts[i] {
			r.Components[i].Tables[t] = b.sum()
		}
	}
	if r.Payout != nil {
		r.Payout.Rows, r.Payout.IndexZero = payout.sum(), payoutIndexZero
	} else if payout.n != 0 {
		r.UnboundRows += payout.n
	}
	if r.Tail != nil {
		r.Tail.Tables = map[string]tableHash{}
		for t, b := range tail {
			r.Tail.Tables[t] = b.sum()
		}
	}
	return nil
}

// buildReceipt recomputes the whole receipt from the database and the sealed
// files. It writes nothing to the database: the session is read-only.
func buildReceipt(m Manifest, rederive bool) (StagingReceipt, error) {
	r := StagingReceipt{Schema: receiptSchema, Chain: chainID, First: 1, Last: m.Last, CanonicalThrough: m.Last, CanonicalHeight: canonicalHeight, CompleteChain: m.Last == canonicalHeight && m.Tail != nil, ManifestSHA: fileSHA(manifestPath), SourceCommit: m.SourceCommit, RowFormat: rowFormat,
		ProjectionNote: "equal to the sha256 of the loader export's ndjson file of the same table; genesis_files there keeps loaded_at"}
	if pins(m) != nil {
		return r, why("manifest layout or a pinned file changed")
	}
	for _, c := range m.Components {
		seal, e := sealOf(m, c)
		if e != nil {
			return r, why("component %d has no pinned seal", c.Last)
		}
		raw, e := os.ReadFile(seal)
		if e != nil {
			return r, refused
		}
		r.Components = append(r.Components, receiptComponent{First: c.First, Last: c.Last, SealPath: seal, SealSHA: m.Pins[seal], SealNamesSource: strings.Contains(string(raw), c.RecordsSHA), RecordsSHA: c.RecordsSHA, StatusSHA: m.Pins[c.Status], CacheManifestSHA: m.Pins[c.CacheManifest], Rederived: rederive})
	}
	var export *replayrecords.Height
	if x := m.CanonicalExport; x != nil {
		var e error
		if export, e = loadExport(x); e != nil {
			return r, why("canonical export records")
		}
		r.Payout = &receiptPayout{Height: canonicalHeight, ExportSHA: x.Proof.SHA256, ExportPath: x.Records, Rederived: rederive}
	}
	if t := m.Tail; t != nil {
		r.Last = t.Last
		r.Tail = &receiptTail{First: t.First, Last: t.Last, SealPath: t.Seal, SealSHA: m.Pins[t.Seal], CacheManifestSHA: m.Pins[t.CacheManifest], Rederived: rederive}
	}
	ctx := context.Background()
	d, e := openStagingDestination(ctx, dsn(importerRole), database, true)
	if e != nil {
		return r, why("destination identity, confinement or lock")
	}
	defer d.close()
	if _, e = d.conn.ExecContext(ctx, `SET default_transaction_read_only=on`); e != nil {
		return r, refused
	}
	if _, e = verifyStaged(ctx, d.conn, m); e != nil {
		return r, e
	}
	gf, e := os.Open(m.Genesis)
	if e != nil {
		return r, refused
	}
	opening, e := genesis.Read(gf)
	gf.Close()
	if e != nil || opening.SHA256 != m.Pins[m.Genesis] {
		return r, why("genuine genesis")
	}
	var stored string
	if d.conn.QueryRowContext(ctx, `SELECT sha256 FROM legacy.genesis_files WHERE chain_id=$1`, chainID).Scan(&stored) != nil || stored != opening.SHA256 {
		return r, why("stored genesis is not the pinned genesis")
	}
	r.Genesis.SHA256 = opening.SHA256
	if e = hashStored(ctx, d.conn, m, &r); e != nil {
		return r, e
	}
	if r.UnboundRows != 0 {
		return r, why("%d stored rows belong to no listed part", r.UnboundRows)
	}
	if r.Payout != nil && (!r.Payout.IndexZero || r.Payout.Rows.Rows != m.CanonicalExport.Rows) {
		return r, why("restart payout rows are not all tx 0 message 0, or their count differs from the export")
	}
	if !rederive {
		return r, nil
	}
	gormDB, e := guarded(d.conn)
	if e != nil {
		return r, refused
	}
	// Load on a read-only session can only compare. A stored genesis that
	// differs from the file, or a missing one, fails here.
	gr, e := genesis.Load(gormDB, opening)
	if e != nil || !gr.AlreadyLoaded || !reflect.DeepEqual(gr.File, gr.Stored) {
		return r, why("stored genesis rows differ from the genesis file")
	}
	r.Genesis.StoredEqual = true
	started := time.Now()
	for _, c := range m.Components {
		var x *replayrecords.Height
		if c.Last == canonicalHeight {
			x = export
		}
		if _, e := importComponent(ctx, d.conn, m, c, opening.SHA256, x, false, true, started); e != nil {
			return r, why("component %d: stored rows are not the rows its sealed source gives", c.Last)
		}
	}
	if m.Tail != nil {
		if _, e := importTail(ctx, d.conn, m, true); e != nil {
			return r, e
		}
		var n int64
		if d.conn.QueryRowContext(ctx, `SELECT count(*) FROM legacy.taxable_events WHERE chain_id=$1 AND height>$2`, chainID, canonicalHeight).Scan(&n) != nil || n != 0 {
			return r, why("tax row in the tail")
		}
	}
	if pins(m) != nil {
		return r, why("a pinned file changed while it was read")
	}
	return r, nil
}

func writeReceipt(m Manifest, out string) error {
	if !filepath.IsAbs(out) {
		return refused
	}
	if _, e := os.Lstat(out); e == nil {
		return why("receipt path exists; a receipt is never overwritten")
	}
	r, e := buildReceipt(m, true)
	if e != nil {
		return e
	}
	raw, e := json.MarshalIndent(r, "", " ")
	if e != nil {
		return refused
	}
	f, e := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return why("receipt path exists; a receipt is never overwritten")
	}
	if _, e = f.Write(append(raw, '\n')); e != nil || f.Sync() != nil || f.Close() != nil {
		return refused
	}
	fmt.Printf("staging receipt written sha256=%s through=%d components=%d\n", fileSHA(out), r.Last, len(r.Components))
	return nil
}

// verifyReceipt recomputes the receipt and refuses on any difference. The
// stored row hashes are compared first, then the sealed files are read again.
func verifyReceipt(m Manifest, path string, hashesOnly bool) error {
	var given StagingReceipt
	raw, e := os.ReadFile(path)
	if e != nil || json.Unmarshal(raw, &given) != nil || given.Schema != receiptSchema {
		return why("receipt unreadable or of another schema")
	}
	quick, e := buildReceipt(m, false)
	if e != nil {
		return e
	}
	differs := func(now StagingReceipt) error {
		// The quick pass leaves the rederived statements false; compare
		// everything else first so a changed row is named by its part.
		a, b := given, now
		for i := range a.Components {
			if i < len(b.Components) && !reflect.DeepEqual(a.Components[i].Tables, b.Components[i].Tables) {
				return why("rows of component %d differ from the receipt", a.Components[i].Last)
			}
		}
		if (a.Payout == nil) != (b.Payout == nil) || (a.Payout != nil && a.Payout.Rows != b.Payout.Rows) {
			return why("restart payout rows differ from the receipt")
		}
		if (a.Tail == nil) != (b.Tail == nil) || (a.Tail != nil && !reflect.DeepEqual(a.Tail.Tables, b.Tail.Tables)) {
			return why("tail rows differ from the receipt")
		}
		for _, k := range fingerprintKeys {
			if a.Fingerprint[k[0]] != b.Fingerprint[k[0]] {
				return why("table %s differs from the receipt", k[0])
			}
		}
		return nil
	}
	if e = differs(quick); e != nil {
		return e
	}
	if hashesOnly {
		fmt.Println("staging receipt row hashes and pins equal; sealed files not read again")
		return nil
	}
	full, e := buildReceipt(m, true)
	if e != nil {
		return e
	}
	if e = differs(full); e != nil {
		return e
	}
	again, _ := json.MarshalIndent(full, "", " ")
	if string(append(again, '\n')) != string(raw) {
		return why("recomputed receipt differs from the given receipt")
	}
	fmt.Printf("staging receipt verified sha256=%s through=%d components=%d\n", fileSHA(path), full.Last, len(full.Components))
	return nil
}

func guarded(conn *sql.Conn) (*gorm.DB, error) {
	return gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
}
