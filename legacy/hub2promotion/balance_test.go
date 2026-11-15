package hub2promotion

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func balancePin(t *testing.T, dir, name string, accounts any) Pin {
	b, e := json.Marshal(map[string]any{"app_state": map[string]any{"accounts": accounts}})
	require.NoError(t, e)
	return savePin(t, dir, name, b)
}
func TestApprovedAllAccountAllCoinComparison(t *testing.T) {
	dir := artifactDir(t)
	account := func(address string, coins ...map[string]string) map[string]any {
		return map[string]any{"address": address, "coins": coins}
	}
	coin := func(d, a string) map[string]string { return map[string]string{"denom": d, "amount": a} }
	opening := []any{account("synthetic-a", coin("uatom", "8"), coin("uother", "90071992547409931234567890")), account("synthetic-zero")}
	export := balancePin(t, dir, "export.json", opening)
	same := balancePin(t, dir, "same.json", []any{opening[1], opening[0]})
	r, e := CompareAllAccounts(export, same)
	require.NoError(t, e)
	require.True(t, r.Pass)
	require.Equal(t, 2, r.AccountsChecked)
	require.Equal(t, r.ExportHash, r.GenesisHash)
	cases := []struct {
		name                   string
		accounts               any
		missing, extra, denoms int
	}{
		{"non-uatom", []any{account("synthetic-a", coin("uatom", "8"), coin("uother", "90071992547409931234567891")), opening[1]}, 0, 0, 1},
		{"missing-empty-account", []any{opening[0]}, 1, 0, 0},
		{"extra-empty-account", []any{opening[0], opening[1], account("synthetic-extra")}, 0, 1, 0},
		{"missing-denom", []any{account("synthetic-a", coin("uatom", "8")), opening[1]}, 0, 0, 1},
		{"extra-denom", []any{account("synthetic-a", coin("uatom", "8"), coin("uother", "90071992547409931234567890"), coin("zero", "0")), opening[1]}, 0, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, e := CompareAllAccounts(export, balancePin(t, dir, tc.name+".json", tc.accounts))
			require.NoError(t, e)
			require.False(t, r.Pass)
			require.Equal(t, tc.missing, r.Missing)
			require.Equal(t, tc.extra, r.Extra)
			require.Equal(t, tc.denoms, r.MismatchedDenominations)
		})
	}
	for name, accounts := range map[string]any{
		"duplicate-account": []any{opening[0], opening[0]},
		"duplicate-denom":   []any{account("synthetic-a", coin("uatom", "1"), coin("uatom", "1"))},
		"negative":          []any{account("synthetic-a", coin("uatom", "-1"))},
		"fraction":          []any{account("synthetic-a", coin("uatom", "1.0"))},
		"missing-coins":     []any{map[string]any{"address": "synthetic-a"}},
		"empty":             []any{},
	} {
		t.Run(name, func(t *testing.T) {
			_, e := CompareAllAccounts(export, balancePin(t, dir, name+".json", accounts))
			require.Error(t, e)
		})
	}
	bad := savePin(t, dir, "duplicate-keys.json", []byte(`{"app_state":{"accounts":[{"address":"synthetic-a","coins":[],"coins":[]}]}}`))
	_, e = CompareAllAccounts(export, bad)
	require.Error(t, e)
	encoded, _ := json.Marshal(r)
	require.NotContains(t, string(encoded), "synthetic-a")
	require.NotContains(t, string(encoded), "9007199254740993")
}
func TestApprovedNarrowScopeAndIncompleteSourceRefuse(t *testing.T) {
	for _, chain := range []string{"cosmoshub-3", "cosmoshub-4", "cosmoshub-1' OR true--"} {
		s := Scope{Schema: "h2p_s_bad", Loader: "h2p_l_bad", ViewOwner: "h2p_v_bad", LockOwner: "h2p_k_bad", ChainID: chain}
		_, e := s.SQL()
		require.Error(t, e)
	}
	for _, chain := range []string{"cosmoshub-1", "cosmoshub-2"} {
		s := Scope{Schema: "h2p_s_safe", Loader: "h2p_l_safe", ViewOwner: "h2p_v_safe", LockOwner: "h2p_k_safe", ChainID: chain}
		text, e := s.SQL()
		require.NoError(t, e)
		require.Equal(t, 10, strings.Count(text, "WHERE chain_id = '"+chain+"' WITH CASCADED CHECK OPTION"))
		_, e = MergeApproved(context.Background(), nil, nil, Manifest{Profile: ApprovedProfile, Scope: s}, Identity{})
		require.Error(t, e)
	}
	c, l, n, e := boundaries("cosmoshub-1")
	require.NoError(t, e)
	require.EqualValues(t, 500000, c)
	require.EqualValues(t, 500015, l)
	require.Equal(t, "cosmoshub-2", n)
	c, l, n, e = boundaries("cosmoshub-2")
	require.NoError(t, e)
	require.EqualValues(t, 2902000, c)
	require.EqualValues(t, 2902002, l)
	require.Equal(t, "cosmoshub-3", n)
}
