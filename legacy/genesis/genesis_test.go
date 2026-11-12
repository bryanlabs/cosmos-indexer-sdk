package genesis

import (
	"fmt"
	"math/big"
	"math/rand"
	"strings"
	"testing"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"
)

// Addresses here are made up: the right shape, no real key behind them.
func account(n int) string {
	return "cosmos1" + strings.Repeat("q", 36) + strings.Repeat(string("acdefghj"[n%8]), 2)
}

func operator(n int) string {
	return "cosmosvaloper1" + strings.Repeat("z", 36) + strings.Repeat(string("acdefghj"[n%8]), 2)
}

func uatom(amount string) string {
	return fmt.Sprintf(`[{"amount":"%s","denom":"uatom"}]`, amount)
}

func flat(n int, coins, sequence, vesting, start string) string {
	return fmt.Sprintf(`{"address":"%s","coins":%s,"sequence_number":"%s","account_number":"%d","original_vesting":%s,
		"delegated_free":null,"delegated_vesting":null,"start_time":"%s","end_time":"1600000000"}`, account(n), coins, sequence, n, vesting, start)
}

func hubOne() string {
	return fmt.Sprintf(`{"chain_id":"cosmoshub-1","genesis_time":"2019-03-13T23:00:00Z","app_state":{
		"accounts":[%s,%s,%s],
		"staking":{"validators":null,"delegations":null,"unbonding_delegations":null},
		"distr":{"delegator_withdraw_infos":null,"outstanding_rewards":null,"validator_accumulated_commissions":null},
		"gentxs":[{"value":{"msg":[{"type":"cosmos-sdk/MsgCreateValidator","value":{"delegator_address":"%s","validator_address":"%s","value":{"amount":"400","denom":"uatom"}}}]}}]}}`,
		flat(0, uatom("1000"), "0", "null", "0"), flat(1, uatom("50"), "0", uatom("50"), "0"), flat(2, uatom("70"), "0", uatom("70"), "1550000000"),
		account(0), operator(0))
}

// One validator was slashed: 90 tokens stand behind 100 shares.
func hubTwo(shares string) string {
	return fmt.Sprintf(`{"chain_id":"cosmoshub-2","genesis_time":"2019-04-22T17:00:00Z","app_state":{
		"accounts":[%s,%s],
		"staking":{
			"validators":[{"operator_address":"%s","tokens":"90","delegator_shares":"100.000000000000000000","status":2,"jailed":false},
				{"operator_address":"%s","tokens":"0","delegator_shares":"0","status":0,"jailed":true}],
			"delegations":[{"delegator_address":"%s","validator_address":"%s","shares":"%s"},
				{"delegator_address":"%s","validator_address":"%s","shares":"66.666666666666666667"}],
			"unbonding_delegations":[{"delegator_address":"%s","validator_address":"%s","entries":[
				{"creation_height":"0","completion_time":"2019-05-01T10:00:00.123456789Z","initial_balance":"12","balance":"11"},
				{"creation_height":"0","completion_time":"2019-05-02T10:00:00Z","initial_balance":"5","balance":"5"}]}]},
		"distr":{"delegator_withdraw_infos":[{"delegator_address":"%s","withdraw_address":"%s"}],
			"outstanding_rewards":[{"validator_address":"%s","outstanding_rewards":null}],
			"validator_accumulated_commissions":[{"validator_address":"%s","accumulated":null}]},
		"gentxs":null}}`,
		flat(0, uatom("1000"), "7", "null", "0"), flat(1, "null", "0", "null", "0"),
		operator(0), operator(1), account(0), operator(0), shares, account(1), operator(0),
		account(0), operator(0), account(0), account(1), operator(0), operator(0))
}

func hubFour(supply string) string {
	return fmt.Sprintf(`{"chain_id":"cosmoshub-4","genesis_time":"2019-12-11T16:11:34Z","initial_height":"5200791","app_state":{
		"auth":{"accounts":[
			{"@type":"/cosmos.auth.v1beta1.BaseAccount","address":"%s","pub_key":null,"account_number":"9","sequence":"3"},
			{"@type":"/cosmos.vesting.v1beta1.ContinuousVestingAccount","base_vesting_account":{"base_account":{"address":"%s","pub_key":null,"account_number":"4","sequence":"0"},
				"original_vesting":%s,"delegated_free":%s,"delegated_vesting":%s,"end_time":"1700000000"},"start_time":"1600000000"},
			{"@type":"/cosmos.vesting.v1beta1.DelayedVestingAccount","base_vesting_account":{"base_account":{"address":"%s","pub_key":null,"account_number":"5","sequence":"0"},
				"original_vesting":%s,"delegated_free":[],"delegated_vesting":[],"end_time":"1700000000"}},
			{"@type":"/cosmos.auth.v1beta1.ModuleAccount","base_account":{"address":"%s","pub_key":null,"account_number":"6","sequence":"0"},"name":"bonded_tokens_pool","permissions":["burner","staking"]}]},
		"bank":{"balances":[{"address":"%s","coins":[]},{"address":"%s","coins":%s},{"address":"%s","coins":%s},{"address":"%s","coins":%s}],"supply":%s},
		"staking":{"validators":[{"operator_address":"%s","tokens":"30","delegator_shares":"30.000000000000000000","status":"BOND_STATUS_BONDED","jailed":false}],
			"delegations":[{"delegator_address":"%s","validator_address":"%s","shares":"30.000000000000000000"}],"unbonding_delegations":[]},
		"distribution":{"delegator_withdraw_infos":[],
			"outstanding_rewards":[{"validator_address":"%s","outstanding_rewards":[{"amount":"12.500000000000000000","denom":"uatom"}]}],
			"validator_accumulated_commissions":[{"validator_address":"%s","accumulated":{"commission":[{"amount":"1.250000000000000000","denom":"uatom"}]}}]}}}`,
		account(0), account(1), uatom("100"), uatom("10"), uatom("20"), account(2), uatom("40"), account(3),
		account(0), account(1), uatom("70"), account(2), uatom("40"), account(3), uatom("30"), supply,
		operator(0), account(1), operator(0), operator(0), operator(0))
}

func TestReadHubOneAppliesTheGenesisTransactions(t *testing.T) {
	file, err := Read(strings.NewReader(hubOne()))
	require.NoError(t, err)
	require.Equal(t, "cosmoshub-1", file.Chain)
	require.Equal(t, Counts{Accounts: 3, Delegations: 1, Validators: 1, GenesisTxs: 1}, file.Counts())
	first := file.Accounts[0]
	require.Equal(t, "1000", first.FileCoins.String())
	require.Equal(t, "600", first.Liquid.String())
	require.Equal(t, "400", first.Delegated.String())
	require.Equal(t, "base", first.Kind)
	require.Equal(t, "delayed_vesting", file.Accounts[1].Kind)
	require.Equal(t, "continuous_vesting", file.Accounts[2].Kind)
	require.True(t, file.Delegations[0].FromGenesisTx)
	require.Equal(t, "400", file.Delegations[0].Tokens.String())
	require.Equal(t, "400.000000000000000000", fixed(file.Delegations[0].Shares))
	require.Len(t, file.SHA256, 64)
}

func TestReadValuesSharesTheWayTheChainPaidThemOut(t *testing.T) {
	file, err := Read(strings.NewReader(hubTwo("33.333333333333333333")))
	require.NoError(t, err)
	require.Equal(t, Counts{Accounts: 2, Delegations: 2, Unbondings: 2, Validators: 2, Withdraws: 1}, file.Counts())
	// 33.333333333333333333 shares of 100, with 90 tokens behind them, are
	// 29.9999999999999999997 tokens. The chain rounded that quotient at 18
	// places first, which makes it 30, and then dropped the fraction. Plain
	// rounding down would say 29.
	require.Equal(t, "30", file.Delegations[0].Tokens.String())
	require.Equal(t, "60", file.Delegations[1].Tokens.String())
	require.Equal(t, "30", file.Accounts[0].Delegated.String())
	require.Equal(t, "16", file.Accounts[0].Unbonding.String())
	require.Equal(t, "1000", file.Accounts[0].Liquid.String())
	require.Equal(t, uint64(7), file.Accounts[0].Sequence)
	require.Equal(t, "0", file.Accounts[1].Liquid.String())
	require.Equal(t, "bonded", file.Validators[0].Status)
	require.Equal(t, "unbonded", file.Validators[1].Status)
	require.Zero(t, file.Validators[0].OutstandingRewards.Sign())
	require.Contains(t, file.Checks, "delegations to every validator add up to its shares exactly")
}

func TestReadHubFourJoinsBalancesAndKeepsUnpaidRewards(t *testing.T) {
	file, err := Read(strings.NewReader(hubFour(uatom("140"))))
	require.NoError(t, err)
	kinds := []string{}
	for _, a := range file.Accounts {
		kinds = append(kinds, a.Kind)
	}
	require.Equal(t, []string{"base", "continuous_vesting", "delayed_vesting", "module"}, kinds)
	require.Equal(t, "70", file.Accounts[1].Liquid.String())
	require.Equal(t, "100", file.Accounts[1].OriginalVesting.String())
	require.Equal(t, "10", file.Accounts[1].DelegatedFree.String())
	require.Equal(t, "20", file.Accounts[1].DelegatedVesting.String())
	require.Equal(t, int64(1600000000), file.Accounts[1].VestingStart)
	require.Equal(t, "30", file.Accounts[1].Delegated.String())
	require.Equal(t, "bonded_tokens_pool", file.Accounts[3].Module)
	require.Equal(t, uint64(9), file.Accounts[0].Number)
	require.Equal(t, uint64(3), file.Accounts[0].Sequence)
	require.Equal(t, "12.500000000000000000", fixed(file.Validators[0].OutstandingRewards))
	require.Equal(t, "1.250000000000000000", fixed(file.Validators[0].AccumulatedCommission))
	require.Contains(t, file.Checks, "account balances add up to the stated supply")
}

func TestReadRefusesAFileWhosePartsDisagree(t *testing.T) {
	cases := map[string]string{
		"delegations do not add up to the validator's shares": hubTwo("33.333333333333333332"),
		"balances do not add up to the supply":                hubFour(uatom("141")),
		"a coin that is not uatom":                            strings.Replace(hubOne(), `"amount":"1000","denom":"uatom"`, `"amount":"1000","denom":"photon"`, 1),
		"an account listed twice":                             strings.Replace(hubOne(), account(1), account(0), 1),
		"a genesis transaction larger than the balance":       strings.Replace(hubOne(), `"amount":"400"`, `"amount":"1001"`, 1),
		"an amount that is not a whole number":                strings.Replace(hubOne(), `"amount":"1000"`, `"amount":"1000.5"`, 1),
		"a negative amount":                                   strings.Replace(hubOne(), `"amount":"1000"`, `"amount":"-1000"`, 1),
		"a delegator with no account":                         strings.Replace(hubTwo("33.333333333333333333"), flat(1, "null", "0", "null", "0"), flat(2, "null", "0", "null", "0"), 1),
		"another chain":                                       strings.Replace(hubOne(), "cosmoshub-1", "cosmoshub-5", 1),
		"a balance with no account":                           strings.Replace(hubFour(uatom("140")), `{"address":"`+account(0)+`","coins":[]},`, "", 1),
	}
	for name, text := range cases {
		_, err := Read(strings.NewReader(text))
		require.Error(t, err, name)
		require.NotContains(t, err.Error(), "cosmos1", name)
	}
}

func TestChecksumCoversTheWholeStoredFile(t *testing.T) {
	text := hubOne() + "\n\n"
	stored := NewChecksum(strings.NewReader(text))
	file, err := Read(stored)
	require.NoError(t, err)
	require.Equal(t, file.SHA256, stored.Sum())
	other := NewChecksum(strings.NewReader(text + " "))
	_, err = Read(other)
	require.NoError(t, err)
	require.NotEqual(t, file.SHA256, other.Sum())
}

// The SDK's decimal type still divides the way it did on these chains
// (multiply by the precision twice, divide, round half to even at 18
// places). TokensFromShares must agree with it on every input, including
// the ones that land on or beside a rounding boundary.
func TestTokensFromSharesAgreesWithTheSDKDecimal(t *testing.T) {
	sdk := func(shares, tokens, all *big.Int) string {
		return sdkmath.LegacyNewDecFromBigIntWithPrec(shares, 18).MulInt(sdkmath.NewIntFromBigInt(tokens)).
			Quo(sdkmath.LegacyNewDecFromBigIntWithPrec(all, 18)).TruncateInt().String()
	}
	n := func(text string) *big.Int {
		v, ok := new(big.Int).SetString(text, 10)
		require.True(t, ok)
		return v
	}
	for _, c := range []struct{ shares, tokens, all, want string }{
		{"33333333333333333333", "90", "100000000000000000000", "30"},
		{"66666666666666666667", "90", "100000000000000000000", "60"},
		{"1000000000000000000", "1", "2000000000000000000", "0"},
		{"1000000000000000000", "999999", "1000000000000000000", "999999"},
		{"1", "1", "3", "0"},
		// Exactly half of the last place: 0.5e-18 rounds to even, down here and up below.
		{"1", "1", "2000000000000000000", "0"},
		{"999999999999999999", "2", "2000000000000000000", "0"},
	} {
		got := TokensFromShares(n(c.shares), n(c.tokens), n(c.all))
		require.Equal(t, c.want, got.String(), c)
		require.Equal(t, sdk(n(c.shares), n(c.tokens), n(c.all)), got.String(), c)
	}
	random := rand.New(rand.NewSource(814))
	differsFromPlainRoundingDown := 0
	for i := 0; i < 200000; i++ {
		tokens := big.NewInt(1 + random.Int63n(1_000_000_000_000))
		all := new(big.Int).Mul(big.NewInt(1+random.Int63n(1_000_000_000_000)), big.NewInt(1+random.Int63n(1_000_000_000)))
		shares := new(big.Int).Rand(random, all)
		if i%4 == 0 {
			// A third, a seventh and the like of a slashed validator: the cases that sit beside a whole coin.
			all = new(big.Int).Mul(big.NewInt(1+random.Int63n(1000)), one)
			tokens = big.NewInt(1 + random.Int63n(1000))
			shares = new(big.Int).Quo(new(big.Int).Mul(all, big.NewInt(1+random.Int63n(6))), big.NewInt(7-random.Int63n(5)))
			if shares.Cmp(all) > 0 {
				shares.Set(all)
			}
		}
		got := TokensFromShares(shares, tokens, all)
		require.Equal(t, sdk(shares, tokens, all), got.String(), "shares %s tokens %s all %s", shares, tokens, all)
		plain := new(big.Int).Mul(shares, tokens)
		if plain.Quo(plain, all).Cmp(got) != 0 {
			differsFromPlainRoundingDown++
		}
	}
	require.NotZero(t, differsFromPlainRoundingDown, "no input exercised the 18 place rounding")
}
