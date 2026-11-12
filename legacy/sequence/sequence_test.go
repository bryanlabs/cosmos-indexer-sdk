package sequence

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const hubOneForm = `{"chain_id":"cosmoshub-1","app_state":{"auth":{"params":{}},"accounts":[
 {"address":"a","coins":[],"sequence_number":"0","account_number":"0","original_vesting":null},
 {"address":"b","coins":[],"sequence_number":"7","account_number":"0"}],
 "gentxs":[{"type":"auth/StdTx","value":{"msg":[{"type":"cosmos-sdk/MsgCreateValidator","value":{"delegator_address":"a","validator_address":"v"}}],"signatures":[{"signature":"x"}]}}]}}`

const hubFourForm = `{"chain_id":"cosmoshub-4","app_state":{"auth":{"accounts":[
 {"@type":"/cosmos.auth.v1beta1.BaseAccount","address":"a","pub_key":null,"account_number":"1","sequence":"4"},
 {"@type":"/cosmos.vesting.v1beta1.DelayedVestingAccount","base_vesting_account":{"base_account":{"address":"b","account_number":"2","sequence":"9"},"end_time":"1"}},
 {"@type":"/cosmos.auth.v1beta1.ModuleAccount","base_account":{"address":"m","account_number":"3","sequence":"0"},"name":"gov","permissions":[]}]}}}`

func TestReadGenesisReadsEveryAccountForm(t *testing.T) {
	one, err := ReadGenesis(strings.NewReader(hubOneForm))
	require.NoError(t, err)
	require.Equal(t, "cosmoshub-1", one.Chain)
	require.Equal(t, map[string]uint64{"a": 0, "b": 7}, one.Sequence)
	require.Equal(t, map[string]uint64{"a": 1}, one.GenesisSigned)

	four, err := ReadGenesis(strings.NewReader(hubFourForm))
	require.NoError(t, err)
	require.Equal(t, map[string]uint64{"a": 4, "b": 9, "m": 0}, four.Sequence)
	require.Empty(t, four.GenesisSigned)

	for name, bad := range map[string]string{
		"not JSON":          `{`,
		"no accounts":       `{"chain_id":"cosmoshub-2","app_state":{"accounts":[]}}`,
		"no chain":          `{"app_state":{"accounts":[{"address":"a","sequence_number":"0"}]}}`,
		"two places":        `{"chain_id":"c","app_state":{"accounts":[{"address":"a","sequence_number":"0"}],"auth":{"accounts":[{"address":"a","sequence":"0"}]}}}`,
		"no sequence":       `{"chain_id":"c","app_state":{"accounts":[{"address":"a"}]}}`,
		"bad sequence":      `{"chain_id":"c","app_state":{"accounts":[{"address":"a","sequence_number":"-1"}]}}`,
		"no address":        `{"chain_id":"c","app_state":{"accounts":[{"sequence_number":"1"}]}}`,
		"listed twice":      `{"chain_id":"c","app_state":{"accounts":[{"address":"a","sequence_number":"1"},{"address":"a","sequence_number":"1"}]}}`,
		"other genesis tx":  `{"chain_id":"c","app_state":{"accounts":[{"address":"a","sequence_number":"1"}],"gentxs":[{"value":{"msg":[{"type":"cosmos-sdk/MsgSend","value":{}}],"signatures":[{}]}}]}}`,
		"two message gentx": `{"chain_id":"c","app_state":{"accounts":[{"address":"a","sequence_number":"1"}],"gentxs":[{"value":{"msg":[{"type":"cosmos-sdk/MsgCreateValidator","value":{"delegator_address":"a"}},{"type":"cosmos-sdk/MsgCreateValidator","value":{"delegator_address":"a"}}],"signatures":[{}]}}]}}`,
	} {
		_, err := ReadGenesis(strings.NewReader(bad))
		require.Error(t, err, name)
	}
}

func TestTallyRefusesWhatItCannotCount(t *testing.T) {
	tally := NewTally()
	require.Error(t, tally.Add("k", nil, "charged"))
	require.Error(t, tally.Add("k", []string{"a", "a"}, "charged"))
	require.Error(t, tally.Add("k", []string{""}, "charged"))
	require.Error(t, tally.Add("k", []string{"a"}, "probably"))
	require.Empty(t, tally.Charged)
	require.Empty(t, tally.Transactions)
}

func TestReconcileProvesCompleteAccountsAndSettlesUnprovenFees(t *testing.T) {
	opening := &Genesis{Chain: "cosmoshub-1", GenesisSigned: map[string]uint64{"exact": 1},
		Sequence: map[string]uint64{"exact": 0, "quiet": 3, "missing": 0, "all": 5, "none": 0, "some": 0, "over": 0, "pair-a": 0, "pair-b": 0}}
	closing := &Genesis{Chain: "cosmoshub-2", GenesisSigned: map[string]uint64{},
		Sequence: map[string]uint64{"exact": 3, "quiet": 3, "missing": 4, "all": 8, "none": 1, "some": 2, "over": 9, "new": 2, "pair-a": 1, "pair-b": 0}}

	first, second := NewTally(), NewTally()
	add := func(tally *Tally, key, charge string, signers ...string) {
		require.NoError(t, tally.Add(key, signers, charge))
	}
	// exact: one genesis transaction plus two charged equals the closing 3.
	add(first, "cosmoshub-1/1/0", "charged", "exact")
	add(second, "cosmoshub-1/2/0", "charged", "exact")
	add(first, "cosmoshub-1/2/1", "not charged", "exact")
	// missing: the index holds 3 of the 4 the chain counted.
	add(first, "cosmoshub-1/3/0", "charged", "missing")
	add(first, "cosmoshub-1/3/1", "charged", "missing")
	add(second, "cosmoshub-1/3/2", "charged", "missing")
	// all: moved by 3 with 1 proven, so both unproven failures paid.
	add(first, "cosmoshub-1/4/0", "charged", "all")
	add(first, "cosmoshub-1/4/1", "unknown", "all")
	add(second, "cosmoshub-1/4/2", "unknown", "all")
	// none: moved by 1 with 1 proven, so its unproven failure did not pay.
	add(first, "cosmoshub-1/5/0", "charged", "none")
	add(first, "cosmoshub-1/5/1", "unknown", "none")
	// some: moved by 2 with 1 proven and 2 unproven.
	add(first, "cosmoshub-1/6/0", "charged", "some")
	add(first, "cosmoshub-1/6/1", "unknown", "some")
	add(first, "cosmoshub-1/6/2", "unknown", "some")
	// over: moved by far more than everything the index holds.
	add(first, "cosmoshub-1/7/0", "unknown", "over")
	// new: an account created during the hub.
	add(second, "cosmoshub-1/8/0", "charged", "new")
	add(second, "cosmoshub-1/8/1", "charged", "new")
	// gone: paid a fee yet is not in the closing file.
	add(second, "cosmoshub-1/9/0", "charged", "gone")
	// pair: one failure signed by two accounts that disagree.
	add(first, "cosmoshub-1/10/0", "unknown", "pair-a", "pair-b")
	first.Merge(second)
	require.Equal(t, map[string]uint64{"charged": 11, "not charged": 1, "unknown": 7}, first.Transactions)

	report := Reconcile(opening, closing, first)
	require.Equal(t, 11, report.Accounts)
	require.Equal(t, 3, report.Exact, "exact, quiet and new")
	require.Equal(t, 2, report.Mismatched, "missing and gone")
	require.Equal(t, 1, report.ChargedAbsent)
	require.Equal(t, 4, report.Settled, "all, none, pair-a and pair-b")
	require.Equal(t, 1, report.Partial)
	require.Equal(t, 1, report.Impossible)
	require.Equal(t, 2, report.SettledCharged)
	require.Equal(t, 1, report.SettledNotCharged)
	require.Equal(t, 4, report.Unsettled, "two of some, one of over, the disagreeing pair")
	require.Equal(t, map[string]string{"cosmoshub-1/4/1": "charged", "cosmoshub-1/4/2": "charged", "cosmoshub-1/5/1": "not charged"}, report.Resolutions)
	require.Equal(t, []string{"cosmoshub-1/4/1", "cosmoshub-1/4/2", "cosmoshub-1/5/1"}, report.SortedResolutions())
}

func TestAheadFindsAnAccountCountedPastItsClosingSequence(t *testing.T) {
	opening := &Genesis{Sequence: map[string]uint64{"a": 2, "b": 0}, GenesisSigned: map[string]uint64{"b": 1}}
	closing := &Genesis{Sequence: map[string]uint64{"a": 4, "b": 2}}
	tally := NewTally()
	for i := 0; i < 2; i++ {
		require.NoError(t, tally.Add("k", []string{"a"}, "charged"))
		require.NoError(t, tally.Add("k", []string{"b"}, "charged"))
	}
	checked, ahead := Ahead(opening, closing, tally)
	require.Equal(t, 2, checked)
	require.Equal(t, 1, ahead, "b signed a genesis transaction, so two more put it past 2")
}
