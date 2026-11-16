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

	"github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The defaults name the retained 1.4M rehearsal. -database and -port select a
// different private loopback staging database; see guardTarget.
var database = "legacy_replay_rehearsal_hub2_sealed_1400000_20261114"
var port = 25461

// canonicalHeight is fixed in the binary. Only the private wiring test of the
// restart payout path lowers it, in its own throwaway database.
var canonicalHeight int64 = 2902000

const tailLast int64 = 2902002
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
	// CanonicalExport is the restart payout at 2,902,000. It is required
	// exactly when the manifest reaches that height and is refused before it.
	CanonicalExport *CanonicalExport `json:",omitempty"`
	// Tail is the two historical heights after the export. They are stored
	// as headers and transactions only, never as tax rows.
	Tail *Tail `json:",omitempty"`
}
type CanonicalExport struct {
	Records string
	Proof   replayrecords.Proof
	Rows    int64
}
type Tail struct {
	First, Last          int64
	Cache, CacheManifest string
	// Seal is the tail's own acceptance receipt, when one exists.
	Seal string `json:",omitempty"`
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
	if e != nil || name != database || c.Database != database || int(c.Port) != port || c.User != importerRole || !net.ParseIP(c.Host).IsLoopback() || len(c.Fallbacks) != 0 {
		return refused
	}
	return nil
}
func owner() (*gorm.DB, error) {
	return gorm.Open(postgres.Open(dsn("postgres")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
}
func validateLayout(m Manifest) error {
	if (m.Version != 1 && m.Version != 2) || m.First != 1 || len(m.Components) == 0 || m.SourceCommit != "c3444f7f1d48bb48fe596b80c79bb049a10e04b0" {
		return refused
	}
	next := int64(1)
	for i, c := range m.Components {
		// Every component is 200,000 heights except the last of the chain,
		// which ends at the canonical export height.
		last := c.First + 199999
		if last > canonicalHeight {
			last = canonicalHeight
		}
		if c.First != next || c.Last != last || c.Prefix != (i == 0) || c.Proof.Chain != "cosmoshub-2" || c.Proof.Release != replayrecords.Release034 || c.Proof.From != c.First || c.Proof.Through != c.Last || c.Proof.VerifiedThrough < c.Last || c.Proof.Export || c.RecordsSHA != c.Proof.SHA256 || c.HistoricalRows <= 0 || c.Transactions <= 0 {
			return refused
		}
		next = c.Last + 1
	}
	if next != m.Last+1 || m.Last > canonicalHeight {
		return refused
	}
	x := m.CanonicalExport
	if (x != nil) != (m.Last == canonicalHeight) {
		return refused
	}
	if x != nil && (m.Version != 2 || x.Records == "" || x.Rows <= 0 || !x.Proof.Export || x.Proof.Chain != "cosmoshub-2" || x.Proof.Release != replayrecords.Release034 || x.Proof.From != canonicalHeight || x.Proof.Through != canonicalHeight || x.Proof.VerifiedThrough < canonicalHeight || len(x.Proof.SHA256) != 64) {
		return refused
	}
	if t := m.Tail; t != nil && (m.Version != 2 || m.Last != canonicalHeight || t.First != canonicalHeight+1 || t.Last != tailLast || t.Cache == "" || t.CacheManifest == "") {
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
	if x := m.CanonicalExport; x != nil && m.Pins[x.Records] != x.Proof.SHA256 {
		return refused
	}
	if t := m.Tail; t != nil && (len(m.Pins[t.CacheManifest]) != 64 || (t.Seal != "" && len(m.Pins[t.Seal]) != 64)) {
		return refused
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

var manifestPath string

func main() {
	mode := flag.String("mode", "", "prepare/import/add/extend-manifest/fingerprint/part-entry/receipt/verify-receipt/snapshot/refuse-retry")
	mp := flag.String("manifest", "", "sealed pinned private manifest")
	out := flag.String("out", "", "new retained receipt path")
	name := flag.String("database", database, "private staging database, legacy_replay_rehearsal_hub2_SUFFIX")
	p := flag.Int("port", port, "loopback port of the private staging server, never the serving forward")
	parallel := flag.Int("parallel", 1, "components imported at once, 1 to 4")
	component := flag.String("component", "", "extend-manifest: sealed component directory holding final-component-acceptance.json")
	tailCache := flag.String("tail-cache", "", "extend-manifest at the canonical end: capture root of heights 2902001 and 2902002")
	tailManifest := flag.String("tail-cache-manifest", "", "extend-manifest at the canonical end: SHA256 list of the tail captures")
	tailSeal := flag.String("tail-seal", "", "extend-manifest at the canonical end: the tail's own acceptance receipt, when one exists")
	receiptPath := flag.String("receipt", "", "verify-receipt: the staging receipt to recompute")
	hashesOnly := flag.Bool("hashes-only", false, "verify-receipt: compare stored row hashes and pins only, skip reading the sealed files again")
	entries := flag.String("entries", "", "part-entry: directory of part entries; receipt: build from these entries and one database pass")
	through := flag.Int64("through", 0, "fingerprint: only heights up to this one, 0 for all")
	flag.Parse()
	if guardTarget(*name, *p) != nil || (*tailCache == "") != (*tailManifest == "") {
		fmt.Fprintln(os.Stderr, refused)
		os.Exit(1)
	}
	database, port = *name, *p
	manifestPath = *mp
	var e error
	switch *mode {
	case "prepare":
		e = prepare(*out)
	case "snapshot":
		e = snapshot(*out)
	case "fingerprint":
		e = fingerprint(*out, *through)
	case "import", "add":
		var m Manifest
		if readJSON(*mp, &m) != nil {
			e = refused
		} else {
			e = runAdd(m, *out, *mode == "add", *parallel)
		}
	case "extend-manifest":
		var m Manifest
		if readJSON(*mp, &m) != nil || *component == "" || !filepath.IsAbs(*component) {
			e = refused
		} else {
			e = extendManifest(m, filepath.Clean(*component), *tailCache, *tailManifest, *tailSeal, *out)
		}
	case "receipt", "verify-receipt", "part-entry":
		var m Manifest
		if readJSON(*mp, &m) != nil {
			e = refused
		} else if *mode == "part-entry" {
			e = writeEntries(m, *entries)
		} else if *mode == "receipt" {
			e = writeReceipt(m, *out, *entries)
		} else {
			e = verifyReceipt(m, *receiptPath, *hashesOnly)
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
