package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
)

// These are actual before/after API responses and raw source-file checksums,
// not caller-written replacements for the long runner's original Job status.
func verifyWindowArtifacts(dir string, w windowReceipt) error {
	names := []string{"job.original.json", "job.after.json", "pod.original.json", "pod.after.json", "cache.binding.private.json", "cache.remote.sha256"}
	if len(w.Artifacts) != len(names) {
		return refused
	}
	artifacts := map[string][]byte{}
	for _, name := range names {
		limit := int64(8 << 20)
		if strings.HasPrefix(name, "cache.") {
			limit = 64 << 20
		}
		raw, e := privateRaw(filepath.Join(dir, name), limit)
		if e != nil || w.Artifacts[name] != windowHash(raw) {
			return refused
		}
		artifacts[name] = raw
	}
	var originalSpec any
	for _, name := range names[:2] {
		var doc map[string]json.RawMessage
		if strictWindowJSON(artifacts[name], &doc) != nil {
			return refused
		}
		var job struct {
			Metadata struct {
				Name string `json:"name"`
				UID  string `json:"uid"`
			} `json:"metadata"`
			Spec struct {
				Backoff  int  `json:"backoffLimit"`
				Suspend  bool `json:"suspend"`
				Template struct {
					Spec struct {
						Token      *bool `json:"automountServiceAccountToken"`
						Containers []struct {
							Name    string   `json:"name"`
							Image   string   `json:"image"`
							Command []string `json:"command"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				Active    int `json:"active"`
				Succeeded int `json:"succeeded"`
				Failed    int `json:"failed"`
			} `json:"status"`
		}
		if json.Unmarshal(artifacts[name], &job) != nil || job.Metadata.UID != w.UID || job.Metadata.Name != windowJob || job.Spec.Backoff != 0 || job.Spec.Suspend || job.Status.Active != 1 || job.Status.Succeeded != 0 || job.Status.Failed != 0 || job.Spec.Template.Spec.Token == nil || *job.Spec.Template.Spec.Token || len(job.Spec.Template.Spec.Containers) != 1 {
			return refused
		}
		c := job.Spec.Template.Spec.Containers[0]
		if c.Name != "replay" || !strings.HasSuffix(c.Image, "@"+replayrecords.GasFinancialImage) {
			return refused
		}
		command := strings.Join(c.Command, "\n")
		for _, arg := range []string{"--to 2902000", "--verify-target-next-header", "--sparse-account-checks", "--compare-results=true", `--coin-records "$out/movements.ndjson"`} {
			if !strings.Contains(command, arg) {
				return refused
			}
		}
		var spec any
		if json.Unmarshal(doc["spec"], &spec) != nil {
			return refused
		}
		if name == names[0] {
			originalSpec = spec
		} else if !reflect.DeepEqual(spec, originalSpec) {
			return refused
		}
	}
	for _, name := range names[2:4] {
		var doc map[string]json.RawMessage
		if strictWindowJSON(artifacts[name], &doc) != nil {
			return refused
		}
		var list struct {
			Items []struct {
				Metadata struct {
					UID    string `json:"uid"`
					Owners []struct {
						UID string `json:"uid"`
					} `json:"ownerReferences"`
				} `json:"metadata"`
				Spec struct {
					Token *bool `json:"automountServiceAccountToken"`
				} `json:"spec"`
				Status struct {
					Phase      string `json:"phase"`
					Containers []struct {
						Name     string `json:"name"`
						Image    string `json:"imageID"`
						Ready    bool   `json:"ready"`
						Restarts int    `json:"restartCount"`
					} `json:"containerStatuses"`
				} `json:"status"`
			} `json:"items"`
		}
		if json.Unmarshal(artifacts[name], &list) != nil || len(list.Items) != 1 {
			return refused
		}
		p := list.Items[0]
		if p.Metadata.UID != w.PodUID || len(p.Metadata.Owners) != 1 || p.Metadata.Owners[0].UID != w.UID || p.Spec.Token == nil || *p.Spec.Token || p.Status.Phase != "Running" || len(p.Status.Containers) != 1 {
			return refused
		}
		c := p.Status.Containers[0]
		if c.Name != "replay" || !c.Ready || c.Restarts != 0 || !strings.HasSuffix(c.Image, replayrecords.GasFinancialImage) {
			return refused
		}
	}
	return verifyWindowCache(dir, artifacts["cache.binding.private.json"], artifacts["cache.remote.sha256"], 400000, 482091)
}

// The sealed CLI only opens this copied raw cache. Every compressed source
// member must match both its raw remote checksum and its retained manifest.
// This is input-byte binding, not a fresh independent native AppHash proof.
func verifyWindowCache(dir string, manifest, remote []byte, from, through int64) error {
	var binding struct {
		Members      map[string]string          `json:"all_raw_members"`
		Boundaries   map[string]json.RawMessage `json:"boundaries"`
		Method       string                     `json:"native_state_completion"`
		NextExecuted bool                       `json:"original_job_executed_482091"`
		NoExecution  bool                       `json:"bounded_component_does_not_execute_any_native_block"`
	}
	if strictWindowJSON(manifest, &binding) != nil || len(binding.Members) != int((through-from+1)*2) || len(binding.Boundaries) != 4 || !binding.NextExecuted || !binding.NoExecution || binding.Method != "source-attested by retained successful original status/report; no retained per-height native AppHash or full response bytes" {
		return refused
	}
	// Preserve exact remote checksum bytes, require a complete canonical ordered
	// list, and reject any extra/missing/reordered line rather than normalize it.
	lines := bytes.Split(remote, []byte{'\n'})
	if len(lines) != len(binding.Members)+1 || len(lines[len(lines)-1]) != 0 {
		return refused
	}
	i := 0
	previous := ""
	for h := from; h <= through; h++ {
		for _, kind := range []string{"block", "block_results"} {
			name := "cosmoshub-2/" + strconv.FormatInt(h/10000, 10) + "/" + strconv.FormatInt(h, 10) + "-" + kind + ".raw.gz"
			digest, ok := binding.Members[name]
			if !ok || len(digest) != 64 || !bytes.Equal(lines[i], []byte(digest+"  "+name)) {
				return refused
			}
			i++
			path := filepath.Join(dir, "cache", name)
			info, e := os.Lstat(path)
			if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
				return refused
			}
			f, e := os.Open(path)
			if e != nil {
				return refused
			}
			hash := sha256.New()
			_, e = io.Copy(hash, f)
			closeErr := f.Close()
			if e != nil || closeErr != nil || hex.EncodeToString(hash.Sum(nil)) != digest {
				return refused
			}
			if verifyWindowCacheIdentity(path, h, kind, &previous, binding.Boundaries[strconv.FormatInt(h, 10)]) != nil {
				return refused
			}
		}
	}
	for _, h := range []int64{from, from + 1, through - 1, through} {
		if len(binding.Boundaries[strconv.FormatInt(h, 10)]) == 0 {
			return refused
		}
	}
	return nil
}
