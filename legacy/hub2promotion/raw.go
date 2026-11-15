package hub2promotion

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Raw history is a separate unpublished stage, not financial acceptance. The
// same atomic core writes only its new scope's two physical evidence tables.
// Nothing is inserted into legacy, so later complete imports need no overwrite,
// adoption or deletion of the raw stage.
const ApprovedRawProfile = "approved-hub1-lossless-raw-stage-v1"
const RawFixtureProfile = "SYNTHETIC-ONLY-hub1-raw-stage-v1"
const PrivateRawProfile = "PRIVATE-real-hub1-raw-stage-v1"
const hub1RawSourceSHA = "5fb7bda0fecdcca39a74f8f31e80f439579d84595d16e58dfb689dfb7bd28280"
const hub1InitialSHA = "73a866b21723ecbc28b6d15951b2eb3aa2f2443650ff6df489bf55ac5edceefa"
const hub1ProjectionProfile = "hub1-lossless-raw-history-deferred-financial-v1"

type RawHistoryLoad struct {
	ProjectionReceipt Pin `json:"projection_receipt"`
	RoundtripReceipt  Pin `json:"roundtrip_receipt"`
}
type rawComponent struct {
	First       int64  `json:"first"`
	Last        int64  `json:"last"`
	History     Pin    `json:"history"`
	Artifacts   []Pin  `json:"artifacts"`
	Compression string `json:"compression"`
}
type rawSource struct {
	Version    int            `json:"version"`
	Chain      string         `json:"chain"`
	Components []rawComponent `json:"components"`
}

func (m Manifest) validateRawFixture() error {
	if !m.Fixture || m.Profile != RawFixtureProfile || !m.Scope.RawStage || m.Scope.chain() != "cosmoshub-1" || m.First != 1 || m.Last < 1 || m.Last > 100 || !strings.HasPrefix(m.Destination.Database, "legacy_replay_rehearsal_") || m.RawHistory != nil || m.Approved != nil || m.CanonicalRows != 0 || m.CanonicalProjectionSHA != valueEmptySHA() {
		return rawRefusal()
	}
	if e := m.Scope.valid(); e != nil {
		return e
	}
	if e := validateIdentity(m.Destination); e != nil {
		return e
	}
	if len(m.Protected) != len(allTables) || len(m.Tables) != 2 {
		return rawRefusal()
	}
	for i, t := range m.Scope.insertTables() {
		p := m.Tables[i]
		if p.Table != t.name || strings.Join(p.Columns, ",") != t.columns || p.Rows < 1 {
			return rawRefusal()
		}
	}
	return m.verifyOfflineProjection()
}

func rawRefusal() error {
	return errors.New("lossless unpublished raw stage refused; financial completeness is not claimed")
}

func (m Manifest) validateApprovedRaw() error {
	private := m.Profile == PrivateRawProfile && m.Fixture && strings.HasPrefix(m.Destination.Database, "legacy_replay_rehearsal_")
	approved := m.Profile == ApprovedRawProfile && !m.Fixture && m.Destination.Database == "cosmostax"
	if m.Version != 1 || (!private && !approved) || !m.Scope.RawStage || m.Scope.chain() != "cosmoshub-1" || m.RawHistory == nil || m.Approved == nil {
		return rawRefusal()
	}
	if m.Approved.ApprovalQuote != ApprovalQuote || m.Approved.ApprovalSource == "" {
		return rawRefusal()
	}
	if m.First != 1 || m.Last != 500015 || m.Canonical != 500000 || m.MigrationLast != 500000 || m.TailFirst != 500001 || m.TailLast != 500015 || m.CanonicalRows != 0 || m.CanonicalProjectionSHA != valueEmptySHA() {
		return rawRefusal()
	}
	if m.Financial != (Pin{}) || m.FinancialSignature != (Pin{}) || m.Approved.MigratedExport != nil || m.Approved.MigrationBinary != nil {
		return rawRefusal()
	}
	if e := m.Scope.valid(); e != nil {
		return e
	}
	if e := validateIdentity(m.Destination); e != nil {
		return e
	}
	if m.Genesis.SHA256 != hub1InitialSHA || m.Source.SHA256 != hub1RawSourceSHA || !shaRE.MatchString(m.BuildSHA) || !shaRE.MatchString(m.BinarySHA) || !shaRE.MatchString(m.Hub3GenesisSHA) || !shaRE.MatchString(m.Hub4GenesisSHA) {
		return rawRefusal()
	}
	if len(m.Protected) != len(allTables) {
		return rawRefusal()
	}
	for _, t := range allTables {
		p, ok := m.Protected[t.name]
		if !ok || p.Rows < 0 || !shaRE.MatchString(p.SHA256) {
			return rawRefusal()
		}
	}
	for _, p := range []Pin{m.Genesis, m.BuildIdentity, m.Source, m.RawHistory.ProjectionReceipt, m.RawHistory.RoundtripReceipt} {
		if e := hashFile(p); e != nil {
			return e
		}
	}
	var projection, roundtrip map[string]any
	for _, x := range []struct {
		pin Pin
		out *map[string]any
	}{{m.RawHistory.ProjectionReceipt, &projection}, {m.RawHistory.RoundtripReceipt, &roundtrip}} {
		b, e := readPinnedBytes(x.pin, 16<<20)
		if e != nil {
			return e
		}
		v, e := decodeUnique(b)
		if e != nil {
			return e
		}
		r, ok := v.(map[string]any)
		if !ok {
			return rawRefusal()
		}
		*x.out = r
	}
	if projection["profile"] != hub1ProjectionProfile || projection["full_hub1_history_complete"] != true || projection["full_chain_books_tax_acceptance"] != false || projection["sql_inclusion_projection_complete"] != false || roundtrip["all_raw_bytes_roundtripped"] != true || roundtrip["profile"] != hub1ProjectionProfile {
		return rawRefusal()
	}
	sourceBytes, e := readPinnedBytes(m.Source, 16<<20)
	if e != nil {
		return e
	}
	var source rawSource
	if e = json.Unmarshal(sourceBytes, &source); e != nil || source.Version != 1 || source.Chain != "cosmoshub-1" || len(source.Components) != 4 {
		return rawRefusal()
	}
	expectedFirst := int64(1)
	for _, c := range source.Components {
		if c.First != expectedFirst || c.Last < c.First || (c.Compression != "" && c.Compression != "none") || len(c.Artifacts) == 0 {
			return rawRefusal()
		}
		expectedFirst = c.Last + 1
		for _, p := range append([]Pin{c.History}, c.Artifacts...) {
			if e = hashFile(p); e != nil {
				return e
			}
		}
	}
	if expectedFirst != 500016 || len(m.Components) != 4 {
		return rawRefusal()
	}
	for i, c := range m.Components {
		sourceC := source.Components[i]
		if c.First != sourceC.First || c.Last != sourceC.Last || !c.HistoricalOnly || c.Receipt != sourceC.History || !shaRE.MatchString(c.Opening) || !shaRE.MatchString(c.Closing) || (i > 0 && m.Components[i-1].Closing != c.Opening) {
			return rawRefusal()
		}
	}
	if m.Approved.CanonicalExport.SHA256 != "f191855b5291fad621eb1267049fcb29e845bf801fb3035a6c6f0e791e7918ab" || m.Approved.NextGenesis.SHA256 != acceptedHub2GenesisSHA {
		return rawRefusal()
	}
	_, export, e := readAllAccountCoins(m.Approved.CanonicalExport)
	if e != nil {
		return e
	}
	height, ok := export["source_height"].(json.Number)
	if !ok || height.String() != "500000" || export["for_zero_height"] != true {
		return rawRefusal()
	}
	_, genesis, e := readAllAccountCoins(m.Approved.NextGenesis)
	if e != nil {
		return e
	}
	if genesis["chain_id"] != "cosmoshub-2" {
		return rawRefusal()
	}
	comparison, e := CompareAllAccounts(m.Approved.CanonicalExport, m.Approved.NextGenesis)
	if e != nil {
		return e
	}
	if !comparison.Pass {
		return rawRefusal()
	}
	if len(m.Tables) != 2 {
		return rawRefusal()
	}
	for i, t := range m.Scope.insertTables() {
		p := m.Tables[i]
		if p.Table != t.name || strings.Join(p.Columns, ",") != t.columns || p.Rows < 1 {
			return rawRefusal()
		}
	}
	return m.verifyRawBytes(source)
}

func (m Manifest) protectedSnapshot(ctx context.Context, q querier) (Snapshot, error) {
	return snapshotChain(ctx, q, m.Scope.chain(), !m.Scope.RawStage, true)
}
func (m Manifest) emptyTarget(ctx context.Context, q querier) error {
	if !m.Scope.RawStage {
		return emptyChain(ctx, q, m.Scope.chain())
	}
	for _, t := range m.Scope.insertTables() {
		var yes bool
		if e := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+m.Scope.baseRelation(t)+")").Scan(&yes); e != nil {
			return e
		}
		if yes {
			return errors.New("preexisting raw stage refused, never adopted")
		}
	}
	return nil
}
func (m Manifest) verifyRawSQL(ctx context.Context, q querier) error {
	for i, t := range m.Scope.insertTables() {
		fp, e := fingerprintRelation(ctx, q, t, m.Scope.baseRelation(t), "true", false)
		if e != nil {
			return e
		}
		p := m.Tables[i]
		if fp != (Fingerprint{p.Rows, p.File.SHA256}) {
			return errors.New("exact raw stage projection mismatch")
		}
	}
	var total, distinct, first, last int64
	e := q.QueryRowContext(ctx, "SELECT count(*),count(DISTINCT height),min(height),max(height) FROM "+m.Scope.Schema+".raw_blocks").Scan(&total, &distinct, &first, &last)
	if e != nil {
		return e
	}
	if total != m.Last || distinct != total || first != 1 || last != m.Last {
		return rawRefusal()
	}
	return nil
}

// CaptureApprovedProtected includes every legacy row for raw staging, including
// the selected chain. Financial imports continue to protect every other chain.
func CaptureApprovedProtected(ctx context.Context, q *sql.DB, m Manifest) (Snapshot, error) {
	if e := m.Scope.valid(); e != nil {
		return nil, e
	}
	tx, e := beginAuditBudget(ctx, q, true)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	return m.protectedSnapshot(ctx, tx)
}

// Raw SQL rows contain only scalars and arrays of string pairs. Decode each
// top-level value without recursively tokenizing large base64 strings, while
// still refusing duplicate/missing/extra fields and trailing JSON. Typed row
// decoding below checks every nested value, including duplicate attributes.
func rawRowShape(raw []byte, t table) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, e := d.Token()
	if e != nil || token != json.Delim('{') {
		return rawRefusal()
	}
	seen := map[string]bool{}
	for d.More() {
		key, e := d.Token()
		if e != nil {
			return rawRefusal()
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return rawRefusal()
		}
		seen[name] = true
		var value json.RawMessage
		if e = d.Decode(&value); e != nil || bytes.Equal(value, []byte("null")) {
			return rawRefusal()
		}
	}
	if _, e = d.Token(); e != nil {
		return rawRefusal()
	}
	if _, e = d.Token(); e != io.EOF {
		return rawRefusal()
	}
	columns := projectionColumns(t)
	if len(seen) != len(columns) {
		return rawRefusal()
	}
	for _, c := range columns {
		if !seen[c] {
			return rawRefusal()
		}
	}
	return nil
}

// rawSQLTime mirrors PostgreSQL's ParseFractionalSecond: strtod followed by
// rint(frac * USECS_PER_SEC). Native nanoseconds remain byte-exact in evidence;
// only the separate timestamptz header uses the database's microsecond value.
func rawSQLTime(value string) (time.Time, error) {
	t, e := time.Parse(time.RFC3339Nano, value)
	if e != nil || !strings.HasSuffix(value, "Z") {
		return time.Time{}, rawRefusal()
	}
	if dot := strings.IndexByte(value, '.'); dot >= 0 {
		frac, e := strconv.ParseFloat(value[dot:len(value)-1], 64)
		if e != nil {
			return time.Time{}, rawRefusal()
		}
		return t.Truncate(time.Second).Add(time.Duration(math.RoundToEven(frac*1000000)) * time.Microsecond), nil
	}
	return t, nil
}

// verifyRawBytes restores each event payload against the actual pinned original
// input, not a receipt claiming restoration. Unknown fields, failures, every
// native result and every source-artifact byte must survive unchanged.
func (m Manifest) verifyRawBytes(source rawSource) error {
	blocks, e := openPinned(m.Tables[0].File)
	if e != nil {
		return e
	}
	defer blocks.Close()
	blockHash := sha256.New()
	headers := bufio.NewScanner(io.TeeReader(blocks, blockHash))
	headers.Buffer(make([]byte, 65536), 16<<20)
	events, e := openPinned(m.Tables[1].File)
	if e != nil {
		return e
	}
	defer events.Close()
	eventHash := sha256.New()
	rows := bufio.NewScanner(io.TeeReader(events, eventHash))
	rows.Buffer(make([]byte, 65536), 16<<20)
	component := -1
	var input *os.File
	var zip *gzip.Reader
	var history *bufio.Reader
	var artifact *os.File
	defer func() {
		if zip != nil {
			zip.Close()
		}
		if input != nil {
			input.Close()
		}
		if artifact != nil {
			artifact.Close()
		}
	}()
	artifactIndex, chunkIndex := 0, 0
	var artifactBytes int64
	var heightCount, eventCount int64
	var previousHeight int64 = -1
	previousIndex := -1
	statuses := 0
	finishedHistory := false
	closingCommit := ""
	finishComponent := func() error {
		if component < 0 {
			return nil
		}
		if artifact != nil || artifactIndex != len(source.Components[component].Artifacts) || statuses != 1 || !finishedHistory {
			return rawRefusal()
		}
		if _, e := history.ReadByte(); e != io.EOF {
			return rawRefusal()
		}
		if zip != nil {
			if e := zip.Close(); e != nil {
				return rawRefusal()
			}
			zip = nil
		}
		if e := input.Close(); e != nil {
			return e
		}
		input = nil
		return nil
	}
	for rows.Scan() {
		eventCount++
		if e = rawRowShape(rows.Bytes(), inserts[8]); e != nil {
			return e
		}
		var row struct {
			Chain      string     `json:"chain_id"`
			Height     int64      `json:"height"`
			Phase      int        `json:"phase"`
			Index      int        `json:"event_index"`
			Type       string     `json:"type"`
			Attributes [][]string `json:"attributes"`
		}
		if e = json.Unmarshal(rows.Bytes(), &row); e != nil {
			return rawRefusal()
		}
		if row.Chain != "cosmoshub-1" || row.Phase != 1 || row.Height < 1 || row.Height > m.Last || row.Height < previousHeight || (row.Height == previousHeight && row.Index != previousIndex+1) || (row.Height != previousHeight && row.Index != 0) {
			return rawRefusal()
		}
		previousHeight, previousIndex = row.Height, row.Index
		attrs := map[string]string{}
		for _, pair := range row.Attributes {
			if len(pair) != 2 {
				return rawRefusal()
			}
			if _, ok := attrs[pair[0]]; ok {
				return rawRefusal()
			}
			attrs[pair[0]] = pair[1]
		}
		switch row.Type {
		case "hub1_native033_raw_history":
			if len(attrs) != 3 || attrs["encoding"] != "base64-exact-jsonl-line" {
				return rawRefusal()
			}
			raw, e := base64.StdEncoding.Strict().DecodeString(attrs["raw"])
			if e != nil {
				return rawRefusal()
			}
			var native struct {
				Kind         string          `json:"kind"`
				Height       int64           `json:"height"`
				Hash         string          `json:"block_hash"`
				Transactions []string        `json:"transactions"`
				Commit       json.RawMessage `json:"commit"`
				Header       struct {
					Chain    string `json:"chain_id"`
					Height   int64  `json:"height"`
					Time     string `json:"time"`
					Proposer string `json:"proposer_address"`
				} `json:"header"`
			}
			if e = json.Unmarshal(raw, &native); e != nil || native.Kind != attrs["kind"] {
				return rawRefusal()
			}
			if native.Kind == "source_anchor" {
				if e = finishComponent(); e != nil {
					return e
				}
				component++
				if component >= len(source.Components) {
					return rawRefusal()
				}
				c := source.Components[component]
				if native.Height != c.First-1 || row.Height != c.First {
					return rawRefusal()
				}
				input, e = openPinned(c.History)
				if e != nil {
					return e
				}
				if c.Compression == "none" {
					history = bufio.NewReader(input)
				} else {
					zip, e = gzip.NewReader(input)
					if e != nil {
						return rawRefusal()
					}
					history = bufio.NewReader(zip)
				}
				artifactIndex, chunkIndex, artifactBytes, statuses = 0, 0, 0, 0
				finishedHistory = false
				closingCommit = ""
				if len(m.Components) > 0 {
					commit, e := decodeUnique(native.Commit)
					if e != nil || m.Components[component].Opening != valueSHA(commit) {
						return rawRefusal()
					}
				}
			}
			if component < 0 || finishedHistory {
				return rawRefusal()
			}
			expected, e := history.ReadBytes('\n')
			if e != nil || !bytes.Equal(expected, raw) {
				return rawRefusal()
			}
			switch native.Kind {
			case "source_anchor":
			case "height":
				heightCount++
				if len(m.Components) > 0 {
					commit, e := decodeUnique(native.Commit)
					if e != nil {
						return rawRefusal()
					}
					closingCommit = valueSHA(commit)
				}
				if native.Height != heightCount || row.Height != heightCount || native.Header.Height != heightCount || native.Header.Chain != "cosmoshub-1" || !headers.Scan() {
					return rawRefusal()
				}
				var block struct {
					Chain    string `json:"chain_id"`
					Height   int64  `json:"height"`
					Time     string `json:"time"`
					Hash     string `json:"block_hash"`
					Proposer string `json:"proposer"`
					TxCount  int    `json:"tx_count"`
				}
				if e = rawRowShape(headers.Bytes(), inserts[6]); e != nil {
					return e
				}
				if e = json.Unmarshal(headers.Bytes(), &block); e != nil {
					return rawRefusal()
				}
				hash, e := base64.StdEncoding.Strict().DecodeString(native.Hash)
				if e != nil || len(hash) != 32 {
					return rawRefusal()
				}
				a, e := time.Parse(time.RFC3339Nano, block.Time)
				if e != nil {
					return rawRefusal()
				}
				b, e := rawSQLTime(native.Header.Time)
				if e != nil {
					return rawRefusal()
				}
				if block.Chain != "cosmoshub-1" || block.Height != heightCount || !a.Equal(b) || block.Hash != strings.ToUpper(hex.EncodeToString(hash)) || block.Proposer != native.Header.Proposer || block.TxCount != len(native.Transactions) {
					return rawRefusal()
				}
			case "history_completed", "closed_terminal_history_completed":
				if native.Height != source.Components[component].Last || row.Height != native.Height {
					return rawRefusal()
				}
				if len(m.Components) > 0 && closingCommit != m.Components[component].Closing {
					return rawRefusal()
				}
				finishedHistory = true
			default:
				return rawRefusal()
			}
		case "hub1_raw_history_status":
			if component < 0 || row.Height != source.Components[component].First || len(attrs) != 7 || attrs["profile"] != hub1ProjectionProfile || attrs["derived_inclusions"] != "deferred" || attrs["derived_tax_rows"] != "deferred" || attrs["tax_complete"] != "false" || attrs["canonical_height"] != "500000" || attrs["historical_tail"] != "500001..500015" || attrs["intermediate_zero_height_payouts"] != "validation-only-not-income" {
				return rawRefusal()
			}
			statuses++
		case "hub1_source_artifact_chunk":
			if component < 0 || len(attrs) != 4 || row.Height != source.Components[component].First || attrs["encoding"] != "base64-exact-source-bytes" || attrs["artifact"] != fmt.Sprint(artifactIndex) || attrs["chunk"] != fmt.Sprint(chunkIndex) || artifactIndex >= len(source.Components[component].Artifacts) {
				return rawRefusal()
			}
			if artifact == nil {
				artifact, e = openPinned(source.Components[component].Artifacts[artifactIndex])
				if e != nil {
					return e
				}
			}
			raw, e := base64.StdEncoding.Strict().DecodeString(attrs["raw"])
			if e != nil || len(raw) == 0 || len(raw) > 192*1024 {
				return rawRefusal()
			}
			expected := make([]byte, len(raw))
			if _, e = io.ReadFull(artifact, expected); e != nil || !bytes.Equal(expected, raw) {
				return rawRefusal()
			}
			artifactBytes += int64(len(raw))
			chunkIndex++
		case "hub1_source_artifact_complete":
			if component < 0 || len(attrs) != 5 || row.Height != source.Components[component].First || artifactIndex >= len(source.Components[component].Artifacts) {
				return rawRefusal()
			}
			p := source.Components[component].Artifacts[artifactIndex]
			if attrs["artifact"] != fmt.Sprint(artifactIndex) || attrs["chunks"] != fmt.Sprint(chunkIndex) || attrs["bytes"] != fmt.Sprint(artifactBytes) || attrs["sha256"] != p.SHA256 || attrs["source_path"] != p.Path {
				return rawRefusal()
			}
			if artifact == nil {
				artifact, e = openPinned(p)
				if e != nil {
					return e
				}
			}
			var b [1]byte
			if n, e := artifact.Read(b[:]); n != 0 || e != io.EOF {
				return rawRefusal()
			}
			if e = artifact.Close(); e != nil {
				return e
			}
			artifact = nil
			artifactIndex++
			chunkIndex = 0
			artifactBytes = 0
		default:
			return rawRefusal()
		}
	}
	if e = rows.Err(); e != nil {
		return e
	}
	if e = finishComponent(); e != nil {
		return e
	}
	if component != 3 || heightCount != m.Last || heightCount != m.Tables[0].Rows || eventCount != m.Tables[1].Rows || headers.Scan() {
		return rawRefusal()
	}
	if e = headers.Err(); e != nil {
		return e
	}
	if hex.EncodeToString(blockHash.Sum(nil)) != m.Tables[0].File.SHA256 || hex.EncodeToString(eventHash.Sum(nil)) != m.Tables[1].File.SHA256 {
		return rawRefusal()
	}
	return nil
}
