package main

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"strconv"
)

// This verifies captured identity, parent continuity and the four actual raw
// header/AppHash bindings. Native state equality remains source-attested by the
// successful original report. It is NOT an independent loaded-root proof or a
// reconstruction of missing native DeliverTx response bytes.
func verifyWindowCacheIdentity(path string, height int64, kind string, previous *string, boundary json.RawMessage) error {
	f, e := os.Open(path)
	if e != nil {
		return refused
	}
	defer f.Close()
	z, e := gzip.NewReader(f)
	if e != nil {
		return refused
	}
	defer z.Close()
	raw, e := io.ReadAll(io.LimitReader(z, 64<<20+1))
	if e != nil || len(raw) > 64<<20 {
		return refused
	}
	var all map[string]json.RawMessage
	if strictWindowJSON(raw, &all) != nil {
		return refused
	}
	if kind == "block_results" {
		var r struct {
			Result struct {
				Height string `json:"height"`
			} `json:"result"`
		}
		if json.Unmarshal(raw, &r) != nil || r.Result.Height != strconv.FormatInt(height, 10) {
			return refused
		}
		return nil
	}
	var b struct {
		Result struct {
			Meta struct {
				ID struct {
					Hash string `json:"hash"`
				} `json:"block_id"`
			} `json:"block_meta"`
			Block struct {
				Header struct {
					Height  string `json:"height"`
					Chain   string `json:"chain_id"`
					AppHash string `json:"app_hash"`
					Parent  struct {
						Hash string `json:"hash"`
					} `json:"last_block_id"`
				} `json:"header"`
			} `json:"block"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &b) != nil {
		return refused
	}
	r := b.Result
	h := r.Block.Header
	if h.Height != strconv.FormatInt(height, 10) || h.Chain != "cosmoshub-2" || len(h.AppHash) != 64 || len(r.Meta.ID.Hash) != 64 || *previous != "" && h.Parent.Hash != *previous {
		return refused
	}
	*previous = r.Meta.ID.Hash
	if len(boundary) > 0 {
		var proof struct {
			Raw     string `json:"raw_block_sha256"`
			Hash    string `json:"block_hash"`
			AppHash string `json:"header_app_hash"`
		}
		if strictWindowJSON(boundary, &proof) != nil || proof.Raw != windowHash(raw) || proof.Hash != r.Meta.ID.Hash || proof.AppHash != h.AppHash {
			return refused
		}
	}
	return nil
}
