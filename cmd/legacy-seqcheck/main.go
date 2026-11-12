// Command legacy-seqcheck compares the cached transactions of a Cosmos Hub 1
// to 3 chain with the account sequence numbers in the hub's genesis file and
// in the next hub's. It reads the cache and two genesis files, makes no
// network calls and writes no database. Output is aggregate counts only. The
// optional resolutions file lists chain, height and index with a fee verdict,
// and holds no address.
package main

import (
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/core"
	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	"github.com/DefiantLabs/cosmos-indexer/legacy/sequence"
)

var (
	digits   = regexp.MustCompile(`\d+`)
	safeText = regexp.MustCompile(`^[A-Za-z0-9 _.,:/()'-]{1,90}$`)
)

func safe(err error) string {
	text := digits.ReplaceAllString(err.Error(), "N")
	if !safeText.MatchString(text) {
		return "error text withheld"
	}
	return text
}

func readGenesis(path string) (*sequence.Genesis, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("genesis file unavailable")
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("genesis file is not gzip")
		}
		r = zr
	}
	return sequence.ReadGenesis(r)
}

type part struct {
	tally   *sequence.Tally
	heights map[string]int64
	faults  map[string]int64
}

func main() {
	root := flag.String("cache", "", "cache directory holding one folder per chain")
	chain := flag.String("chain", "", "cosmoshub-1, cosmoshub-2 or cosmoshub-3")
	from := flag.Int64("from", 1, "first height")
	to := flag.Int64("to", 0, "last height")
	openingPath := flag.String("opening", "", "genesis file of this chain")
	closingPath := flag.String("closing", "", "genesis file of the next chain")
	whole := flag.Bool("whole-hub", false, "the range is every height of the chain, so accounts must match exactly")
	out := flag.String("out", "", "write settled failures here as chain/height/index and a verdict")
	signatures := flag.Bool("signatures", false, "follow each account's sequence through the signatures, one transaction at a time")
	workers := flag.Int("workers", 4, "parallel readers")
	every := flag.Duration("progress", 30*time.Second, "time between progress lines of the signature pass")
	maxBody := flag.Int64("max-body", 64<<20, "largest decompressed capture accepted")
	flag.Parse()
	if *to < *from || *from <= 0 || *workers <= 0 {
		fmt.Fprintln(os.Stderr, "a height range is required")
		os.Exit(2)
	}
	opening, err := readGenesis(*openingPath)
	if err == nil && opening.Chain != *chain {
		err = fmt.Errorf("opening genesis is not for this chain")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, safe(err))
		os.Exit(2)
	}
	closing, err := readGenesis(*closingPath)
	if err == nil && closing.Chain == opening.Chain {
		err = fmt.Errorf("closing genesis is for the same chain")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, safe(err))
		os.Exit(2)
	}

	show := func(title string, counts map[string]int64) {
		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Println(title + ":")
		for _, k := range keys {
			fmt.Printf("  %-66s %d\n", k, counts[k])
		}
	}
	if *signatures {
		ledger, heights, faults, unchecked := followSignatures(*root, *chain, *from, *to, *maxBody, *workers, *every, opening, closing)
		complete := heights["read"] == *to-*from+1
		report := ledger.Close(*whole && complete)
		fmt.Printf("chain %s heights %d to %d, signatures followed in chain order\n", *chain, *from, *to)
		show("heights", heights)
		show("heights left out by reason", faults)
		show("signatures", ledger.Signatures)
		show("signatures not checkable by reason", unchecked)
		show("signatures checked and not verified by kind", ledger.Unverified)
		show("failures whose fee the block results cannot prove", map[string]int64{
			"settled as charged":                             int64(report.SettledCharged),
			"settled as not charged":                         int64(report.SettledNotCharged),
			"still unsettled":                                int64(report.Unsettled),
			"settled differently by two signers (must be 0)": int64(report.Conflicts),
		})
		bad := report.Impossible + report.Conflicts
		if *whole && complete {
			show("accounts", map[string]int64{
				"in either genesis file or the range":                             int64(report.Accounts),
				"exact: ends on its closing sequence":                             int64(report.Exact),
				"mismatched: ends on another sequence (must be 0)":                int64(report.Mismatched),
				"not comparable: a fee paying signature did not verify":           int64(report.Loose),
				"impossible: signed sequences contradict fee charges (must be 0)": int64(report.Impossible),
			})
			bad += report.Mismatched
		} else {
			show("accounts", map[string]int64{"impossible: signed sequences contradict fee charges (must be 0)": int64(report.Impossible)})
			fmt.Println("end of hub comparison not run: it needs --whole-hub and every height of the chain read")
		}
		// A failure settled between two verified sequences is proven by the
		// signatures alone, so the file is written for a partial range too.
		if *out != "" {
			var b strings.Builder
			for _, key := range report.SortedResolutions() {
				b.WriteString(key + " " + report.Resolutions[key] + "\n")
			}
			if err := os.WriteFile(*out, []byte(b.String()), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, "resolutions file not written")
				os.Exit(2)
			}
		}
		if bad > 0 {
			os.Exit(1)
		}
		return
	}

	jobs := make(chan int64, 1024)
	parts := make([]*part, *workers)
	var wg sync.WaitGroup
	for w := range parts {
		p := &part{tally: sequence.NewTally(), heights: map[string]int64{}, faults: map[string]int64{}}
		parts[w] = p
		wg.Add(1)
		go func() {
			defer wg.Done()
			for height := range jobs {
				block, results, err := capture.Load(*root, *chain, height, *maxBody)
				if errors.Is(err, os.ErrNotExist) {
					p.heights["not in the cache"]++
					continue
				}
				if err != nil {
					p.heights["unreadable"]++
					p.faults[safe(err)]++
					continue
				}
				items, err := core.DecodeLegacyBlock(block, results)
				if err != nil {
					p.heights["not decoded"]++
					p.faults[safe(err)]++
					continue
				}
				// A height is counted whole or not at all.
				local := sequence.NewTally()
				for _, item := range items {
					if !item.Tx.SignersComplete {
						err = fmt.Errorf("transaction signers incomplete")
						break
					}
					signers := make([]string, 0, len(item.Tx.Signers))
					for _, signer := range item.Tx.Signers {
						signers = append(signers, signer.String())
					}
					if err = local.Add(item.Identity, signers, item.Fee.Charge); err != nil {
						break
					}
				}
				if err != nil {
					p.heights["not counted"]++
					p.faults[safe(err)]++
					continue
				}
				p.tally.Merge(local)
				p.heights["read"]++
			}
		}()
	}
	for height := *from; height <= *to; height++ {
		jobs <- height
	}
	close(jobs)
	wg.Wait()

	tally := sequence.NewTally()
	heights := map[string]int64{}
	faults := map[string]int64{}
	for _, p := range parts {
		tally.Merge(p.tally)
		for k, v := range p.heights {
			heights[k] += v
		}
		for k, v := range p.faults {
			faults[k] += v
		}
	}
	fmt.Printf("chain %s heights %d to %d, opening %s (%d accounts, %d genesis transactions), closing %s (%d accounts)\n",
		*chain, *from, *to, opening.Chain, len(opening.Sequence), len(opening.GenesisSigned), closing.Chain, len(closing.Sequence))
	show("heights", heights)
	show("heights left out by reason", faults)
	transactions := map[string]int64{}
	for charge, n := range tally.Transactions {
		transactions["fee "+charge] = int64(n)
	}
	show("transactions counted", transactions)

	checked, ahead := sequence.Ahead(opening, closing, tally)
	show("accounts with a fee paying transaction in the range", map[string]int64{
		"checked": int64(checked),
		"counted past their closing sequence (must be 0)": int64(ahead),
	})
	complete := heights["read"] == *to-*from+1
	if !*whole || !complete {
		fmt.Println("exact comparison not run: it needs --whole-hub and every height of the chain read and counted")
		if ahead > 0 {
			os.Exit(1)
		}
		return
	}
	report := sequence.Reconcile(opening, closing, tally)
	show("accounts", map[string]int64{
		"in either genesis file or the range":                                int64(report.Accounts),
		"exact: no unproven failure, sequence matches":                       int64(report.Exact),
		"mismatched: no unproven failure, sequence differs (must be 0)":      int64(report.Mismatched),
		"settled: unproven failures all paid or all did not":                 int64(report.Settled),
		"partial: some unproven failures paid":                               int64(report.Partial),
		"impossible: moved outside proven and unproven together (must be 0)": int64(report.Impossible),
		"paid a fee yet absent from the closing file (must be 0)":            int64(report.ChargedAbsent),
	})
	show("unproven failures", map[string]int64{
		"settled as charged":     int64(report.SettledCharged),
		"settled as not charged": int64(report.SettledNotCharged),
		"still unsettled":        int64(report.Unsettled),
	})
	if *out != "" {
		var b strings.Builder
		for _, key := range report.SortedResolutions() {
			b.WriteString(key + " " + report.Resolutions[key] + "\n")
		}
		if err := os.WriteFile(*out, []byte(b.String()), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "resolutions file not written")
			os.Exit(2)
		}
	}
	if report.Mismatched > 0 || report.Impossible > 0 || report.ChargedAbsent > 0 {
		os.Exit(1)
	}
}
