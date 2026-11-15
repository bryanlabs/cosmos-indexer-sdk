//go:build ops818dynamic07openingv2

package taxapi

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func b8ValidateAttempts(p b8Proof) error {
	own := os.Getenv("OPS818_OWN_RPC")
	if own == "" || len(p.AttemptedEndpoints) < 1 || len(p.AttemptedEndpoints) > 2 {
		return errors.New("own node attempt required")
	}
	first := p.AttemptedEndpoints[0]
	if first.OriginSHA != v8Hash([]byte(own)) {
		return errors.New("own node origin rejected")
	}
	selected := first
	if len(p.AttemptedEndpoints) == 2 {
		archive := os.Getenv("H_ARCHIVE_RPC")
		if archive == "" || !first.Failed || first.PruningBodySHA == "" {
			return errors.New("archive requires own node pruning")
		}
		selected = p.AttemptedEndpoints[1]
		if selected.OriginSHA != v8Hash([]byte(archive)) {
			return errors.New("archive origin rejected")
		}
		bound := false
		for path, want := range first.RawHTTP {
			b, e := os.ReadFile(path)
			if e != nil || v8Hash(b) != want {
				return errors.New("own raw proof changed")
			}
			var raw b8HTTPRecord
			if v8Read(path, &raw) != nil || raw.EndpointSHA != first.OriginSHA || raw.BodySHA != v8Hash(raw.Body) {
				return errors.New("own raw binding rejected")
			}
			if raw.BodySHA == first.PruningBodySHA && raw.Status == first.Status && b8PruningEvidence(&b8Transport{Used: map[string]string{path: want}}) {
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
		if e != nil || v8Hash(b) != want {
			return errors.New("selected raw changed")
		}
		var raw b8HTTPRecord
		if v8Read(path, &raw) != nil || raw.EndpointSHA != selected.OriginSHA {
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
func TestOPS818Dynamic07V2EndpointProvenance(t *testing.T) {
	t.Setenv("OPS818_OWN_RPC", "http://own.invalid")
	t.Setenv("H_ARCHIVE_RPC", "http://archive.invalid")
	if b8ValidateAttempts(b8Proof{}) == nil {
		t.Fatal("absent own attempt accepted")
	}
	dir := t.TempDir()
	own := b8HTTPRecord{EndpointSHA: v8Hash([]byte(os.Getenv("OPS818_OWN_RPC"))), Status: 200, Body: []byte(`{"error":"height is not available, lowest height is 200"}`)}
	own.BodySHA = v8Hash(own.Body)
	op := filepath.Join(dir, "own")
	v8Write(op, own, false)
	ob, _ := os.ReadFile(op)
	ar := b8HTTPRecord{EndpointSHA: v8Hash([]byte(os.Getenv("H_ARCHIVE_RPC"))), Status: 200, Body: []byte(`{}`)}
	ar.BodySHA = v8Hash(ar.Body)
	ap := filepath.Join(dir, "archive")
	v8Write(ap, ar, false)
	ab, _ := os.ReadFile(ap)
	p := b8Proof{RawHTTP: map[string]string{ap: v8Hash(ab)}, AttemptedEndpoints: []b8EndpointAttempt{{OriginSHA: own.EndpointSHA, Failed: true, Status: 200, PruningBodySHA: own.BodySHA, RawHTTP: map[string]string{op: v8Hash(ob)}}, {OriginSHA: ar.EndpointSHA, Status: 200, RawHTTP: map[string]string{ap: v8Hash(ab)}}}}
	if e := b8ValidateAttempts(p); e != nil {
		t.Fatal(e)
	}
	p.AttemptedEndpoints[0].PruningBodySHA = ""
	if b8ValidateAttempts(p) == nil {
		t.Fatal("missing pruning accepted")
	}
	p.AttemptedEndpoints[0].PruningBodySHA = own.BodySHA
	p.AttemptedEndpoints[0].OriginSHA = "wrong"
	if b8ValidateAttempts(p) == nil {
		t.Fatal("wrong own origin accepted")
	}
}
