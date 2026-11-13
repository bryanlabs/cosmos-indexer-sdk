package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

var recheckEvidence bool

// Subspace ignores query height in SDK v0.37.4. Enumerate latest state only
// after proving the entire staking IAVL root is unchanged from export height
// to the archive tip. The export root is also anchored in the next cached
// block header's application hash through the multistore proof.
func rpcEvidence(endpoint, path, method string, params interface{}) ([]byte, error) {
	if recheckEvidence {
		raw, e := os.ReadFile(path)
		if e != nil {
			return nil, fmt.Errorf("preserved evidence missing")
		}
		return raw, nil
	}
	body, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, e := http.NewRequestWithContext(context.Background(), "POST", endpoint, bytes.NewReader(body))
	if e != nil {
		return nil, fmt.Errorf("archive endpoint unusable")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, e := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if e != nil {
		return nil, fmt.Errorf("archive transport failed")
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if e != nil || len(raw) > 16<<20 {
		return nil, fmt.Errorf("archive reply unreadable or exceeds limit")
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return nil, fmt.Errorf("fresh evidence file required")
	}
	_, we := f.Write(raw)
	ce := f.Close()
	if we != nil || ce != nil {
		return nil, fmt.Errorf("evidence write failed")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("archive HTTP failure, evidence preserved")
	}
	time.Sleep(250 * time.Millisecond)
	return raw, nil
}
func readUniverse(raw []byte) ([]sdk.ValAddress, error) {
	var d struct {
		Chain      string           `json:"chain"`
		Through    int64            `json:"through"`
		Stored     int64            `json:"heights_stored"`
		Validators []sdk.ValAddress `json:"validators"`
	}
	if json.Unmarshal(raw, &d) != nil || d.Chain != "cosmoshub-3" || d.Through != exportHeight || d.Stored != exportHeight || len(d.Validators) == 0 {
		return nil, fmt.Errorf("validator universe lacks complete Hub 3 history")
	}
	seen := map[string]bool{}
	for _, v := range d.Validators {
		if len(v) != 20 || seen[string(v)] {
			return nil, fmt.Errorf("validator universe invalid or duplicated")
		}
		seen[string(v)] = true
	}
	return d.Validators, nil
}
func fetchSnapshot(endpoint, out string, n int, universe []sdk.ValAddress, expectedAppHash []byte) (snapshot, error) {
	var s snapshot
	info := func(label string) error {
		raw, e := rpcEvidence(endpoint, filepath.Join(out, fmt.Sprintf("archive-%d-%s.json", n, label)), "abci_info", map[string]interface{}{})
		if e != nil {
			return e
		}
		var r struct {
			Error  json.RawMessage `json:"error"`
			Result struct {
				Response struct {
					Height string `json:"last_block_height"`
				} `json:"response"`
			} `json:"result"`
		}
		if json.Unmarshal(raw, &r) != nil || (len(r.Error) > 0 && string(r.Error) != "null") || r.Result.Response.Height != "5200791" {
			return fmt.Errorf("archive is not at the fixed terminal height")
		}
		return nil
	}
	query := func(label, path string, key []byte, height int64, prove bool) ([]byte, error) {
		return rpcEvidence(endpoint, filepath.Join(out, fmt.Sprintf("archive-%d-%s.json", n, label)), "abci_query", map[string]interface{}{"path": path, "data": hex.EncodeToString(key), "height": strconv.FormatInt(height, 10), "prove": prove})
	}
	if e := info("info-before"); e != nil {
		return s, e
	}
	exportProof, e := query("export-root", "/store/staking/key", []byte{0x12}, exportHeight, true)
	if e != nil {
		return s, e
	}
	exportRoot, appHash, e := verifiedStakingRoot(exportProof, exportHeight)
	if e != nil {
		return s, e
	}
	if !bytes.Equal(appHash, expectedAppHash) {
		return s, fmt.Errorf("export proof does not match cached next-header application hash")
	}
	latestProof, e := query("latest-root-before", "/store/staking/key", []byte{0x12}, exportHeight+1, true)
	if e != nil {
		return s, e
	}
	latestRoot, _, e := verifiedStakingRoot(latestProof, exportHeight+1)
	if e != nil {
		return s, e
	}
	if !bytes.Equal(exportRoot, latestRoot) {
		return s, fmt.Errorf("latest staking store differs from export height, refusing subspace")
	}
	all := map[byte][]sdk.KVPair{}
	for _, kind := range []byte{0x21, 0x31, 0x32} {
		shards := 1
		if kind == 0x31 || kind == 0x32 {
			shards = 256
		}
		for shard := 0; shard < shards; shard++ {
			prefix := []byte{kind}
			if shards > 1 {
				prefix = append(prefix, byte(shard))
			}
			raw, e := query(fmt.Sprintf("subspace-%02x-%03d", kind, shard), "/store/staking/subspace", prefix, exportHeight, false)
			if e != nil {
				return s, e
			}
			pairs, e := decodeReply(raw, prefix)
			if e != nil {
				return s, e
			}
			all[kind] = append(all[kind], pairs...)
			if shard%32 == 31 {
				fmt.Printf("staking_fetch archive=%d kind=%02x partitions=%d of=%d\n", n, kind, shard+1, shards)
			}
		}
	}
	afterProof, e := query("latest-root-after", "/store/staking/key", []byte{0x12}, exportHeight+1, true)
	if e != nil {
		return s, e
	}
	afterRoot, _, e := verifiedStakingRoot(afterProof, exportHeight+1)
	if e != nil {
		return s, e
	}
	if !bytes.Equal(exportRoot, afterRoot) {
		return s, fmt.Errorf("staking store changed during enumeration")
	}
	if e := info("info-after"); e != nil {
		return s, e
	}
	s, e = decodeStore(all)
	if e != nil {
		return s, e
	}
	allValidators := map[string]bool{}
	for _, v := range universe {
		allValidators[string(v)] = true
	}
	for _, v := range s.Validators {
		if !allValidators[string(v.OperatorAddress)] {
			return s, fmt.Errorf("terminal validator absent from complete history universe")
		}
	}
	if _, e := compare(s, s); e != nil {
		return s, e
	}
	raw, e := json.Marshal(s)
	if e != nil {
		return s, fmt.Errorf("snapshot encoding failed")
	}
	snapshotPath := filepath.Join(out, fmt.Sprintf("archive-%d-snapshot.json", n))
	if recheckEvidence {
		old, e := os.ReadFile(snapshotPath)
		if e != nil || !bytes.Equal(old, raw) {
			return s, fmt.Errorf("preserved decoded snapshot differs")
		}
	} else {
		f, e := os.OpenFile(snapshotPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return s, fmt.Errorf("fresh snapshot evidence file required")
		}
		_, we := f.Write(raw)
		ce := f.Close()
		if we != nil || ce != nil {
			return s, fmt.Errorf("snapshot evidence write failed")
		}
	}
	return s, nil
}
