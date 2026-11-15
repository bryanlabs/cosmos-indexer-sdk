package taxapi

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func checkpointTestID() flowCheckpointIdentity {
	return flowCheckpointIdentity{Schema: 1, Root: "synthetic-root", Chain: "synthetic-chain", Build: "code-v1", SourceEpoch: "frozen-fixture-v1", SourceSeal: "fixture-seal", SettingsSHA256: "settings", ReceiversSHA256: "receivers", From: flowTestStart, AsOf: flowTestStart.AddDate(0, 3, 0)}
}
func checkpointTestHooks() flowCheckpointHooks {
	return flowCheckpointHooks{VerifySource: func() error { return nil }, Capture: func() (json.RawMessage, error) { return json.RawMessage(`{"receipt":"synthetic-proof"}`), nil }, Restore: func(p json.RawMessage) error {
		if string(p) != `{"receipt":"synthetic-proof"}` {
			return errors.New("invalid receipt")
		}
		return nil
	}}
}
func checkpointTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	return dir
}

// Interrupt every possible successful-read boundary. Each resumed engine must
// rebuild exactly the same heap/sequence/discovery order and financial state.
// Cycles, duplicate receiving roots, equal-time events, IBC and stop labels are
// included. The journal stores bounded batches, never growing graph snapshots.
func checkpointTestLedger() *fakeFlowLedger {
	l := newFakeFlowLedger()
	l.openings[flowTestReceiver] = flowHoldings{Liquid: amt(10)}
	l.commission(flowAt(1, 0), flowTestReceiver, 100)
	l.send(flowAt(2, 0), flowTestReceiver, "child-a", 40)
	l.send(flowAt(2, 0), flowTestReceiver, "child-b", 20)
	l.send(flowAt(3, 0), "child-a", flowTestReceiver, 10)
	l.send(flowAt(4, 0), "child-b", "child-a", 5)
	l.send(flowAt(5, 0), "child-a", flowTestExchange, 12)
	l.rows["child-b"] = append(l.rows["child-b"], flowEvent{Key: flowAt(6, 0), Category: flowCatIBCOut, Dir: apiOut, Amount: amt(2), From: "child-b", Detail: "synthetic-channel"})
	return l
}

type checkpointInterruptLoader struct {
	flowLoader
	left  int
	cause error
}

func (l *checkpointInterruptLoader) next() error {
	if l.left == 0 {
		if l.cause != nil {
			return l.cause
		}
		return errors.New("synthetic deadline")
	}
	l.left--
	return nil
}
func (l *checkpointInterruptLoader) opening(a string, d time.Time) (flowHoldings, bool, error) {
	if e := l.next(); e != nil {
		return flowHoldings{}, false, e
	}
	return l.flowLoader.opening(a, d)
}
func (l *checkpointInterruptLoader) events(a string, s, e time.Time) ([]flowEvent, error) {
	if err := l.next(); err != nil {
		return nil, err
	}
	return l.flowLoader.events(a, s, e)
}
func (l *checkpointInterruptLoader) stop(a string) (flowStop, error) {
	if e := l.next(); e != nil {
		return flowStop{}, e
	}
	return l.flowLoader.stop(a)
}

func checkpointCanonical(t *testing.T, tr *flowTrace) []byte {
	t.Helper()
	type fateRow struct {
		Key    FlowFateKey
		Amount string
	}
	f := []fateRow{}
	for k, v := range tr.Fates {
		f = append(f, fateRow{k, v.String()})
	}
	// Stable result and account/event coverage, independent of progress clocks.
	sortFates := func(i, j int) bool {
		a, _ := json.Marshal(f[i])
		b, _ := json.Marshal(f[j])
		return string(a) < string(b)
	}
	sort.Slice(f, sortFates)
	wallets := []any{}
	for _, w := range tr.order {
		// Pools have unexported fields, so retain each exact compartment explicitly.
		bucketParts := func(m map[string]*flowPool) map[string][]flowPart {
			out := map[string][]flowPart{}
			for k, p := range m {
				out[k] = p.parts
			}
			return out
		}
		compartments := []any{}
		for _, p := range w.pools {
			compartments = append(compartments, []any{p.liquid.parts, bucketParts(p.staked), bucketParts(p.unbonding), p.locked.parts, bucketParts(p.bids)})
		}
		wallets = append(wallets, []any{w.Addr, w.Hop, w.Since, w.Known, w.OpeningSource, w.Overdraw, w.simulated().String(), compartments})
	}
	b, e := json.Marshal([]any{f, tr.Commission, wallets, tr.seq, tr.appliedQueuedEvents, len(tr.queue), tr.Unresolved, tr.discoveryComplete})
	require.NoError(t, e)
	return b
}
func TestFlowCheckpointInterruptResumeCanonical(t *testing.T) {
	baseline := runFlowTest(t, checkpointTestLedger(), flowTestReceiver)
	canonical := checkpointCanonical(t, baseline)
	for boundary := 0; boundary < 18; boundary++ {
		t.Run(fmt.Sprint(boundary), func(t *testing.T) {
			dir := checkpointTestDir(t)
			interrupted := &checkpointInterruptLoader{flowLoader: checkpointTestLedger(), left: boundary}
			first, e := openFlowCheckpoint(dir, checkpointTestID(), interrupted, checkpointTestHooks())
			require.NoError(t, e)
			tr := newFlowTrace(flowTestValidator, flowTestStart, checkpointTestID().AsOf, first)
			err := tr.run([]string{flowTestReceiver})
			if err == nil {
				require.NoError(t, first.check())
			}
			loaded := checkpointTestLedger()
			second, e := openFlowCheckpoint(dir, checkpointTestID(), loaded, checkpointTestHooks())
			require.NoError(t, e)
			resumed := newFlowTrace(flowTestValidator, flowTestStart, checkpointTestID().AsOf, second)
			require.NoError(t, resumed.run([]string{flowTestReceiver}))
			require.NoError(t, second.check())
			require.Equal(t, canonical, checkpointCanonical(t, resumed))
			require.Equal(t, sha256.Sum256(canonical), sha256.Sum256(checkpointCanonical(t, resumed)))
			for _, name := range mustCheckpointEntries(t, dir) {
				info, e := os.Stat(filepath.Join(dir, name))
				require.NoError(t, e)
				require.Equal(t, os.FileMode(0600), info.Mode().Perm())
			}
		})
	}
}
func mustCheckpointEntries(t *testing.T, dir string) []string {
	t.Helper()
	es, e := os.ReadDir(dir)
	require.NoError(t, e)
	names := []string{}
	for _, entry := range es {
		names = append(names, entry.Name())
	}
	return names
}
func TestFlowCheckpointRejectsUnsafePrefix(t *testing.T) {
	for _, mode := range []string{"corrupt", "torn", "gap", "mode", "symlink", "source", "build", "root", "settings", "cutoff", "schema", "receivers", "provenance", "traversal"} {
		t.Run(mode, func(t *testing.T) {
			dir := checkpointTestDir(t)
			id := checkpointTestID()
			hooks := checkpointTestHooks()
			l, e := openFlowCheckpoint(dir, id, checkpointTestLedger(), hooks)
			require.NoError(t, e)
			_, _, e = l.opening(flowTestReceiver, flowTestStart)
			require.NoError(t, e)
			path := filepath.Join(dir, checkpointName(0))
			switch mode {
			case "corrupt":
				require.NoError(t, os.WriteFile(path, []byte("{}"), 0600))
			case "torn":
				require.NoError(t, os.WriteFile(path+".pending", []byte("{"), 0600))
			case "gap":
				require.NoError(t, os.Rename(path, filepath.Join(dir, checkpointName(1))))
			case "mode":
				require.NoError(t, os.Chmod(path, 0644))
			case "symlink":
				require.NoError(t, os.Rename(path, path+".outside"))
				require.NoError(t, os.Symlink(path+".outside", path))
			case "source":
				id.SourceEpoch = "different"
			case "build":
				id.Build = "different"
			case "root":
				id.Root = "different"
			case "settings":
				id.SettingsSHA256 = "different"
			case "cutoff":
				id.AsOf = id.AsOf.Add(time.Hour)
			case "schema":
				id.Schema = 2
			case "receivers":
				id.ReceiversSHA256 = "different"
			case "provenance":
				hooks.Restore = func(json.RawMessage) error { return errors.New("invalid") }
			}
			resumed, e := openFlowCheckpoint(dir, id, checkpointTestLedger(), hooks)
			if mode == "traversal" {
				require.NoError(t, e)
				_, _, e = resumed.opening("different", flowTestStart)
			}
			if mode == "provenance" {
				require.NoError(t, e)
				_, _, e = resumed.opening(flowTestReceiver, flowTestStart)
			}
			require.Error(t, e)
		})
	}
}
func TestFlowCheckpointSourceChangesFailClosed(t *testing.T) {
	dir := checkpointTestDir(t)
	hooks := checkpointTestHooks()
	valid := true
	hooks.VerifySource = func() error {
		if !valid {
			return errors.New("changed")
		}
		return nil
	}
	l, e := openFlowCheckpoint(dir, checkpointTestID(), checkpointTestLedger(), hooks)
	require.NoError(t, e)
	_, _, e = l.opening(flowTestReceiver, flowTestStart)
	require.NoError(t, e)
	valid = false
	_, e = l.events(flowTestReceiver, flowTestStart, checkpointTestID().AsOf)
	require.Error(t, e)
	require.Error(t, l.check())
	_, e = openFlowCheckpoint(dir, checkpointTestID(), checkpointTestLedger(), hooks)
	require.Error(t, e)
}
func TestFlowCheckpointRepeatedResumeDoesNotReread(t *testing.T) {
	dir := checkpointTestDir(t)
	for attempt := 0; attempt < 8; attempt++ {
		base := &checkpointInterruptLoader{flowLoader: checkpointTestLedger(), left: 2}
		l, e := openFlowCheckpoint(dir, checkpointTestID(), base, checkpointTestHooks())
		require.NoError(t, e)
		tr := newFlowTrace(flowTestValidator, flowTestStart, checkpointTestID().AsOf, l)
		e = tr.run([]string{flowTestReceiver})
		if e == nil {
			require.NoError(t, l.check())
			require.Equal(t, checkpointCanonical(t, runFlowTest(t, checkpointTestLedger(), flowTestReceiver)), checkpointCanonical(t, tr))
			return
		}
	}
	t.Fatal("bounded batches did not advance across attempts")
}
