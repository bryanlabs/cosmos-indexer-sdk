package tax

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/config"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	distribution "github.com/cosmos/cosmos-sdk/x/distribution/types"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
)

func rewardFixture(t *testing.T, name string) *txlog.LogMessage {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Code   int                     `json:"code"`
		Events []txlog.LogMessageEvent `json:"events"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Code != 0 {
		t.Fatal("fixture transaction failed")
	}
	log := &txlog.LogMessage{}
	for _, event := range fixture.Events {
		if attrMap(event)["msg_index"] == "0" {
			log.Events = append(log.Events, event)
		}
	}
	return log
}

func TestMalformedRewardFailsInsteadOfDeletingIncome(t *testing.T) {
	msg := &distribution.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val}
	for _, amount := range []string{"not-coins", "-12uatom"} {
		log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
			ev("withdraw_rewards", [2]string{"delegator", del}, [2]string{"validator", val}, [2]string{"amount", amount}),
		}}
		if _, err := (&Parser{}).ParseMessage(msg, log, config.IndexConfig{}); err == nil {
			t.Fatalf("malformed reward amount %q silently accepted", amount)
		}
	}
}

func TestLegacyWithdrawalsWithoutDelegatorAndFlattenedValidators(t *testing.T) {
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("withdraw_rewards", [2]string{"amount", "35629uatom"}, [2]string{"validator", "source"}, [2]string{"amount", "47507213uatom"}, [2]string{"validator", "destination"}),
	}}
	for _, msg := range []sdk.Msg{&staking.MsgBeginRedelegate{DelegatorAddress: del}, &distribution.MsgWithdrawDelegatorReward{DelegatorAddress: del}} {
		data, err := (&Parser{}).ParseMessage(msg, log, config.IndexConfig{})
		if err != nil {
			t.Fatal(err)
		}
		out := (*data).([]TaxableEvent)
		if len(out) != 2 || out[0].Amount != "35629" || out[0].ValidatorAddress != "source" || out[1].Amount != "47507213" || out[1].ValidatorAddress != "destination" || out[0].ToAddr != del {
			t.Fatalf("legacy rewards lost: %+v", out)
		}
	}
	inner := &distribution.MsgWithdrawDelegatorReward{DelegatorAddress: del}
	exec := authz.NewMsgExec(sdk.AccAddress("grantee"), []sdk.Msg{inner})
	log.Events[0].Attributes = append(log.Events[0].Attributes, txlog.Attribute{Key: "authz_msg_index", Value: "0"})
	if out := classify(&exec, log); len(out) != 2 {
		t.Fatalf("legacy authz rewards lost: %+v", out)
	}
}

func TestDelegatePrincipalMainnetIsNotReward(t *testing.T) {
	msg := &staking.MsgDelegate{DelegatorAddress: "cosmos1ts4vwmfccwjv0vehlzd4x5rw5xzljjnhgm7gnz", ValidatorAddress: "cosmosvaloper1clpqr4nrk4khgkxj78fcwwh6dl3uw4epsluffn", Amount: sdk.NewInt64Coin("uatom", 94405000000)}
	log := rewardFixture(t, "delegate-principal")
	if out := classify(msg, log); len(out) != 0 {
		t.Fatalf("94,405 ATOM principal became income: %+v", out)
	}
	p := &Parser{}
	dataset, err := p.ParseMessage(msg, log, config.IndexConfig{})
	if err != nil || dataset == nil {
		t.Fatalf("empty reclassification must reach IndexMessage: %v", err)
	}
}

func TestClaimMulticoinMainnetKeepsValidator(t *testing.T) {
	wallet := "cosmos1ts4vwmfccwjv0vehlzd4x5rw5xzljjnhgm7gnz"
	validator := "cosmosvaloper1n229vhepft6wnkt5tjpwmxdmcnfz55jv3vp77d"
	out := classify(&distribution.MsgWithdrawDelegatorReward{DelegatorAddress: wallet, ValidatorAddress: validator}, rewardFixture(t, "claim-multicoin"))
	if len(out) != 2 {
		t.Fatalf("want ATOM and USDC rewards: %+v", out)
	}
	amounts := map[string]string{}
	for _, e := range out {
		if e.ValidatorAddress != validator || e.RewardTrigger != "claim" || e.ToAddr != wallet {
			t.Fatalf("bad attribution: %+v", e)
		}
		amounts[e.Denom] = e.Amount
	}
	if amounts["uatom"] != "939305598" || amounts["ibc/27BCBC098A3AE31C80E18A3EA7A516F2530B7362F83D7992A4D7888DBB586D33"] != "243" {
		t.Fatal(amounts)
	}
}

func TestRedelegateMainnetSeparatesBothValidators(t *testing.T) {
	wallet := "cosmos1f3vdsge09avpxsym5233xgskwv2q5s3cg57dcs"
	src := "cosmosvaloper1gjtvly9lel6zskvwtvlg5vhwpu9c9waw7sxzwx"
	dst := "cosmosvaloper13x77yexvf6qexfjg9czp6jhpv7vpjdwwkyhe4p"
	msg := &staking.MsgBeginRedelegate{DelegatorAddress: wallet, ValidatorSrcAddress: src, ValidatorDstAddress: dst, Amount: sdk.NewInt64Coin("uatom", 18885870000)}
	out := classify(msg, rewardFixture(t, "redelegate-two-validators"))
	if len(out) != 2 {
		t.Fatalf("must retain two validator rewards: %+v", out)
	}
	if out[0].ValidatorAddress != src || out[0].Amount != "35629" || out[1].ValidatorAddress != dst || out[1].Amount != "47507213" {
		t.Fatalf("bad reward attribution: %+v", out)
	}
	for _, e := range out {
		if e.RewardTrigger != "redelegate" {
			t.Fatal(e)
		}
	}
}

func TestStakingAutoWithdrawIgnoresPoolPrincipal(t *testing.T) {
	for name, msg := range map[string]sdk.Msg{
		"delegate":   &staking.MsgDelegate{DelegatorAddress: del},
		"undelegate": &staking.MsgUndelegate{DelegatorAddress: del},
		"redelegate": &staking.MsgBeginRedelegate{DelegatorAddress: del},
	} {
		t.Run(name, func(t *testing.T) {
			log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
				ev("coin_received", [2]string{"receiver", "staking-pool"}, [2]string{"amount", "94405000000uatom"}),
				ev("coin_received", [2]string{"receiver", other}, [2]string{"amount", "500uatom"}),
				ev("withdraw_rewards", [2]string{"delegator", del}, [2]string{"validator", val}, [2]string{"amount", "500uatom"}),
				ev("withdraw_rewards", [2]string{"delegator", other}, [2]string{"validator", val}, [2]string{"amount", "100uatom"}),
				ev("withdraw_rewards", [2]string{"delegator", del}, [2]string{"validator", val}, [2]string{"amount", "0uatom"}),
			}}
			out := classify(msg, log)
			if len(out) != 1 || out[0].Amount != "500" || out[0].RewardTrigger != name || out[0].ValidatorAddress != val {
				t.Fatalf("wrong income: %+v", out)
			}
		})
	}
}

func TestAuthzClaimAndDelegateDoNotCountTwice(t *testing.T) {
	inner := []sdk.Msg{&distribution.MsgWithdrawDelegatorReward{DelegatorAddress: del, ValidatorAddress: val}, &staking.MsgDelegate{DelegatorAddress: del, ValidatorAddress: val}}
	msg := authz.NewMsgExec(sdk.AccAddress("grantee"), inner)
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("withdraw_rewards", [2]string{"delegator", del}, [2]string{"validator", val}, [2]string{"amount", "500uatom"}, [2]string{"authz_msg_index", "0"}),
		ev("coin_received", [2]string{"receiver", "pool"}, [2]string{"amount", "500uatom"}, [2]string{"authz_msg_index", "1"}),
	}}
	out := classify(&msg, log)
	if len(out) != 1 || out[0].Amount != "500" || out[0].RewardTrigger != "claim" {
		t.Fatalf("authz double counted: %+v", out)
	}
}
