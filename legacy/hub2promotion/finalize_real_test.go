package hub2promotion

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

// These tests read the real boundary files and sealed Hub 2 components kept on
// the build host. They run only when WHOLE_CHAIN_REAL_EVIDENCE names the
// evidence root, and they print counts and hashes only.
func realEvidence(t *testing.T) (cache, tmp string) {
	t.Helper()
	root := os.Getenv("WHOLE_CHAIN_REAL_EVIDENCE")
	if root == "" {
		t.Skip("WHOLE_CHAIN_REAL_EVIDENCE not set: real boundary files live only on the build host")
	}
	return root, filepath.Join(root, "cosmos-tax-tmp")
}
func pinFile(t *testing.T, path string) Pin {
	t.Helper()
	p := Pin{Path: path}
	f, e := os.Open(path)
	require.NoError(t, e, filepath.Base(path))
	defer f.Close()
	h := newSHA()
	_, e = bufio.NewReaderSize(f, 1<<20).WriteTo(h)
	require.NoError(t, e)
	p.SHA256 = fmt.Sprintf("%x", h.Sum(nil))
	return p
}
func buildCommand(t *testing.T, name string) Pin {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, "github.com/DefiantLabs/cosmos-indexer/cmd/"+name)
	b, e := cmd.CombinedOutput()
	require.NoError(t, e, string(b))
	return pinFile(t, out)
}
func syntheticCapture(t *testing.T, dir, chain string) Pin {
	t.Helper()
	protected := Snapshot{}
	for _, tb := range allTables {
		protected[tb.name] = Fingerprint{Rows: 1, SHA256: digest([]byte("protected " + tb.name))}
	}
	scope, e := ApprovedScope(chain)
	require.NoError(t, e)
	capture := DestinationCapture{Schema: DestinationCaptureSchema, Chain: chain, Scope: scope,
		Destination: Identity{Database: "cosmostax", OID: 1, SystemHash: digest([]byte("system")), DeploymentUID: "synthetic-deployment", PodUID: "synthetic-pod", PVCUID: "synthetic-pvc", Endpoint: "synthetic-endpoint", CatalogSHA: digest([]byte("catalog")), ScopeSHA: digest([]byte("scope"))},
		TargetEmpty: true, Protected: protected, Hub3GenesisSHA: acceptedHub3GenesisSHA, Hub4GenesisSHA: digest([]byte("hub4")), CapturedUTC: time.Now().UTC().Format(time.RFC3339)}
	pin, e := writeNew(filepath.Join(dir, "destination-capture.json"), capture)
	require.NoError(t, e)
	return pin
}

// The loader object names must render the exact SQL the production
// prerequisites review read, or the scope digest in the manifest would not
// match the reviewed install.
func TestApprovedScopeRendersReviewedSQL(t *testing.T) {
	cache, _ := realEvidence(t)
	for chain, file := range map[string]string{"cosmoshub-1": "cosmoshub-1.scope.sql", "cosmoshub-2": "cosmoshub-2.scope.sql"} {
		s, e := ApprovedScope(chain)
		require.NoError(t, e)
		text, e := s.SQL()
		require.NoError(t, e)
		reviewed, e := os.ReadFile(filepath.Join(cache, "cosmos-tax-claude-20261116", "prereq-work", "scope-review", file))
		require.NoError(t, e)
		require.Equal(t, strings.TrimSpace(string(reviewed)), strings.TrimSpace(text), chain)
	}
}

// Hub 1 at its real boundaries: the real starting genesis, the real restart
// export at 500,000, its real state proof and the real Hub 2 genesis, joined
// through synthetic part seals and 500,015 synthetic block rows. It proves
// that a report the finalizer accepts becomes a manifest the loader's own
// validation, and the loader command itself, accept.
func TestWholeChainHub1RealBoundariesReachLoaderValidate(t *testing.T) {
	cache, _ := realEvidence(t)
	h1 := filepath.Join(cache, "cosmos-tax-hub1-native-boundary-aa6b004")
	w := newWholeChain(t, "cosmoshub-1")
	var e error
	w.b, e = approvedBounds("cosmoshub-1")
	require.NoError(t, e)
	genesis, export, next, proof := pinFile(t, filepath.Join(h1, "genesis.cosmoshub-1.json")), pinFile(t, filepath.Join(h1, "native-export.json")), pinFile(t, filepath.Join(h1, "genesis.cosmoshub-2.json")), pinFile(t, filepath.Join(h1, "checkpoint-proof.json"))
	w.genesis, w.export, w.next, w.canonical = &genesis, &export, &next, &proof
	var real struct {
		Hash string `json:"application_hash"`
	}
	require.NoError(t, readPinnedJSONLoose(proof, &real))
	w.states = map[int64]string{500000: real.Hash}
	w.parts = [][2]int64{{1, 100000}, {100001, 137500}, {137501, 200000}, {200001, 300000}, {300001, 400000}, {400001, 486010}, {486011, 500000}, {500001, 500015}}
	w.blocks = 500015
	for n := range w.payouts {
		w.payouts[n].Height = 500000
	}
	for _, row := range w.taxRows[1:] {
		row["height"] = 500000
	}
	// A dry run of the load runbook needs these same inputs outside the test.
	if keep := os.Getenv("WHOLE_CHAIN_KEEP_DIR"); keep != "" {
		w.dir, e = os.MkdirTemp(keep, "hub1-real-boundaries-")
		require.NoError(t, e)
	}
	w.build()
	loader := buildCommand(t, "legacy-approved-load")
	w.spec.LoaderBinary = loader
	if os.Getenv("WHOLE_CHAIN_KEEP_DIR") != "" {
		_, e = writeNew(filepath.Join(w.dir, "finalize-spec.json"), w.spec)
		require.NoError(t, e)
		t.Logf("inputs kept in %s", w.dir)
	}

	started := time.Now()
	dir := filepath.Join(w.dir, "final")
	r, pin, e := Finalize(context.Background(), w.spec, dir)
	require.NoError(t, e)
	t.Logf("hub1 finalize at real boundaries: %s, %d accounts compared, %d differences", time.Since(started).Round(time.Millisecond), r.Balance.AccountsChecked, r.Balance.Missing+r.Balance.Extra+r.Balance.MismatchedDenominations)
	require.True(t, r.Accepted)
	require.Equal(t, 1027, r.Balance.AccountsChecked)
	require.Equal(t, int64(500015), r.Last)

	started = time.Now()
	manifest := filepath.Join(w.dir, "manifest.json")
	m, mpin, e := AssembleManifest(AssembleSpec{Report: pin, Destination: syntheticCapture(t, w.dir, "cosmoshub-1"), ApprovalSource: "synthetic test"}, manifest)
	require.NoError(t, e)
	require.NoError(t, m.ValidateApproved())
	t.Logf("hub1 manifest assembled and validated: %s", time.Since(started).Round(time.Millisecond))
	out, e := exec.Command(loader.Path, "-operation", "validate", "-manifest", manifest, "-sha256", mpin.SHA256).CombinedOutput()
	require.NoError(t, e, string(out))
	t.Logf("legacy-approved-load -operation validate: %s", strings.TrimSpace(string(out)))

	// A second manifest can never overwrite the first, and a changed report
	// can never be assembled.
	_, _, e = AssembleManifest(AssembleSpec{Report: pin, Destination: syntheticCapture(t, t.TempDir(), "cosmoshub-1"), ApprovalSource: "synthetic test"}, manifest)
	require.Error(t, e)
	changed := pin
	w.rewrite(&changed, func(v map[string]any) { v["canonical_rows"] = 2 })
	_, _, e = AssembleManifest(AssembleSpec{Report: changed, Destination: syntheticCapture(t, t.TempDir(), "cosmoshub-1"), ApprovalSource: "synthetic test"}, filepath.Join(w.dir, "manifest-2.json"))
	require.ErrorContains(t, e, "loader's own validation refuses it")

	// One real account off by one unit against the real Hub 2 genesis.
	off := newWholeChain(t, "cosmoshub-1")
	off.b, off.states, off.parts, off.blocks, off.payouts, off.taxRows = w.b, w.states, w.parts, w.blocks, w.payouts, w.taxRows
	raw, e := os.ReadFile(export.Path)
	require.NoError(t, e)
	var doc map[string]any
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	require.NoError(t, d.Decode(&doc))
	accounts := doc["app_state"].(map[string]any)["accounts"].([]any)
	bumped := false
	for _, a := range accounts {
		coins, _ := a.(map[string]any)["coins"].([]any)
		if len(coins) > 0 && !bumped {
			coin := coins[0].(map[string]any)
			coin["amount"] = addOne(coin["amount"].(string))
			bumped = true
		}
	}
	require.True(t, bumped)
	changedExport := off.save("export-off-by-one.json", doc)
	off.genesis, off.export, off.next, off.canonical = &genesis, &changedExport, &next, &proof
	off.build()
	off.refused("balance comparison", "0 missing, 0 extra, 1 coin amounts differ")
}
func addOne(amount string) string {
	digits := []byte(amount)
	for i := len(digits) - 1; i >= 0; i-- {
		if digits[i] != '9' {
			digits[i]++
			return string(digits)
		}
		digits[i] = '0'
	}
	return "1" + string(digits)
}

func hub2Components(t *testing.T, tmp string, through int64) []ComponentInput {
	t.Helper()
	pin := func(parts ...string) Pin { return pinFile(t, filepath.Join(append([]string{tmp}, parts...)...)) }
	ptr := func(p Pin) *Pin { return &p }
	out := []ComponentInput{
		{Profile: "hub2-genesis-prefix-v1", Acceptance: pin("hub2-genesis-prefix-20261114T134144Z", "final-acceptance.json"), Closing: ptr(pin("hub2-genesis-prefix-20261114T134144Z", "normal200000-native-header-proof.json"))},
		{Profile: "hub2-range-result-v1", Acceptance: pin("first-range-seal-20261114T050601Z", "financial-component-v3", "component-result.json"), Opening: ptr(pin("first-range-seal-20261114T050601Z", "candidate", "source-next-header-proof.json")), Closing: ptr(pin("continuity-20261114T053543Z", "ending-proof.json"))},
	}
	for _, end := range []string{"600000", "800000", "1000000", "1200000", "1400000"} {
		d := filepath.Join("current-ranges-20261114T120600Z", end)
		seam := "normal-seam"
		if end == "1200000" {
			// The first seam attempt for this range left no proof; the retained second one did.
			seam = "normal-seam-v2"
		}
		out = append(out, ComponentInput{Profile: "hub2-range-result-v1", Acceptance: pin(d, "financial", "component-result.json"), Opening: ptr(pin(d, "candidate", "source-next-header-proof.json")), Closing: ptr(pin(d, seam, "ending-native-proof.json"))})
	}
	next := "hub2-next-components-20261114T142600Z"
	for _, c := range []struct {
		end          int64
		dir, closing string
	}{{1600000, "1600000", "ending-normal-join/native-header-proof.json"}, {1800000, "1800000-ready-continuation/1800000", "normal-seam/ending-native-proof.json"}, {2000000, "2000000-ready-continuation/2000000", "normal-seam/ending-native-proof.json"}} {
		if c.end > through {
			break
		}
		out = append(out, ComponentInput{Profile: "hub2-continuation-v1", Acceptance: pin(next, c.dir, "final-component-acceptance.json"), Opening: ptr(pin(next, c.dir, "candidate", "source-next-header-proof.json")), Closing: ptr(pin(next, c.dir, c.closing))})
	}
	return out
}

// The real sealed Hub 2 history so far: ten parts joining 1 to 2,000,000 at
// the real chain boundaries. It can join but never be accepted, and dropping
// the 1.8M part refuses at the gap.
func TestWholeChainHub2RealSealedPartsJoin(t *testing.T) {
	cache, tmp := realEvidence(t)
	genesis := pinFile(t, filepath.Join(cache, "cosmos-tax-hub1-native-boundary-aa6b004", "genesis.cosmoshub-2.json"))
	spec := FinalizeSpec{Chain: "cosmoshub-2", Genesis: genesis, Components: hub2Components(t, tmp, 2000000)}
	started := time.Now()
	chain, e := VerifyComponents(spec)
	require.NoError(t, e)
	require.Equal(t, int64(2000000), chain.Through)
	require.Len(t, chain.Components, 10)
	require.Len(t, chain.Seams, 9)
	for _, c := range chain.Components {
		t.Logf("sealed %d to %d, closing state bound by %s", c.First, c.Last, c.ClosingBoundBy)
	}
	t.Logf("ten real parts joined through %d in %s", chain.Through, time.Since(started).Round(time.Millisecond))

	_, _, e = Finalize(context.Background(), spec, filepath.Join(t.TempDir(), "final"))
	require.ErrorContains(t, e, "sealed history reaches height 2000000, the chain ends at 2902002")

	gap := spec
	gap.Components = append(append([]ComponentInput{}, spec.Components[:8]...), spec.Components[9])
	_, e = VerifyComponents(gap)
	require.ErrorContains(t, e, "gap: heights 1600001 to 1800000 are not covered")

	wrong := spec
	wrong.Genesis = pinFile(t, filepath.Join(cache, "cosmos-tax-hub1-native-boundary-aa6b004", "genesis.cosmoshub-1.json"))
	_, e = VerifyComponents(wrong)
	require.ErrorContains(t, e, "not the genuine cosmoshub-2 genesis")
}

// The real 1.4M rehearsal end to end under reduced boundaries: rows exported
// read-only from the restored private staging database by the new command,
// the seven real sealed parts, the real restart export at 1,400,000 and its
// real payout records, and the official migration helper. The rehearsal
// database deliberately holds no payout rows, so the test adds the rows the
// real export records imply to a copy of the exported file, and compares the
// migrated export against a next genesis derived from it.
func TestWholeChainHub2RealRehearsalFinalizes(t *testing.T) {
	cache, tmp := realEvidence(t)
	dsn := os.Getenv("WHOLE_CHAIN_STAGING_DSN")
	require.NotEmpty(t, dsn, "private staging connection required")
	c, e := pgx.ParseConfig(dsn)
	require.NoError(t, e)
	require.NotEqual(t, uint16(15432), c.Port, "never production")
	require.True(t, strings.HasPrefix(c.Database, "legacy_replay_rehearsal_"))
	// About 5 GiB of private rows: the caller names a directory on a real
	// volume. The test never falls back to the system temp directory.
	dir := os.Getenv("WHOLE_CHAIN_WORK_DIR")
	require.True(t, filepath.IsAbs(dir), "WHOLE_CHAIN_WORK_DIR must name an absolute working directory")
	const canonical = 1400000
	range14 := filepath.Join(tmp, "current-ranges-20261114T120600Z", "1400000")
	genesis := pinFile(t, filepath.Join(cache, "cosmos-tax-hub1-native-boundary-aa6b004", "genesis.cosmoshub-2.json"))
	export := pinFile(t, filepath.Join(range14, "candidate", "export.json"))
	records := pinFile(t, filepath.Join(range14, "candidate", "export-movements.ndjson"))
	migration := pinFile(t, filepath.Join(cache, "cosmos-tax-hub2-approved-load-20261115", "hub2-migrate036.final"))
	require.Equal(t, acceptedMigrationBinarySHA, migration.SHA256)

	db := stdlib.OpenDB(*c)
	defer db.Close()
	started := time.Now()
	index, _, e := ExportStaging(context.Background(), db, filepath.Join(dir, "projection"), "cosmoshub-2")
	require.NoError(t, e)
	exportTime := time.Since(started)
	rows := map[string]int64{}
	var bytes int64
	for _, p := range index.Tables {
		rows[p.Table] = p.Rows
		st, e := os.Stat(p.File.Path)
		require.NoError(t, e)
		bytes += st.Size()
	}
	t.Logf("staging export: %s, %d MiB, blocks %d, tx %d, events %d, tax rows %d", exportTime.Round(time.Second), bytes>>20, rows["blocks"], rows["tx_inclusions"], rows["block_events"], rows["taxable_events"])
	require.Equal(t, int64(1400000), rows["blocks"])
	require.Equal(t, int64(382592), rows["taxable_events"])

	// Derive the payout rows the real export records imply.
	tax := index.Tables[9]
	original, e := os.ReadFile(tax.File.Path)
	require.NoError(t, e)
	var withPayout strings.Builder
	withPayout.Write(original)
	f, e := os.Open(records.Path)
	require.NoError(t, e)
	sc := bufio.NewScanner(f)
	sub, expected := 0, int64(0)
	for sc.Scan() {
		var r exportRecord
		require.NoError(t, json.Unmarshal(sc.Bytes(), &r))
		if r.Kind != "movement" {
			continue
		}
		category, to := "reward", r.Delegator
		if r.Label == "validator_commission/bank.add" {
			category, to = "commission", "operator-of-validator"
		}
		line, e := json.Marshal(syntheticRow(inserts[9], "cosmoshub-2", map[string]any{"height": canonical, "phase": 4, "tx_index": -1, "msg_index": -1, "sub_index": sub, "category": category, "amount": r.Amount, "denom": r.Denom, "to_addr": to, "validator_address": r.Validator, "amount_source": "export_payout"}))
		require.NoError(t, e)
		withPayout.Write(line)
		withPayout.WriteString("\n")
		sub++
		expected++
	}
	require.NoError(t, f.Close())
	require.Equal(t, int64(10993), expected)
	once := index
	once.Tables = append([]Projection{}, index.Tables...)
	once.Tables[9].File, once.Tables[9].Rows = savePin(t, dir, "taxable_events.with-payout.jsonl", []byte(withPayout.String())), tax.Rows+expected
	projection, e := writeNew(filepath.Join(dir, "projection-index.with-payout.json"), once)
	require.NoError(t, e)

	migrated := Pin{Path: filepath.Join(dir, "export.migrated036.json")}
	cmd := exec.Command(migration.Path, "-input", export.Path, "-sha256", export.SHA256, "-output", migrated.Path)
	cmd.Env = []string{"GOMAXPROCS=2", "GOMEMLIMIT=512MiB"}
	out, e := cmd.CombinedOutput()
	require.NoError(t, e, len(out))
	migrated = pinFile(t, migrated.Path)
	var doc map[string]any
	raw, e := os.ReadFile(migrated.Path)
	require.NoError(t, e)
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	require.NoError(t, d.Decode(&doc))
	nextDoc := map[string]any{"chain_id": "cosmoshub-3", "app_state": map[string]any{"accounts": doc["app_state"].(map[string]any)["accounts"]}}
	encoded, e := json.Marshal(nextDoc)
	require.NoError(t, e)
	next := savePin(t, dir, "next-genesis.derived.json", encoded)

	binding := pinFile(t, filepath.Join(tmp, "current-ranges-20261114T120600Z", "final-bounded-acceptance.json"))
	spec := FinalizeSpec{Chain: "cosmoshub-2", Genesis: genesis, Components: hub2Components(t, tmp, canonical), CanonicalExport: export, ExportBinding: &binding, PayoutRecords: records, PayoutProof: pinFile(t, filepath.Join(range14, "financial", "export-proof.json")),
		NextGenesis: next, MigratedExport: &migrated, MigrationBinary: &migration, Projection: projection, LoaderBinary: migration, BuildCommit: strings.Repeat("ab", 20)}
	bounds := finalBounds{chain: "cosmoshub-2", next: "cosmoshub-3", canonical: canonical, last: canonical, genesisSHA: acceptedHub2GenesisSHA, nextGenesisSHA: next.SHA256, migrationBinarySHA: acceptedMigrationBinarySHA, migrates: true}
	started = time.Now()
	r, pin, e := finalize(context.Background(), spec, filepath.Join(dir, "final"), bounds)
	require.NoError(t, e)
	t.Logf("finalize over 1.4M blocks: %s, %d parts, %d payout rows matched once, %d accounts compared with 0 differences", time.Since(started).Round(time.Second), len(r.Components), r.CanonicalRows, r.Balance.AccountsChecked)
	require.False(t, r.Accepted, "reduced boundaries never accept a chain")
	require.True(t, r.FixtureChecksPassed)
	require.Equal(t, expected, r.CanonicalRows)
	// 18,037 accounts in the export, plus the accounts the 0.36 migration adds.
	require.GreaterOrEqual(t, r.Balance.AccountsChecked, 18037)
	require.NoError(t, hashFile(pin))

	refusedWith := func(name string, change func(s *FinalizeSpec, b *finalBounds), want string) {
		s, b := spec, bounds
		change(&s, &b)
		_, _, e := finalize(context.Background(), s, filepath.Join(dir, "refused-"+name), b)
		require.ErrorContains(t, e, want, name)
		require.True(t, IsRefusal(e))
		_, statErr := os.Stat(filepath.Join(dir, "refused-"+name, "finalization-report.json"))
		require.Error(t, statErr)
		t.Logf("refused (%s): %s", name, e)
	}
	// Payout counted twice: the same real payout rows appended a second time.
	refusedWith("payout-twice", func(s *FinalizeSpec, _ *finalBounds) {
		var twice strings.Builder
		twice.WriteString(withPayout.String())
		for _, line := range strings.Split(strings.TrimSuffix(withPayout.String()[len(original):], "\n"), "\n") {
			var row map[string]any
			dd := json.NewDecoder(strings.NewReader(line))
			dd.UseNumber()
			require.NoError(t, dd.Decode(&row))
			n, _ := row["sub_index"].(json.Number).Int64()
			row["sub_index"] = n + expected
			again, _ := json.Marshal(row)
			twice.Write(again)
			twice.WriteString("\n")
		}
		doubled := once
		doubled.Tables = append([]Projection{}, once.Tables...)
		doubled.Tables[9].File, doubled.Tables[9].Rows = savePin(t, dir, "taxable_events.payout-twice.jsonl", []byte(twice.String())), tax.Rows+2*expected
		p, e := writeNew(filepath.Join(dir, "projection-index.payout-twice.json"), doubled)
		require.NoError(t, e)
		s.Projection = p
	}, "must be included exactly once")
	// The payout left out, which is the rehearsal database as it stands.
	refusedWith("payout-missing", func(s *FinalizeSpec, _ *finalBounds) {
		p, e := writeNew(filepath.Join(dir, "projection-index.no-payout.json"), index)
		require.NoError(t, e)
		s.Projection = p
	}, "must be included exactly once")
	// One real account off by one unit in the next chain's starting balances.
	refusedWith("account-off-by-one", func(s *FinalizeSpec, b *finalBounds) {
		accounts := nextDoc["app_state"].(map[string]any)["accounts"].([]any)
		var restore func()
		for _, a := range accounts {
			coins, _ := a.(map[string]any)["coins"].([]any)
			if len(coins) > 0 {
				coin := coins[0].(map[string]any)
				was := coin["amount"].(string)
				coin["amount"] = addOne(was)
				restore = func() { coin["amount"] = was }
				break
			}
		}
		require.NotNil(t, restore)
		changed, e := json.Marshal(nextDoc)
		restore()
		require.NoError(t, e)
		s.NextGenesis = savePin(t, dir, "next-genesis.off-by-one.json", changed)
		b.nextGenesisSHA = s.NextGenesis.SHA256
	}, "0 missing, 0 extra, 1 coin amounts differ")
	refusedWith("wrong-next-genesis", func(s *FinalizeSpec, b *finalBounds) { b.nextGenesisSHA = acceptedHub3GenesisSHA }, "not the genuine cosmoshub-3 starting genesis")
	refusedWith("gap", func(s *FinalizeSpec, _ *finalBounds) {
		s.Components = append(append([]ComponentInput{}, spec.Components[:4]...), spec.Components[5:]...)
	}, "gap: heights 800001 to 1000000 are not covered")
	refusedWith("wrong-genesis", func(s *FinalizeSpec, _ *finalBounds) {
		s.Genesis = pinFile(t, filepath.Join(cache, "cosmos-tax-hub1-native-boundary-aa6b004", "genesis.cosmoshub-1.json"))
	}, "not the genuine cosmoshub-2 genesis")
	refusedWith("unmigrated-export", func(s *FinalizeSpec, _ *finalBounds) { s.MigratedExport = &export }, "does not reproduce it")
	refusedWith("real-boundaries", func(_ *FinalizeSpec, b *finalBounds) { *b, _ = approvedBounds("cosmoshub-2") }, "sealed history reaches height 1400000, the chain ends at 2902002")
}
