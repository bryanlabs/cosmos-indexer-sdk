// Command legacy-replay-import is offline Hub2 rehearsal only. It never
// migrates a database, contacts a chain, or writes the serving schema.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/DefiantLabs/cosmos-indexer/core"
	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	"github.com/jackc/pgx/v5/pgconn"
)

var rehearsalName = regexp.MustCompile(`^legacy_replay_rehearsal_[a-z0-9_]{8,48}$`)
var refused = fmt.Errorf("rehearsal input or destination refused; private details withheld")

type replayStatus struct {
	Release   string `json:"release"`
	Chain     string `json:"chain"`
	From      int64  `json:"first_height_this_run"`
	Committed int64  `json:"last_height_committed"`
	Verified  int64  `json:"state_hash_verified_through"`
	Diffs     int64  `json:"heights_where_results_differ_from_cache"`
}

func readJSON(path string, v any, strict bool) error {
	f, e := os.Open(path)
	if e != nil {
		return refused
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	if strict {
		d.DisallowUnknownFields()
	}
	if d.Decode(v) != nil {
		return refused
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return refused
	}
	return nil
}
func verifyStatus(p replayrecords.Proof, s replayStatus) error {
	if s.Release != replayrecords.Release034 || s.Chain != "cosmoshub-2" || s.From < 1 || s.From > p.From || s.Committed < p.Through || s.Verified < p.Through || s.Verified != p.VerifiedThrough || s.Verified > s.Committed || s.Diffs < 0 || s.Diffs > s.Committed-s.From+1 {
		return refused
	}
	a := p.ResultDifferencesAdmission
	if a == nil {
		if s.Diffs != 0 || p.ResultDifferencesSHA256 != "" {
			return refused
		}
	} else if a.RawSHA256 != p.ResultDifferencesSHA256 || a.From != s.From || a.Committed != s.Committed || a.StatusDiffs != s.Diffs || a.RunLines != s.Diffs {
		return refused
	}
	return nil
}

// Admission always rereads all raw differences, including when a supplied proof
// already carries a classification receipt. Nothing rewrites the runner status.
func admitStatus(p *replayrecords.Proof, s replayStatus, path string) error {
	if path == "" {
		if p.ResultDifferencesAdmission != nil {
			return refused
		}
		return verifyStatus(*p, s)
	}
	f, err := os.Open(path)
	if err != nil {
		return refused
	}
	defer f.Close()
	a, err := replayrecords.AdmitTagDifferences(f, p.ResultDifferencesSHA256, s.From, s.Committed, s.Diffs)
	if err != nil {
		return refused
	}
	if p.ResultDifferencesAdmission != nil && *p.ResultDifferencesAdmission != a {
		return refused
	}
	p.ResultDifferencesAdmission = &a
	return verifyStatus(*p, s)
}
func guardDSN(dsn, name string) error {
	if strings.TrimSpace(dsn) == "" || !rehearsalName.MatchString(name) {
		return refused
	}
	c, e := pgconn.ParseConfig(dsn)
	if e != nil || c.Database != name || c.Port == 15432 || c.Database == "postgres" {
		return refused
	}
	for _, f := range c.Fallbacks {
		if f.Port == 15432 {
			return refused
		}
	}
	return nil
}
func run() error {
	root := flag.String("cache", "", "retained block captures")
	records := flag.String("records", "", "immutable private recorder JSONL")
	proof := flag.String("proof", "", "exact record digest and pinned release receipt")
	status := flag.String("status", "", "retained replay run status with next-header verification")
	differences := flag.String("result-differences", "", "original retained result-differences.txt, SHA256 pinned in proof")
	exportRecords := flag.String("export-records", "", "optional actual zero-height export JSONL")
	exportProof := flag.String("export-proof", "", "optional export digest receipt")
	target := flag.String("rehearsal-db", "", "new empty database named legacy_replay_rehearsal_SUFFIX")
	write := flag.Bool("write", false, "write rehearsal instead of validating only")
	maxBody := flag.Int64("max-body", 64<<20, "largest decompressed capture")
	flag.Parse()
	if *root == "" || *records == "" || *proof == "" || *status == "" || *maxBody < 1 {
		return refused
	}
	var p replayrecords.Proof
	var s replayStatus
	if readJSON(*proof, &p, true) != nil || p.Export || readJSON(*status, &s, false) != nil || admitStatus(&p, s, *differences) != nil {
		return refused
	}
	f, e := os.Open(*records)
	if e != nil {
		return refused
	}
	defer f.Close()
	var export *replayrecords.Height
	var admittedExportProof *replayrecords.Proof
	if (*exportRecords == "") != (*exportProof == "") {
		return refused
	}
	if *exportRecords != "" {
		var ep replayrecords.Proof
		if readJSON(*exportProof, &ep, true) != nil || !ep.Export || ep.From != p.Through || ep.Through != p.Through || admitStatus(&ep, s, *differences) != nil {
			return refused
		}
		admittedExportProof = &ep
		ef, e := os.Open(*exportRecords)
		if e != nil {
			return refused
		}
		defer ef.Close()
		e = replayrecords.Stream(ef, ep, func(h replayrecords.Height) error { export = &h; return nil })
		if e != nil {
			return refused
		}
	}
	ctx := context.Background()
	var destination *rehearsalDestination
	// Stream validates the complete digest before its first callback, so this
	// destination opener cannot run until every evidence height has closed.
	defer func() {
		if destination != nil {
			destination.close()
		}
	}()
	heights, txs, rows, held := 0, 0, 0, 0
	receipt, _ := json.Marshal(p)
	parent := ""
	e = replayrecords.Stream(f, p, func(r replayrecords.Height) error {
		if *write && destination == nil {
			destination, e = openRehearsalDestination(ctx, os.Getenv("LEGACY_REPLAY_IMPORT_DSN"), *target)
			if e != nil {
				return refused
			}
		}
		h, err := capture.LoadHeight(*root, "cosmoshub-2", r.Number, *maxBody)
		if err != nil {
			return refused
		}
		if parent != "" && h.Previous != parent {
			return refused
		}
		parent = h.Hash
		items, err := core.DecodeLegacyBlock(h.Block, h.Results)
		if err != nil {
			held++
			return nil
		}
		b, err := core.LegacyReplayBlock(h, items, r)
		if err != nil {
			held++
			return nil
		}
		if export != nil && r.Number == p.Through {
			b, err = core.LegacyReplayExport(b, *export)
			if err != nil {
				held++
				return nil
			}
		}
		attributes := [][2]string{{"json", string(receipt)}}
		if admittedExportProof != nil && r.Number == p.Through {
			exportReceipt, _ := json.Marshal(admittedExportProof)
			attributes = append(attributes, [2]string{"export_json", string(exportReceipt)})
		}
		b.Events = append(b.Events, ledger.Event{Phase: ledger.PhaseBegin, Index: 2, Type: "pinned_replay_receipt", Attributes: attributes})
		if destination != nil {
			if err = destination.write(ctx, b); err != nil {
				return refused
			}
		}
		parent = h.Hash
		heights++
		txs += len(b.Inclusions)
		rows += len(b.Rows)
		return nil
	})
	if e != nil {
		return refused
	}
	fmt.Printf("rehearsal validated heights=%d held=%d transactions=%d rows=%d written=%t\n", heights, held, txs, rows, *write)
	if held != 0 {
		return refused
	}
	return nil
}
func main() {
	if run() != nil {
		fmt.Fprintln(os.Stderr, refused)
		os.Exit(1)
	}
}
