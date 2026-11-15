//go:build ops818dynamic07openingv3

package taxapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This one-shot supplier services only the latest literal retained-opening request.
// A lifetime lock prevents a second owned capture, while the shared reply lock
// serializes additive publication with the metadata owner.
func TestOPS818Dynamic07V3OpeningV1Capture(t *testing.T) {
	if os.Getenv("OPS818_DYNAMIC07_CAPTURE") != "1" {
		t.Skip("private one-shot only")
	}
	syscall.Umask(0077)
	if os.Getenv("OPS818_OWN_RPC") == "" {
		t.Fatal("own node endpoint required before capture")
	}
	root := filepath.Dir(v9Dir)
	lock, e := os.OpenFile(filepath.Join(v9Root, "dynamic07-opening-capture.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal("capture lock unavailable")
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		t.Fatal("conflicting opening capture, no source read")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	head, e := exec.Command("git", "rev-parse", "HEAD").Output()
	if e != nil || strings.TrimSpace(string(head)) != v9Source {
		t.Fatal("source mismatch")
	}
	if exec.Command("git", "diff", "--quiet", v9Source, "--", ".", ":(exclude)**/zz_*").Run() != nil {
		t.Fatal("tracked production source drift")
	}
	for _, p := range []string{root, v9Dir, b9Dir, b9CacheRoot(), os.Getenv("TMPDIR"), os.Getenv("GOTMPDIR")} {
		s, e := os.Stat(p)
		if e != nil || !s.IsDir() || s.Mode().Perm() != 0700 {
			t.Fatal("private directory mode rejected")
		}
	}
	os.Setenv("NODE_REST_API", "http://127.0.0.1:1")
	os.Setenv("NODE_RPC_API", "http://127.0.0.1:1")
	raw, e := os.ReadFile(filepath.Join(v9Root, "extra-bank-request.private.jsonl"))
	if e != nil {
		t.Fatal("journal unavailable")
	}
	var r v9Request
	lines := bytes.Split(raw, []byte("\n"))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var x v9Request
		if json.Unmarshal(line, &x) != nil {
			t.Fatal("partial or malformed journal")
		}
		r = x
	}
	if r.Kind != "retained-opening" || r.Source != v9Source || r.Request.Address == "" || !r.Request.Day.Equal(flowDay(r.Request.Day)) {
		t.Fatal("latest exact request rejected")
	}
	if e = v9Write(filepath.Join(root, "actual-request.private.json"), r, false); e != nil {
		t.Fatal("request binding failed")
	}
	if _, supplied, e := b9Admit(r); e != nil || supplied {
		t.Fatal("opening already supplied or reply rejected, no duplicate capture")
	}
	var disk syscall.Statfs_t
	if syscall.Statfs(root, &disk) != nil || uint64(disk.Bavail)*uint64(disk.Bsize) < 4*1024*1024*1024 {
		t.Fatal("private reserve stop")
	}
	db, e := gorm.Open(postgres.Open("host=127.0.0.1 port=15439 user=postgres dbname=cosmostax sslmode=disable application_name=ops818-private-dynamic07-opening-v1 default_transaction_read_only=on"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal("readonly source transport failed")
	}
	pool, e := db.DB()
	if e != nil {
		t.Fatal("pool unavailable")
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	start := time.Now()
	e = v9Capture(db, r, 1)
	if e != nil && !errors.Is(e, v9MissingStored) {
		t.Fatal("stored source failed, private evidence retained")
	}
	if errors.Is(e, v9MissingStored) {
		missing, supplied, e := b9Admit(r)
		if e != nil || supplied || missing == "" {
			t.Fatal("stored absence admission rejected")
		}
		if e = b9Capture(db, r, 1, missing); e != nil {
			_ = v9Write(filepath.Join(root, "capture-failure.private.json"), map[string]any{"error": e.Error(), "request": r}, false)
			t.Fatal("native source failed, private evidence retained")
		}
	}
	if _, supplied, e := b9Admit(r); e != nil || !supplied {
		t.Fatal("actual published opening missing")
	}
	if e = v9Write(filepath.Join(root, "capture-complete.private.json"), map[string]any{"source": v9Source, "elapsed_ms": time.Since(start).Milliseconds(), "request": r, "financially_accepted": false, "closing_reconciled": false}, false); e != nil {
		t.Fatal("completion evidence failed")
	}
}

func TestOPS818Dynamic07V3V1SourceGuards(t *testing.T) {
	if !strings.HasSuffix(v9Root, "/validator-07") {
		t.Fatal("wrong owned root")
	}
	for _, n := range []string{"zz_ops818_dynamic07_stored_v3_test.go", "zz_ops818_dynamic07_archive_v3_test.go"} {
		b, e := os.ReadFile(n)
		if e != nil {
			t.Fatal(e)
		}
		s := string(b)
		if strings.Contains(s, "opening06-reply-data.lock") || strings.Contains(s, "validator-06") || !strings.Contains(s, "shared07-reply-data.lock") {
			t.Fatal("root/lock guard")
		}
	}
	for _, text := range []string{"height 100 is not available, lowest height is 200", "state pruned"} {
		dir := t.TempDir()
		tr := &b9Transport{Used: map[string]string{filepath.Join(dir, "raw"): ""}}
		if e := v9Write(filepath.Join(dir, "raw"), b9HTTPRecord{Body: []byte(text)}, false); e != nil {
			t.Fatal(e)
		}
		if !b9PruningEvidence(tr) {
			t.Fatal("actual pruning refused")
		}
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "raw")
	v9Write(p, b9HTTPRecord{Error: "transport failed"}, false)
	if b9PruningEvidence(&b9Transport{Used: map[string]string{p: ""}}) {
		t.Fatal("transport failure relabeled as pruning")
	}
}
