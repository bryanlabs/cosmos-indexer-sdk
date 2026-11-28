// balanceexpect writes the chain's own record of wallets' holdings at block
// heights, for balancecheck -expect: liquid coins by denom, delegations and
// unbonding by validator, in base units. A height one below a genesis
// export's initial height is read from the export; any other height from an
// archive node's REST API at that height. The output (created 0600) holds
// balances, so keep it private; the terminal gets counts only.
//
//	balanceexpect -genesis hub4-genesis.json -archive-rest https://archive \
//	    -heights 5200790,8700000 -out private/expected.jsonl < addresses.txt
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/taxapi"
	"github.com/shopspring/decimal"
)

type coin struct {
	Denom  string `json:"denom"`
	Amount string `json:"amount"`
}

type genesis struct {
	InitialHeight string `json:"initial_height"`
	AppState      struct {
		Bank struct {
			Balances []struct {
				Address string `json:"address"`
				Coins   []coin `json:"coins"`
			} `json:"balances"`
		} `json:"bank"`
		Staking struct {
			Validators []struct {
				OperatorAddress string `json:"operator_address"`
				Tokens          string `json:"tokens"`
				DelegatorShares string `json:"delegator_shares"`
			} `json:"validators"`
			Delegations []struct {
				DelegatorAddress string `json:"delegator_address"`
				ValidatorAddress string `json:"validator_address"`
				Shares           string `json:"shares"`
			} `json:"delegations"`
			UnbondingDelegations []struct {
				DelegatorAddress string `json:"delegator_address"`
				ValidatorAddress string `json:"validator_address"`
				Entries          []struct {
					Balance string `json:"balance"`
				} `json:"entries"`
			} `json:"unbonding_delegations"`
		} `json:"staking"`
	} `json:"app_state"`
}

func add(m map[string]string, k, v string) error {
	n, ok := new(big.Int).SetString(v, 10)
	if !ok {
		return fmt.Errorf("amount %q", v)
	}
	if prev, ok := new(big.Int).SetString(m[k], 10); ok {
		n.Add(n, prev)
	}
	m[k] = n.String()
	return nil
}

// fromGenesis reads each wanted wallet's state at the export. A delegation's
// tokens are its shares at the validator's rate, truncated as the chain does.
func fromGenesis(path string, wanted map[string]bool) (int64, map[string]taxapi.HeightHoldings, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	var g genesis
	if err := json.NewDecoder(f).Decode(&g); err != nil {
		return 0, nil, err
	}
	initial, err := strconv.ParseInt(g.InitialHeight, 10, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("initial_height %q", g.InitialHeight)
	}
	h := initial - 1
	out := map[string]taxapi.HeightHoldings{}
	get := func(addr string) taxapi.HeightHoldings {
		r, ok := out[addr]
		if !ok {
			r = taxapi.HeightHoldings{Address: addr, Height: h, Liquid: map[string]string{}, Delegations: map[string]string{}, Unbonding: map[string]string{}}
		}
		return r
	}
	for addr := range wanted {
		out[addr] = get(addr)
	}
	for _, b := range g.AppState.Bank.Balances {
		if !wanted[b.Address] {
			continue
		}
		r := get(b.Address)
		for _, c := range b.Coins {
			if err := add(r.Liquid, c.Denom, c.Amount); err != nil {
				return 0, nil, err
			}
		}
		out[b.Address] = r
	}
	type rate struct{ tokens, shares decimal.Decimal }
	rates := map[string]rate{}
	for _, v := range g.AppState.Staking.Validators {
		t, err1 := decimal.NewFromString(v.Tokens)
		s, err2 := decimal.NewFromString(v.DelegatorShares)
		if err1 != nil || err2 != nil {
			return 0, nil, fmt.Errorf("validator rate unreadable")
		}
		rates[v.OperatorAddress] = rate{t, s}
	}
	for _, d := range g.AppState.Staking.Delegations {
		if !wanted[d.DelegatorAddress] {
			continue
		}
		r := get(d.DelegatorAddress)
		v, ok := rates[d.ValidatorAddress]
		shares, err := decimal.NewFromString(d.Shares)
		if !ok || err != nil || v.shares.IsZero() {
			return 0, nil, fmt.Errorf("delegation shares unreadable")
		}
		tokens, _ := shares.Mul(v.tokens).QuoRem(v.shares, 0)
		if err := add(r.Delegations, d.ValidatorAddress, tokens.String()); err != nil {
			return 0, nil, err
		}
		out[d.DelegatorAddress] = r
	}
	for _, u := range g.AppState.Staking.UnbondingDelegations {
		if !wanted[u.DelegatorAddress] {
			continue
		}
		r := get(u.DelegatorAddress)
		for _, e := range u.Entries {
			if err := add(r.Unbonding, u.ValidatorAddress, e.Balance); err != nil {
				return 0, nil, err
			}
		}
		out[u.DelegatorAddress] = r
	}
	return h, out, nil
}

type archive struct {
	origin string
	client *http.Client
	gap    time.Duration
	last   time.Time
}

// get reads every page of one REST list at a height, waiting out the
// archive's request budget between requests.
func (a *archive) get(path string, height int64, field string, each func(json.RawMessage) error) error {
	key := ""
	for {
		if wait := a.gap - time.Since(a.last); wait > 0 {
			time.Sleep(wait)
		}
		q := url.Values{"pagination.limit": {"1000"}}
		if key != "" {
			q.Set("pagination.key", key)
		}
		req, err := http.NewRequest(http.MethodGet, a.origin+path+"?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		req.Header.Set("x-cosmos-block-height", strconv.FormatInt(height, 10))
		a.last = time.Now()
		resp, err := a.client.Do(req)
		if err != nil {
			// The request error names the URL, which holds the wallet.
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return fmt.Errorf("archive %s at %d: %w", strings.Split(path, "/")[2], height, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("archive %s at %d: HTTP %d", strings.Split(path, "/")[2], height, resp.StatusCode)
		}
		var page map[string]json.RawMessage
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		var items []json.RawMessage
		if err := json.Unmarshal(page[field], &items); err != nil {
			return err
		}
		for _, it := range items {
			if err := each(it); err != nil {
				return err
			}
		}
		var p struct {
			NextKey string `json:"next_key"`
		}
		_ = json.Unmarshal(page["pagination"], &p)
		if p.NextKey == "" {
			return nil
		}
		key = p.NextKey
	}
}

func (a *archive) holdings(addr string, height int64) (taxapi.HeightHoldings, error) {
	r := taxapi.HeightHoldings{Address: addr, Height: height, Liquid: map[string]string{}, Delegations: map[string]string{}, Unbonding: map[string]string{}}
	if err := a.get("/cosmos/bank/v1beta1/balances/"+addr, height, "balances", func(m json.RawMessage) error {
		var c coin
		if err := json.Unmarshal(m, &c); err != nil {
			return err
		}
		return add(r.Liquid, c.Denom, c.Amount)
	}); err != nil {
		return r, err
	}
	if err := a.get("/cosmos/staking/v1beta1/delegations/"+addr, height, "delegation_responses", func(m json.RawMessage) error {
		var d struct {
			Delegation struct {
				ValidatorAddress string `json:"validator_address"`
			} `json:"delegation"`
			Balance coin `json:"balance"`
		}
		if err := json.Unmarshal(m, &d); err != nil {
			return err
		}
		return add(r.Delegations, d.Delegation.ValidatorAddress, d.Balance.Amount)
	}); err != nil {
		return r, err
	}
	err := a.get("/cosmos/staking/v1beta1/delegators/"+addr+"/unbonding_delegations", height, "unbonding_responses", func(m json.RawMessage) error {
		var u struct {
			ValidatorAddress string `json:"validator_address"`
			Entries          []struct {
				Balance string `json:"balance"`
			} `json:"entries"`
		}
		if err := json.Unmarshal(m, &u); err != nil {
			return err
		}
		for _, e := range u.Entries {
			if err := add(r.Unbonding, u.ValidatorAddress, e.Balance); err != nil {
				return err
			}
		}
		return nil
	})
	return r, err
}

func main() {
	genesisPath := flag.String("genesis", "", "genesis export; serves the height one below its initial height")
	origin := flag.String("archive-rest", os.Getenv("ARCHIVE_REST"), "archive node REST origin for every other height")
	heightList := flag.String("heights", "", "comma-separated block heights")
	outPath := flag.String("out", "", "private JSONL output")
	rps := flag.Float64("rps", 1, "archive requests per second")
	flag.Parse()
	if *outPath == "" || *heightList == "" {
		log.Fatal("-heights and -out are required")
	}
	var heights []int64
	for _, h := range strings.Split(*heightList, ",") {
		n, err := strconv.ParseInt(strings.TrimSpace(h), 10, 64)
		if err != nil {
			log.Fatalf("bad height %q", h)
		}
		heights = append(heights, n)
	}
	var addrs []string
	wanted := map[string]bool{}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		if a := strings.TrimSpace(sc.Text()); a != "" && !wanted[a] {
			addrs = append(addrs, a)
			wanted[a] = true
		}
	}
	var genesisHeight int64 = -1
	var atGenesis map[string]taxapi.HeightHoldings
	if *genesisPath != "" {
		var err error
		if genesisHeight, atGenesis, err = fromGenesis(*genesisPath, wanted); err != nil {
			log.Fatalf("genesis: %v", err)
		}
	}
	a := &archive{origin: strings.TrimRight(*origin, "/"), client: &http.Client{Timeout: 60 * time.Second}, gap: time.Duration(float64(time.Second) / *rps)}
	written, failed, err := write(*outPath, addrs, heights, genesisHeight, atGenesis, a)
	if err != nil {
		log.Fatalf("out: %v", err)
	}
	fmt.Printf("wrote %d expected states, %d failed\n", written, failed)
}

func write(path string, addrs []string, heights []int64, genesisHeight int64, atGenesis map[string]taxapi.HeightHoldings, a *archive) (written, failed int, err error) {
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	enc := json.NewEncoder(out)
	for i, addr := range addrs {
		for _, h := range heights {
			r := atGenesis[addr]
			if h != genesisHeight {
				if a.origin == "" {
					return written, failed, fmt.Errorf("height %d needs -archive-rest", h)
				}
				var gerr error
				if r, gerr = a.holdings(addr, h); gerr != nil {
					failed++
					fmt.Printf("wallet %d height %d: %v\n", i+1, h, gerr)
					continue
				}
			}
			if err := enc.Encode(r); err != nil {
				return written, failed, err
			}
			written++
		}
	}
	return written, failed, nil
}
