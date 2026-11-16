package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/DefiantLabs/cosmos-indexer/core"
	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
)

// hub1Export is the height cosmoshub-2 began from. Hub 1 produced a few more
// blocks after it; they are history and never tax rows.
const hub1Export = 500000

var hub1RehearsalName = regexp.MustCompile(`^legacy_replay_rehearsal_hub1_[a-z0-9_]{4,43}$`)

type hub1Source struct {
	dir   string
	proof replayrecords.Proof
	read  *hub1Read
}

// hub1Read lists the sealed files one run read from one source, with the
// digest of each as it was read, and what the source contributed.
type hub1Read struct {
	Source                 string           `json:"source"`
	From                   int64            `json:"from"`
	Through                int64            `json:"through"`
	CaptureReceiptSHA256   string           `json:"capture_receipt_sha256"`
	CompletionMarkerSHA256 string           `json:"completion_marker_sha256"`
	CompletionListSHA256   string           `json:"completion_list_sha256,omitempty"`
	MovementsFileSHA256    string           `json:"movements_file_sha256"`
	MovementsFileBytes     int64            `json:"movements_file_bytes"`
	MovementsStreamSHA256  string           `json:"movements_stream_sha256"`
	MovementsStreamRecords int64            `json:"movements_stream_records"`
	MovementsStreamBytes   int64            `json:"movements_stream_bytes"`
	BlockCaptureFiles      int64            `json:"block_capture_files"`
	BlockCaptureListSHA256 string           `json:"block_capture_list_sha256"`
	Heights                int64            `json:"heights"`
	Transactions           int64            `json:"transactions"`
	Rows                   int64            `json:"rows"`
	RowsByCategory         map[string]int64 `json:"rows_by_category_and_source"`
	captures               hash.Hash
	part                   *hub1Slice
}

// hub1Slice is what one listed part took from a source it shares with other
// parts: the capture files of its own heights only.
type hub1Slice struct {
	first, last, heights, transactions, files int64
	captures                                  hash.Hash
}

func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// capture adds the block capture files of one height to the list digest:
// one line per file, its path under the source and its digest.
func (m *hub1Read) capture(dir string, height int64) error {
	base := filepath.Join("cache", "cosmoshub-1", fmt.Sprint(height/capture.DirSpan), fmt.Sprint(height))
	for _, suffix := range []string{"-block.raw.gz", "-block_results.raw.gz"} {
		sum, _, err := fileSHA256(filepath.Join(dir, base+suffix))
		if errors.Is(err, os.ErrNotExist) && suffix == "-block_results.raw.gz" {
			continue
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(m.captures, "%s  %s\n", sum, filepath.ToSlash(base+suffix))
		m.BlockCaptureFiles++
		if m.part != nil {
			fmt.Fprintf(m.part.captures, "%s  %s\n", sum, filepath.ToSlash(base+suffix))
			m.part.files++
		}
	}
	return nil
}

// hub1Admit reads the capture receipt of one closed recording and builds the
// proof its movement stream must match. It reads nothing else of the source.
func hub1Admit(dir string) (hub1Source, error) {
	var receipt struct {
		Schema   string `json:"schema"`
		First    int64  `json:"first"`
		Through  int64  `json:"through"`
		Complete bool   `json:"source_whole_complete"`
		Streams  struct {
			Movements struct {
				Completion string `json:"completion_kind"`
				RawBytes   int64  `json:"raw_bytes"`
				RawSHA256  string `json:"raw_sha256"`
				Records    int64  `json:"records"`
			} `json:"movements"`
		} `json:"streams"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "capture.private.json"))
	if err != nil || json.Unmarshal(raw, &receipt) != nil {
		return hub1Source{}, fmt.Errorf("Hub 1 source has no readable capture receipt")
	}
	if _, err := os.Stat(filepath.Join(dir, "recording-complete.private")); err != nil {
		return hub1Source{}, fmt.Errorf("Hub 1 source has no completion marker")
	}
	m := receipt.Streams.Movements
	if receipt.Schema != "native033-closed-source-capture-v1" || !receipt.Complete || m.Completion != "observation_completed" {
		return hub1Source{}, fmt.Errorf("Hub 1 source is not a complete closed capture")
	}
	read := &hub1Read{Source: dir, From: receipt.First, Through: receipt.Through, MovementsStreamSHA256: m.RawSHA256, MovementsStreamRecords: m.Records,
		MovementsStreamBytes: m.RawBytes, RowsByCategory: map[string]int64{}, captures: sha256.New()}
	sum := sha256.Sum256(raw)
	read.CaptureReceiptSHA256 = hex.EncodeToString(sum[:])
	if read.CompletionMarkerSHA256, _, err = fileSHA256(filepath.Join(dir, "recording-complete.private")); err != nil {
		return hub1Source{}, fmt.Errorf("Hub 1 source has no completion marker")
	}
	if list, _, err := fileSHA256(filepath.Join(dir, "completed.private.sha256")); err == nil {
		read.CompletionListSHA256 = list
	}
	return hub1Source{dir: dir, read: read, proof: replayrecords.Proof{Chain: "cosmoshub-1", Release: replayrecords.Release033, From: receipt.First, Through: receipt.Through,
		VerifiedThrough: receipt.Through, SHA256: m.RawSHA256, Records: m.Records, RawBytes: m.RawBytes}}, nil
}

type gzipFile struct {
	*gzip.Reader
	file   *os.File
	sum    hash.Hash
	source hub1Source
}

// Close digests whatever the gzip reader left of the file, so the digest is
// of the whole file as stored, and checks it against any earlier pass.
func (g gzipFile) Close() error {
	_, err := io.Copy(g.sum, g.file)
	if closeErr := g.file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	info, err := os.Stat(g.file.Name())
	if err != nil {
		return err
	}
	got := hex.EncodeToString(g.sum.Sum(nil))
	if g.source.read.MovementsFileSHA256 != "" && g.source.read.MovementsFileSHA256 != got {
		return fmt.Errorf("Hub 1 movement file changed between passes")
	}
	g.source.read.MovementsFileSHA256, g.source.read.MovementsFileBytes = got, info.Size()
	return nil
}

func (s hub1Source) open() (io.ReadCloser, error) {
	f, err := os.Open(filepath.Join(s.dir, "movements.source-closed.gz"))
	if err != nil {
		return nil, fmt.Errorf("Hub 1 movement stream unreadable")
	}
	sum := sha256.New()
	z, err := gzip.NewReader(bufio.NewReaderSize(io.TeeReader(f, sum), 4<<20))
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("Hub 1 movement stream is not gzip")
	}
	return gzipFile{z, f, sum, s}, nil
}

// runHub1 converts closed Hub 1 recordings into legacy rows. Unlike the
// Hub 2 path it says why it stopped: the height, the record kind and the
// label, never an address or an amount.
func runHub1(dirs []string, exportRecords, exportProof, target string, write bool, maxBody int64, manifest string) error {
	_, err := hub1Options{dirs: dirs, exportRecords: exportRecords, exportProof: exportProof, target: target, write: write, maxBody: maxBody, manifest: manifest}.run()
	return err
}

// hub1Options is one conversion. With compare set nothing is written: every
// height derived from the sealed files must equal what that session already
// stores. With tail set the blocks after the export height follow as
// history, with no tax rows.
type hub1Options struct {
	dirs                               []string
	exportRecords, exportProof, target string
	write                              bool
	maxBody                            int64
	manifest                           string
	compare                            *sql.Conn
	tail                               *hub1TailSpec
	bound                              func([]hub1Source) error
	slices                             []*hub1Slice
}

type hub1Converted struct {
	sources    []hub1Source
	export     *replayrecords.Proof
	payoutRows int64
	tailList   string
}

func (o hub1Options) run() (done hub1Converted, _ error) {
	dirs, exportRecords, exportProof, target, write, maxBody, manifest := o.dirs, o.exportRecords, o.exportProof, o.target, o.write, o.maxBody, o.manifest
	if write && o.compare != nil {
		return done, refused
	}
	fail := func(err error) error {
		fmt.Fprintln(os.Stderr, "hub1:", err)
		return refused
	}
	var sources []hub1Source
	for _, dir := range dirs {
		s, err := hub1Admit(strings.TrimSpace(dir))
		if err != nil {
			return done, fail(err)
		}
		if len(sources) > 0 && s.proof.From != sources[len(sources)-1].proof.Through+1 {
			return done, fail(fmt.Errorf("Hub 1 sources are not consecutive ranges"))
		}
		if s.proof.Through > hub1Export {
			return done, fail(fmt.Errorf("Hub 1 source runs past the export height; later blocks are never tax rows"))
		}
		sources = append(sources, s)
	}
	done.sources = sources
	if o.bound != nil {
		if err := o.bound(sources); err != nil {
			return done, fail(err)
		}
	}
	last := sources[len(sources)-1].proof.Through
	if write && !hub1RehearsalName.MatchString(target) {
		return done, fail(fmt.Errorf("Hub 1 rows go only to a database named legacy_replay_rehearsal_hub1_SUFFIX"))
	}
	var export *replayrecords.Height
	var admittedExport *replayrecords.Proof
	if (exportRecords == "") != (exportProof == "") {
		return done, refused
	}
	if exportRecords != "" {
		var ep replayrecords.Proof
		if readJSON(exportProof, &ep, true) != nil || !ep.Export || last != hub1Export || ep.From != hub1Export || ep.Through != hub1Export {
			return done, fail(fmt.Errorf("Hub 1 export proof must be the zero-height export of the last source height %d", hub1Export))
		}
		_, err := replayrecords.StreamHub1(func() (io.ReadCloser, error) { return os.Open(exportRecords) }, ep, func(h replayrecords.Height) error { export = &h; return nil })
		if err != nil || export == nil {
			return done, fail(fmt.Errorf("Hub 1 export records: %w", err))
		}
		admittedExport = &ep
		done.export = &ep
	}
	// Before anything is written every stream is checked whole against its
	// pin. A run that writes nothing checks each pin at the end of its pass.
	var wg sync.WaitGroup
	checked := make([]error, len(sources))
	counted := make([]replayrecords.Hub1Counts, len(sources))
	for i, s := range sources {
		if !write {
			break
		}
		wg.Add(1)
		go func(i int, s hub1Source) {
			defer wg.Done()
			r, err := s.open()
			if err != nil {
				checked[i] = err
				return
			}
			counted[i], checked[i] = replayrecords.ScanHub1(r, s.proof, nil)
			if err := r.Close(); checked[i] == nil {
				checked[i] = err
			}
		}(i, s)
	}
	wg.Wait()
	admitted := func(i int) {
		kinds := make([]string, 0, len(counted[i].Kinds))
		for kind, n := range counted[i].Kinds {
			kinds = append(kinds, fmt.Sprintf("%s=%d", kind, n))
		}
		sort.Strings(kinds)
		fmt.Printf("hub1 source %d heights %d to %d admitted: records=%d %s\n", i+1, sources[i].proof.From, sources[i].proof.Through, counted[i].Records, strings.Join(kinds, " "))
	}
	for i, err := range checked {
		if err != nil {
			return done, fail(fmt.Errorf("source %d: %w", i+1, err))
		}
		if write {
			admitted(i)
		}
	}
	ctx := context.Background()
	var destination *rehearsalDestination
	defer func() {
		if destination != nil {
			destination.close()
		}
	}()
	heights, txs, rows, held := 0, 0, 0, 0
	categories := map[string]int{}
	recorded := map[int64]core.ReplayNet{}
	parent := ""
	at := 0
	for i, s := range sources {
		receipt, _ := json.Marshal(s.proof)
		hold := func(n int64, err error) error {
			held++
			if held <= 40 {
				fmt.Fprintf(os.Stderr, "hub1: height %d held: %v\n", n, err)
			}
			if write {
				return err
			}
			return nil
		}
		visit := func(r replayrecords.Height) error {
			if write && destination == nil {
				d, err := openRehearsalDestination(ctx, os.Getenv("LEGACY_REPLAY_IMPORT_DSN"), target)
				if err != nil {
					return fmt.Errorf("rehearsal destination refused")
				}
				destination = d
			}
			s.read.part = nil
			if len(o.slices) != 0 {
				for at < len(o.slices) && r.Number > o.slices[at].last {
					at++
				}
				if at == len(o.slices) || r.Number < o.slices[at].first {
					return fmt.Errorf("height %d belongs to no listed part", r.Number)
				}
				s.read.part = o.slices[at]
			}
			h, err := capture.LoadHeight(filepath.Join(s.dir, "cache"), "cosmoshub-1", r.Number, maxBody)
			if err == nil {
				err = s.read.capture(s.dir, r.Number)
			}
			if err != nil {
				return fmt.Errorf("height %d: block capture: %w", r.Number, err)
			}
			if parent != "" && h.Previous != parent {
				return fmt.Errorf("height %d does not follow the block before it", r.Number)
			}
			parent = h.Hash
			items, err := core.DecodeLegacyBlock(h.Block, h.Results)
			if err != nil {
				return hold(r.Number, fmt.Errorf("block does not decode: %w", err))
			}
			b, err := core.LegacyReplayBlock(h, items, r)
			if err != nil {
				return hold(r.Number, err)
			}
			evidence := []replayrecords.Height{r}
			attributes := [][2]string{{"json", string(receipt)}}
			if export != nil && r.Number == hub1Export {
				if b, err = core.LegacyReplayExport(b, *export); err != nil {
					return hold(r.Number, err)
				}
				evidence = append(evidence, *export)
				exportReceipt, _ := json.Marshal(admittedExport)
				attributes = append(attributes, [2]string{"export_json", string(exportReceipt)})
			}
			// The rows must move exactly what the bank keeper was seen to move.
			want, err := core.LegacyReplayRecorded(evidence...)
			if err != nil {
				return hold(r.Number, err)
			}
			got, err := core.LegacyReplayLiquid(b)
			if err != nil {
				return hold(r.Number, err)
			}
			if !got.Equal(want) {
				return hold(r.Number, fmt.Errorf("rows move %d balances and the recording moved %d, or by other amounts", len(got), len(want)))
			}
			if len(want) != 0 {
				recorded[r.Number] = want
			}
			b.Events = append(b.Events, ledger.Event{Phase: ledger.PhaseBegin, Index: 2, Type: "pinned_replay_receipt", Attributes: attributes})
			if o.compare != nil {
				if n, err := ledger.Write(ctx, o.compare, []ledger.Block{b}); err != nil || n != 0 {
					return fmt.Errorf("height %d: stored rows are not the rows its sealed source gives", r.Number)
				}
			} else if destination != nil {
				if err = destination.write(ctx, b); err != nil {
					return fmt.Errorf("height %d: rehearsal write refused", r.Number)
				}
			}
			heights++
			txs += len(b.Inclusions)
			rows += len(b.Rows)
			s.read.Heights++
			if s.read.part != nil {
				s.read.part.heights++
				s.read.part.transactions += int64(len(b.Inclusions))
			}
			s.read.Transactions += int64(len(b.Inclusions))
			s.read.Rows += int64(len(b.Rows))
			for _, row := range b.Rows {
				if row.Source == "export_payout" {
					done.payoutRows++
				}
				categories[row.Category+"/"+row.Source]++
				s.read.RowsByCategory[row.Category+"/"+row.Source]++
			}
			return nil
		}
		r, err := s.open()
		if err != nil {
			return done, fail(err)
		}
		counted[i], err = replayrecords.ScanHub1(r, s.proof, visit)
		if closeErr := r.Close(); err == nil {
			err = closeErr
		}
		s.read.BlockCaptureListSHA256 = hex.EncodeToString(s.read.captures.Sum(nil))
		if err != nil {
			return done, fail(fmt.Errorf("source %d: %w", i+1, err))
		}
		if !write {
			admitted(i)
		}
	}
	names := make([]string, 0, len(categories))
	for name, n := range categories {
		names = append(names, fmt.Sprintf("%s=%d", name, n))
	}
	sort.Strings(names)
	fmt.Printf("hub1 converted heights=%d held=%d transactions=%d rows=%d heights_with_balance_change=%d export=%t written=%t\nhub1 rows by category and source: %s\n",
		heights, held, txs, rows, len(recorded), export != nil, write, strings.Join(names, " "))
	if held != 0 {
		return done, refused
	}
	if o.tail != nil {
		if last != hub1Export {
			return done, fail(errors.New("the tail follows only the export height"))
		}
		blocks, list, err := hub1TailBlocks(*o.tail, parent, maxBody)
		if err != nil {
			return done, fail(err)
		}
		done.tailList = list
		conn := o.compare
		if conn == nil && destination != nil {
			conn = destination.conn
		}
		if conn != nil {
			if n, err := ledger.Write(ctx, conn, blocks); err != nil || (o.compare != nil && n != 0) {
				return done, fail(errors.New("tail write refused, or the stored tail differs from its captures"))
			}
		}
		fmt.Printf("hub1 tail heights %d to %d: blocks=%d tax_rows=0\n", o.tail.First, o.tail.Last, len(blocks))
	}
	record := func() error {
		if manifest == "" {
			return nil
		}
		out := struct {
			Schema  string               `json:"schema"`
			Chain   string               `json:"chain"`
			Written bool                 `json:"written"`
			Target  string               `json:"rehearsal_db,omitempty"`
			Sources []*hub1Read          `json:"sources"`
			Export  *replayrecords.Proof `json:"export,omitempty"`
		}{Schema: "hub1-converter-read-manifest-v1", Chain: "cosmoshub-1", Written: write, Export: admittedExport}
		if write {
			out.Target = target
		}
		for _, s := range sources {
			out.Sources = append(out.Sources, s.read)
		}
		body, _ := json.MarshalIndent(out, "", "  ")
		f, err := os.OpenFile(manifest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("manifest file must be new")
		}
		if _, err = f.Write(append(body, '\n')); err == nil {
			err = f.Sync()
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		return err
	}
	if destination == nil {
		if err := record(); err != nil {
			return done, fail(err)
		}
		return done, nil
	}
	// Read the stored rows back and prove them, height by height, against
	// what was recorded. This is the check on the staging rows themselves.
	first := sources[0].proof.From
	var batch []int64
	stored, proven := 0, 0
	flush := func() error {
		blocks, err := ledger.Load(ctx, destination.conn, "cosmoshub-1", batch)
		if err != nil || len(blocks) != len(batch) {
			return errors.New("stored heights could not all be read back")
		}
		for _, b := range blocks {
			got, err := core.LegacyReplayLiquid(b)
			if err != nil {
				return err
			}
			want := recorded[b.Height]
			if want == nil {
				want = core.ReplayNet{}
			}
			if !got.Equal(want) {
				return fmt.Errorf("stored height %d moves other balances than the recording", b.Height)
			}
			stored++
			if len(want) != 0 {
				proven++
			}
		}
		batch = batch[:0]
		return nil
	}
	for n := first; n <= last; n++ {
		batch = append(batch, n)
		if len(batch) == 2000 || n == last {
			if err := flush(); err != nil {
				return done, fail(err)
			}
		}
	}
	fmt.Printf("hub1 stored rows proven against the recording: heights=%d heights_with_balance_change=%d\n", stored, proven)
	if proven != len(recorded) {
		return done, refused
	}
	if err := record(); err != nil {
		return done, fail(err)
	}
	return done, nil
}
