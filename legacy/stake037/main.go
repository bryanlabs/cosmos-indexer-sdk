// stake037 compares the entire terminal Hub 3 staking store with Hub 4's
// genesis. It never writes a database. Raw archive replies remain evidence.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
)

const exportHeight = int64(5200790)
const nextGenesisSHA = "6ad715c1ab5637e505e7248bb4366e79d5dec1a24f4fac7db33fead567041633"

type snapshot struct {
	Validators  []staking.Validator           `json:"validators"`
	Delegations []staking.Delegation          `json:"delegations"`
	Unbondings  []staking.UnbondingDelegation `json:"unbonding_delegations"`
}
type summary struct {
	Validators            int `json:"validators"`
	Delegations           int `json:"delegations"`
	UnbondingPairs        int `json:"unbonding_pairs"`
	UnbondingEntries      int `json:"unbonding_entries"`
	ValidatorDifferences  int `json:"validator_differences"`
	DelegationDifferences int `json:"delegation_differences"`
	UnbondingDifferences  int `json:"unbonding_differences"`
}

func codec037() *codec.Codec {
	c := codec.New()
	codec.RegisterCrypto(c)
	return c
}

// fetch requires a fresh output path, an exact returned height and a successful
// ABCI result. No retry can silently change which source supplied a receipt.
func fetch(ctx context.Context, endpoint, out, path string, data []byte) ([]byte, error) {
	body, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": "abci_query", "params": map[string]interface{}{"path": path, "data": hex.EncodeToString(data), "height": strconv.FormatInt(exportHeight, 10), "prove": false}})
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("archive endpoint unusable")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("archive transport failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20+1))
	if err != nil || len(raw) > 128<<20 {
		return nil, fmt.Errorf("archive reply unreadable or exceeds limit")
	}
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("evidence path already exists or is unwritable")
	}
	_, we := f.Write(raw)
	ce := f.Close()
	if we != nil || ce != nil {
		return nil, fmt.Errorf("evidence write failed")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("archive HTTP failure, raw evidence preserved")
	}
	return decodeQueryValue(raw)
}
func decodeQueryValue(raw []byte) ([]byte, error) {
	var r struct {
		Error  json.RawMessage `json:"error"`
		Result *struct {
			Response struct {
				Code   uint32 `json:"code"`
				Height string `json:"height"`
				Value  string `json:"value"`
			} `json:"response"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &r) != nil || len(r.Error) > 0 && string(r.Error) != "null" || r.Result == nil {
		return nil, fmt.Errorf("archive RPC failure")
	}
	a := r.Result.Response
	if a.Code != 0 || a.Height != strconv.FormatInt(exportHeight, 10) {
		return nil, fmt.Errorf("archive did not serve exact export height")
	}
	v, err := base64.StdEncoding.DecodeString(a.Value)
	if err != nil {
		return nil, fmt.Errorf("archive value malformed")
	}
	return v, nil
}
func decodeReply(raw []byte, prefix []byte) ([]sdk.KVPair, error) {
	v, err := decodeQueryValue(raw)
	if err != nil {
		return nil, err
	}
	var pairs []sdk.KVPair
	if codec037().UnmarshalBinaryLengthPrefixed(v, &pairs) != nil {
		return nil, fmt.Errorf("subspace unreadable")
	}
	seen := map[string]bool{}
	for _, p := range pairs {
		if !bytes.HasPrefix(p.Key, prefix) || seen[string(p.Key)] {
			return nil, fmt.Errorf("subspace duplicate or wrong prefix")
		}
		seen[string(p.Key)] = true
	}
	return pairs, nil
}
func decodeStore(all map[byte][]sdk.KVPair) (snapshot, error) {
	var s snapshot
	c := codec037()
	for _, p := range all[0x21] {
		v, err := staking.UnmarshalValidator(c, p.Value)
		if err != nil || !bytes.Equal(p.Key, staking.GetValidatorKey(v.OperatorAddress)) {
			return s, fmt.Errorf("validator record or key invalid")
		}
		s.Validators = append(s.Validators, v)
	}
	for _, p := range all[0x31] {
		d, err := staking.UnmarshalDelegation(c, p.Value)
		if err != nil || !bytes.Equal(p.Key, staking.GetDelegationKey(d.DelegatorAddress, d.ValidatorAddress)) {
			return s, fmt.Errorf("delegation record or key invalid")
		}
		s.Delegations = append(s.Delegations, d)
	}
	for _, p := range all[0x32] {
		u, err := staking.UnmarshalUBD(c, p.Value)
		if err != nil || !bytes.Equal(p.Key, staking.GetUBDKey(u.DelegatorAddress, u.ValidatorAddress)) {
			return s, fmt.Errorf("unbonding record or key invalid")
		}
		s.Unbondings = append(s.Unbondings, u)
	}
	return s, nil
}
func pair(a sdk.AccAddress, v sdk.ValAddress) string { return string(a) + string(v) }
func validAmount(n sdk.Int) bool {
	parsed, ok := sdk.NewIntFromString(n.String())
	return ok && !parsed.IsNegative()
}

// Compare checks exact delegation shares and their SDK token valuation.
// Hub 4 continues at initial_height 5200791, not zero. Every unbonding field,
// including its original creation height and entry ordering, must be retained.
func compare(before, after snapshot) (summary, error) {
	out := summary{Validators: len(before.Validators), Delegations: len(before.Delegations), UnbondingPairs: len(before.Unbondings)}
	vals := func(s snapshot) (map[string]staking.Validator, error) {
		m := map[string]staking.Validator{}
		for _, v := range s.Validators {
			k := string(v.OperatorAddress)
			if len(v.OperatorAddress) != 20 || v.DelegatorShares.IsNil() || !validAmount(v.Tokens) || v.DelegatorShares.IsNegative() {
				return nil, fmt.Errorf("invalid validator")
			}
			if _, ok := m[k]; ok {
				return nil, fmt.Errorf("duplicate validator")
			}
			m[k] = v
		}
		return m, nil
	}
	bv, err := vals(before)
	if err != nil {
		return out, err
	}
	av, err := vals(after)
	if err != nil {
		return out, err
	}
	for k, v := range bv {
		w, ok := av[k]
		if !ok || !v.Tokens.Equal(w.Tokens) || !v.DelegatorShares.Equal(w.DelegatorShares) || v.Status != w.Status || v.Jailed != w.Jailed {
			out.ValidatorDifferences++
		}
	}
	for k := range av {
		if _, ok := bv[k]; !ok {
			out.ValidatorDifferences++
		}
	}
	dels := func(s snapshot) (map[string]staking.Delegation, error) {
		m := map[string]staking.Delegation{}
		for _, d := range s.Delegations {
			k := pair(d.DelegatorAddress, d.ValidatorAddress)
			if len(d.DelegatorAddress) != 20 || len(d.ValidatorAddress) != 20 || d.Shares.IsNil() || !d.Shares.IsPositive() {
				return nil, fmt.Errorf("invalid delegation")
			}
			if _, ok := m[k]; ok {
				return nil, fmt.Errorf("duplicate delegation")
			}
			m[k] = d
		}
		return m, nil
	}
	bd, err := dels(before)
	if err != nil {
		return out, err
	}
	ad, err := dels(after)
	if err != nil {
		return out, err
	}
	for k, d := range bd {
		w, ok := ad[k]
		v, vok := bv[string(d.ValidatorAddress)]
		nv, nok := av[string(d.ValidatorAddress)]
		if !vok || !nok || v.DelegatorShares.IsZero() || nv.DelegatorShares.IsZero() {
			return out, fmt.Errorf("delegation has no usable validator")
		}
		if !ok || !d.Shares.Equal(w.Shares) || !v.TokensFromShares(d.Shares).TruncateInt().Equal(nv.TokensFromShares(w.Shares).TruncateInt()) {
			out.DelegationDifferences++
		}
	}
	for k := range ad {
		if _, ok := bd[k]; !ok {
			out.DelegationDifferences++
		}
	}
	ubds := func(s snapshot) (map[string]staking.UnbondingDelegation, error) {
		m := map[string]staking.UnbondingDelegation{}
		for _, u := range s.Unbondings {
			k := pair(u.DelegatorAddress, u.ValidatorAddress)
			if len(u.DelegatorAddress) != 20 || len(u.ValidatorAddress) != 20 || len(u.Entries) == 0 {
				return nil, fmt.Errorf("invalid unbonding")
			}
			if _, ok := m[k]; ok {
				return nil, fmt.Errorf("duplicate unbonding")
			}
			for _, entry := range u.Entries {
				if !validAmount(entry.Balance) || !validAmount(entry.InitialBalance) || entry.Balance.GT(entry.InitialBalance) || entry.CompletionTime.IsZero() || entry.CreationHeight < 0 {
					return nil, fmt.Errorf("invalid unbonding entry")
				}
			}
			m[k] = u
		}
		return m, nil
	}
	bu, err := ubds(before)
	if err != nil {
		return out, err
	}
	au, err := ubds(after)
	if err != nil {
		return out, err
	}
	for k, u := range bu {
		out.UnbondingEntries += len(u.Entries)
		w, ok := au[k]
		same := ok && len(u.Entries) == len(w.Entries)
		if same {
			for i, e := range u.Entries {
				n := w.Entries[i]
				if n.CreationHeight != e.CreationHeight || !e.CompletionTime.Equal(n.CompletionTime) || !e.InitialBalance.Equal(n.InitialBalance) || !e.Balance.Equal(n.Balance) {
					same = false
				}
			}
		}
		if !same {
			out.UnbondingDifferences++
		}
	}
	for k := range au {
		if _, ok := bu[k]; !ok {
			out.UnbondingDifferences++
		}
	}
	return out, nil
}
func readGenesis(path string) (snapshot, string, error) {
	var s snapshot
	raw, err := os.ReadFile(path)
	if err != nil {
		return s, "", fmt.Errorf("genesis unreadable")
	}
	if len(raw) > 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		r, e := gzip.NewReader(bytes.NewReader(raw))
		if e != nil {
			return s, "", e
		}
		raw, err = io.ReadAll(io.LimitReader(r, 256<<20))
		r.Close()
		if err != nil {
			return s, "", fmt.Errorf("genesis decompression failed")
		}
	}
	return decodeGenesis(raw)
}
func decodeGenesis(raw []byte) (snapshot, string, error) {
	var s snapshot
	sum := sha256.Sum256(raw)
	var doc struct {
		Chain string `json:"chain_id"`
		App   struct {
			Staking struct {
				Validators []struct {
					Operator sdk.ValAddress `json:"operator_address"`
					Tokens   sdk.Int        `json:"tokens"`
					Shares   sdk.Dec        `json:"delegator_shares"`
					Status   string         `json:"status"`
					Jailed   bool           `json:"jailed"`
				} `json:"validators"`
				Delegations []staking.Delegation          `json:"delegations"`
				Unbondings  []staking.UnbondingDelegation `json:"unbonding_delegations"`
			} `json:"staking"`
		} `json:"app_state"`
	}
	if codec037().UnmarshalJSON(raw, &doc) != nil || doc.Chain != "cosmoshub-4" {
		return s, "", fmt.Errorf("wrong or malformed genesis")
	}
	statuses := map[string]sdk.BondStatus{"BOND_STATUS_UNBONDED": sdk.Unbonded, "BOND_STATUS_UNBONDING": sdk.Unbonding, "BOND_STATUS_BONDED": sdk.Bonded}
	for _, v := range doc.App.Staking.Validators {
		status, ok := statuses[v.Status]
		if !ok {
			return s, "", fmt.Errorf("unrecognized genesis validator status")
		}
		s.Validators = append(s.Validators, staking.Validator{OperatorAddress: v.Operator, Tokens: v.Tokens, DelegatorShares: v.Shares, Status: status, Jailed: v.Jailed})
	}
	s.Delegations = doc.App.Staking.Delegations
	s.Unbondings = doc.App.Staking.Unbondings
	return s, hex.EncodeToString(sum[:]), nil
}
func run() error {
	genesis := flag.String("genesis", "", "preserved Hub 4 genesis")
	out := flag.String("out", "", "fresh evidence directory")
	universePath := flag.String("validator-universe", "", "private validator universe from the complete Hub 3 rehearsal ledger")
	headerPath := flag.String("terminal-header", "", "preserved cached height 5200791 block response anchoring export state")
	sourceEvidence := flag.String("recheck-evidence", "", "read-only recheck of an existing complete evidence directory; out must still be fresh")
	flag.Parse()
	if *genesis == "" || *out == "" {
		return fmt.Errorf("genesis and out required")
	}
	if err := os.Mkdir(*out, 0700); err != nil {
		return fmt.Errorf("fresh evidence directory required")
	}
	sourceRoot := *out
	if *sourceEvidence != "" {
		recheckEvidence = true
		sourceRoot = *sourceEvidence
		if *headerPath == "" {
			*headerPath = filepath.Join(sourceRoot, "terminal-header.json")
		}
		if *universePath == "" {
			*universePath = filepath.Join(sourceRoot, "validator-universe.json")
		}
	}
	next, hash, err := readGenesis(*genesis)
	if err != nil {
		return err
	}
	if hash != nextGenesisSHA {
		return fmt.Errorf("genesis digest is not the preserved Hub 4 genesis")
	}
	universeRaw, err := os.ReadFile(*universePath)
	if err != nil {
		return fmt.Errorf("validator universe unreadable")
	}
	universe, err := readUniverse(universeRaw)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, "validator-universe.json"), universeRaw, 0600); err != nil {
		return fmt.Errorf("universe evidence write failed")
	}
	headerRaw, err := os.ReadFile(*headerPath)
	if err != nil {
		return fmt.Errorf("terminal header unreadable")
	}
	var header struct {
		Result struct {
			Block struct {
				Header struct {
					Chain   string `json:"chain_id"`
					Height  string `json:"height"`
					AppHash string `json:"app_hash"`
				} `json:"header"`
			} `json:"block"`
		} `json:"result"`
	}
	if json.Unmarshal(headerRaw, &header) != nil || header.Result.Block.Header.Chain != "cosmoshub-3" || header.Result.Block.Header.Height != "5200791" {
		return fmt.Errorf("terminal header chain or height invalid")
	}
	expectedAppHash, err := hex.DecodeString(header.Result.Block.Header.AppHash)
	if err != nil || len(expectedAppHash) != 32 {
		return fmt.Errorf("terminal header app hash invalid")
	}
	if err := os.WriteFile(filepath.Join(*out, "terminal-header.json"), headerRaw, 0600); err != nil {
		return fmt.Errorf("terminal header evidence write failed")
	}
	var snapshots []snapshot
	for n := 1; n <= 2; n++ {
		endpoint := os.Getenv(fmt.Sprintf("LEGACY_ARCHIVE_%d", n))
		if endpoint == "" && !recheckEvidence {
			return fmt.Errorf("two archive endpoints required")
		}
		s, e := fetchSnapshot(endpoint, sourceRoot, n, universe, expectedAppHash)
		if e != nil {
			return e
		}
		snapshots = append(snapshots, s)
	}
	// Same-height sources must agree field-for-field before genesis comparison.
	a, _ := json.Marshal(snapshots[0])
	b, _ := json.Marshal(snapshots[1])
	if !bytes.Equal(a, b) {
		return fmt.Errorf("archives disagree, comparison stopped")
	}
	result, err := compare(snapshots[0], next)
	if err != nil {
		return err
	}
	receipt := struct {
		Release    string  `json:"release"`
		Height     int64   `json:"height"`
		GenesisSHA string  `json:"genesis_sha256"`
		Result     summary `json:"result"`
	}{"cosmos-sdk v0.37.4", exportHeight, hash, result}
	raw, _ := json.MarshalIndent(receipt, "", " ")
	if err := os.WriteFile(filepath.Join(*out, "receipt.json"), append(raw, '\n'), 0600); err != nil {
		return fmt.Errorf("receipt write failed")
	}
	files, err := os.ReadDir(sourceRoot)
	if err != nil {
		return fmt.Errorf("evidence listing failed")
	}
	var sums bytes.Buffer
	for _, entry := range files {
		if entry.IsDir() || entry.Name() == "receipt.json" || entry.Name() == "SHA256SUMS" {
			continue
		}
		data, e := os.ReadFile(filepath.Join(sourceRoot, entry.Name()))
		if e != nil {
			return fmt.Errorf("evidence hashing failed")
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(data), entry.Name())
	}
	if err := os.WriteFile(filepath.Join(*out, "SHA256SUMS"), sums.Bytes(), 0600); err != nil {
		return fmt.Errorf("evidence manifest write failed")
	}
	fmt.Println(string(raw))
	if result.ValidatorDifferences+result.DelegationDifferences+result.UnbondingDifferences != 0 {
		return fmt.Errorf("staking differs, no acceptance")
	}
	return nil
}
func main() {
	defer func() {
		if recover() != nil {
			fmt.Fprintln(os.Stderr, "staking comparison stopped: malformed private evidence")
			os.Exit(1)
		}
	}()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "staking comparison stopped:", err)
		os.Exit(1)
	}
}
