//go:build ops818opening06storedv3 && ops818opening06archive

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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/taxapi/internal/servicehttp"
	"github.com/shopspring/decimal"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var a6Dir = os.Getenv("OPS818_ARCHIVE06_ROOT")

type a6HTTPRecord struct {
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
type a6Transport struct {
	Root, Origin string
	Offline      bool
	Used         map[string]string
	Network      int
	mu           sync.Mutex
}

func (tr *a6Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	key := v6Hash([]byte(r.Method + "/" + r.URL.String()))
	path := filepath.Join(tr.Root, key+".http.private.json")
	var rec a6HTTPRecord
	if e := v6Read(path, &rec); e == nil {
		if rec.RequestSHA != key || rec.EndpointSHA != v6Hash([]byte(tr.Origin)) || rec.BodySHA != v6Hash(rec.Body) {
			return nil, errors.New("cached source HTTP hash rejected")
		}
	} else if !os.IsNotExist(e) {
		return nil, errors.New("source HTTP cache malformed")
	} else {
		if tr.Offline {
			return nil, errors.New("uncaptured source request forbidden during replay")
		}
		u, _ := url.Parse(tr.Origin)
		rec = a6HTTPRecord{EndpointHost: u.Hostname(), EndpointSHA: v6Hash([]byte(tr.Origin)), RequestSHA: key, Query: map[string]string{}}
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
		rec.BodySHA = v6Hash(rec.Body)
		if e = v6Write(path, rec, false); e != nil {
			return nil, e
		}
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	tr.Used[path] = v6Hash(raw)
	if rec.Error != "" {
		return nil, errors.New(rec.Error)
	}
	return &http.Response{StatusCode: rec.Status, Status: strconv.Itoa(rec.Status), Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(rec.Body)), Request: r}, nil
}
func a6Archive(origin string, tr *a6Transport) (*flowArchive, error) {
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

type a6Proof struct {
	Source                string               `json:"source"`
	Controls              v6Controls           `json:"controls"`
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

func a6Validate(p a6Proof, r v6Request) error {
	if p.Source != v6Source || p.Controls.Readonly != "on" || p.Controls.AssignedXID != nil || p.Controls.Database != "cosmostax" || p.Controls.Snapshot == "" {
		return errors.New("archive source/read controls rejected")
	}
	if p.Address != r.Request.Address || !p.Day.Equal(r.Request.Day) || !p.Day.Equal(flowDay(p.Day)) || !p.Known || p.Error != "<nil>" {
		return errors.New("archive opening identity rejected")
	}
	if p.Holdings.Source.Kind != "archive_exact" || p.Holdings.Source.Height != p.Boundary.Height || p.Boundary.Height <= 0 || !p.IndependentlyReplayed {
		return errors.New("archive provenance/replay rejected")
	}
	if len(p.Receipts) < 5 || len(p.RawHTTP) < 5 {
		return errors.New("archive raw state proof incomplete")
	}
	hashes := map[string]bool{}
	for _, rec := range p.Receipts {
		if rec.Address != p.Address || (rec.Height != p.Boundary.Height && rec.Height != p.Boundary.Height+1) || rec.SHA256 != v6Hash([]byte(rec.Response)) {
			return errors.New("archive receipt hash/identity rejected")
		}
		hashes[rec.SHA256] = true
	}
	for _, h := range p.Holdings.Source.ReceiptHashes {
		if !hashes[h] {
			return errors.New("archive holding receipt hash unbound")
		}
	}
	for path, want := range p.RawHTTP {
		b, e := os.ReadFile(path)
		if e != nil || v6Hash(b) != want {
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
func a6Publish(r v6Request, p a6Proof, n int) error {
	if e := a6Validate(p, r); e != nil {
		return e
	}
	lock, e := os.OpenFile(filepath.Join(v6Root, "opening06-reply-data.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		return e
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	pp := filepath.Join(a6Dir, fmt.Sprintf("archive-opening-%06d.proof.private.json", n))
	if e = v6Write(pp, p, false); e != nil {
		return e
	}
	pb, e := os.ReadFile(pp)
	if e != nil {
		return e
	}
	bp := filepath.Join(v6Root, "source-evidence.private.json")
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
	if binding["source"] != v6Source || binding["raw_sha256"] != v6Hash(raw) {
		return errors.New("prior source hash rejected")
	}
	var cumulative map[string]any
	if e = json.Unmarshal(raw, &cumulative); e != nil {
		return e
	}
	if cumulative["source"] != v6Source || cumulative["readonly"] != "on" || cumulative["assigned_xid"] != nil {
		return errors.New("prior source controls rejected")
	}
	channels, ok := cumulative["channels"].([]any)
	if !ok || len(channels) < 44 {
		return errors.New("retained channels missing")
	}
	openings, _ := cumulative["openings"].([]any)
	for _, entry := range openings {
		x := entry.(map[string]any)
		if x["Address"] == p.Address && x["Day"] == p.Day.Format(time.RFC3339Nano) {
			return errors.New("archive opening already supplied")
		}
	}
	cumulative["openings"] = append(openings, v6Opening{Address: p.Address, Day: p.Day, Holdings: p.Holdings, Known: true, Error: "<nil>", SourcePath: pp, SourceSHA256: v6Hash(pb), OriginalSource: v6Source, IndependentlyDecoded: true})
	refs, _ := cumulative["external_proofs"].([]any)
	cumulative["external_proofs"] = append(refs, map[string]any{"path": path, "sha256": v6Hash(raw)}, map[string]any{"path": pp, "sha256": v6Hash(pb)})
	np := filepath.Join(a6Dir, fmt.Sprintf("cumulative-%06d.private.json", n))
	if e = v6Write(np, cumulative, false); e != nil {
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
	binding["raw_sha256"] = v6Hash(nb)
	if e = v6Write(bp, binding, true); e != nil {
		return e
	}
	fmt.Printf("phase exact_archive_opening_supplied count %d proof_sha256 %s\n", n, v6Hash(pb))
	return nil
}
func a6Capture(db *gorm.DB, r v6Request, n int) error {
	rec := &v6Log{Interface: logger.Default.LogMode(logger.Silent)}
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
	var controls v6Controls
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
	var selectedTransport *a6Transport
	var holdings flowHoldings
	endpoints := []string{os.Getenv("OPS818_OWN_RPC"), os.Getenv("H_ARCHIVE_RPC")}
	for index, origin := range endpoints {
		if origin == "" {
			continue
		}
		tr := &a6Transport{Root: filepath.Join(a6Dir, "http"), Origin: origin, Used: map[string]string{}}
		a, e := a6Archive(origin, tr)
		if e != nil {
			return e
		}
		holdings, e = a.holdingsAt(s, apiMainnet, r.Request.Address, r.Request.Day)
		if e != nil {
			if err := v6Write(filepath.Join(a6Dir, fmt.Sprintf("endpoint-%06d-%02d.failure.private.json", n, index)), map[string]any{"source": v6Source, "error": e.Error(), "request": r, "endpoint_sha256": v6Hash([]byte(origin)), "raw_http_sha256": tr.Used, "network_requests": tr.Network, "queries": rec.Queries}, false); err != nil {
				return err
			}
			fmt.Printf("phase historical_endpoint_failed count %d\n", index+1)
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
	replayTransport := &a6Transport{Root: selectedTransport.Root, Origin: selectedTransport.Origin, Offline: true, Used: map[string]string{}}
	replay, e := a6Archive(selectedTransport.Origin, replayTransport)
	if e != nil {
		return e
	}
	again, e := replay.holdingsAt(s, apiMainnet, r.Request.Address, r.Request.Day)
	if e != nil {
		return e
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
	for _, name := range []string{"commissionflows_archive.go", "commissionflows.go", "balancehistory.go", "server.go", "zz_ops818_cohort06_archive_opening_test.go"} {
		b, e := os.ReadFile(filepath.Join("/Users/danb/code/worktrees/cosmos-tax-commission-route-8704/taxapi", name))
		if e != nil {
			return e
		}
		hashes[name] = v6Hash(b)
	}
	missing := "/Users/danb/.cache/ops818-handoff/cohorts0607-exact-20261114T1500/stored-opening-v3-supplier-20261114T1533/missing-000001.proof.private.json"
	mb, e := os.ReadFile(missing)
	if e != nil {
		return e
	}
	p := a6Proof{Source: v6Source, Controls: controls, Address: r.Request.Address, Day: r.Request.Day, Boundary: boundary, Holdings: holdings, Known: true, Error: "<nil>", Receipts: selected.evidence(nil), RawHTTP: selectedTransport.Used, Queries: rec.Queries, CodeHashes: hashes, MissingSourceProof: missing, MissingSourceSHA: v6Hash(mb), IndependentlyReplayed: true}
	return a6Publish(r, p, n)
}
func TestOPS818Cohort06ExactArchiveSupplier(t *testing.T) {
	if a6Dir == "" {
		t.Skip("private archive run only")
	}
	syscall.Umask(0077)
	head, e := exec.Command("git", "rev-parse", "HEAD").Output()
	if e != nil || strings.TrimSpace(string(head)) != v6Source {
		t.Fatal("archive engine source mismatch")
	}
	cmd := exec.Command("git", "diff", "--quiet", v6Source, "--", "taxapi", ":(exclude)taxapi/zz_*")
	if cmd.Run() != nil {
		t.Fatal("production source changed")
	}
	if e = os.MkdirAll(filepath.Join(a6Dir, "http"), 0700); e != nil {
		t.Fatal("archive private cache unavailable")
	}
	db, e := gorm.Open(postgres.Open("host=127.0.0.1 port=15439 user=postgres dbname=cosmostax sslmode=disable application_name=ops818-private-opening06-archive default_transaction_read_only=on"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal("archive metadata transport failed")
	}
	pool, e := db.DB()
	if e != nil {
		t.Fatal("archive pool failed")
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	seen := map[string]bool{}
	n := 0
	for {
		var disk syscall.Statfs_t
		if syscall.Statfs(a6Dir, &disk) != nil || uint64(disk.Bavail)*uint64(disk.Bsize) < 4*1024*1024*1024 {
			t.Fatal("archive reserve stop")
		}
		b, e := os.ReadFile(filepath.Join(v6Root, "extra-bank-request.private.jsonl"))
		if e != nil {
			t.Fatal("archive request journal unavailable")
		}
		for _, line := range bytes.Split(b, []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var r v6Request
			if json.Unmarshal(line, &r) != nil {
				t.Fatal("archive request malformed")
			}
			if r.Kind != "retained-opening" {
				continue
			}
			if r.Source != v6Source || r.Request.Address == "" || !r.Request.Day.Equal(flowDay(r.Request.Day)) {
				t.Fatal("archive request source/day rejected")
			}
			key := v6Hash([]byte(r.Source + "/" + r.Request.Address + "/" + r.Request.Day.Format(time.RFC3339Nano)))
			if seen[key] {
				continue
			}
			n++
			if e = a6Capture(db, r, n); e != nil {
				_ = v6Write(filepath.Join(a6Dir, fmt.Sprintf("archive-%06d.failure.private.json", n)), map[string]any{"error": e.Error(), "request": r}, false)
				t.Fatal("exact archive source failed explicitly")
			}
			seen[key] = true
		}
		time.Sleep(time.Second)
	}
}
