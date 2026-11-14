// Private terminal-only validation count. No historical replay or database access.
package main

import (
	"encoding/json"
	"fmt"
	"github.com/DefiantLabs/cosmos-indexer/core"
	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	"github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	"io"
	"os"
)

type input struct {
	Records, Cache, Export, SHA string
	Offset, Bytes               int64
	Proof                       replayrecords.Proof
}

func run() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("args")
	}
	raw, e := os.ReadFile(os.Args[1])
	if e != nil {
		return e
	}
	var in input
	if json.Unmarshal(raw, &in) != nil {
		return fmt.Errorf("input")
	}
	p := in.Proof
	p.Export = false
	p.From = p.Through
	p.SHA256 = in.SHA
	p.ResultDifferencesSHA256 = ""
	p.ResultDifferencesAdmission = nil
	f, e := os.Open(in.Records)
	if e != nil {
		return e
	}
	defer f.Close()
	var records replayrecords.Height
	if replayrecords.Stream(io.NewSectionReader(f, in.Offset, in.Bytes), p, func(h replayrecords.Height) error { records = h; return nil }) != nil {
		return fmt.Errorf("terminal")
	}
	ef, e := os.Open(in.Export)
	if e != nil {
		return e
	}
	defer ef.Close()
	var export replayrecords.Height
	if replayrecords.Stream(ef, in.Proof, func(h replayrecords.Height) error { export = h; return nil }) != nil {
		return fmt.Errorf("export")
	}
	h, e := capture.LoadHeight(in.Cache, "cosmoshub-2", p.Through, 64<<20)
	if e != nil {
		return e
	}
	items, e := core.DecodeLegacyBlock(h.Block, h.Results)
	if e != nil {
		return e
	}
	b, e := core.LegacyReplayBlock(h, items, records)
	if e != nil {
		return e
	}
	before := len(b.Rows)
	b, e = core.LegacyReplayExport(b, export)
	if e != nil {
		return e
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"terminal_historical_rows": before, "zeroheight_validation_only_rows": len(b.Rows) - before, "historical_income": false, "production_access": false})
}
func main() {
	if run() != nil {
		fmt.Fprintln(os.Stderr, "private terminal count refused")
		os.Exit(1)
	}
}
