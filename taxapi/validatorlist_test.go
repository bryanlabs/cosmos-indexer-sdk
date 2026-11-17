package taxapi

import (
	"testing"

	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

func TestLatestMonikers(t *testing.T) {
	const createID, editID = 7, 9
	types := map[int64]string{createID: validatorDescriptionTypes[0], editID: validatorDescriptionTypes[1]}
	wire := func(msg interface{ Marshal() ([]byte, error) }) []byte {
		b, err := msg.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	create := func(op, name string) validatorDescriptionMsg {
		return validatorDescriptionMsg{TypeID: createID, Raw: wire(&stakingtypes.MsgCreateValidator{
			ValidatorAddress: op, Description: stakingtypes.Description{Moniker: name}})}
	}
	edit := func(op, name string) validatorDescriptionMsg {
		return validatorDescriptionMsg{TypeID: editID, Raw: wire(&stakingtypes.MsgEditValidator{
			ValidatorAddress: op, Description: stakingtypes.Description{Moniker: name}})}
	}
	got := latestMonikers([]validatorDescriptionMsg{
		create("opA", "Alpha"),
		edit("opA", "Alpha Two"),
		edit("opA", stakingtypes.DoNotModifyDesc),
		create("opB", "Beta"),
		{TypeID: editID, Raw: []byte{0xff, 0xff}},
		edit("opC", stakingtypes.DoNotModifyDesc),
		{TypeID: 3, Raw: wire(&stakingtypes.MsgEditValidator{ValidatorAddress: "opB", Description: stakingtypes.Description{Moniker: "other type"}})},
	}, types)
	want := map[string]string{"opA": "Alpha Two", "opB": "Beta"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for op, name := range want {
		if got[op] != name {
			t.Errorf("%s: got %q want %q", op, got[op], name)
		}
	}
}
