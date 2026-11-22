package main

import "testing"

// A recovered body may stand only for a stored message of the same type, and
// only for the types a replay cannot rebuild from events: MsgExec and the IBC
// refund lifecycle messages.
func TestRecoveredBodyTypes(t *testing.T) {
	const hash, v1Timeout, refused = "ABC", "/ibc.core.channel.v1.MsgTimeout", "type"
	for _, tc := range []struct {
		body, stored, want string
	}{
		{typeURLAuthzExec, typeURLAuthzExec, ""},
		{v1Timeout, v1Timeout, ""},
		{"/ibc.core.channel.v2.MsgTimeout", "/ibc.core.channel.v2.MsgTimeout", ""},
		{"/ibc.core.channel.v1.MsgAcknowledgement", "/ibc.core.channel.v1.MsgAcknowledgement", ""},
		{v1Timeout, typeURLAuthzExec, refused},
		{typeURLAuthzExec, v1Timeout, refused},
		{"/cosmos.bank.v1beta1.MsgSend", "/cosmos.bank.v1beta1.MsgSend", refused},
		{"/ibc.core.channel.v1.MsgRecvPacket", "/ibc.core.channel.v1.MsgRecvPacket", refused},
	} {
		got := refuseRecoveredBody(mbRow{TxHash: hash, MessageIndex: 1, TypeURL: tc.body}, "abc", 1, tc.stored)
		if got != tc.want {
			t.Errorf("body %s stored %s: got %q, want %q", tc.body, tc.stored, got, tc.want)
		}
	}
	if got := refuseRecoveredBody(mbRow{TxHash: hash, MessageIndex: 0, TypeURL: typeURLAuthzExec}, hash, 1, typeURLAuthzExec); got != "index" {
		t.Errorf("index mismatch: got %q", got)
	}
}
