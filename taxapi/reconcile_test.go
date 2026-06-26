package taxapi

import (
	"testing"
	"time"
)

func TestSummarizeCoverage(t *testing.T) {
	rows := []typeCount{
		{MessageType: "/cosmwasm.wasm.v1.MsgExecuteContract", Total: 128, Classified: 0},
		{MessageType: "/cosmos.bank.v1beta1.MsgSend", Total: 38, Classified: 38},
		{MessageType: "/cosmos.staking.v1beta1.MsgDelegate", Total: 8, Classified: 5},
	}
	rep := summarizeCoverage(rows, time.Time{}, time.Time{})

	if rep.TotalMessages != 174 || rep.ClassifiedMsgs != 43 {
		t.Fatalf("totals wrong: total=%d classified=%d", rep.TotalMessages, rep.ClassifiedMsgs)
	}
	// 43/174 = 24.71%
	if rep.CoveragePercent < 24.7 || rep.CoveragePercent > 24.72 {
		t.Fatalf("coverage%% wrong: %v", rep.CoveragePercent)
	}
	if rep.SupportedTypes != 2 || rep.UnsupportedTypes != 1 {
		t.Fatalf("type split wrong: supported=%d unsupported=%d", rep.SupportedTypes, rep.UnsupportedTypes)
	}
	// The single gap must be the WASM contract type, with its full count surfaced.
	if len(rep.Gaps) != 1 || rep.Gaps[0].MessageType != "/cosmwasm.wasm.v1.MsgExecuteContract" || rep.Gaps[0].Unclassified != 128 {
		t.Fatalf("gap detection wrong: %+v", rep.Gaps)
	}
	// A partially-classified supported type reports the remainder as unclassified.
	var del CoverageRow
	for _, r := range rep.Rows {
		if r.MessageType == "/cosmos.staking.v1beta1.MsgDelegate" {
			del = r
		}
	}
	if !del.Supported || del.Unclassified != 3 {
		t.Fatalf("delegate row wrong: %+v", del)
	}
}

// supportedTypes must stay in lockstep with the parser's registered URLs.
func TestSupportedTypesMatchParser(t *testing.T) {
	if len(supportedTypes) == 0 {
		t.Fatal("supportedTypes is empty")
	}
	for _, u := range []string{
		"/cosmos.bank.v1beta1.MsgSend",
		"/cosmos.distribution.v1beta1.MsgWithdrawDelegatorReward",
		"/cosmos.authz.v1beta1.MsgExec",
	} {
		if !supportedTypes[u] {
			t.Fatalf("expected %s to be supported", u)
		}
	}
}
