package taxapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The live check runs one root's private trace against a read-only database
// connection, stops it at a fixed number of wallet openings, and writes a
// digest of everything the trace decided and every read it made, so a serial
// run and a read-ahead run can be compared. It writes only to its own
// directory and prints no addresses or amounts.
//
//	COMMISSION_PREFETCH_LIVE_DIR   directory with validator.txt, profile.json,
//	                               source-corrections.json, probe.toml, env.json
//	COMMISSION_PREFETCH_LIVE_SLOTS read-ahead slots, 0 for serial
//	COMMISSION_PREFETCH_LIVE_WALLETS openings before the trace is stopped
//	COMMISSION_PREFETCH_LIVE_CLOSING closing reads to compare after the stop
//	COMMISSION_PREFETCH_LIVE_OUT   output file for the digests and timings

var errLiveCap = errors.New("live check wallet cap reached")

type liveCapLoader struct {
	*dbFlowLoader
	cap    int
	mu     sync.Mutex
	calls  []string
	opened []time.Duration
	start  time.Time
	// spent is the time the trace waited in each call, by call and by what
	// the read-ahead held for the wallet when the trace asked.
	spent map[string]float64
	count map[string]int
}

func (c *liveCapLoader) ahead(addr string) string {
	p := c.pre
	if p == nil {
		return "serial"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	j := p.jobs[addr]
	switch {
	case j == nil:
		return "none"
	case j.state == flowJobQueued:
		return "queued"
	case j.state == flowJobRunning:
		return "running"
	}
	return "done"
}

func (c *liveCapLoader) note(call, state string, began time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spent == nil {
		c.spent, c.count = map[string]float64{}, map[string]int{}
	}
	c.spent[call+"|"+state] += time.Since(began).Seconds()
	c.count[call+"|"+state]++
}

func (c *liveCapLoader) opening(addr string, day time.Time) (flowHoldings, bool, error) {
	c.mu.Lock()
	if len(c.opened) >= c.cap {
		c.mu.Unlock()
		return flowHoldings{}, false, errLiveCap
	}
	c.mu.Unlock()
	state, began := c.ahead(addr), time.Now()
	h, known, err := c.dbFlowLoader.opening(addr, day)
	src := h.Source.Kind
	c.note("opening:"+src, state, began)
	b, _ := json.Marshal([]any{h, known, errString(err)})
	c.mu.Lock()
	c.calls = append(c.calls, "opening|"+addr+"|"+day.String()+"|"+string(b))
	c.opened = append(c.opened, time.Since(c.start))
	c.mu.Unlock()
	return h, known, err
}

func (c *liveCapLoader) events(addr string, start, end time.Time) ([]flowEvent, error) {
	state, began := c.ahead(addr), time.Now()
	ev, err := c.dbFlowLoader.events(addr, start, end)
	c.note("events", state, began)
	b, _ := json.Marshal([]any{ev, errString(err)})
	c.mu.Lock()
	c.calls = append(c.calls, "events|"+addr+"|"+start.String()+"|"+end.String()+"|"+string(b))
	c.mu.Unlock()
	return ev, err
}

func (c *liveCapLoader) stop(addr string, start, end time.Time) (flowStop, error) {
	state, began := c.ahead(addr), time.Now()
	s, err := c.dbFlowLoader.stop(addr, start, end)
	c.note("stop", state, began)
	b, _ := json.Marshal([]any{s.Class, s.Detail, errString(err)})
	c.mu.Lock()
	c.calls = append(c.calls, "stop|"+addr+"|"+string(b))
	c.mu.Unlock()
	return s, err
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func hashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestCommissionPrefetchLive(t *testing.T) {
	dir := os.Getenv("COMMISSION_PREFETCH_LIVE_DIR")
	if dir == "" {
		t.Skip("live check only")
	}
	slots, _ := strconv.Atoi(os.Getenv("COMMISSION_PREFETCH_LIVE_SLOTS"))
	capN, _ := strconv.Atoi(os.Getenv("COMMISSION_PREFETCH_LIVE_WALLETS"))
	closingN, _ := strconv.Atoi(os.Getenv("COMMISSION_PREFETCH_LIVE_CLOSING"))
	require.Positive(t, capN)
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return strings.TrimSpace(string(b))
	}
	var env map[string]string
	require.NoError(t, json.Unmarshal([]byte(read("env.json")), &env))
	validator := read("validator.txt")
	rpc := regexp.MustCompile(`rpc\s*=\s*"([^"]+)"`).FindStringSubmatch(read("probe.toml"))
	require.Len(t, rpc, 2)

	dsn := fmt.Sprintf("host=%s port=%s dbname=%s user=%s password=%s sslmode=disable default_transaction_read_only=on jit=off effective_io_concurrency=200",
		os.Getenv("COMMISSION_PREFETCH_LIVE_DB_HOST"), os.Getenv("COMMISSION_PREFETCH_LIVE_DB_PORT"), env["DB_NAME"], env["DB_USER"], env["DB_PASS"])
	gdb, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	var ro string
	require.NoError(t, gdb.Raw("SHOW default_transaction_read_only").Scan(&ro).Error)
	require.Equal(t, "on", ro, "the live check must run read-only")

	profile, err := LoadCommissionPrivateProfile(filepath.Join(dir, "profile.json"), "53da2904e3a6893e92745b1db9766b356da09042f52ec5384d0beb11a46aa913")
	require.NoError(t, err)
	corrections, err := LoadCommissionSourceCorrections(filepath.Join(dir, "source-corrections.json"), "40d6961721383adb2aa8bf4ad19ac9b8df7f834a3687dd1cd63a144bef654eb4")
	require.NoError(t, err)
	node, err := NewLedgerNode()
	require.NoError(t, err)
	chain := apiMainnet
	from := time.Date(2024, 3, 21, 0, 0, 0, 0, time.UTC)
	asOf := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)

	// The same reader and loader BuildCommissionFlows makes for a versioned run.
	s := NewServer(gdb, NewOracle(os.Getenv("ORACLE_URL"), os.Getenv("NODE_REST_API")), DefaultNativeAsset)
	reader := NewServer(s.db, s.oracle, s.native)
	reader.readOnlyLedger = true
	reader.privateReadSnapshotTimeout = 60 * time.Second
	reader.commissionPrivateProfile = profile
	reader.commissionSourceCorrections = corrections
	reader.sourceCorrectionReceipts = newCorrectionJournal()
	reader.privateProjectionReceipts = &commissionProjectionJournal{rows: map[string]CommissionPrivateProjectionReceipt{}}
	validators, err := s.commissionValidators(chain)
	require.NoError(t, err)
	derived := derivedFlowLabels(chain, validators)
	var labels []AddressLabel
	require.NoError(t, s.db.Where("chain = ?", chain).Find(&labels).Error)
	base := &dbFlowLoader{s: reader, chain: chain, node: node, labels: map[string]AddressLabel{}, mu: &sync.Mutex{}, chans: map[string]string{}, private: true, prefetchN: slots}
	base.archive, err = newFlowArchive(rpc[1])
	require.NoError(t, err)
	for _, l := range labels {
		base.labels[l.Address] = l
	}
	for _, l := range derived {
		if _, exists := base.labels[l.Address]; !exists {
			base.labels[l.Address] = l
		}
	}

	// The same steps traceCommissionSaved takes before the trace runs.
	rs, loader := reader.privateProjectionTrace(base)
	if loader.prefetchN > 0 {
		own := *loader
		own.pre = newFlowPrefetch(&own, own.prefetchN)
		loader = &own
		defer loader.closePrefetch()
	}
	receivers, err := rs.commissionReceivers(chain, validator)
	require.NoError(t, err)
	group := []string{}
	seen := map[string]bool{}
	for _, r := range receivers {
		if r.Address != "" && !seen[r.Address] {
			seen[r.Address] = true
			group = append(group, r.Address)
		}
	}
	if op := operatorAccount(validator); op != "" && !seen[op] {
		group = append(group, op)
	}

	capped := &liveCapLoader{dbFlowLoader: loader, cap: capN, start: time.Now()}
	tr := newFlowTrace(validator, from, asOf, capped)
	tr.strict = true
	runErr := tr.run(group)
	traceTime := time.Since(capped.start)
	require.ErrorIs(t, runErr, errLiveCap, "the trace must stop at the cap, not on another error")

	// Closing reads for the first wallets of the order, as the closing loop
	// makes them.
	asOfDay := asOf.AddDate(0, 0, -1)
	order := tr.order
	if closingN < len(order) {
		order = order[:closingN]
	}
	closingStart := time.Now()
	closing := loader.closingReads(order, asOfDay)
	var closed []string
	for i, w := range order {
		h, known, err := closing.holdingsAt(loader, i, w.Addr, asOfDay)
		b, _ := json.Marshal([]any{w.Addr, h, known, errString(err)})
		closed = append(closed, string(b))
	}
	closing.close()
	closingTime := time.Since(closingStart)

	evidence := loader.evidence()
	for i := range evidence.PrivateProjections {
		evidence.PrivateProjections[i].Snapshot = ""
	}
	for i := range evidence.SourceCorrectionReads {
		evidence.SourceCorrectionReads[i].Snapshot = ""
	}
	for i := range evidence.Labels {
		evidence.Labels[i].CreatedAt = time.Time{}
	}
	sortLabels(evidence.Labels)
	// The journals key receipts by a hash that includes the snapshot name,
	// so with the snapshot blanked they are put in content order again.
	sortByJSON(evidence.PrivateProjections)
	sortByJSON(evidence.SourceCorrectionReads)
	archive := loader.archive.evidence(tr.attempted)

	out := map[string]any{
		"slots":               slots,
		"cap":                 capN,
		"wallets_in_order":    len(tr.order),
		"openings":            len(capped.opened),
		"loader_calls":        len(capped.calls),
		"trace_seconds":       traceTime.Seconds(),
		"openings_per_second": float64(len(capped.opened)) / traceTime.Seconds(),
		"closing_reads":       len(order),
		"closing_seconds":     closingTime.Seconds(),
		"closing_per_second":  float64(len(order)) / closingTime.Seconds(),
		"trace_digest":        flowTraceDigest(tr),
		"calls_digest":        hashJSON(capped.calls),
		"closing_digest":      hashJSON(closed),
		"projection_digest":   hashJSON(evidence.PrivateProjections),
		"correction_digest":   hashJSON(evidence.SourceCorrectionReads),
		"labels_digest":       hashJSON(evidence.Labels),
		"channels_digest":     hashJSON(evidence.Channels),
		"archive_digest":      hashJSON(archive),
		"archive_receipts":    len(archive),
		"projection_receipts": len(evidence.PrivateProjections),
		"correction_receipts": len(evidence.SourceCorrectionReads),
		"opened_at_seconds":   durationsSeconds(capped.opened),
		"spent_seconds":       capped.spent,
		"spent_count":         capped.count,
		"ahead_counts":        tr.ahead.counts,
	}
	b, err := json.MarshalIndent(out, "", " ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(os.Getenv("COMMISSION_PREFETCH_LIVE_OUT"), b, 0o600))
}

func sortByJSON[T any](v []T) {
	keys := make([]string, len(v))
	for i := range v {
		b, _ := json.Marshal(v[i])
		keys[i] = string(b)
	}
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func sortLabels(l []AddressLabel) {
	for i := 1; i < len(l); i++ {
		for j := i; j > 0 && l[j].Address < l[j-1].Address; j-- {
			l[j], l[j-1] = l[j-1], l[j]
		}
	}
}

func durationsSeconds(d []time.Duration) []float64 {
	out := make([]float64, len(d))
	for i, x := range d {
		out[i] = float64(x.Milliseconds()) / 1000
	}
	return out
}
