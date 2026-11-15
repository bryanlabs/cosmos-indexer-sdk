package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestOfficialMigrationAcceptedPrefixFixture(t *testing.T) {
	const path = "/Users/danb/.cache/cosmos-tax-tmp/hub2-next-components-20261114T142600Z/1800000-ready-continuation/1800000/candidate/export.json"
	const acceptedSHA = "86ddb6a7223cf0559ab0e0f0b78b9d0495da97dead2344988aaf3450442a5618"
	original, e := pinned(path, acceptedSHA)
	if e != nil {
		t.Fatal("retained accepted fixture unavailable")
	}
	out, e := migrate(original)
	if e != nil {
		t.Fatal(e)
	}
	again, e := migrate(original)
	if e != nil || !bytes.Equal(out, again) {
		t.Fatal("official migration is not reproducible")
	}
	var before, after struct {
		AppState struct {
			Accounts []struct {
				Address string          `json:"address"`
				Coins   json.RawMessage `json:"coins"`
				Module  string          `json:"module_name"`
			} `json:"accounts"`
		} `json:"app_state"`
	}
	if json.Unmarshal(original, &before) != nil || json.Unmarshal(out, &after) != nil {
		t.Fatal("invalid transformed fixture")
	}
	if len(after.AppState.Accounts) != len(before.AppState.Accounts)+4 {
		t.Fatal("official two-old-to-six-module conversion missing")
	}
	modules := map[string]bool{}
	accounts := map[string]json.RawMessage{}
	for _, a := range after.AppState.Accounts {
		if a.Module != "" {
			modules[a.Module] = true
		}
		if _, ok := accounts[a.Address]; ok {
			t.Fatal("duplicate transformed account")
		}
		accounts[a.Address] = a.Coins
	}
	for _, name := range []string{"fee_collector", "gov", "distribution", "bonded_tokens_pool", "not_bonded_tokens_pool", "mint"} {
		if !modules[name] {
			t.Fatal("official module account missing")
		}
	}
	coinSet := func(raw json.RawMessage) map[string]string {
		var coins []struct {
			Denom  string `json:"denom"`
			Amount string `json:"amount"`
		}
		if json.Unmarshal(raw, &coins) != nil {
			t.Fatal("invalid coin array")
		}
		out := map[string]string{}
		for _, c := range coins {
			if _, ok := out[c.Denom]; ok {
				t.Fatal("duplicate coin")
			}
			out[c.Denom] = c.Amount
		}
		return out
	}
	unchanged := 0
	for _, a := range before.AppState.Accounts {
		if coins, ok := accounts[a.Address]; ok {
			if !reflect.DeepEqual(coinSet(coins), coinSet(a.Coins)) {
				t.Fatal("ordinary account coin changed")
			}
			unchanged++
		}
	}
	if unchanged != len(before.AppState.Accounts)-2 {
		t.Fatal("ordinary account lost in migration")
	}
	actual, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256(actual)
	if hex.EncodeToString(h[:]) != acceptedSHA {
		t.Fatal("original fixture changed")
	}
	bad := []byte(`{"source_height":2902002,"for_zero_height":false,"app_state":{}}`)
	if _, e = migrate(bad); e == nil {
		t.Fatal("normal/tail input accepted as zero-height source")
	}
}
