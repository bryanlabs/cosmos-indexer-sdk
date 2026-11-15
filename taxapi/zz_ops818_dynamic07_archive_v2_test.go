//go:build ops818dynamic07openingv2

package taxapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/taxapi/internal/servicehttp"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var b8Dir = os.Getenv("OPS818_DYNAMIC07_ARCHIVE_ROOT")
var b8AlreadySupplied = errors.New("exact opening already supplied")

func b8CacheRoot() string {
	if p := os.Getenv("OPS818_DYNAMIC07_HTTP_CACHE"); p != "" {
		return p
	}
	return filepath.Join(b8Dir, "http")
}

type b8HTTPRecord struct {
	EndpointHost string            `json:"endpoint_host"`
	EndpointSHA  string            `json:"endpoint_sha256"`
	RequestSHA   string            `json:"request_sha256"`
	Method       string            `json:"method"`
	Query        map[string]string `json:"query"`
	Status       int               `json:"status"`
	Body         []byte            `json:"body"`
	BodySHA      string            `json:"body_sha256"`
	Error        string            `json:"error"`
}
type b8Transport struct {
	Root, Origin string
	Offline      bool
	Used         map[string]string
	Network      int
	mu           sync.Mutex
}

func (tr *b8Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	key := v8Hash([]byte(r.Method + "/" + r.URL.String()))
	path := filepath.Join(tr.Root, key+".http.private.json")
	var rec b8HTTPRecord
	if e := v8Read(path, &rec); e == nil {
		if rec.RequestSHA != key || rec.EndpointSHA != v8Hash([]byte(tr.Origin)) || rec.BodySHA != v8Hash(rec.Body) {
			return nil, errors.New("cached source HTTP hash rejected")
		}
	} else if !os.IsNotExist(e) {
		return nil, errors.New("source HTTP cache malformed")
	} else {
		if tr.Offline {
			return nil, errors.New("uncaptured source request forbidden during replay")
		}
		u, _ := url.Parse(tr.Origin)
		rec = b8HTTPRecord{EndpointHost: u.Hostname(), EndpointSHA: v8Hash([]byte(tr.Origin)), RequestSHA: key, Query: map[string]string{}}
		if strings.HasSuffix(r.URL.Path, "/block") {
			rec.Method = "block"
		} else if strings.HasSuffix(r.URL.Path, "/abci_query") {
			rec.Method = "abci_query"
		} else {
			return nil, errors.New("unexpected historical source RPC method")
		}
		for _, k := range []string{"height", "path", "data"} {
			rec.Query[k] = r.URL.Query().Get(k)
		}
		fmt.Printf("phase archive_http_started count %d\n", tr.Network+1)
		res, e := http.DefaultTransport.RoundTrip(r)
		tr.Network++
		if e != nil {
			rec.Error = "historical source HTTP transport failed"
		} else {
			rec.Status = res.StatusCode
			rec.Body, e = io.ReadAll(io.LimitReader(res.Body, 64*1024*1024+1))
			res.Body.Close()
			if e != nil || len(rec.Body) > 64*1024*1024 {
				rec.Error = "historical source HTTP response incomplete or oversized"
			}
		}
		rec.BodySHA = v8Hash(rec.Body)
		if e = v8Write(path, rec, false); e != nil {
			return nil, e
		}
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	tr.Used[path] = v8Hash(raw)
	if rec.Error != "" {
		return nil, errors.New(rec.Error)
	}
	return &http.Response{StatusCode: rec.Status, Status: strconv.Itoa(rec.Status), Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(rec.Body)), Request: r}, nil
}
func b8Archive(origin string, tr *b8Transport) (*flowArchive, error) {
	a, e := newFlowArchive(origin)
	if e != nil {
		return nil, e
	}
	ep, e := servicehttp.New(&http.Client{Transport: tr, Timeout: 30 * time.Second}, origin)
	if e != nil {
		return nil, errors.New("archive source endpoint rejected")
	}
	a.rpc = ep
	return a, nil
}

type b8EndpointAttempt struct {
	OriginSHA      string            `json:"origin_sha256"`
	Failed         bool              `json:"failed"`
	Status         int               `json:"status"`
	PruningBodySHA string            `json:"pruning_body_sha256"`
	RawHTTP        map[string]string `json:"raw_http_sha256"`
}

type b8Proof struct {
	AttemptedEndpoints    []b8EndpointAttempt  `json:"attempted_endpoints"`
	Source                string               `json:"source"`
	Controls              v8Controls           `json:"controls"`
	Address               string               `json:"address"`
	Day                   time.Time            `json:"day"`
	Boundary              heightTime           `json:"boundary"`
	Holdings              flowHoldings         `json:"holdings"`
	Known                 bool                 `json:"known"`
	Error                 string               `json:"error"`
	Receipts              []FlowArchiveReceipt `json:"receipts"`
	RawHTTP               map[string]string    `json:"raw_http_sha256"`
	Queries               []string             `json:"queries"`
	CodeHashes            map[string]string    `json:"code_sha256"`
	MissingSourceProof    string               `json:"missing_stored_source_proof"`
	MissingSourceSHA      string               `json:"missing_stored_source_sha256"`
	IndependentlyReplayed bool                 `json:"independently_replayed_from_raw_http"`
}

func b8Validate(p b8Proof, r v8Request) error {
	if e := b8ValidateAttempts(p); e != nil {
		return e
	}
	if p.Source != v8Source || p.Controls.Readonly != "on" || p.Controls.AssignedXID != nil || p.Controls.Database != "cosmostax" || p.Controls.Snapshot == "" {
		return errors.New("archive source/read controls rejected")
	}
	if p.Address != r.Request.Address || !p.Day.Equal(r.Request.Day) || !p.Day.Equal(flowDay(p.Day)) || !p.Known || p.Error != "<nil>" {
		return errors.New("archive opening identity rejected")
	}
	if p.Holdings.Source.Kind != "archive_exact" || p.Holdings.Source.Height != p.Boundary.Height || p.Boundary.Height <= 0 || !p.IndependentlyReplayed {
		return errors.New("archive provenance/replay rejected")
	}
	missingBytes, e := os.ReadFile(p.MissingSourceProof)
	if e != nil || v8Hash(missingBytes) != p.MissingSourceSHA {
		return errors.New("exact missing source hash rejected")
	}
	var memo struct {
		Source   string     `json:"source"`
		Controls v8Controls `json:"controls"`
		Request  v8Request  `json:"request"`
		Phase    string     `json:"phase"`
	}
	if json.Unmarshal(missingBytes, &memo) != nil || memo.Source != v8Source || memo.Controls.Readonly != "on" || memo.Controls.AssignedXID != nil || memo.Controls.Database != "cosmostax" || memo.Request.Request.Address != p.Address || !memo.Request.Request.Day.Equal(p.Day) || memo.Phase != "exact-stored-opening-unavailable-no-ledger-fallback" {
		return errors.New("exact missing source request binding rejected")
	}
	if len(p.Receipts) < 5 || len(p.RawHTTP) < 5 {
		return errors.New("archive raw state proof incomplete")
	}
	hashes := map[string]bool{}
	for _, rec := range p.Receipts {
		if rec.Address != p.Address || (rec.Height != p.Boundary.Height && rec.Height != p.Boundary.Height+1) || rec.SHA256 != v8Hash([]byte(rec.Response)) {
			return errors.New("archive receipt hash/identity rejected")
		}
		hashes[rec.SHA256] = true
	}
	for _, receipt := range p.Receipts {
		linked := false
		for path, want := range p.RawHTTP {
			b, e := os.ReadFile(path)
			if e != nil || v8Hash(b) != want {
				return errors.New("raw archive source file changed")
			}
			var raw b8HTTPRecord
			if json.Unmarshal(b, &raw) != nil || raw.BodySHA != v8Hash(raw.Body) {
				return errors.New("raw source body hash rejected")
			}
			var response json.RawMessage
			if json.Unmarshal(raw.Body, &response) != nil {
				continue
			}
			if raw.Method == receipt.Method && raw.Query["height"] == strconv.FormatInt(receipt.Height, 10) && raw.Query["path"] == receipt.QueryPath && raw.Query["data"] == receipt.RequestData && v8Hash(response) == receipt.SHA256 {
				linked = true
				break
			}
		}
		if !linked {
			return errors.New("receipt not linked to actual raw HTTP body/request")
		}
	}
	for _, h := range p.Holdings.Source.ReceiptHashes {
		if !hashes[h] {
			return errors.New("archive holding receipt hash unbound")
		}
	}
	for path, want := range p.RawHTTP {
		b, e := os.ReadFile(path)
		if e != nil || v8Hash(b) != want {
			return errors.New("raw source proof hash changed")
		}
	}
	if p.Holdings.Liquid.IsNegative() {
		return errors.New("negative archive opening")
	}
	for _, m := range []map[string]decimal.Decimal{p.Holdings.Staked, p.Holdings.Unbonding} {
		for _, v := range m {
			if v.IsNegative() {
				return errors.New("negative archive stake")
			}
		}
	}
	for _, q := range p.Queries {
		if strings.Contains(q, "message_event_attributes") || strings.Contains(q, "block_event_attributes") {
			return errors.New("native event query forbidden")
		}
	}
	return nil
}
func b8Publish(r v8Request, p b8Proof, n int) error {
	if e := b8Validate(p, r); e != nil {
		return e
	}
	lock, e := os.OpenFile(filepath.Join(v8Root, "shared07-reply-data.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		return e
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	pp := filepath.Join(b8Dir, fmt.Sprintf("archive-opening-%06d.proof.private.json", n))
	if e = v8Write(pp, p, false); e != nil {
		return e
	}
	pb, e := os.ReadFile(pp)
	if e != nil {
		return e
	}
	bp := filepath.Join(v8Root, "source-evidence.private.json")
	prior, e := os.ReadFile(bp)
	if e != nil {
		return e
	}
	var binding map[string]any
	if e = json.Unmarshal(prior, &binding); e != nil {
		return e
	}
	path, ok := binding["raw_path"].(string)
	if !ok {
		return errors.New("prior raw path invalid")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	if binding["source"] != v8Source || binding["raw_sha256"] != v8Hash(raw) {
		return errors.New("prior source hash rejected")
	}
	var cumulative map[string]any
	if e = json.Unmarshal(raw, &cumulative); e != nil {
		return e
	}
	if cumulative["source"] != v8Source || cumulative["readonly"] != "on" || cumulative["assigned_xid"] != nil {
		return errors.New("prior source controls rejected")
	}
	channels, ok := cumulative["channels"].([]any)
	if !ok || len(channels) < 44 {
		return errors.New("retained channels missing")
	}
	openings, _ := cumulative["openings"].([]any)
	for _, entry := range openings {
		x := entry.(map[string]any)
		day, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(x["Day"]))
		if x["Address"] == p.Address && day.Equal(p.Day) {
			return b8AlreadySupplied
		}
	}
	cumulative["openings"] = append(openings, v8Opening{Address: p.Address, Day: p.Day, Holdings: p.Holdings, Known: true, Error: "<nil>", SourcePath: pp, SourceSHA256: v8Hash(pb), OriginalSource: v8Source, IndependentlyDecoded: true})
	refs, _ := cumulative["external_proofs"].([]any)
	cumulative["external_proofs"] = append(refs, map[string]any{"path": path, "sha256": v8Hash(raw)}, map[string]any{"path": pp, "sha256": v8Hash(pb)})
	np := filepath.Join(b8Dir, fmt.Sprintf("cumulative-%06d.private.json", n))
	if e = v8Write(np, cumulative, false); e != nil {
		return e
	}
	nb, e := os.ReadFile(np)
	if e != nil {
		return e
	}
	now, e := os.ReadFile(bp)
	if e != nil || !bytes.Equal(now, prior) {
		return errors.New("reply changed; no replacement")
	}
	binding["raw_path"] = np
	binding["raw_sha256"] = v8Hash(nb)
	if e = v8Write(bp, binding, true); e != nil {
		return e
	}
	fmt.Printf("phase exact_archive_opening_supplied count %d proof_sha256 %s\n", n, v8Hash(pb))
	return nil
}
func b8Capture(db *gorm.DB, r v8Request, n int, missing string) error {
	rec := &v8Log{Interface: logger.Default.LogMode(logger.Silent)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tx := db.WithContext(ctx).Session(&gorm.Session{Logger: rec}).Begin(&sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if tx.Error != nil {
		return tx.Error
	}
	defer tx.Rollback()
	for _, q := range []string{"SET LOCAL statement_timeout='180s'", "SET LOCAL lock_timeout='2s'"} {
		if e := tx.Exec(q).Error; e != nil {
			return e
		}
	}
	var controls v8Controls
	control := func() error {
		if e := tx.Raw("SELECT current_setting('transaction_read_only') AS readonly,txid_current_if_assigned() AS assigned_x_id,txid_current_snapshot()::text AS snapshot,current_database() AS database").Scan(&controls).Error; e != nil {
			return e
		}
		if controls.Readonly != "on" || controls.AssignedXID != nil {
			return errors.New("archive metadata snapshot rejected")
		}
		return nil
	}
	if e := control(); e != nil {
		return e
	}
	s := &Server{db: tx, native: DefaultNativeAsset, readOnlyLedger: true}
	ids, e := s.chainRowIDs(apiMainnet)
	if e != nil {
		return e
	}
	boundary, e := s.dayEndBlock(ids, r.Request.Day)
	if e != nil {
		return e
	}
	var selected *flowArchive
	var selectedTransport *b8Transport
	var holdings flowHoldings
	own := os.Getenv("OPS818_OWN_RPC")
	if own == "" {
		return errors.New("own node endpoint required")
	}
	var attempts []b8EndpointAttempt
	endpoints := []string{own, os.Getenv("H_ARCHIVE_RPC")}
	for index, origin := range endpoints {
		if origin == "" {
			continue
		}
		tr := &b8Transport{Root: b8CacheRoot(), Origin: origin, Used: map[string]string{}}
		a, e := b8Archive(origin, tr)
		if e != nil {
			return e
		}
		holdings, e = a.holdingsAt(s, apiMainnet, r.Request.Address, r.Request.Day)
		attempt := b8EndpointAttempt{OriginSHA: v8Hash([]byte(origin)), Failed: e != nil, RawHTTP: tr.Used}
		for path := range tr.Used {
			var raw b8HTTPRecord
			if v8Read(path, &raw) == nil {
				attempt.Status = raw.Status
				if b8PruningEvidence(&b8Transport{Used: map[string]string{path: tr.Used[path]}}) {
					attempt.PruningBodySHA = raw.BodySHA
					break
				}
			}
		}
		attempts = append(attempts, attempt)
		if e != nil {
			if err := v8Write(filepath.Join(b8Dir, fmt.Sprintf("endpoint-%06d-%02d.failure.private.json", n, index)), map[string]any{"source": v8Source, "error": e.Error(), "request": r, "endpoint_sha256": v8Hash([]byte(origin)), "raw_http_sha256": tr.Used, "network_requests": tr.Network, "queries": rec.Queries}, false); err != nil {
				return err
			}
			fmt.Printf("phase historical_endpoint_failed count %d\n", index+1)
			if index == 0 && !b8PruningEvidence(tr) {
				return errors.New("own node failed without actual pruning proof; archive fallback refused")
			}
			continue
		}
		selected = a
		selectedTransport = tr
		break
	}
	if selected == nil {
		return errors.New("all actual historical source endpoints failed; explicit raw cache retained")
	}
	// Re-decode every response offline through the original source methods. No
	// second HTTP read, and no event/native capture or financial acceptance.
	replayTransport := &b8Transport{Root: selectedTransport.Root, Origin: selectedTransport.Origin, Offline: true, Used: map[string]string{}}
	replay, e := b8Archive(selectedTransport.Origin, replayTransport)
	if e != nil {
		return e
	}
	again, e := replay.holdingsAt(s, apiMainnet, r.Request.Address, r.Request.Day)
	if e != nil {
		return e
	}
	usedBefore, _ := json.Marshal(selectedTransport.Used)
	usedAfter, _ := json.Marshal(replayTransport.Used)
	if !bytes.Equal(usedBefore, usedAfter) {
		return errors.New("offline replay raw source set differs")
	}
	x, _ := json.Marshal(holdings)
	y, _ := json.Marshal(again)
	if !bytes.Equal(x, y) || replayTransport.Network != 0 {
		return errors.New("offline production archive decode differs")
	}
	if e = control(); e != nil {
		return e
	}
	hashes := map[string]string{}
	for _, name := range []string{"commissionflows_archive.go", "commissionflows.go", "balancehistory.go", "server.go", "zz_ops818_dynamic07_archive_v2_test.go", "zz_ops818_dynamic07_stored_v2_test.go"} {
		b, e := os.ReadFile(filepath.Join("/Users/danb/code/worktrees/cosmos-tax-commission-route-8704/taxapi", name))
		if e != nil {
			return e
		}
		hashes[name] = v8Hash(b)
	}
	mb, e := os.ReadFile(missing)
	if e != nil {
		return e
	}
	p := b8Proof{AttemptedEndpoints: attempts, Source: v8Source, Controls: controls, Address: r.Request.Address, Day: r.Request.Day, Boundary: boundary, Holdings: holdings, Known: true, Error: "<nil>", Receipts: selected.evidence(nil), RawHTTP: selectedTransport.Used, Queries: rec.Queries, CodeHashes: hashes, MissingSourceProof: missing, MissingSourceSHA: v8Hash(mb), IndependentlyReplayed: true}
	return b8Publish(r, p, n)
}
func b8Admit(r v8Request) (missing string, supplied bool, err error) {
	lock, e := os.OpenFile(filepath.Join(v8Root, "shared07-reply-data.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return "", false, e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		return "", false, e
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	var bind struct {
		Source string `json:"source"`
		Path   string `json:"raw_path"`
		SHA    string `json:"raw_sha256"`
	}
	if e = v8Read(filepath.Join(v8Root, "source-evidence.private.json"), &bind); e != nil {
		return "", false, e
	}
	raw, e := os.ReadFile(bind.Path)
	if e != nil || bind.Source != v8Source || v8Hash(raw) != bind.SHA {
		return "", false, errors.New("archive admission reply hash rejected")
	}
	var cumulative struct {
		Source   string          `json:"source"`
		Readonly string          `json:"readonly"`
		XID      json.RawMessage `json:"assigned_xid"`
		Openings []v8Opening     `json:"openings"`
	}
	if json.Unmarshal(raw, &cumulative) != nil || cumulative.Source != v8Source || cumulative.Readonly != "on" || string(cumulative.XID) != "null" {
		return "", false, errors.New("archive admission source controls rejected")
	}
	for _, o := range cumulative.Openings {
		if o.Address == r.Request.Address && o.Day.Equal(r.Request.Day) {
			return "", true, nil
		}
	}
	paths, e := filepath.Glob(filepath.Join(v8Dir, "missing-*.proof.private.json"))
	if e != nil {
		return "", false, e
	}
	for _, path := range paths {
		var memo struct {
			Source   string     `json:"source"`
			Controls v8Controls `json:"controls"`
			Request  v8Request  `json:"request"`
			Phase    string     `json:"phase"`
		}
		if v8Read(path, &memo) != nil {
			return "", false, errors.New("stored absence memo malformed")
		}
		if memo.Source == v8Source && memo.Controls.Readonly == "on" && memo.Controls.AssignedXID == nil && memo.Controls.Database == "cosmostax" && memo.Request.Request.Address == r.Request.Address && memo.Request.Request.Day.Equal(r.Request.Day) && memo.Phase == "exact-stored-opening-unavailable-no-ledger-fallback" {
			return path, false, nil
		}
	}
	return "", false, nil
}

// Only an actual retained pruning response authorizes fallback, never a timeout.
func b8PruningEvidence(tr *b8Transport) bool {
	for p := range tr.Used {
		var r b8HTTPRecord
		if v8Read(p, &r) != nil || r.Error != "" {
			continue
		}
		text := strings.ToLower(string(r.Body))
		for _, token := range []string{"lowest height", "pruned", "height is not available", "height not available", "version does not exist", "failed to load state at height"} {
			if strings.Contains(text, token) {
				return true
			}
		}
	}
	return false
}
