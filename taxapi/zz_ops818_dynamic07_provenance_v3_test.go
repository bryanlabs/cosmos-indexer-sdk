//go:build ops818dynamic07openingv3

package taxapi

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func b9ValidateAttempts(p b9Proof) error {
	own := os.Getenv("OPS818_OWN_RPC")
	if own == "" || len(p.AttemptedEndpoints) < 1 || len(p.AttemptedEndpoints) > 2 {
		return errors.New("own node attempt required")
	}
	first := p.AttemptedEndpoints[0]
	if first.OriginSHA != v9Hash([]byte(own)) {
		return errors.New("own node origin rejected")
	}
	selected := first
	if len(p.AttemptedEndpoints) == 2 {
		archive := os.Getenv("H_ARCHIVE_RPC")
		if archive == "" || !first.Failed || first.PruningBodySHA == "" {
			return errors.New("archive requires own node pruning")
		}
		selected = p.AttemptedEndpoints[1]
		if selected.OriginSHA != v9Hash([]byte(archive)) {
			return errors.New("archive origin rejected")
		}
		bound := false
		for path, want := range first.RawHTTP {
			b, e := os.ReadFile(path)
			if e != nil || v9Hash(b) != want {
				return errors.New("own raw proof changed")
			}
			var raw b9HTTPRecord
			if v9Read(path, &raw) != nil || raw.EndpointSHA != first.OriginSHA || raw.BodySHA != v9Hash(raw.Body) {
				return errors.New("own raw binding rejected")
			}
			if raw.BodySHA == first.PruningBodySHA && raw.Status == first.Status && b9PruningEvidence(&b9Transport{Used: map[string]string{path: want}}) {
				bound = true
			}
		}
		if !bound {
			return errors.New("own pruning response unbound")
		}
	}
	if selected.Failed {
		return errors.New("selected endpoint failed")
	}
	if len(p.RawHTTP) == 0 {
		return errors.New("selected raw proof absent")
	}
	for path, want := range p.RawHTTP {
		b, e := os.ReadFile(path)
		if e != nil || v9Hash(b) != want {
			return errors.New("selected raw changed")
		}
		var raw b9HTTPRecord
		if v9Read(path, &raw) != nil || raw.EndpointSHA != selected.OriginSHA {
			return errors.New("selected origin differs")
		}
		if selected.RawHTTP[path] != want {
			return errors.New("selected attempt raw set differs")
		}
	}
	if len(selected.RawHTTP) != len(p.RawHTTP) {
		return errors.New("selected attempt raw count differs")
	}
	return nil
}
func TestOPS818Dynamic07V3EndpointProvenance(t *testing.T) {
	t.Setenv("OPS818_OWN_RPC", "http://own.invalid")
	t.Setenv("H_ARCHIVE_RPC", "http://archive.invalid")
	if b9ValidateAttempts(b9Proof{}) == nil {
		t.Fatal("absent own attempt accepted")
	}
	dir := t.TempDir()
	own := b9HTTPRecord{EndpointSHA: v9Hash([]byte(os.Getenv("OPS818_OWN_RPC"))), Status: 200, Body: []byte(`{"error":"height is not available, lowest height is 200"}`)}
	own.BodySHA = v9Hash(own.Body)
	op := filepath.Join(dir, "own")
	v9Write(op, own, false)
	ob, _ := os.ReadFile(op)
	ar := b9HTTPRecord{EndpointSHA: v9Hash([]byte(os.Getenv("H_ARCHIVE_RPC"))), Status: 200, Body: []byte(`{}`)}
	ar.BodySHA = v9Hash(ar.Body)
	ap := filepath.Join(dir, "archive")
	v9Write(ap, ar, false)
	ab, _ := os.ReadFile(ap)
	p := b9Proof{RawHTTP: map[string]string{ap: v9Hash(ab)}, AttemptedEndpoints: []b9EndpointAttempt{{OriginSHA: own.EndpointSHA, Failed: true, Status: 200, PruningBodySHA: own.BodySHA, RawHTTP: map[string]string{op: v9Hash(ob)}}, {OriginSHA: ar.EndpointSHA, Status: 200, RawHTTP: map[string]string{ap: v9Hash(ab)}}}}
	if e := b9ValidateAttempts(p); e != nil {
		t.Fatal(e)
	}
	p.AttemptedEndpoints[0].PruningBodySHA = ""
	if b9ValidateAttempts(p) == nil {
		t.Fatal("missing pruning accepted")
	}
	p.AttemptedEndpoints[0].PruningBodySHA = own.BodySHA
	p.AttemptedEndpoints[0].OriginSHA = "wrong"
	if b9ValidateAttempts(p) == nil {
		t.Fatal("wrong own origin accepted")
	}
}
