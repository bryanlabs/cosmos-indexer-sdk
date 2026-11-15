package taxapi

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The actual production receipt adapter is used here, not a receipt stub. The
// fixture DB reader supplies only synthetic facts; no financial source is read.
type checkpointEvidenceFixture struct {
	flowLoader
	host   *dbFlowLoader
	budget int
}

func (f *checkpointEvidenceFixture) next(kind, addr string, start, end time.Time) error {
	if f.budget == 0 {
		return context.DeadlineExceeded
	}
	f.budget--
	f.host.s.privateProjectionReceipts.retain([]string{addr}, start, end, CommissionPrivateProjectionReceipt{ProfileSHA256: "profile", SourceSHA256: "source", Snapshot: "original-epoch"})
	f.host.s.sourceCorrectionReceipts.retain(CommissionSourceCorrectionReceipt{ProfileSHA256: "correction", Snapshot: "original-epoch", WalletSHA256: privateSHA([]byte(addr)), Start: start, End: end, Status: kind})
	key, _ := json.Marshal([]any{kind, addr, start, end})
	h := sha256.Sum256(key)
	height := int64(binary.BigEndian.Uint32(h[:4])) + 1
	raw := `{"fixture_hash":"` + privateSHA(key) + `"}`
	cached, reused, err := f.host.archive.headers.get(height, func() (string, error) { return raw, nil })
	if err != nil {
		return err
	}
	f.host.archive.receipts = append(f.host.archive.receipts, FlowArchiveReceipt{Address: addr, Height: height, Method: "block", Response: cached, SHA256: privateSHA([]byte(cached)), Reused: reused})
	return nil
}
func (f *checkpointEvidenceFixture) opening(a string, d time.Time) (flowHoldings, bool, error) {
	if err := f.next("opening", a, d, time.Time{}); err != nil {
		return flowHoldings{}, false, err
	}
	return f.flowLoader.opening(a, d)
}
func (f *checkpointEvidenceFixture) events(a string, s, e time.Time) ([]flowEvent, error) {
	if err := f.next("events", a, s, e); err != nil {
		return nil, err
	}
	return f.flowLoader.events(a, s, e)
}
func (f *checkpointEvidenceFixture) stop(a string) (flowStop, error) {
	if err := f.next("stop", a, time.Time{}, time.Time{}); err != nil {
		return flowStop{}, err
	}
	return f.flowLoader.stop(a)
}
func (f *checkpointEvidenceFixture) holdingsAt(a string, d time.Time) (flowHoldings, bool, error) {
	if err := f.next("closing", a, d, time.Time{}); err != nil {
		return flowHoldings{}, false, err
	}
	return f.flowLoader.opening(a, d)
}
func (f *checkpointEvidenceFixture) restoreCheckpointResult(req flowCheckpointRequest, r flowCheckpointResult) {
	f.host.restoreCheckpointResult(req, r)
}
func checkpointEvidenceAttempt(t *testing.T, dir string, budget int) ([]byte, error) {
	t.Helper()
	host := checkpointReceiptLoader()
	source := &checkpointEvidenceFixture{flowLoader: checkpointTestLedger(), host: host, budget: budget}
	hooks := host.checkpointReceiptHooks()
	hooks.VerifySource = func() error { return nil }
	l, err := openFlowCheckpoint(dir, checkpointTestID(), source, hooks)
	if err != nil {
		return nil, err
	}
	trace := newFlowTrace(flowTestValidator, flowTestStart, checkpointTestID().AsOf, l)
	if err := trace.run([]string{flowTestReceiver}); err != nil {
		return nil, err
	}
	wallets := []CommissionFlowWalletEvidence{}
	for _, w := range trace.order {
		actual, known, err := l.holdingsAt(w.Addr, checkpointTestID().AsOf.AddDate(0, 0, -1))
		if err != nil {
			return nil, err
		}
		if !known {
			return nil, errors.New("fixture closing unknown")
		}
		wallets = append(wallets, commissionFlowWalletEvidence(w, actual, checkpointTestID().AsOf.AddDate(0, 0, -1)))
	}
	if err := l.check(); err != nil {
		return nil, err
	}
	e := host.evidence()
	e.Wallets = wallets
	e.Archive = host.archive.evidence(trace.attempted)
	e.Checkpoint = l.completedEvidence()
	return json.Marshal([]any{json.RawMessage(checkpointCanonical(t, trace)), e})
}
func TestFlowCheckpointCanonicalFactsHashAndPrivateEvidence(t *testing.T) {
	baseline, err := checkpointEvidenceAttempt(t, checkpointTestDir(t), 1000)
	require.NoError(t, err)
	for boundary := 0; boundary < 18; boundary++ {
		dir := checkpointTestDir(t)
		_, _ = checkpointEvidenceAttempt(t, dir, boundary)
		resumed, err := checkpointEvidenceAttempt(t, dir, 1000)
		require.NoError(t, err)
		require.Equal(t, string(baseline), string(resumed), "boundary %d", boundary)
		require.Equal(t, sha256.Sum256(baseline), sha256.Sum256(resumed))
	}
	dir := checkpointTestDir(t)
	for attempt := 0; attempt < 20; attempt++ {
		actual, err := checkpointEvidenceAttempt(t, dir, 2)
		if err == nil {
			require.Equal(t, string(baseline), string(actual))
			return
		}
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
	t.Fatal("receipt-bearing bounded attempts failed to complete")
}
