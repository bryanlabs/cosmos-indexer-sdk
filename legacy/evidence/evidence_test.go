package evidence

import (
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/legacy"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/stretchr/testify/require"
)

func txOf(names ...string) *legacy.Tx {
	tx := &legacy.Tx{}
	for _, name := range names {
		tx.Messages = append(tx.Messages, legacy.Message{AminoName: "cosmos-sdk/" + name})
	}
	return tx
}

func tagsOf(pairs ...string) []rpc.LegacyTag {
	var tags []rpc.LegacyTag
	for i := 0; i < len(pairs); i += 2 {
		tags = append(tags, rpc.LegacyTag{Key: []byte(pairs[i]), Value: []byte(pairs[i+1])})
	}
	return tags
}

func TestTagsAreSplitByMessageOnHubOneAndTwo(t *testing.T) {
	for _, chain := range []string{"cosmoshub-1", "cosmoshub-2"} {
		tx := txOf("MsgWithdrawDelegationReward", "MsgWithdrawValidatorCommission", "MsgDelegate")
		tags := tagsOf(
			"action", "withdraw_delegator_reward", "rewards", "first", "delegator", "d",
			"action", "withdraw_validator_rewards_all", "commission", "second",
			"action", "delegate", "delegator", "d")
		got, err := For(chain, tx, &abci.ResponseDeliverTx{}, tags)
		require.NoError(t, err)
		require.Len(t, got, 3)
		require.Equal(t, tagsOf("rewards", "first", "delegator", "d"), got[0].Tags)
		require.Equal(t, tagsOf("commission", "second"), got[1].Tags)
		require.Equal(t, tagsOf("delegator", "d"), got[2].Tags)
		for i, item := range got {
			require.Equal(t, i, item.Index)
			require.True(t, item.Success)
		}
	}
}

func TestUnjailKeepsItsOwnActionTag(t *testing.T) {
	tx := txOf("MsgUnjail", "MsgDelegate")
	tags := tagsOf("action", "unjail", "action", "validator-unjailed", "validator", "v", "action", "delegate")
	got, err := For("cosmoshub-2", tx, &abci.ResponseDeliverTx{}, tags)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, tagsOf("action", "validator-unjailed", "validator", "v"), got[0].Tags)
	require.Empty(t, got[1].Tags)
}

func TestFailedTagsShowWhichMessageStopped(t *testing.T) {
	tx := txOf("MsgSend", "MsgDelegate", "MsgSend")
	got, err := For("cosmoshub-2", tx, &abci.ResponseDeliverTx{Code: 10}, tagsOf("action", "send", "sender", "s", "action", "delegate"))
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.True(t, got[0].Success)
	require.False(t, got[1].Success)

	got, err = For("cosmoshub-2", tx, &abci.ResponseDeliverTx{Code: 12}, nil)
	require.NoError(t, err)
	require.Empty(t, got, "a failure with no marker is no evidence, not an error")
}

func TestTagsThatDoNotFitFailClosed(t *testing.T) {
	ok := &abci.ResponseDeliverTx{}
	for name, c := range map[string]struct {
		tx   *legacy.Tx
		tags []rpc.LegacyTag
	}{
		"wrong order":        {txOf("MsgSend", "MsgDelegate"), tagsOf("action", "delegate", "action", "send")},
		"missing message":    {txOf("MsgSend", "MsgDelegate"), tagsOf("action", "send")},
		"extra message":      {txOf("MsgSend"), tagsOf("action", "send", "action", "send")},
		"no leading action":  {txOf("MsgSend"), tagsOf("sender", "s", "action", "send")},
		"hub three name":     {txOf("MsgWithdrawValidatorCommission"), tagsOf("action", "withdraw_validator_commission")},
		"unknown message":    {txOf("MsgSomethingElse"), tagsOf("action", "something")},
		"stray unjail value": {txOf("MsgSend"), tagsOf("action", "send", "action", "validator-unjailed")},
		"no tags on success": {txOf("MsgSend"), nil},
	} {
		got, err := For("cosmoshub-1", c.tx, ok, c.tags)
		require.Error(t, err, name)
		require.Nil(t, got, name)
	}
	_, err := For("cosmoshub-1", nil, ok, nil)
	require.Error(t, err)
}

const hubThreeLog = `[{"msg_index":0,"success":true,"log":"","events":[{"type":"message","attributes":[{"key":"sender","value":"s"},{"key":"action","value":"withdraw_delegator_reward"}]},{"type":"withdraw_rewards","attributes":[{"key":"amount","value":"first"}]}]},` +
	`{"msg_index":1,"success":true,"log":"","events":[{"type":"message","attributes":[{"key":"action","value":"withdraw_validator_commission"}]},{"type":"withdraw_commission","attributes":[{"key":"amount","value":"second"}]}]}]`

func TestLogEventsAreSplitByMessageOnHubThree(t *testing.T) {
	tx := txOf("MsgWithdrawDelegationReward", "MsgWithdrawValidatorCommission")
	got, err := For("cosmoshub-3", tx, &abci.ResponseDeliverTx{Log: hubThreeLog}, nil)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "withdraw_rewards", got[0].Events[1].Type)
	require.Equal(t, "first", got[0].Events[1].Attributes[0].Value)
	require.Equal(t, "second", got[1].Events[1].Attributes[0].Value)
	require.True(t, got[0].Success && got[1].Success)
}

func TestHubThreeFailuresAndRefusals(t *testing.T) {
	tx := txOf("MsgSend", "MsgDelegate")
	stopped := `[{"msg_index":0,"success":true,"log":"","events":[{"type":"message","attributes":[{"key":"action","value":"send"}]}]},{"msg_index":1,"success":false,"log":"x","events":[{"type":"message","attributes":[{"key":"action","value":"delegate"}]}]}]`
	got, err := For("cosmoshub-3", tx, &abci.ResponseDeliverTx{Code: 102, Log: stopped}, nil)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.False(t, got[1].Success)

	for _, log := range []string{`{"codespace":"sdk","code":12,"message":"out of gas"}`, "out of gas", ""} {
		got, err = For("cosmoshub-3", tx, &abci.ResponseDeliverTx{Code: 12, Log: log}, nil)
		require.NoError(t, err)
		require.Empty(t, got)
	}

	for name, log := range map[string]string{
		"object log on success": `{"message":"x"}`,
		"one entry of two":      `[{"msg_index":0,"success":true,"log":"","events":[{"type":"message","attributes":[{"key":"action","value":"send"}]}]}]`,
		"index out of order":    `[{"msg_index":1,"success":true,"log":"","events":[{"type":"message","attributes":[{"key":"action","value":"send"}]}]},{"msg_index":0,"success":true,"log":"","events":[{"type":"message","attributes":[{"key":"action","value":"delegate"}]}]}]`,
		"wrong action":          `[{"msg_index":0,"success":true,"log":"","events":[{"type":"message","attributes":[{"key":"action","value":"delegate"}]}]},{"msg_index":1,"success":true,"log":"","events":[{"type":"message","attributes":[{"key":"action","value":"send"}]}]}]`,
		"no message event":      `[{"msg_index":0,"success":true,"log":"","events":[]},{"msg_index":1,"success":true,"log":"","events":[]}]`,
		"continues after fail":  `[{"msg_index":0,"success":false,"log":"","events":[{"type":"message","attributes":[{"key":"action","value":"send"}]}]},{"msg_index":1,"success":true,"log":"","events":[{"type":"message","attributes":[{"key":"action","value":"delegate"}]}]}]`,
		"missing success":       `[{"msg_index":0,"log":"","events":[{"type":"message","attributes":[{"key":"action","value":"send"}]}]},{"msg_index":1,"success":true,"log":"","events":[{"type":"message","attributes":[{"key":"action","value":"delegate"}]}]}]`,
	} {
		got, err := For("cosmoshub-3", tx, &abci.ResponseDeliverTx{Log: log}, nil)
		require.Error(t, err, name)
		require.Nil(t, got, name)
	}
	_, err = For("cosmoshub-3", txOf("MsgSend"), &abci.ResponseDeliverTx{Log: stopped}, nil)
	require.ErrorContains(t, err, "more entries than messages")
}

func eventOf(kind string, pairs ...string) abci.Event {
	e := abci.Event{Type: kind}
	for i := 0; i < len(pairs); i += 2 {
		e.Attributes = append(e.Attributes, abci.EventAttribute{Key: pairs[i], Value: pairs[i+1]})
	}
	return e
}

func TestTransferSendersPairsBankPayersInOrder(t *testing.T) {
	// An undelegation: the reward payout, then the pool move, then the
	// handler's own message event, then the action marker.
	events := []abci.Event{
		eventOf("message", "sender", "distribution", "sender", "bonded", "module", "staking", "sender", "delegator", "action", "begin_unbonding"),
		eventOf("transfer", "recipient", "delegator", "amount", "5uatom", "recipient", "notbonded", "amount", "9uatom"),
		eventOf("unbond", "validator", "v", "amount", "9"),
	}
	got, err := TransferSenders("cosmos-sdk/MsgUndelegate", events)
	require.NoError(t, err)
	require.Equal(t, events[0], got[0])
	require.Equal(t, eventOf("transfer", "recipient", "delegator", "sender", "distribution", "amount", "5uatom",
		"recipient", "notbonded", "sender", "bonded", "amount", "9uatom"), got[1])
	require.Equal(t, events[2], got[2])
	require.Len(t, events[1].Attributes, 4, "the recorded events are not modified")
}

func TestTransferSendersLeavesOthersAloneAndRefusesMismatch(t *testing.T) {
	plain := []abci.Event{eventOf("message", "module", "staking", "sender", "delegator", "action", "delegate"), eventOf("delegate", "validator", "v", "amount", "9")}
	got, err := TransferSenders("cosmos-sdk/MsgDelegate", plain)
	require.NoError(t, err)
	require.Equal(t, plain, got)

	multi := []abci.Event{eventOf("message", "sender", "a", "sender", "b", "module", "bank", "action", "multisend"), eventOf("transfer", "recipient", "c", "amount", "1uatom")}
	got, err = TransferSenders("cosmos-sdk/MsgMultiSend", multi)
	require.NoError(t, err)
	require.Equal(t, multi, got)

	for name, events := range map[string][]abci.Event{
		"no payer":        {eventOf("message", "module", "staking", "sender", "d", "action", "delegate"), eventOf("transfer", "recipient", "d", "amount", "1uatom")},
		"extra payer":     {eventOf("message", "sender", "x", "sender", "y", "action", "send"), eventOf("transfer", "recipient", "d", "amount", "1uatom")},
		"odd attributes":  {eventOf("message", "sender", "x"), eventOf("transfer", "recipient", "d")},
		"already has one": {eventOf("message", "sender", "x"), eventOf("transfer", "recipient", "d", "sender", "x", "amount", "1uatom")},
	} {
		got, err := TransferSenders("cosmos-sdk/MsgDelegate", events)
		require.Error(t, err, name)
		require.Nil(t, got, name)
	}
}
