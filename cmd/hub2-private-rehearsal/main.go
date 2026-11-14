// Command hub2-private-rehearsal joins only the sealed private Hub2 prefix.
// It has no production DSN, environment fallback, network client or publication path.
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
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/core"
	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	"github.com/DefiantLabs/cosmos-indexer/legacy/genesis"
	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const database = "legacy_replay_rehearsal_hub2_sealed_1400000_20261114"
const port = 25461
const importerRole = "hub2_sealed_importer_20261114"

var refused = errors.New("private sealed rehearsal refused; evidence retained")
var tables = []string{"balance_days", "block_events", "blocks", "eras", "genesis_accounts", "genesis_delegations", "genesis_files", "genesis_unbondings", "genesis_validators", "genesis_withdraw_addresses", "import_ranges", "reconciliation", "taxable_events", "tx_inclusions"}

type Component struct {
	First, Last, RecordBytes                                                int64
	Records, RecordsSHA, Cache, CacheManifest, Status, Differences, Profile string
	Proof                                                                   replayrecords.Proof
	Prefix                                                                  bool
	HistoricalRows, Transactions                                            int64
}
type Manifest struct {
	Version      int
	First, Last  int64
	Genesis      string
	Pins         map[string]string
	Components   []Component
	SourceCommit string
	BinarySHA    string
}

func fileSHA(p string) string {
	f, e := os.Open(p)
	if e != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
func readJSON(p string, v any) error {
	f, e := os.Open(p)
	if e != nil {
		return refused
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return refused
	}
	var x any
	if d.Decode(&x) != io.EOF {
		return refused
	}
	return nil
}
func save(p string, v any) error {
	f, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(v)
}
func dsn(role string) string {
	return fmt.Sprintf("host=127.0.0.1 port=%d user=%s dbname=%s sslmode=disable connect_timeout=5", port, role, database)
}
func guardDSN(s, name string) error {
	c, e := pgconn.ParseConfig(s)
	if e != nil || name != database || c.Database != database || c.Port != port || c.User != importerRole || !net.ParseIP(c.Host).IsLoopback() || len(c.Fallbacks) != 0 {
		return refused
	}
	return nil
}
func owner() (*gorm.DB, error) {
	return gorm.Open(postgres.Open(dsn("postgres")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
}
func validateLayout(m Manifest) error {
	if m.Version != 1 || m.First != 1 || m.Last != 1400000 || len(m.Components) != 7 || m.SourceCommit != "c3444f7f1d48bb48fe596b80c79bb049a10e04b0" {
		return refused
	}
	next := int64(1)
	for i, c := range m.Components {
		if c.First != next || c.Last != c.First+199999 || c.Prefix != (i == 0) || c.Proof.Chain != "cosmoshub-2" || c.Proof.Release != replayrecords.Release034 || c.Proof.From != c.First || c.Proof.Through != c.Last || c.Proof.VerifiedThrough < c.Last || c.Proof.Export || c.RecordsSHA != c.Proof.SHA256 || c.HistoricalRows <= 0 || c.Transactions <= 0 {
			return refused
		}
		next = c.Last + 1
	}
	if next != m.Last+1 {
		return refused
	}
	return nil
}
func pins(m Manifest) error {
	if validateLayout(m) != nil || m.Pins[m.Genesis] != "1e349fb39b85f7707ee78d39879f9d5d61f4d30f67980bb0bf07bd35b2f8bf30" {
		return refused
	}
	for _, c := range m.Components {
		required := []string{c.Records, c.Status, c.Differences, c.CacheManifest}
		if c.Profile != "" {
			required = append(required, c.Profile)
		}
		for _, path := range required {
			if path == "" || len(m.Pins[path]) != 64 {
				return refused
			}
		}
		if !c.Prefix && m.Pins[c.Records] != c.RecordsSHA {
			return refused
		}
		if c.Prefix && (m.Pins[c.Records] != "49e31ce5aaacb5e56cc5d3c501345e69ee61642a4a8b3253820b0cfbf0301867" || m.Pins[c.Status] != "3d1a5550f0334b1c7b4e9bae07b1d1becb4c77806a88215bc97b34c96e7a963c" || m.Pins[c.Differences] != "6bd71d017dbb2e84ee3845041377c094f55c63389784b22af574f9ed29bf5bb9") {
			return refused
		}
	}
	for p, h := range m.Pins {
		if len(h) != 64 || fileSHA(p) != h {
			return refused
		}
	}
	return nil
}

type sourceStatus struct {
	Release   string `json:"release"`
	Chain     string `json:"chain"`
	From      int64  `json:"first_height_this_run"`
	Committed int64  `json:"last_height_committed"`
	Verified  int64  `json:"state_hash_verified_through"`
	Target    int64  `json:"target"`
	Diffs     int64  `json:"heights_where_results_differ_from_cache"`
	Stop      string `json:"stop_reason"`
}

func admission(c Component) (replayrecords.Proof, *replayrecords.ApprovedGasProfile, error) {
	p := c.Proof
	var s sourceStatus
	raw, e := os.ReadFile(c.Status)
	if e != nil || json.Unmarshal(raw, &s) != nil || s.Release != p.Release || s.Chain != p.Chain || s.From != c.First || s.Verified < c.Last || s.Committed < c.Last {
		return p, nil, refused
	}
	markers, e := os.ReadFile(c.Differences)
	if e != nil {
		return p, nil, refused
	}
	if c.Prefix {
		if s.Target != 2902000 || s.Committed >= s.Target || s.Verified != s.Committed-1 || s.Stop != "" || c.RecordBytes != 157340898 || p.SHA256 != "8605b28aa2567d8febc465a9f0d0149e63165b908cfc3f3ed625703675c073a9" || fileSHA(c.Records) != "49e31ce5aaacb5e56cc5d3c501345e69ee61642a4a8b3253820b0cfbf0301867" {
			return p, nil, refused
		}
		scanner := bufio.NewScanner(bytes.NewReader(markers))
		var previous, count, selected int64
		var prefix bytes.Buffer
		for scanner.Scan() {
			line := scanner.Text()
			fields := strings.SplitN(line, " ", 2)
			if len(fields) != 2 {
				return p, nil, refused
			}
			h, e := strconv.ParseInt(fields[0], 10, 64)
			if e != nil || h <= previous || h > s.Committed || strconv.FormatInt(h, 10) != fields[0] {
				return p, nil, refused
			}
			previous = h
			count++
			if h <= c.Last {
				selected++
				prefix.WriteString(line + "\n")
			}
		}
		if scanner.Err() != nil || count != s.Diffs {
			return p, nil, refused
		}
		h := sha256.Sum256(prefix.Bytes())
		p.ResultDifferencesSHA256 = hex.EncodeToString(h[:])
		a, e := replayrecords.AdmitTagDifferences(bytes.NewReader(prefix.Bytes()), p.ResultDifferencesSHA256, 1, c.Last, selected)
		if e != nil {
			return p, nil, refused
		}
		p.ResultDifferencesAdmission = &a
		return p, nil, nil
	}
	if s.Committed != c.Last || s.Verified != c.Last || s.Target != c.Last || s.Stop != "target state verified by next cached header; next block was not executed" {
		return p, nil, refused
	}
	if c.First == 400001 {
		if p.ResultDifferencesSHA256 != "2ff60b284b334e2b29ff2523a9ab7d961a6ed2ec20f29d2976a294bcc6fb36a6" || s.Diffs != 3387 || c.Profile == "" {
			return p, nil, refused
		}
		f, e := os.Open(c.Profile)
		if e != nil {
			return p, nil, refused
		}
		gas, e := replayrecords.LoadApprovedGasProfile(f)
		f.Close()
		if e != nil {
			return p, nil, refused
		}
		lineage := replayrecords.GasRecording{Source: replayrecords.GasFinancialSource, Image: replayrecords.GasFinancialImage, ObserverTree: replayrecords.GasObserverTree, SDKMod: replayrecords.GasSDKModBlob, SDKSum: replayrecords.GasSDKSumBlob}
		if _, _, e = replayrecords.AdmitDifferences(bytes.NewReader(markers), p.ResultDifferencesSHA256, 400001, 482090, 3387, gas, &lineage); e != nil {
			return p, nil, refused
		}
		return p, gas, nil
	}
	if c.Profile != "" {
		return p, nil, refused
	}
	a, _, e := replayrecords.AdmitDifferences(bytes.NewReader(markers), p.ResultDifferencesSHA256, s.From, s.Committed, s.Diffs, nil, nil)
	if e != nil {
		return p, nil, refused
	}
	p.ResultDifferencesAdmission = &a
	return p, nil, nil
}
func cachePins(c Component) (map[string]string, error) {
	f, e := os.Open(c.CacheManifest)
	if e != nil {
		return nil, refused
	}
	defer f.Close()
	out := map[string]string{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if len(line) < 67 || line[64:66] != "  " {
			return nil, refused
		}
		name := line[66:]
		if filepath.IsAbs(name) || strings.Contains(name, "..") || out[name] != "" {
			return nil, refused
		}
		out[name] = line[:64]
	}
	return out, s.Err()
}
func checkHeight(c Component, h int64, expected map[string]string) error {
	dir := strconv.FormatInt(h/10000, 10)
	for _, kind := range []string{"block", "block_results"} {
		name := fmt.Sprintf("cosmoshub-2/%s/%d-%s.raw.gz", dir, h, kind)
		if expected[name] == "" || fileSHA(filepath.Join(c.Cache, name)) != expected[name] {
			return refused
		}
	}
	return nil
}
func prepare(out string) error {
	root, e := sql.Open("pgx", fmt.Sprintf("host=127.0.0.1 port=%d user=postgres dbname=postgres sslmode=disable", port))
	if e != nil {
		return refused
	}
	defer root.Close()
	if _, e = root.Exec("CREATE DATABASE " + database); e != nil {
		return refused
	}
	db, e := owner()
	if e != nil {
		return refused
	}
	pool, e := db.DB()
	if e != nil {
		return refused
	}
	defer pool.Close()
	var name, host string
	var actualPort int
	var system string
	if db.Raw("SELECT current_database()").Scan(&name).Error != nil || name != database {
		return refused
	}
	if db.Raw("SELECT host(inet_server_addr())").Scan(&host).Error != nil || host != "127.0.0.1" {
		return refused
	}
	if db.Raw("SELECT inet_server_port()").Scan(&actualPort).Error != nil || actualPort != port {
		return refused
	}
	if db.Raw("SELECT system_identifier::text FROM pg_control_system()").Scan(&system).Error != nil {
		return refused
	}
	if schema.Migrate(db, importerRole) != nil {
		return refused
	}
	if db.Exec("CREATE TABLE public.protected_fixture (id integer PRIMARY KEY, evidence text NOT NULL); INSERT INTO public.protected_fixture VALUES (1,'synthetic private protected record')").Error != nil {
		return refused
	}
	return save(filepath.Join(out, "prepared.json"), map[string]any{"database": name, "loopback": host, "port": actualPort, "system_identifier": system, "production_access": false})
}
func snapshot(out string) error {
	db, e := owner()
	if e != nil {
		return refused
	}
	pool, e := db.DB()
	if e != nil {
		return refused
	}
	defer pool.Close()
	ctx := context.Background()
	var actual []string
	if db.Raw("SELECT tablename FROM pg_tables WHERE schemaname='legacy' ORDER BY tablename").Scan(&actual).Error != nil || !reflect.DeepEqual(actual, tables) {
		return refused
	}
	result := map[string]any{}
	names := append(append([]string{}, tables...), "public.protected_fixture")
	for _, t := range names {
		full := "legacy." + t
		if strings.HasPrefix(t, "public.") {
			full = t
		}
		rows, e := pool.QueryContext(ctx, "SELECT row_to_json(r)::text,ctid::text,xmin::text FROM "+full+" r ORDER BY row_to_json(r)::text COLLATE \"C\"")
		if e != nil {
			return refused
		}
		content, physical := sha256.New(), sha256.New()
		var n int64
		for rows.Next() {
			var line, ctid, xmin string
			if rows.Scan(&line, &ctid, &xmin) != nil {
				return refused
			}
			fmt.Fprintln(content, line)
			fmt.Fprintln(physical, line+" "+ctid+" "+xmin)
			n++
		}
		if rows.Err() != nil {
			return refused
		}
		rows.Close()
		result[t] = map[string]any{"rows": n, "ordered_full_row_sha256": hex.EncodeToString(content.Sum(nil)), "physical_row_sha256": hex.EncodeToString(physical.Sum(nil))}
	}
	return save(out, result)
}
func runImport(m Manifest, out string) error {
	executable, e := os.Executable()
	if e != nil || fileSHA(executable) != m.BinarySHA || pins(m) != nil {
		return refused
	}
	ctx := context.Background()
	for _, c := range m.Components {
		if _, _, e := admission(c); e != nil {
			return refused
		}
	}
	d, e := openRehearsalDestination(ctx, dsn(importerRole), database)
	if e != nil {
		return refused
	}
	defer d.close()
	guard, e := gorm.Open(postgres.New(postgres.Config{Conn: d.conn}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		return refused
	}
	gf, e := os.Open(m.Genesis)
	if e != nil {
		return refused
	}
	opening, e := genesis.Read(gf)
	gf.Close()
	if e != nil || opening.Chain != "cosmoshub-2" || opening.SHA256 != m.Pins[m.Genesis] {
		return refused
	}
	gr, e := genesis.Load(guard, opening)
	if e != nil || gr.AlreadyLoaded || !reflect.DeepEqual(gr.File, gr.Stored) {
		return refused
	}
	parent := ""
	var allHeights, allRows, allTx, allEvents int64
	started := time.Now()
	results := []map[string]any{}
	for _, c := range m.Components {
		began := time.Now()
		p, gas, e := admission(c)
		if e != nil {
			return refused
		}
		expected, e := cachePins(c)
		if e != nil {
			return refused
		}
		f, e := os.Open(c.Records)
		if e != nil {
			return refused
		}
		var reader io.ReadSeeker = f
		if c.Prefix {
			reader = io.NewSectionReader(f, 0, c.RecordBytes)
		}
		batch := []ledger.Block{}
		var heights, rows, txs, events int64
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			n, e := ledger.Write(ctx, d.conn, batch)
			if e != nil || n != len(batch) {
				return refused
			}
			n, e = ledger.Write(ctx, d.conn, batch)
			if e != nil || n != 0 {
				return refused
			}
			batch = nil
			return nil
		}
		receipt, _ := json.Marshal(map[string]any{"sealed_manifest_sha256": fileSHA(manifestPath), "component_first": c.First, "component_last": c.Last, "records_sha256": c.RecordsSHA, "source_commit": m.SourceCommit, "native_zeroheight_exports": "validation_only_excluded_from_historical_rows", "genuine_opening_sha256": opening.SHA256})
		e = replayrecords.Stream(reader, p, func(r replayrecords.Height) error {
			if checkHeight(c, r.Number, expected) != nil {
				return refused
			}
			if gas != nil && gas.CheckCachedFiles(c.Cache, r.Number) != nil {
				return refused
			}
			h, e := capture.LoadHeight(c.Cache, "cosmoshub-2", r.Number, 64<<20)
			if e != nil || parent != "" && h.Previous != parent {
				return refused
			}
			if gas != nil {
				if h.Results.Legacy == nil {
					return refused
				}
				raw := make([][]byte, len(h.Block.Block.Txs))
				for i, t := range h.Block.Block.Txs {
					raw[i] = t
				}
				if gas.CheckCachedInputs(r.Number, raw, h.Results.Legacy.RawJSON) != nil {
					return refused
				}
			}
			items, e := core.DecodeLegacyBlock(h.Block, h.Results)
			if e != nil {
				return refused
			}
			b, e := core.LegacyReplayBlock(h, items, r)
			if e != nil {
				return refused
			}
			for _, row := range b.Rows {
				if row.Phase == ledger.PhaseExport || row.Source == "export_payout" || row.Trigger == "zero_height_export" {
					return refused
				}
			}
			b.Events = append(b.Events, ledger.Event{Phase: ledger.PhaseBegin, Index: 2, Type: "pinned_replay_receipt", Attributes: [][2]string{{"json", string(receipt)}}})
			if checkHeight(c, r.Number, expected) != nil {
				return refused
			}
			parent = h.Hash
			heights++
			rows += int64(len(b.Rows))
			txs += int64(len(b.Inclusions))
			events += int64(len(b.Events))
			batch = append(batch, b)
			if len(batch) == 500 {
				if flush() != nil {
					return refused
				}
				fmt.Printf("progress height=%d elapsed_seconds=%.3f historical_rows=%d transactions=%d\n", r.Number, time.Since(started).Seconds(), allRows+rows, allTx+txs)
			}
			return nil
		})
		f.Close()
		if e != nil || flush() != nil || heights != 200000 || rows != c.HistoricalRows || txs != c.Transactions {
			return refused
		}
		allHeights += heights
		allRows += rows
		allTx += txs
		allEvents += events
		results = append(results, map[string]any{"first": c.First, "last": c.Last, "heights": heights, "historical_rows": rows, "transactions": txs, "events": events, "seconds": time.Since(began).Seconds(), "every_batch_exact_readback": true})
		if save(filepath.Join(out, fmt.Sprintf("component-%d.json", c.Last)), results[len(results)-1]) != nil {
			return refused
		}
	}
	if pins(m) != nil {
		return refused
	}
	var zero int64
	if d.conn.QueryRowContext(ctx, "SELECT count(*) FROM legacy.taxable_events WHERE height=0 OR phase=4 OR amount_source='export_payout' OR reward_trigger='zero_height_export'").Scan(&zero) != nil || zero != 0 {
		return refused
	}
	return save(filepath.Join(out, "import-result.json"), map[string]any{"heights": allHeights, "historical_rows": allRows, "transactions": allTx, "events": allEvents, "seconds": time.Since(started).Seconds(), "components": results, "genesis_exact_readback": gr, "production_access": false, "whole_chain_books_tax_accepted": false, "zeroheight_historical_rows": zero})
}

var manifestPath string

func main() {
	mode := flag.String("mode", "", "prepare/import/snapshot/refuse-retry")
	mp := flag.String("manifest", "", "sealed pinned private manifest")
	out := flag.String("out", "", "new retained receipt path")
	flag.Parse()
	manifestPath = *mp
	var e error
	switch *mode {
	case "prepare":
		e = prepare(*out)
	case "snapshot":
		e = snapshot(*out)
	case "import":
		var m Manifest
		if readJSON(*mp, &m) != nil {
			e = refused
		} else {
			e = runImport(m, *out)
		}
	case "refuse-retry":
		var d *rehearsalDestination
		d, e = openRehearsalDestination(context.Background(), dsn(importerRole), database)
		if d != nil {
			d.close()
		}
		if e == nil {
			e = refused
		} else {
			e = provePopulatedRefusal(*out)
		}
	default:
		e = refused
	}
	if e != nil {
		fmt.Fprintln(os.Stderr, refused)
		os.Exit(1)
	}
}
