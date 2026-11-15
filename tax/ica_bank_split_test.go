package tax

import (
	"testing"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
)

func TestICAHostRealBankBatchLayouts(t *testing.T) {
	coins := sdk.NewCoins(sdk.NewInt64Coin(fixtureNativeDenom, 7))
	for _, multi := range []bool{false, true} {
		name := "send"
		first := icaSendAny(t, other)
		if multi {
			name = "multisend"
			first = icaAny(t, &banktypes.MsgMultiSend{Inputs: []banktypes.Input{{Address: icaTestAccount, Coins: coins}}, Outputs: []banktypes.Output{{Address: other, Coins: coins}}})
		}
		t.Run(name, func(t *testing.T) {
			msgs := []*codectypes.Any{first, icaSendAny(t, del)}
			for _, merged := range []bool{false, true} {
				log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{icaSuccess(), ev(eventMessage, [2]string{attributeAction, typeURLIBCRecvPacket}, [2]string{attributeSender, other}, [2]string{attributeModule, moduleIBCChannel})}}
				for i, recipient := range []string{other, del} {
					transfer := ev(string(CategoryTransfer), [2]string{attributeRecipient, recipient})
					if !multi || i != 0 {
						transfer.Attributes = append(transfer.Attributes, txlog.Attribute{Key: attributeSender, Value: icaTestAccount})
					}
					transfer.Attributes = append(transfer.Attributes, txlog.Attribute{Key: attributeAmount, Value: icaTestSevenUatom})
					log.Events = append(log.Events, ev(eventCoinSpent, [2]string{attributeSpender, icaTestAccount}, [2]string{attributeAmount, icaTestSevenUatom}), ev(eventCoinReceived, [2]string{attributeReceiver, recipient}, [2]string{attributeAmount, icaTestSevenUatom}), transfer, ev(eventMessage, [2]string{attributeSender, icaTestAccount}))
				}
				if merged {
					byType := map[string]int{}
					var events []txlog.LogMessageEvent
					for _, e := range log.Events {
						if j, ok := byType[e.Type]; ok {
							events[j].Attributes = append(events[j].Attributes, e.Attributes...)
						} else {
							byType[e.Type] = len(events)
							events = append(events, e)
						}
					}
					log.Events = events
				}
				out, err := icaParse(t, msgs, log)
				if err != nil || len(out) != 2 {
					t.Fatalf("real bank layout merged=%t: rows=%d error=%v", merged, len(out), err)
				}
				for _, kind := range []string{eventCoinSpent, eventCoinReceived, string(CategoryTransfer)} {
					bad := &txlog.LogMessage{}
					for _, e := range log.Events {
						attrs := append([]txlog.Attribute(nil), e.Attributes...)
						if e.Type == kind {
							for j := range attrs {
								if attrs[j].Key == attributeAmount {
									attrs[j].Value = icaTestNineUatom
									break
								}
							}
						}
						bad.Events = append(bad.Events, txlog.LogMessageEvent{Type: e.Type, Attributes: attrs})
					}
					icaAssertUnresolved(t, msgs, bad)
				}
				log.Events = append(log.Events, ev(eventWithdrawRewards, [2]string{attributeAmount, icaTestElevenUatom}, [2]string{attributeValidator, lsmTestValoperA}))
				icaAssertUnresolved(t, msgs, log)
			}
		})
	}
}

// SDK 0.47 outer signer bookkeeping cannot change body-proven bank rows.
func TestICAHostBankBatchOuterSenderBookkeeping(t *testing.T) {
	msgs := []*codectypes.Any{icaSendAny(t, other), icaSendAny(t, del)}
	inners := make([]sdk.Msg, len(msgs))
	for i, a := range msgs {
		inners[i], _ = unpackAuthzInner(a)
	}
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev(eventMessage, [2]string{attributeAction, typeURLIBCRecvPacket}, [2]string{attributeSender, other}, [2]string{attributeModule, moduleIBCChannel}, [2]string{attributeSender, icaTestAccount}, [2]string{attributeSender, icaTestAccount}),
		ev(string(CategoryTransfer), [2]string{attributeRecipient, other}, [2]string{attributeSender, icaTestAccount}, [2]string{attributeAmount, icaTestSevenUatom}, [2]string{attributeRecipient, del}, [2]string{attributeSender, icaTestAccount}, [2]string{attributeAmount, icaTestSevenUatom}),
	}}
	// The bank-only path must not depend on message bookkeeping at all.
	if _, err := icaHostSubLogs(msgs, inners, log); err != nil {
		t.Fatal(err)
	}
}
