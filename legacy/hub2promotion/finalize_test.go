package hub2promotion

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// wholeChain builds a complete, tiny, self-consistent set of sealed parts,
// staging rows, restart payout and boundary files. Every value is synthetic.
type wholeChain struct {
	t         *testing.T
	dir       string
	b         finalBounds
	spec      FinalizeSpec
	parts     [][2]int64
	accounts  []any
	payouts   []exportRecord
	taxRows   []map[string]any
	blocks    int64
	states    map[int64]string
	genesis   *Pin
	export    *Pin
	next      *Pin
	canonical *Pin
	runs      int
	receipts  int
}

func (w *wholeChain) state(h int64) string {
	if s, ok := w.states[h]; ok {
		return s
	}
	return stateAt(h)
}
func (w *wholeChain) stateBase64(h int64) string {
	raw, _ := hex.DecodeString(w.state(h))
	return base64.StdEncoding.EncodeToString(raw)
}

func stateAt(h int64) string { return digest([]byte(fmt.Sprintf("synthetic-state-%d", h))) }
func stateBase64(h int64) string {
	raw, _ := hex.DecodeString(stateAt(h))
	return base64.StdEncoding.EncodeToString(raw)
}
func pick(real *Pin, synthetic func() Pin) Pin {
	if real != nil {
		return *real
	}
	return synthetic()
}
func (w *wholeChain) save(name string, v any) Pin {
	w.t.Helper()
	b, e := json.Marshal(v)
	require.NoError(w.t, e)
	return savePin(w.t, w.dir, name, b)
}
func (w *wholeChain) proof(name string, h int64) *Pin {
	p := w.save(name, map[string]any{"kind": "checkpoint_next_header_only", "chain": w.b.chain, "checkpoint_height": h, "state_hash_verified_through": h, "next_header_height": h + 1, "application_hash": w.state(h)})
	return &p
}
func account(address, amount string) map[string]any {
	return map[string]any{"address": address, "coins": []map[string]string{{"denom": "uatom", "amount": amount}}}
}
func syntheticRow(t table, chain string, set map[string]any) map[string]any {
	row := map[string]any{}
	for _, c := range projectionColumns(t) {
		row[c] = "synthetic"
	}
	row["chain_id"] = chain
	for k, v := range set {
		row[k] = v
	}
	return row
}
func newWholeChain(t *testing.T, chain string) *wholeChain {
	t.Helper()
	w := &wholeChain{t: t, dir: t.TempDir(), blocks: 8}
	w.b = finalBounds{chain: chain, next: "cosmoshub-3", canonical: 6, last: 8}
	if chain == "cosmoshub-1" {
		w.b.next = "cosmoshub-2"
	}
	w.parts = [][2]int64{{1, 2}, {3, 4}, {5, 6}, {7, 8}}
	w.accounts = []any{account("synthetic-a", "1000"), account("synthetic-b", "7")}
	record := func(kind, label, validator, delegator, amount string) exportRecord {
		r := exportRecord{Validator: validator, Delegator: delegator, Kind: kind, Height: 6, Tx: -1, Message: -1, Phase: "zero_height_export", Label: label, Status: "exported"}
		if kind == "movement" {
			r.Address, r.Denom, r.Amount = "synthetic-paid", "uatom", amount
		}
		return r
	}
	w.payouts = []exportRecord{record("accounting", "delegator_reward", "synthetic-val", "synthetic-a", ""), record("movement", "delegator_reward/bank.add", "synthetic-val", "synthetic-a", "5"), record("movement", "delegator_reward/bank.add", "synthetic-val", "synthetic-b", "5"), record("movement", "validator_commission/bank.add", "synthetic-val", "", "3"), record("export_completed", "", "", "", "")}
	w.taxRows = []map[string]any{
		{"height": 2, "phase": 2, "tx_index": 0, "msg_index": 0, "sub_index": 0, "category": "transfer", "amount": "9", "amount_source": "message"},
		{"height": 6, "phase": 4, "tx_index": 0, "msg_index": 0, "sub_index": 0, "category": "reward", "amount": "5", "denom": "uatom", "to_addr": "synthetic-a", "validator_address": "synthetic-val", "amount_source": "export_payout"},
		{"height": 6, "phase": 4, "tx_index": 0, "msg_index": 0, "sub_index": 1, "category": "reward", "amount": "5", "denom": "uatom", "to_addr": "synthetic-b", "validator_address": "synthetic-val", "amount_source": "export_payout"},
		{"height": 6, "phase": 4, "tx_index": 0, "msg_index": 0, "sub_index": 2, "category": "commission", "amount": "3", "denom": "uatom", "to_addr": "synthetic-operator", "validator_address": "synthetic-val", "amount_source": "export_payout"},
	}
	return w
}

// build writes every file and the input list. Tests change the builder's
// fields first, or the written files and pins afterwards.
func (w *wholeChain) build() {
	t, b := w.t, w.b
	t.Helper()
	genesis := pick(w.genesis, func() Pin {
		return w.save("genesis.json", map[string]any{"chain_id": b.chain, "app_state": map[string]any{"accounts": []any{account("synthetic-a", "1")}}})
	})
	export := pick(w.export, func() Pin {
		return w.save("export.json", map[string]any{"source_height": b.canonical, "for_zero_height": true, "source_app_hash": stateAt(b.canonical), "app_state": map[string]any{"accounts": w.accounts}})
	})
	next := pick(w.next, func() Pin {
		return w.save("next-genesis.json", map[string]any{"chain_id": b.next, "app_state": map[string]any{"accounts": []any{account("synthetic-b", "7"), account("synthetic-a", "1000")}}})
	})
	if !b.approved {
		w.b.genesisSHA, w.b.nextGenesisSHA = genesis.SHA256, next.SHA256
	}
	var lines strings.Builder
	for _, r := range w.payouts {
		line, e := json.Marshal(r)
		require.NoError(t, e)
		lines.Write(line)
		lines.WriteString("\n")
	}
	records := savePin(t, w.dir, "export-records.ndjson", []byte(lines.String()))
	payoutProof := w.save("export-proof.json", map[string]any{"chain": b.chain, "from": b.canonical, "through": b.canonical, "state_hash_verified_through": b.canonical, "zero_height_export": true, "records_sha256": records.SHA256})
	w.spec = FinalizeSpec{Chain: b.chain, Genesis: genesis, CanonicalExport: export, PayoutRecords: records, PayoutProof: payoutProof, NextGenesis: next, BuildCommit: strings.Repeat("ab", 20), LoaderBinary: savePin(t, w.dir, "loader", []byte("synthetic loader"))}
	var seals []any
	prior := ""
	for n, part := range w.parts {
		first, last := part[0], part[1]
		name := fmt.Sprintf("part-%d", n+1)
		require.NoError(t, os.MkdirAll(filepath.Join(w.dir, name), 0700))
		in := ComponentInput{}
		tail := first > b.canonical
		sealsExport := map[string]any{}
		if last == b.canonical {
			sealsExport = map[string]any{"export.json": export.SHA256, "export-records.ndjson": records.SHA256}
		}
		if b.chain == "cosmoshub-1" {
			if tail {
				in.Profile = "hub1-tail-v1"
				in.Acceptance = w.save(name+"/seal.json", map[string]any{"complete_scope": []int64{first, last}, "canonical_migration_height": b.canonical, "terminal_next_header_proof": false, "prior500000_export_or_payout_reexecuted": false})
			} else {
				opening := map[string]any{"Version": first - 1, "Hash": w.stateBase64(first - 1)}
				if first == 1 {
					opening["Hash"] = nil
				}
				result := w.save(name+"/result.json", map[string]any{"schema": "native033-split-range-financial-v1", "first": first, "through": last, "heights": last - first + 1, "opening_commit": opening, "ending_commit": map[string]any{"Version": last, "Hash": w.stateBase64(last)}})
				in.Profile, in.Result = "hub1-part-v1", &result
				in.Acceptance = w.save(name+"/seal.json", map[string]any{"schema": "native033-complete-profile-seal-v1", "source_preserved_after_proof": true, "actual_negative_controls_passed": true, "financial_admission": false, "result_sha256": result.SHA256, "artifact_pins": sealsExport})
				seals = append(seals, map[string]any{"path": in.Acceptance.Path, "sha256": in.Acceptance.SHA256})
			}
		} else {
			receipt := map[string]any{"first": first, "last": last, "held": 0, "validated_heights": last - first + 1}
			switch {
			case first == 1:
				in.Profile, in.Closing = "hub2-genesis-prefix-v1", w.proof(name+"/closing.json", last)
				receipt["component_accepted"], receipt["normal_terminal_height"], receipt["normal_native_root"] = true, last, w.state(last)
				receipt["native_genesis_opening"] = map[string]any{"full_native_all10modules_exact_equal_official_genesis": true, "header1_application_hash_genuinely_empty": true, "opening_from_zeroheight_export": false}
				receipt["synthetic_zeroheight_payouts_counted_as_income_or_opening"] = false
				receipt["source_closed_checkpoint_and_reference_and_cache_before_after_hashes_equal"] = true
			case n == 1:
				in.Profile, in.Opening, in.Closing = "hub2-range-result-v1", w.proof(name+"/opening.json", first-1), w.proof(name+"/closing.json", last)
				receipt["component_validated"], receipt["exit_code"], receipt["all_source_pre_post_and_local_member_hashes_equal"] = true, 0, true
			default:
				in.Profile, in.Opening = "hub2-continuation-v1", w.proof(name+"/opening.json", first-1)
				receipt["component_accepted"], receipt["original_source_and_jobs_preserved"] = true, true
				if prior != "" {
					receipt["prior_acceptance_sha256"] = prior
				}
				if tail {
					in.Profile = "hub2-tail-v1"
					receipt["schema"], receipt["complete_scope"], receipt["canonical_migration_height"] = "hub2-historical-tail-seal-v1", []int64{first, last}, b.canonical
					receipt["terminal_next_header_proof"], receipt["rehearsal"] = false, false
					receipt["opening_state_proof"] = map[string]any{"path": in.Opening.Path, "sha256": in.Opening.SHA256}
				} else {
					in.Closing = w.proof(name+"/closing.json", last)
					sealsExport[in.Closing.Path] = in.Closing.SHA256
				}
				receipt["mandatory_artifact_sha256"] = sealsExport
			}
			in.Acceptance = w.save(name+"/acceptance.json", receipt)
			prior = ""
			if in.Profile == "hub2-continuation-v1" {
				prior = in.Acceptance.SHA256
			}
		}
		w.spec.Components = append(w.spec.Components, in)
	}
	if b.chain == "cosmoshub-1" {
		tail := w.spec.Components[len(w.spec.Components)-1].Acceptance
		join := w.save("join.json", map[string]any{"schema": "native033-whole-source-financial-reconciliation-v1", "first": 1, "through": b.last, "canonical_migration_height": b.canonical, "all_historical_sources_and_coinflows_reconciled": true, "books_tax_acceptance": false, "profile_seals": seals, "accepted_terminal_seal_sha256": tail.SHA256})
		w.spec.Join, w.spec.CanonicalProof = &join, w.canonical
		if w.canonical == nil {
			w.spec.CanonicalProof = w.proof("canonical-proof.json", b.canonical)
		}
	}
	w.writeProjection(genesis.SHA256)
}
func (w *wholeChain) writeProjection(genesisSHA string) {
	t := w.t
	t.Helper()
	index := ProjectionIndex{Schema: ProjectionIndexSchema, Chain: w.b.chain, Staging: Identity{Database: "legacy_replay_rehearsal_synthetic", OID: 1, SystemHash: digest([]byte("staging"))}}
	for _, tb := range inserts {
		var rows []map[string]any
		switch tb.name {
		case "genesis_files":
			rows = append(rows, syntheticRow(tb, w.b.chain, map[string]any{"sha256": genesisSHA}))
		case "blocks":
			for h := int64(1); h <= w.blocks; h++ {
				rows = append(rows, syntheticRow(tb, w.b.chain, map[string]any{"height": h}))
			}
		case "taxable_events":
			for _, set := range w.taxRows {
				rows = append(rows, syntheticRow(tb, w.b.chain, set))
			}
		}
		var out strings.Builder
		for _, r := range rows {
			line, e := json.Marshal(r)
			require.NoError(t, e)
			out.Write(line)
			out.WriteString("\n")
		}
		index.Tables = append(index.Tables, Projection{Table: tb.name, Columns: projectionColumns(tb), Rows: int64(len(rows)), File: savePin(t, w.dir, tb.name+".jsonl", []byte(out.String()))})
	}
	w.spec.Projection = w.save("projection-index.json", index)
	w.receipts++
	receipt := stagingReceiptFor(t, w.dir, fmt.Sprintf("staging-receipt-%d.json", w.receipts), w.spec, w.b)
	w.spec.StagingReceipt = &receipt
}

// stagingReceiptFor writes the receipt a staging importer would give for the
// listed parts and the given projection. It hashes the files with its own
// grouping by height, independent of the finalizer's.
func stagingReceiptFor(t *testing.T, dir, name string, spec FinalizeSpec, b finalBounds) Pin {
	t.Helper()
	parts, e := verifyComponents(spec, b)
	require.NoError(t, e)
	var index ProjectionIndex
	require.NoError(t, readPinnedJSON(spec.Projection, &index))
	type sum struct {
		n int64
		h hash.Hash
	}
	pair := func(s *sum) map[string]any {
		return map[string]any{"rows": s.n, "sha256": hex.EncodeToString(s.h.Sum(nil))}
	}
	add := func(s *sum, line []byte) { s.n++; s.h.Write(line); s.h.Write([]byte("\n")) }
	payout, projection, genesis := &sum{h: sha256.New()}, map[string]any{}, map[string]any{}
	perPart := make([]map[string]*sum, len(parts.Components))
	for n := range perPart {
		perPart[n] = map[string]*sum{}
		for _, table := range splitTables {
			perPart[n][table] = &sum{h: sha256.New()}
		}
	}
	for _, p := range index.Tables {
		projection[p.Table] = map[string]any{"rows": p.Rows, "sha256": p.File.SHA256}
		if strings.HasPrefix(p.Table, "genesis_") {
			genesis[p.Table] = projection[p.Table]
			continue
		}
		f, e := os.Open(p.File.Path)
		require.NoError(t, e)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			var row struct {
				Height int64 `json:"height"`
				Phase  int   `json:"phase"`
			}
			require.NoError(t, json.Unmarshal(sc.Bytes(), &row))
			if p.Table == "taxable_events" && row.Phase == 4 {
				add(payout, sc.Bytes())
				continue
			}
			for n, c := range parts.Components {
				if row.Height >= c.First && row.Height <= c.Last {
					add(perPart[n][p.Table], sc.Bytes())
				}
			}
		}
		require.NoError(t, sc.Err())
		require.NoError(t, f.Close())
	}
	receipt := map[string]any{"schema": stagingReceiptSchemas[spec.Chain], "chain": spec.Chain, "first": 1, "last": b.last, "canonical_through": b.canonical, "canonical_height": b.canonical, "complete_chain": true,
		"genesis": map[string]any{"sha256": spec.Genesis.SHA256, "stored_rows_equal_genesis_file": true, "tables": genesis}, "projection_tables": projection, "rows_bound_to_no_listed_part": 0,
		"fingerprint_all_tables_sha256": digest([]byte("synthetic whole-database fingerprint")), "historical_tail": nil,
		"restart_payout": map[string]any{"height": b.canonical, "export_records_sha256": spec.PayoutRecords.SHA256, "stored_rows_equal_rows_rederived_from_export": true, "taxable_events_phase4": pair(payout), "every_row_tx_index_0_msg_index_0": true}}
	var components []any
	for n, c := range parts.Components {
		tables := map[string]any{}
		for _, table := range splitTables {
			tables[table] = pair(perPart[n][table])
		}
		if c.Tail {
			delete(tables, "taxable_events")
			receipt["historical_tail"] = map[string]any{"first": c.First, "last": c.Last, "tail_seal_path": c.Acceptance.Path, "tail_seal_sha256": c.Acceptance.SHA256, "stored_rows_equal_rows_rederived_from_captures": true, "tax_rows": 0, "tables": tables}
			continue
		}
		components = append(components, map[string]any{"first": c.First, "last": c.Last, "seal_path": c.Acceptance.Path, "seal_sha256": c.Acceptance.SHA256, "stored_rows_equal_rows_rederived_from_sealed_source": true, "tables": tables})
	}
	receipt["components"] = components
	raw, e := json.MarshalIndent(receipt, "", " ")
	require.NoError(t, e)
	return savePin(t, dir, name, append(raw, '\n'))
}
func (w *wholeChain) finalize() (FinalReport, Pin, error) {
	w.runs++
	return finalize(context.Background(), w.spec, filepath.Join(w.dir, fmt.Sprintf("final-%d", w.runs)), w.b)
}

// refused asserts a refusal that names the failing input, leaves no report
// behind, and never quotes an account or an amount.
func (w *wholeChain) refused(input, reason string) {
	w.t.Helper()
	r, pin, e := w.finalize()
	require.Error(w.t, e)
	require.True(w.t, IsRefusal(e), e.Error())
	require.Contains(w.t, e.Error(), input)
	require.Contains(w.t, e.Error(), reason)
	require.NotContains(w.t, e.Error(), "synthetic-")
	require.False(w.t, r.Accepted)
	require.Empty(w.t, pin.Path)
	_, e = os.Lstat(filepath.Join(w.dir, fmt.Sprintf("final-%d", w.runs), "finalization-report.json"))
	require.Error(w.t, e, "a refused finalization must not leave a report")
}
func (w *wholeChain) rewrite(p *Pin, change func(map[string]any)) {
	w.t.Helper()
	b, e := os.ReadFile(p.Path)
	require.NoError(w.t, e)
	var v map[string]any
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	require.NoError(w.t, d.Decode(&v))
	change(v)
	out, e := json.Marshal(v)
	require.NoError(w.t, e)
	require.NoError(w.t, os.WriteFile(p.Path, out, 0600))
	sum := sha256.Sum256(out)
	p.SHA256 = hex.EncodeToString(sum[:])
}

func TestWholeChainFinalizeJoinsSealedParts(t *testing.T) {
	for _, chain := range []string{"cosmoshub-1", "cosmoshub-2"} {
		t.Run(chain, func(t *testing.T) {
			w := newWholeChain(t, chain)
			w.build()
			r, pin, e := w.finalize()
			require.NoError(t, e)
			// Reduced boundaries prove the checks. They never accept a chain.
			require.False(t, r.Accepted)
			require.True(t, r.FixtureChecksPassed)
			require.Equal(t, int64(3), r.CanonicalRows)
			require.Equal(t, []int64{2, 4, 6}, r.Seams)
			require.Equal(t, 2, r.Balance.AccountsChecked)
			require.Len(t, r.Components, 4)
			require.Equal(t, valueSHA(r.Components), r.ComponentsSHA)
			require.NoError(t, hashFile(pin))
			raw, e := os.ReadFile(pin.Path)
			require.NoError(t, e)
			require.NotContains(t, string(raw), "synthetic-a")
			require.Contains(t, string(raw), `"full_chain_books_tax_acceptance": false`)
			_, _, e = AssembleManifest(AssembleSpec{Report: pin, Destination: pin, ApprovalSource: "test"}, filepath.Join(w.dir, "manifest.json"))
			require.ErrorContains(t, e, "whole-chain acceptance is not true")
			// The component receipts themselves are untouched.
			for _, c := range w.spec.Components {
				require.NoError(t, hashFile(c.Acceptance))
			}
			_, _, e = Finalize(context.Background(), w.spec, filepath.Join(w.dir, "real-bounds"))
			require.Error(t, e, "synthetic evidence can never satisfy the real chain boundaries")
		})
	}
}

func TestWholeChainFinalizeRefusals(t *testing.T) {
	both := []string{"cosmoshub-1", "cosmoshub-2"}
	type refusalCase struct {
		name, input, reason string
		chains              []string
		before              func(w *wholeChain)
		after               func(w *wholeChain)
	}
	cases := []refusalCase{
		{name: "gap between components", input: "component 2", reason: "gap: heights 3 to 4 are not covered", chains: both, after: func(w *wholeChain) {
			w.spec.Components = append(w.spec.Components[:1], w.spec.Components[2:]...)
		}},
		{name: "unsealed hub2 receipt", input: "component 2", reason: "is not sealed", chains: []string{"cosmoshub-2"}, after: func(w *wholeChain) {
			w.rewrite(&w.spec.Components[1].Acceptance, func(v map[string]any) { v["component_validated"] = false })
		}},
		{name: "unsealed hub1 receipt", input: "component 2", reason: "not a sealed Hub 1 part", chains: []string{"cosmoshub-1"}, after: func(w *wholeChain) {
			w.rewrite(&w.spec.Components[1].Acceptance, func(v map[string]any) { v["source_preserved_after_proof"] = false })
		}},
		{name: "receipt changed after pinning", input: "component 2", reason: "differ from the pinned hash", chains: both, after: func(w *wholeChain) {
			pin := w.spec.Components[1].Acceptance
			w.rewrite(&pin, func(v map[string]any) { v["relabelled"] = true })
		}},
		{name: "heights held back", input: "component 2", reason: "holds heights back", chains: []string{"cosmoshub-2"}, after: func(w *wholeChain) {
			w.rewrite(&w.spec.Components[1].Acceptance, func(v map[string]any) { v["held"] = 1 })
		}},
		{name: "source not preserved", input: "component 2", reason: "original recordings unchanged", chains: []string{"cosmoshub-2"}, after: func(w *wholeChain) {
			w.rewrite(&w.spec.Components[1].Acceptance, func(v map[string]any) { v["all_source_pre_post_and_local_member_hashes_equal"] = false })
		}},
		{name: "wrong genesis hash", input: "starting genesis", reason: "not the genuine", chains: both, after: func(w *wholeChain) {
			w.rewrite(&w.spec.Genesis, func(v map[string]any) { v["genesis_time"] = "changed" })
		}},
		{name: "history built from another genesis", input: "genesis_files", reason: "different genesis file", chains: both, after: func(w *wholeChain) {
			w.writeProjection(digest([]byte("another genesis")))
		}},
		{name: "wrong next genesis", input: "next genesis", reason: "not the genuine", chains: both, after: func(w *wholeChain) {
			w.rewrite(&w.spec.NextGenesis, func(v map[string]any) { v["extra"] = "changed" })
		}},
		{name: "seam state mismatch", input: "component 3", reason: "seam at height 4", chains: []string{"cosmoshub-2"}, after: func(w *wholeChain) {
			w.rewrite(w.spec.Components[2].Opening, func(v map[string]any) { v["application_hash"] = stateAt(99) })
		}},
		{name: "hub1 seam state mismatch", input: "component 3", reason: "seam at height 4", chains: []string{"cosmoshub-1"}, after: func(w *wholeChain) {
			w.rewrite(w.spec.Components[2].Result, func(v map[string]any) {
				v["opening_commit"] = map[string]any{"Version": 4, "Hash": stateBase64(99)}
			})
			w.rewrite(&w.spec.Components[2].Acceptance, func(v map[string]any) { v["result_sha256"] = w.spec.Components[2].Result.SHA256 })
		}},
		{name: "one account off by one unit", input: "balance comparison", reason: "1 coin amounts differ", chains: both, before: func(w *wholeChain) {
			w.accounts = []any{account("synthetic-a", "1001"), account("synthetic-b", "7")}
		}},
		{name: "account missing from export", input: "balance comparison", reason: "1 extra", chains: both, before: func(w *wholeChain) {
			w.accounts = []any{account("synthetic-a", "1000")}
		}},
		{name: "payout counted twice", input: "restart payout", reason: "exactly once", chains: both, before: func(w *wholeChain) {
			for _, row := range w.taxRows[1:] {
				again := map[string]any{}
				for k, v := range row {
					again[k] = v
				}
				again["sub_index"] = row["sub_index"].(int) + 3
				w.taxRows = append(w.taxRows, again)
			}
		}},
		{name: "payout missing", input: "restart payout", reason: "exactly once", chains: both, before: func(w *wholeChain) { w.taxRows = w.taxRows[:1] }},
		{name: "payout amount changed", input: "restart payout", reason: "exactly once", chains: both, before: func(w *wholeChain) { w.taxRows[1]["amount"] = "6" }},
		{name: "payout row repeated under one key", input: "taxable_events", reason: "duplicated", chains: both, before: func(w *wholeChain) {
			w.taxRows = append(w.taxRows, w.taxRows[1])
		}},
		{name: "payout at the wrong height", input: "taxable_events", reason: "row 2", chains: both, before: func(w *wholeChain) { w.taxRows[1]["height"] = 5 }},
		{name: "tax row in the tail", input: "taxable_events", reason: "above the migration height", chains: both, before: func(w *wholeChain) {
			w.taxRows = append(w.taxRows, map[string]any{"height": 7, "phase": 2, "tx_index": 0, "msg_index": 0, "sub_index": 0, "amount": "1", "amount_source": "message"})
		}},
		{name: "missing block", input: "blocks", reason: "8", chains: both, before: func(w *wholeChain) { w.blocks = 7 }},
		{name: "tail missing", input: "component list", reason: "reaches height 6", chains: both, after: func(w *wholeChain) {
			w.spec.Components = w.spec.Components[:3]
		}},
		{name: "same part listed twice", input: "component 3", reason: "listed twice", chains: both, after: func(w *wholeChain) {
			w.spec.Components = append(w.spec.Components[:2], append([]ComponentInput{w.spec.Components[1]}, w.spec.Components[2:]...)...)
		}},
		{name: "export from another height", input: "canonical export", reason: "migration height 6", chains: both, after: func(w *wholeChain) {
			w.rewrite(&w.spec.CanonicalExport, func(v map[string]any) { v["source_height"] = 4 })
		}},
		{name: "export not sealed with the migration part", input: "canonical export", reason: "not named by the sealed evidence", chains: both, after: func(w *wholeChain) {
			w.rewrite(&w.spec.CanonicalExport, func(v map[string]any) { v["note"] = "another export" })
		}},
		{name: "payout records swapped", input: "restart payout records", reason: "not the records the proof names", chains: both, after: func(w *wholeChain) {
			w.spec.PayoutRecords = savePin(w.t, w.dir, "other-records.ndjson", []byte("{}\n"))
		}},
		{name: "part claims whole-chain acceptance", input: "component 2", reason: "cannot carry whole-chain acceptance", chains: both, after: func(w *wholeChain) {
			w.rewrite(&w.spec.Components[1].Acceptance, func(v map[string]any) { v["full_chain_books_tax_acceptance"] = true })
		}},
		{name: "join names another seal", input: "join receipt", reason: "does not join exactly", chains: []string{"cosmoshub-1"}, after: func(w *wholeChain) {
			w.rewrite(w.spec.Join, func(v map[string]any) { v["accepted_terminal_seal_sha256"] = stateAt(1) })
		}},
		{name: "continuation from another receipt", input: "component 3", reason: "different earlier receipt", chains: []string{"cosmoshub-2"}, after: func(w *wholeChain) {
			w.rewrite(&w.spec.Components[2].Acceptance, func(v map[string]any) { v["prior_acceptance_sha256"] = stateAt(1) })
		}},
	}
	for _, tc := range cases {
		for _, chain := range tc.chains {
			t.Run(tc.name+"/"+chain, func(t *testing.T) {
				w := newWholeChain(t, chain)
				if tc.before != nil {
					tc.before(w)
				}
				w.build()
				if tc.after != nil {
					tc.after(w)
				}
				w.refused(tc.input, tc.reason)
			})
		}
	}
}

func newSHA() hash.Hash { return sha256.New() }
func readPinnedJSONLoose(p Pin, v any) error {
	b, e := readPinnedBytes(p, 1<<20)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}

// An interrupted attempt is kept beside the accepted second attempt of the
// same range. Only the sealed attempt that the next part pins can be a part;
// the abandoned root refuses however it is offered.
func TestWholeChainAbandonedAttemptBesideAcceptedSecondAttempt(t *testing.T) {
	w := newWholeChain(t, "cosmoshub-2")
	w.build()
	accepted := w.spec.Components[2]
	// The accepted part lives in a second-attempt folder with a claim, an
	// accepted exit and a recorded prior seal.
	root := filepath.Dir(filepath.Dir(accepted.Acceptance.Path))
	require.Equal(t, w.dir, root)
	write := func(dir, name string, v any) {
		b, e := json.Marshal(v)
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), b, 0600))
	}
	// The fixture keeps every part under one folder, so give part three its own roots.
	second := filepath.Join(w.dir, "6-attempt2", "6-ready-continuation")
	first := filepath.Join(w.dir, "6-ready-continuation")
	require.NoError(t, os.MkdirAll(filepath.Join(second, "6"), 0700))
	require.NoError(t, os.MkdirAll(filepath.Join(first, "6"), 0700))
	move := func(p *Pin, name string) {
		b, e := os.ReadFile(p.Path)
		require.NoError(t, e)
		p.Path = filepath.Join(second, "6", name)
		require.NoError(t, os.WriteFile(p.Path, b, 0600))
	}
	move(accepted.Opening, "opening.json")
	move(accepted.Closing, "closing.json")
	closing := *accepted.Closing
	w.rewrite(&accepted.Acceptance, func(v map[string]any) {
		v["mandatory_artifact_sha256"].(map[string]any)[closing.Path] = closing.SHA256
	})
	move(&accepted.Acceptance, "final-component-acceptance.json")
	write(second, "execution-claim.json", map[string]any{"first": 5, "last": 6})
	write(second, "execution-exit.json", map[string]any{"component_accepted": true, "exit_code": 0})
	write(second, "component-config.json", map[string]any{"first": 5, "last": 6, "prior_root": w.dir, "prior_seal": w.spec.Components[1].Acceptance.Path})
	w.spec.Components[2] = accepted
	// The tail pins the accepted second attempt's seal.
	w.rewrite(&w.spec.Components[3].Acceptance, func(v map[string]any) { v["prior_acceptance_sha256"] = accepted.Acceptance.SHA256 })
	// Staging happens after sealing, so its receipt names the seals as they now stand.
	restaged := stagingReceiptFor(t, w.dir, "staging-receipt-second-attempt.json", w.spec, w.b)
	w.spec.StagingReceipt = &restaged
	r, _, e := w.finalize()
	require.NoError(t, e)
	require.Equal(t, w.spec.Components[1].Acceptance.Path, r.Sealed[2].PriorSeal)

	// The abandoned first attempt: a claim, partial files, no seal.
	write(first, "execution-claim.json", map[string]any{"first": 5, "last": 6})
	abandoned := accepted
	abandoned.Acceptance = Pin{Path: filepath.Join(first, "6", "final-component-acceptance.json"), SHA256: accepted.Acceptance.SHA256}
	good := w.spec.Components
	offer := func(parts ...ComponentInput) {
		w.spec.Components = append(append(append([]ComponentInput{}, good[:2]...), parts...), good[3])
	}
	offer(abandoned)
	w.refused("component 3", "claimed but never sealed")
	// Somebody copies the accepted seal into the abandoned root: still no accepted exit.
	b, e := os.ReadFile(accepted.Acceptance.Path)
	require.NoError(t, e)
	require.NoError(t, os.WriteFile(abandoned.Acceptance.Path, b, 0600))
	offer(abandoned)
	w.refused("component 3", "claim with no accepted exit")
	// Both attempts listed: the abandoned one refuses before anything joins.
	offer(abandoned, accepted)
	w.refused("component 3", "claim with no accepted exit")
	offer(accepted, abandoned)
	w.refused("component 4", "the same receipt is listed twice")
	// A sealed root that records a different earlier seal than the part before it.
	write(second, "component-config.json", map[string]any{"first": 5, "last": 6, "prior_root": first, "prior_seal": abandoned.Acceptance.Path})
	offer(accepted)
	w.refused("component 3", "records a different earlier seal")
	// A different sealed receipt for the same range, which the tail does not pin.
	write(second, "component-config.json", map[string]any{"first": 5, "last": 6, "prior_root": w.dir, "prior_seal": good[1].Acceptance.Path})
	other := accepted
	w.rewrite(&other.Acceptance, func(v map[string]any) { v["note"] = "another sealing run" })
	offer(other)
	w.refused("component 4", "continues from a different earlier receipt")
}

func TestWholeChainHub2TailSealIsStrict(t *testing.T) {
	tail := func(w *wholeChain) *ComponentInput { return &w.spec.Components[len(w.spec.Components)-1] }
	change := func(key string, value any) func(w *wholeChain) {
		return func(w *wholeChain) { w.rewrite(&tail(w).Acceptance, func(v map[string]any) { v[key] = value }) }
	}
	cases := []struct {
		name, reason string
		after        func(w *wholeChain)
	}{
		{"rehearsal receipt", "a rehearsal or any other kind of receipt is not accepted", change("rehearsal", true)},
		{"rehearsal flag absent", "a rehearsal or any other kind of receipt is not accepted", func(w *wholeChain) {
			w.rewrite(&tail(w).Acceptance, func(v map[string]any) { delete(v, "rehearsal") })
		}},
		{"another schema", "a rehearsal or any other kind of receipt is not accepted", change("schema", "hub2-continuation-seal-v1")},
		{"scope stops short", "does not cover exactly heights 7 to 8", change("complete_scope", []int64{7, 7})},
		{"scope starts at the migration height", "does not cover exactly heights 7 to 8", change("complete_scope", []int64{6, 8})},
		{"another migration height", "after the migration height 6", change("canonical_migration_height", 5)},
		{"claims a later block", "claims a later block that does not exist", change("terminal_next_header_proof", true)},
		{"not accepted", "tail seal is not accepted", change("component_accepted", false)},
		{"opens from another proof", "opens from a different state proof", change("opening_state_proof", map[string]any{"sha256": digest([]byte("other"))})},
		{"closing proof supplied", "the tail has no closing state proof", func(w *wholeChain) { tail(w).Closing = w.proof("tail-closing.json", 8) }},
		{"opening proof not listed", "opens from a different state proof", func(w *wholeChain) { tail(w).Opening = nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWholeChain(t, "cosmoshub-2")
			w.build()
			c.after(w)
			w.refused("component 4", c.reason)
		})
	}
	t.Run("real rehearsal tail receipt", func(t *testing.T) {
		path := "/Users/danb/.cache/cosmos-tax-claude-20261116/hub2-component-chain/REHEARSAL-tail-mechanics-2000002-20261116T0523Z/2000002/REHEARSAL-mechanics-only.json"
		raw, e := os.ReadFile(path)
		if e != nil {
			t.Skip("rehearsal tail receipt is not on this machine")
		}
		w := newWholeChain(t, "cosmoshub-2")
		w.build()
		sum := sha256.Sum256(raw)
		tail(w).Acceptance = Pin{Path: path, SHA256: hex.EncodeToString(sum[:])}
		w.refused("component 4", "a rehearsal or any other kind of receipt is not accepted")
	})
}

// changeTable rewrites one exported table and its index entry, as someone
// handing the finalizer a different projection under the same seals would.
func (w *wholeChain) changeTable(table string, change func(lines []string) []string) {
	w.t.Helper()
	var index ProjectionIndex
	require.NoError(w.t, readPinnedJSON(w.spec.Projection, &index))
	for n, p := range index.Tables {
		if p.Table != table {
			continue
		}
		raw, e := os.ReadFile(p.File.Path)
		require.NoError(w.t, e)
		lines := change(strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"))
		w.receipts++
		index.Tables[n].Rows = int64(len(lines))
		index.Tables[n].File = savePin(w.t, w.dir, fmt.Sprintf("%s.changed-%d.jsonl", table, w.receipts), []byte(strings.Join(lines, "\n")+"\n"))
	}
	w.spec.Projection = w.save(fmt.Sprintf("projection-index-changed-%d.json", w.receipts), index)
}
func changedLine(t *testing.T, line string, change func(row map[string]any)) string {
	t.Helper()
	var row map[string]any
	d := json.NewDecoder(strings.NewReader(line))
	d.UseNumber()
	require.NoError(t, d.Decode(&row))
	change(row)
	out, e := json.Marshal(row)
	require.NoError(t, e)
	return string(out)
}

// The seals, the staging receipt and the exported rows must be one chain of
// hashes. A different projection under the same seals, a receipt for other
// seals, and one altered, extra or missing row must each refuse.
func TestWholeChainStagingReceiptBindsRowsToSeals(t *testing.T) {
	both := []string{"cosmoshub-1", "cosmoshub-2"}
	receipt := func(change func(v map[string]any)) func(w *wholeChain) {
		return func(w *wholeChain) { w.rewrite(w.spec.StagingReceipt, change) }
	}
	object := func(v any, keys ...any) map[string]any {
		for _, k := range keys {
			switch key := k.(type) {
			case string:
				v = v.(map[string]any)[key]
			case int:
				v = v.([]any)[key]
			}
		}
		return v.(map[string]any)
	}
	other := digest([]byte("other"))
	cases := []struct {
		name, reason string
		chains       []string
		after        func(w *wholeChain)
	}{
		{"no receipt", "required", both, func(w *wholeChain) { w.spec.StagingReceipt = nil }},
		{"swapped projection under the same seals", "table taxable_events: the exported rows are not the rows the receipt bound", both, func(w *wholeChain) {
			kept := w.spec.StagingReceipt
			w.taxRows[0]["amount"] = "8"
			w.writeProjection(w.spec.Genesis.SHA256)
			w.spec.StagingReceipt = kept
		}},
		{"one altered row", "table taxable_events: the exported rows are not the rows the receipt bound", both, func(w *wholeChain) {
			w.changeTable("taxable_events", func(l []string) []string {
				l[0] = changedLine(w.t, l[0], func(r map[string]any) { r["amount"] = "10" })
				return l
			})
		}},
		{"one extra row", "table taxable_events: the exported rows are not the rows the receipt bound to the sealed parts (5 exported, 4 in the receipt)", both, func(w *wholeChain) {
			w.changeTable("taxable_events", func(l []string) []string {
				extra := changedLine(w.t, l[0], func(r map[string]any) { r["sub_index"] = 1 })
				return append([]string{l[0], extra}, l[1:]...)
			})
		}},
		{"one missing row", "table taxable_events: the exported rows are not the rows the receipt bound to the sealed parts (3 exported, 4 in the receipt)", both, func(w *wholeChain) {
			w.changeTable("taxable_events", func(l []string) []string { return l[1:] })
		}},
		{"one altered block", "table blocks: the exported rows are not the rows the receipt bound", both, func(w *wholeChain) {
			w.changeTable("blocks", func(l []string) []string {
				l[2] = changedLine(w.t, l[2], func(r map[string]any) { r["tx_count"] = 7 })
				return l
			})
		}},
		{"totals restated but a part's rows differ", "part 1 (heights 1 to 2) table taxable_events: exported rows differ from the rows that sealed part contributed", both, func(w *wholeChain) {
			kept := w.spec.StagingReceipt
			w.taxRows[0]["amount"] = "8"
			w.writeProjection(w.spec.Genesis.SHA256)
			restated := object(readObject(w.t, *w.spec.StagingReceipt), "projection_tables")
			w.spec.StagingReceipt = kept
			w.rewrite(w.spec.StagingReceipt, func(v map[string]any) { v["projection_tables"] = restated })
		}},
		{"row moved to another part", "part 1 (heights 1 to 2) table taxable_events", both, func(w *wholeChain) {
			kept := w.spec.StagingReceipt
			w.taxRows[0]["height"] = 3
			w.writeProjection(w.spec.Genesis.SHA256)
			restated := object(readObject(w.t, *w.spec.StagingReceipt), "projection_tables")
			w.spec.StagingReceipt = kept
			w.rewrite(w.spec.StagingReceipt, func(v map[string]any) { v["projection_tables"] = restated })
		}},
		{"receipt for another seal", "part 2 (heights 3 to 4) was staged under a different seal", both, receipt(func(v map[string]any) { object(v, "components", 1)["seal_sha256"] = other })},
		{"receipt for a shorter seal list", "lists 2 sealed parts, 3 are being finalized", both, receipt(func(v map[string]any) { v["components"] = v["components"].([]any)[:2] })},
		{"receipt for a longer seal list", "lists 4 sealed parts, 3 are being finalized", both, receipt(func(v map[string]any) {
			v["components"] = append(v["components"].([]any), v["components"].([]any)[2])
		})},
		{"receipt lists the seals in another order", "part 1 (heights 1 to 2) was staged under a different seal", both, receipt(func(v map[string]any) {
			c := v["components"].([]any)
			c[0], c[1] = c[1], c[0]
		})},
		{"part resealed after staging", "part 2 (heights 3 to 4) was staged under a different seal", []string{"cosmoshub-2"}, func(w *wholeChain) {
			w.rewrite(&w.spec.Components[1].Acceptance, func(v map[string]any) { v["note"] = "sealed again" })
		}},
		{"part ranges differ", "part 3 (heights 5 to 6) was staged under a different seal or range", both, receipt(func(v map[string]any) { object(v, "components", 2)["first"] = 4 })},
		{"part not checked against its recordings", "stored rows were not checked against the sealed recordings", both, receipt(func(v map[string]any) {
			object(v, "components", 0)["stored_rows_equal_rows_rederived_from_sealed_source"] = false
		})},
		{"payout rows from other records", "not bound to the payout records being finalized", both, receipt(func(v map[string]any) { object(v, "restart_payout")["export_records_sha256"] = other })},
		{"payout rows differ", "exported restart payout rows differ", both, receipt(func(v map[string]any) { object(v, "restart_payout", "taxable_events_phase4")["sha256"] = other })},
		{"payout rows missing from the receipt", "not bound to the payout records being finalized", both, receipt(func(v map[string]any) { v["restart_payout"] = nil })},
		{"tail from another seal", "tail rows are not bound to the tail seal being finalized", both, receipt(func(v map[string]any) { object(v, "historical_tail")["tail_seal_sha256"] = other })},
		{"tail seal not yet recorded", "tail rows are not bound to the tail seal being finalized", both, receipt(func(v map[string]any) { object(v, "historical_tail")["tail_seal_sha256"] = "" })},
		{"tail missing from the receipt", "tail rows are not bound to the tail seal being finalized", both, receipt(func(v map[string]any) { v["historical_tail"] = nil })},
		{"tail rows differ", "tail table blocks: exported rows differ", both, receipt(func(v map[string]any) { object(v, "historical_tail", "tables", "blocks")["sha256"] = other })},
		{"incomplete chain", "does not cover the complete chain", both, receipt(func(v map[string]any) { v["complete_chain"] = false })},
		{"rows bound to no part", "not bound to any sealed part", both, receipt(func(v map[string]any) { v["rows_bound_to_no_listed_part"] = 1 })},
		{"receipt for the other chain", "not a staging receipt for", both, receipt(func(v map[string]any) { v["schema"] = "hub9-staging-receipt-v1" })},
		{"staged from another genesis", "built from a different genesis file", both, receipt(func(v map[string]any) { object(v, "genesis")["sha256"] = other })},
		{"receipt changed after pinning", "missing or changed since it was pinned", both, func(w *wholeChain) {
			pin := *w.spec.StagingReceipt
			w.rewrite(&pin, func(v map[string]any) { v["note"] = "edited" })
		}},
	}
	for _, tc := range cases {
		for _, chain := range tc.chains {
			t.Run(tc.name+"/"+chain, func(t *testing.T) {
				w := newWholeChain(t, chain)
				w.build()
				tc.after(w)
				w.refused("staging receipt", tc.reason)
			})
		}
	}
	// The report names the receipt, so the manifest can carry it.
	for _, chain := range both {
		w := newWholeChain(t, chain)
		w.build()
		r, _, e := w.finalize()
		require.NoError(t, e)
		require.Equal(t, w.spec.StagingReceipt.SHA256, r.StagingReceiptSHA)
		require.Equal(t, *w.spec.StagingReceipt, r.Inputs["staging_receipt"])
	}
}
func readObject(t *testing.T, p Pin) map[string]any {
	t.Helper()
	raw, e := os.ReadFile(p.Path)
	require.NoError(t, e)
	var v map[string]any
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	require.NoError(t, d.Decode(&v))
	return v
}
