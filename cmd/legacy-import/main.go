// Command legacy-import stores one explicit range of Cosmos Hub 3 heights
// in the legacy schema. It reads retained block captures, decodes them with
// the legacy decoder, builds tax rows with the serving classifier and writes
// them through the legacy ledger. It makes no network call other than its
// database connection.
//
// It refuses to write through a connection that can reach anything outside
// the legacy schema, so it cannot touch a serving table even when pointed at
// the serving database. Output is counts only: no addresses, amounts, memos
// or hashes are printed.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/core"
	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	digits   = regexp.MustCompile(`\d+`)
	safeText = regexp.MustCompile(`^[A-Za-z0-9 _.,:/()'-]{1,160}$`)
)

func safe(err error) string {
	text := digits.ReplaceAllString(err.Error(), "N")
	if !safeText.MatchString(text) {
		return "error text withheld"
	}
	return text
}

func fail(text string) {
	fmt.Fprintln(os.Stderr, text)
	os.Exit(2)
}

type counter map[string]int64

func (c counter) print(title string) {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool {
		if c[keys[a]] != c[keys[b]] {
			return c[keys[a]] > c[keys[b]]
		}
		return keys[a] < keys[b]
	})
	fmt.Println(title + ":")
	for _, k := range keys {
		fmt.Printf("  %-70s %d\n", k, c[k])
	}
}

// outcome is what reading one height gave: a block to store, or why not.
type outcome struct {
	block    *ledger.Block
	previous string
	missing  bool
	err      error
	txs      int
	removals int
}

func open() (*sql.DB, error) {
	db, err := gorm.Open(postgres.Open(os.Getenv("LEGACY_IMPORT_DSN")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	findings, err := schema.Confined(db)
	if err != nil {
		return nil, fmt.Errorf("catalog not readable")
	}
	if len(findings) > 0 {
		for _, finding := range findings {
			fmt.Fprintln(os.Stderr, finding)
		}
		return nil, fmt.Errorf("refusing a connection that is not confined to the legacy schema")
	}
	return db.DB()
}

func main() {
	root := flag.String("cache", "/cache", "block cache root")
	chain := flag.String("chain", "cosmoshub-3", "legacy chain to import")
	from := flag.Int64("from", 0, "first height of this importer's range")
	to := flag.Int64("to", 0, "last height of this importer's range")
	maxBody := flag.Int64("max-body", 64<<20, "largest decompressed capture accepted")
	resolutions := flag.String("resolutions", "", "file written by legacy-seqcheck: chain/height/index and a fee verdict per line")
	removed := flag.String("removals", "", "file written by legacy-removals: chain/height/index/message and a removal verdict per line")
	workers := flag.Int("workers", 4, "heights decoded at once")
	batch := flag.Int64("batch", 500, "heights stored per database transaction")
	every := flag.Duration("progress", 30*time.Second, "time between progress lines")
	build := flag.String("build", os.Getenv("LEGACY_BUILD"), "build label recorded with the range")
	flag.Parse()
	verdicts, removals := map[string]string{}, map[string]string{}
	for path, into := range map[string]map[string]string{*resolutions: verdicts, *removed: removals} {
		if path == "" {
			continue
		}
		text, err := os.ReadFile(path)
		if err != nil {
			fail("verdict file unavailable")
		}
		for _, line := range strings.Split(strings.TrimSpace(string(text)), "\n") {
			key, verdict, ok := strings.Cut(line, " ")
			if !ok || !strings.HasPrefix(key, *chain+"/") || into[key] != "" {
				fail("a verdict file has a line for another chain, a repeated line or no verdict")
			}
			into[key] = verdict
		}
	}
	if *from <= 0 || *to < *from || *workers < 1 || *batch < 1 {
		fail("a height range, at least one worker and a batch size are required")
	}
	ctx := context.Background()
	db, err := open()
	if err != nil {
		fail(safe(err))
	}
	claim, progress, err := ledger.Begin(ctx, db, ledger.Range{Chain: *chain, From: *from, To: *to}, *build)
	if err != nil {
		fail(safe(err))
	}
	defer claim.Release()
	fmt.Printf("range chain=%s from=%d to=%d resume=%d workers=%d batch=%d\n", *chain, *from, *to, progress.Next, *workers, *batch)

	// The block before the first one read, when it is stored, anchors the
	// check that each block names the one before it as its parent.
	parent := ""
	if progress.Next > 1 {
		if before, err := ledger.Load(ctx, db, *chain, []int64{progress.Next - 1}); err == nil && len(before) == 1 {
			parent = before[0].Hash
		}
	}
	started, lastLine := time.Now(), time.Now()
	first := progress.Next
	heights, held, messages := counter{}, counter{}, counter{}
	categories := counter{}
	var readTxs, readHeights, added, storedRows, attached int64
	contiguous := true
	line := func() {
		seconds := time.Since(started).Seconds()
		fmt.Printf("progress chain=%s next=%d of=%d heights_read=%d stored=%d held=%d transactions=%d tax_rows=%d heights_per_second=%.1f transactions_per_second=%.1f\n",
			*chain, progress.Next, *to, readHeights, heights["stored"], progress.Held, readTxs, storedRows, float64(readHeights)/seconds, float64(readTxs)/seconds)
		lastLine = time.Now()
	}
	for start := progress.Next; start <= *to; start += *batch {
		end := start + *batch - 1
		if end > *to {
			end = *to
		}
		outcomes := make([]outcome, end-start+1)
		jobs := make(chan int64)
		var wait sync.WaitGroup
		for w := 0; w < *workers; w++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				for height := range jobs {
					outcomes[height-start] = read(*root, *chain, height, *maxBody, verdicts, removals)
				}
			}()
		}
		for height := start; height <= end; height++ {
			jobs <- height
		}
		close(jobs)
		wait.Wait()

		var blocks []ledger.Block
		for i := range outcomes {
			o := &outcomes[i]
			readHeights++
			attached += int64(o.removals)
			switch {
			case o.missing:
				heights["not in the cache"]++
				parent = ""
				continue
			case o.err == nil && parent != "" && o.previous != parent:
				o.err = fmt.Errorf("the block does not name the block before it as its parent")
			}
			if o.err != nil {
				heights["held"]++
				held[safe(o.err)]++
				parent = ""
				if o.block != nil {
					parent = o.block.Hash
				}
				o.block = nil
				continue
			}
			parent = o.block.Hash
			blocks = append(blocks, *o.block)
		}
		fresh, err := ledger.Write(ctx, db, blocks)
		if err != nil {
			progress.Status = "failed"
			_ = claim.Record(ctx, progress)
			if errors.Is(err, ledger.ErrStoredDiffers) {
				fail(safe(err))
			}
			fail("the database refused a run of heights: " + safe(err))
		}
		added += int64(fresh)
		for i := range outcomes {
			o := &outcomes[i]
			if o.block == nil {
				contiguous = false
				progress.Held++
				continue
			}
			heights["stored"]++
			readTxs += int64(o.txs)
			if o.txs > 0 {
				heights["stored with transactions"]++
			}
			storedRows += int64(len(o.block.Rows))
			for _, row := range o.block.Rows {
				categories[fmt.Sprintf("phase %d %s (%s)", row.Phase, row.Category, row.Source)]++
			}
			for _, event := range o.block.Events {
				messages[fmt.Sprintf("phase %d %s", event.Phase, event.Type)]++
			}
			if contiguous {
				progress.Next = start + int64(i) + 1
				progress.Stored++
				progress.Transactions += int64(o.txs)
				progress.Rows += int64(len(o.block.Rows))
			}
		}
		progress.Status = "running"
		if err := claim.Record(ctx, progress); err != nil {
			fail("progress could not be recorded: " + safe(err))
		}
		if time.Since(lastLine) >= *every {
			line()
		}
	}
	line()
	// A removal verdict that names no message of its height was not used: the
	// file does not belong to these captures. Heights an earlier run stored
	// are read again for their verdicts, so a rerun cannot pass a file the
	// run before it refused.
	expected, earlier := int64(0), map[int64]bool{}
	for key := range removals {
		parts := strings.Split(key, "/")
		if height, err := strconv.ParseInt(parts[min(1, len(parts)-1)], 10, 64); err != nil || len(parts) != 4 {
			fail("a removal verdict is not keyed by chain, height, transaction and message")
		} else if height >= *from && height <= *to {
			expected++
			if height < first {
				earlier[height] = true
			}
		}
	}
	for height := range earlier {
		attached += int64(read(*root, *chain, height, *maxBody, verdicts, removals).removals)
	}
	// Every stored message that could have removed a validator needs a
	// verdict. Without one its payout was read as a reward on the count of
	// payments alone, which a removal that paid only commission also fits.
	leaves, err := ledger.LastLeaves(ctx, db, ledger.Range{Chain: *chain, From: *from, To: *to})
	if err != nil {
		fail("the messages that could have removed a validator could not be listed: " + safe(err))
	}
	unproven := 0
	for _, key := range leaves {
		if _, ok := removals[key]; !ok {
			unproven++
		}
	}
	progress.Status = "failed"
	if progress.Next == *to+1 && progress.Held == 0 && attached == expected && unproven == 0 {
		progress.Status = "done"
	}
	if err := claim.Record(ctx, progress); err != nil {
		fail("the result could not be recorded: " + safe(err))
	}
	elapsed := time.Since(started)
	fmt.Printf("chain %s heights %d to %d, read from %d, in %s: range %s\n", *chain, *from, *to, first, elapsed.Round(time.Second), progress.Status)
	fmt.Printf("heights newly stored %d, already stored and equal %d\n", added, heights["stored"]-added)
	fmt.Printf("removal verdicts in the file %d, for this range %d, matched to a message %d\n", len(removals), expected, attached)
	fmt.Printf("stored messages that could have removed a validator %d, without a removal verdict %d\n", len(leaves), unproven)
	heights.print("heights")
	categories.print("tax rows by phase, category and amount source")
	messages.print("block events kept")
	held.print("held heights by reason")
	if progress.Status != "done" {
		os.Exit(1)
	}
}

func read(root, chain string, height, limit int64, verdicts, removals map[string]string) outcome {
	h, err := capture.LoadHeight(root, chain, height, limit)
	if errors.Is(err, os.ErrNotExist) {
		return outcome{missing: true}
	}
	if err != nil {
		return outcome{err: err}
	}
	out := outcome{previous: h.Previous, txs: len(h.Block.Block.Txs)}
	items, err := core.DecodeLegacyBlock(h.Block, h.Results)
	if err == nil {
		err = core.ResolveLegacyFees(items, verdicts)
	}
	if err == nil {
		out.removals, err = core.ResolveLegacyRemovals(items, removals)
	}
	var block ledger.Block
	if err == nil {
		block, err = core.LegacyLedgerBlock(h, items)
	}
	if err != nil {
		// The hash still anchors the parent check of the next height.
		out.block, out.err = &ledger.Block{Hash: h.Hash}, err
		return out
	}
	out.block = &block
	return out
}
