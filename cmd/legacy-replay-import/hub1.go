package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	return hub1Source{dir: dir, proof: replayrecords.Proof{Chain: "cosmoshub-1", Release: replayrecords.Release033, From: receipt.First, Through: receipt.Through,
		VerifiedThrough: receipt.Through, SHA256: m.RawSHA256, Records: m.Records, RawBytes: m.RawBytes}}, nil
}

type gzipFile struct {
	*gzip.Reader
	file *os.File
}

func (g gzipFile) Close() error { return g.file.Close() }

func (s hub1Source) open() (io.ReadCloser, error) {
	f, err := os.Open(filepath.Join(s.dir, "movements.source-closed.gz"))
	if err != nil {
		return nil, fmt.Errorf("Hub 1 movement stream unreadable")
	}
	z, err := gzip.NewReader(bufio.NewReaderSize(f, 4<<20))
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("Hub 1 movement stream is not gzip")
	}
	return gzipFile{z, f}, nil
}

// runHub1 converts closed Hub 1 recordings into legacy rows. Unlike the
// Hub 2 path it says why it stopped: the height, the record kind and the
// label, never an address or an amount.
func runHub1(dirs []string, exportRecords, exportProof, target string, write bool, maxBody int64) error {
	fail := func(err error) error {
		fmt.Fprintln(os.Stderr, "hub1:", err)
		return refused
	}
	var sources []hub1Source
	for _, dir := range dirs {
		s, err := hub1Admit(strings.TrimSpace(dir))
		if err != nil {
			return fail(err)
		}
		if len(sources) > 0 && s.proof.From != sources[len(sources)-1].proof.Through+1 {
			return fail(fmt.Errorf("Hub 1 sources are not consecutive ranges"))
		}
		if s.proof.Through > hub1Export {
			return fail(fmt.Errorf("Hub 1 source runs past the export height; later blocks are never tax rows"))
		}
		sources = append(sources, s)
	}
	last := sources[len(sources)-1].proof.Through
	if write && !hub1RehearsalName.MatchString(target) {
		return fail(fmt.Errorf("Hub 1 rows go only to a database named legacy_replay_rehearsal_hub1_SUFFIX"))
	}
	var export *replayrecords.Height
	var admittedExport *replayrecords.Proof
	if (exportRecords == "") != (exportProof == "") {
		return refused
	}
	if exportRecords != "" {
		var ep replayrecords.Proof
		if readJSON(exportProof, &ep, true) != nil || !ep.Export || last != hub1Export || ep.From != hub1Export || ep.Through != hub1Export {
			return fail(fmt.Errorf("Hub 1 export proof must be the zero-height export of the last source height %d", hub1Export))
		}
		_, err := replayrecords.StreamHub1(func() (io.ReadCloser, error) { return os.Open(exportRecords) }, ep, func(h replayrecords.Height) error { export = &h; return nil })
		if err != nil || export == nil {
			return fail(fmt.Errorf("Hub 1 export records: %w", err))
		}
		admittedExport = &ep
	}
	// Every stream is checked whole against its pin before anything is built.
	var wg sync.WaitGroup
	checked := make([]error, len(sources))
	counted := make([]replayrecords.Hub1Counts, len(sources))
	for i, s := range sources {
		wg.Add(1)
		go func(i int, s hub1Source) {
			defer wg.Done()
			r, err := s.open()
			if err != nil {
				checked[i] = err
				return
			}
			defer r.Close()
			counted[i], checked[i] = replayrecords.ScanHub1(r, s.proof, nil)
		}(i, s)
	}
	wg.Wait()
	for i, err := range checked {
		if err != nil {
			return fail(fmt.Errorf("source %d: %w", i+1, err))
		}
		kinds := make([]string, 0, len(counted[i].Kinds))
		for kind, n := range counted[i].Kinds {
			kinds = append(kinds, fmt.Sprintf("%s=%d", kind, n))
		}
		sort.Strings(kinds)
		fmt.Printf("hub1 source %d heights %d to %d admitted: records=%d %s\n", i+1, sources[i].proof.From, sources[i].proof.Through, counted[i].Records, strings.Join(kinds, " "))
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
			h, err := capture.LoadHeight(filepath.Join(s.dir, "cache"), "cosmoshub-1", r.Number, maxBody)
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
			if destination != nil {
				if err = destination.write(ctx, b); err != nil {
					return fmt.Errorf("height %d: rehearsal write refused", r.Number)
				}
			}
			heights++
			txs += len(b.Inclusions)
			rows += len(b.Rows)
			for _, row := range b.Rows {
				categories[row.Category+"/"+row.Source]++
			}
			return nil
		}
		r, err := s.open()
		if err != nil {
			return fail(err)
		}
		_, err = replayrecords.ScanHub1(r, s.proof, visit)
		r.Close()
		if err != nil {
			return fail(fmt.Errorf("source %d: %w", i+1, err))
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
		return refused
	}
	if destination == nil {
		return nil
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
				return fail(err)
			}
		}
	}
	fmt.Printf("hub1 stored rows proven against the recording: heights=%d heights_with_balance_change=%d\n", stored, proven)
	if proven != len(recorded) {
		return refused
	}
	return nil
}
