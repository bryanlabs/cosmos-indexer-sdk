package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/legacy"
	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	"github.com/DefiantLabs/probe/client"
	abci "github.com/cometbft/cometbft/abci/types"
	tmjson "github.com/cometbft/cometbft/libs/json"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	tm "github.com/cometbft/cometbft/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/stretchr/testify/require"
	amino "github.com/tendermint/go-amino"
)

type intakeMsg interface{ marker() }
type intakeSend struct {
	From, To []byte
	Amount   []intakeCoin
}

func (intakeSend) marker() {}

type intakeCoin struct{ Denom, Amount string }
type intakeFee struct {
	Amount []intakeCoin
	Gas    uint64
}
type intakeSignature struct {
	PubKey    []byte
	Signature []byte
}
type intakeTx struct {
	Msgs       []intakeMsg
	Fee        intakeFee
	Signatures []intakeSignature
	Memo       string
}

func intakeRaw(t *testing.T, chain string) tm.Tx {
	t.Helper()
	cdc := amino.NewCodec()
	cdc.RegisterInterface((*intakeMsg)(nil), nil)
	cdc.RegisterConcrete(intakeSend{}, "cosmos-sdk/MsgSend", nil)
	name := "auth/StdTx"
	if chain == "cosmoshub-3" {
		name = "cosmos-sdk/StdTx"
	}
	cdc.RegisterConcrete(intakeTx{}, name, nil)
	a, b := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20)
	raw, err := cdc.MarshalBinaryLengthPrefixed(intakeTx{Msgs: []intakeMsg{intakeSend{a, b, []intakeCoin{{"uatom", "53"}}}}, Fee: intakeFee{Amount: []intakeCoin{{"uatom", "7"}}, Gas: 90000}})
	require.NoError(t, err)
	return raw
}
func TestLegacyOfflineAdapterFeesExecutionAndDuplicateInclusions(t *testing.T) {
	for _, chain := range []string{"cosmoshub-1", "cosmoshub-2", "cosmoshub-3"} {
		t.Run(chain, func(t *testing.T) {
			raw := intakeRaw(t, chain)
			input := &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: chain, Height: 9}, Data: tm.Data{Txs: tm.Txs{raw, raw}}}}
			json := `{"height":"9","results":{"DeliverTx":[{"log":"[]"},{"code":10}]}}`
			if chain == "cosmoshub-3" {
				json = `{"height":"9","results":{"deliver_tx":[{"code":0},{"code":10}]}}`
			}
			var results rpc.CustomBlockResults
			require.NoError(t, tmjson.Unmarshal([]byte(json), &results))
			items, err := DecodeLegacyBlock(input, &results)
			require.NoError(t, err)
			require.Len(t, items, 2)
			require.NotEqual(t, items[0].Identity, items[1].Identity)
			require.Equal(t, items[0].Hash, items[1].Hash)
			require.Equal(t, raw, tm.Tx(items[0].Tx.Raw))
			require.Equal(t, "succeeded", items[0].Execution)
			require.Equal(t, "failed", items[1].Execution)
			require.Equal(t, "charged", items[0].Fee.Charge)
			require.Equal(t, "unknown", items[1].Fee.Charge)
			require.Equal(t, "7", items[0].Fee.Declared[0].Amount.String())
			require.Equal(t, "7", items[1].Fee.Declared[0].Amount.String())
			require.Equal(t, bytes.Repeat([]byte{1}, 20), []byte(items[1].Fee.Payer))
			for _, item := range items {
				require.False(t, item.ClassificationReady)
				require.NotEmpty(t, item.Issues)
				require.Equal(t, "cosmos-sdk/MsgSend", item.Tx.Messages[0].AminoName)
			}
			rows, _, err := ProcessRPCBlockByHeightTXs(&config.IndexConfig{}, nil, &client.ChainClient{}, nil, nil, input, &results, nil)
			require.ErrorContains(t, err, "legacy serving intake blocked")
			require.Empty(t, rows)
		})
	}
}
func TestLegacyFailedFeeChargedOnlyWithExecutionMarker(t *testing.T) {
	for chain, json := range map[string]string{
		"cosmoshub-2": `{"height":"9","results":{"DeliverTx":[{"tags":[{"key":"YWN0aW9u","value":"c2VuZA=="}]},{"code":10,"tags":[{"key":"YWN0aW9u","value":"c2VuZA=="}]},{"code":12},{"code":12,"log":"{\"codespace\":\"sdk\",\"code\":12,\"message\":\"out of gas in location: txSize; gasWanted: 1, gasUsed: 2\"}"}]}}`,
		"cosmoshub-3": `{"height":"9","results":{"deliver_tx":[{"code":0,"log":"[{\"msg_index\":0,\"success\":true,\"log\":\"\",\"events\":[{\"type\":\"message\",\"attributes\":[{\"key\":\"action\",\"value\":\"send\"}]}]}]"},{"code":10,"log":"[{\"msg_index\":0,\"success\":false,\"log\":\"\",\"events\":[{\"type\":\"message\",\"attributes\":[{\"key\":\"action\",\"value\":\"send\"}]}]}]"},{"code":12,"log":"{\"code\":12}"},{"code":12,"log":"{\"codespace\":\"sdk\",\"code\":12,\"message\":\"out of gas in location: ante verify: secp256k1; gasWanted: 1, gasUsed: 2\"}"}]}}`,
	} {
		t.Run(chain, func(t *testing.T) {
			raw := intakeRaw(t, chain)
			input := &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: chain, Height: 9}, Data: tm.Data{Txs: tm.Txs{raw, raw, raw, raw}}}}
			var results rpc.CustomBlockResults
			require.NoError(t, tmjson.Unmarshal([]byte(json), &results))
			items, err := DecodeLegacyBlock(input, &results)
			require.NoError(t, err)
			require.Len(t, items, 4)
			require.Empty(t, items[3].Messages)
			require.Equal(t, "not charged", items[3].Fee.Charge)
			require.Len(t, items[0].Messages, 1)
			require.True(t, items[0].Messages[0].Success)
			require.Equal(t, "charged", items[0].Fee.Charge)
			require.Len(t, items[1].Messages, 1)
			require.False(t, items[1].Messages[0].Success)
			require.Equal(t, "charged", items[1].Fee.Charge)
			require.Empty(t, items[2].Messages)
			require.Equal(t, "unknown", items[2].Fee.Charge)
			for _, issue := range items[0].Issues {
				require.NotContains(t, issue, "unattributed")
			}
			require.Contains(t, items[2].Issues, "failed legacy transaction left no execution marker; fee commitment needs gas or state evidence")
		})
	}
}

func TestLegacyLedgerBlockBuildsHubThreeRowsOrHoldsTheHeight(t *testing.T) {
	raw := intakeRaw(t, "cosmoshub-3")
	when := time.Date(2020, 3, 1, 12, 0, 0, 0, time.UTC)
	input := &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: "cosmoshub-3", Height: 9, Time: when}, Data: tm.Data{Txs: tm.Txs{raw, raw}}}}
	blockHash := strings.Repeat("AB", 32)
	decode := func(json string) (*capture.Height, []LegacyInclusion) {
		var results rpc.CustomBlockResults
		require.NoError(t, tmjson.Unmarshal([]byte(json), &results))
		items, err := DecodeLegacyBlock(input, &results)
		require.NoError(t, err)
		return &capture.Height{Block: input, Results: &results, Hash: blockHash, Proposer: "AA11"}, items
	}
	const sent = `{"code":0,"log":"[{\"msg_index\":0,\"success\":true,\"log\":\"\",\"events\":[{\"type\":\"message\",\"attributes\":[{\"key\":\"sender\",\"value\":\"payer\"},{\"key\":\"module\",\"value\":\"bank\"},{\"key\":\"action\",\"value\":\"send\"}]},{\"type\":\"transfer\",\"attributes\":[{\"key\":\"recipient\",\"value\":\"someone\"},{\"key\":\"amount\",\"value\":\"53uatom\"}]}]}]"}`
	const failedAfter = `{"code":10,"log":"[{\"msg_index\":0,\"success\":false,\"log\":\"\",\"events\":[{\"type\":\"message\",\"attributes\":[{\"key\":\"action\",\"value\":\"send\"}]}]}]"}`
	const unproven = `{"code":12,"log":"{\"code\":12}"}`
	block, err := LegacyLedgerBlock(decode(`{"height":"9","results":{"deliver_tx":[` + sent + `,` + failedAfter + `]}}`))
	require.NoError(t, err)
	require.Equal(t, "cosmoshub-3", block.Chain)
	require.Equal(t, int64(9), block.Height)
	require.Equal(t, when, block.Time)
	require.Equal(t, blockHash, block.Hash)
	require.Equal(t, "AA11", block.Proposer)
	require.Len(t, block.Inclusions, 2)
	from := sdk.AccAddress(bytes.Repeat([]byte{1}, 20)).String()
	to := sdk.AccAddress(bytes.Repeat([]byte{2}, 20)).String()
	first := block.Inclusions[0]
	require.Equal(t, block.Inclusions[1].Hash, first.Hash, "both inclusions of the same bytes are kept")
	require.Equal(t, []string{from}, first.Signers)
	require.Equal(t, from, first.FeePayer)
	require.Equal(t, "7uatom", first.Fee)
	require.True(t, first.FeeCharged)
	require.Equal(t, "the transaction succeeded", first.FeeEvidence)
	require.Equal(t, []byte(raw), first.Raw)
	require.Equal(t, []ledger.Row{{Phase: ledger.PhaseTx, TxIndex: 0, MsgIndex: 0, SubIndex: 0, Category: "transfer", Amount: "53", Denom: "uatom",
		From: from, To: to, TxHash: first.Hash, Source: "message"}}, block.Rows, "a failed transaction moved nothing")
	failed := block.Inclusions[1]
	require.Equal(t, uint32(10), failed.Code)
	require.True(t, failed.FeeCharged, "a failure after execution began still paid its fee")
	require.Equal(t, "it failed after a message began executing", failed.FeeEvidence)
	require.Equal(t, "7uatom", failed.Fee)

	_, err = LegacyLedgerBlock(decode(`{"height":"9","results":{"deliver_tx":[` + sent + `,` + unproven + `]}}`))
	require.ErrorContains(t, err, "legacy transaction 1 held: fee charge unknown")
	for verdict, charged := range map[string]bool{"charged": true, "not charged": false} {
		h, items := decode(`{"height":"9","results":{"deliver_tx":[` + sent + `,` + unproven + `]}}`)
		require.Error(t, ResolveLegacyFees(items, map[string]string{"cosmoshub-3/9/1": "maybe"}))
		require.Error(t, ResolveLegacyFees(items, map[string]string{"cosmoshub-3/9/0": "not charged"}), "a proven fee is never overturned")
		require.NoError(t, ResolveLegacyFees(items, map[string]string{"cosmoshub-3/9/0": "charged", "cosmoshub-3/9/1": verdict, "cosmoshub-3/8/0": "charged"}))
		settled, err := LegacyLedgerBlock(h, items)
		require.NoError(t, err)
		require.Equal(t, uint32(12), settled.Inclusions[1].Code)
		require.Equal(t, charged, settled.Inclusions[1].FeeCharged, verdict)
		require.Equal(t, "7uatom", settled.Inclusions[1].Fee, "the signed fee is kept either way")
		require.Equal(t, "the payer's account sequence", settled.Inclusions[1].FeeEvidence)
		require.Len(t, settled.Rows, 1)
	}
	// A fee the sequence check left open is settled from the chain state, by
	// two archives only, and a settled fee is never touched that way.
	for verdict, charged := range map[string]bool{"charged": true, "not charged": false} {
		h, items := decode(`{"height":"9","results":{"deliver_tx":[` + sent + `,` + unproven + `]}}`)
		for _, refused := range []string{verdict, verdict + ", one archive", "maybe, two archives", verdict + ", two archives "} {
			_, err := ResolveLegacyFeeStates(items, map[string]string{"cosmoshub-3/9/1": refused})
			require.ErrorContains(t, err, "not recognized", refused)
		}
		_, err := ResolveLegacyFeeStates(items, map[string]string{"cosmoshub-3/9/0": "charged, two archives"})
		require.ErrorContains(t, err, "already settled")
		attached, err := ResolveLegacyFeeStates(items, map[string]string{"cosmoshub-3/9/1": verdict + ", two archives", "cosmoshub-3/8/1": "charged, two archives"})
		require.NoError(t, err)
		require.Equal(t, 1, attached, "a verdict for another height is not counted")
		settled, err := LegacyLedgerBlock(h, items)
		require.NoError(t, err)
		require.Equal(t, charged, settled.Inclusions[1].FeeCharged, verdict)
		require.Equal(t, "7uatom", settled.Inclusions[1].Fee)
		require.Equal(t, "the payer's account sequence in the chain state around the block", settled.Inclusions[1].FeeEvidence)
		require.Len(t, settled.Rows, 1)
		_, err = ResolveLegacyFeeStates(items, map[string]string{"cosmoshub-3/9/1": verdict + ", two archives"})
		require.ErrorContains(t, err, "already settled", "the sequence check or an earlier verdict got there first")
	}
	h, items := decode(`{"height":"9","results":{"deliver_tx":[` + sent + `,` + failedAfter + `]}}`)
	_, err = LegacyLedgerBlock(h, items[:1])
	require.ErrorContains(t, err, "do not cover the block")
	older := &capture.Height{Block: &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: "cosmoshub-2", Height: 9, Time: when}}}, Results: &rpc.CustomBlockResults{}}
	_, err = LegacyLedgerBlock(older, nil)
	require.ErrorContains(t, err, "only built for cosmoshub-3")
}

// What the chain itself paid to accounts at the end of a block becomes rows.
// Coins it moved between its own module accounts and the per validator
// allocations of every block are dropped. The rest is kept as evidence, and
// a movement nobody has classified holds the height.
func TestLegacyLedgerBlockReadsBlockLevelMovements(t *testing.T) {
	when := time.Date(2020, 3, 1, 12, 0, 0, 0, time.UTC)
	input := &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: "cosmoshub-3", Height: 9, Time: when}}}
	gov := authtypes.NewModuleAddress("gov").String()
	pool := authtypes.NewModuleAddress("distribution").String()
	collector := authtypes.NewModuleAddress("fee_collector").String()
	someone := sdk.AccAddress(bytes.Repeat([]byte{7}, 20)).String()
	event := func(kind string, pairs ...string) abci.Event {
		e := abci.Event{Type: kind}
		for i := 0; i < len(pairs); i += 2 {
			e.Attributes = append(e.Attributes, abci.EventAttribute{Key: pairs[i], Value: pairs[i+1]})
		}
		return e
	}
	h := &capture.Height{Block: input, Hash: strings.Repeat("CD", 32), Proposer: "AA11", Evidence: []json.RawMessage{json.RawMessage(`{ "type": "x" }`)},
		Results: &rpc.CustomBlockResults{
			BeginBlockEvents: []abci.Event{
				event("transfer", "recipient", pool, "amount", "5uatom"), event("message", "sender", collector),
				event("mint", "amount", "5"), event("rewards", "amount", "1uatom", "validator", "v"), event("liveness", "address", "c"),
				event("slash", "address", "c", "power", "10", "reason", "double_sign"),
			},
			EndBlockEvents: []abci.Event{
				event("complete_unbonding", "amount", "40uatom", "validator", "v", "delegator", someone),
				event("transfer", "recipient", someone, "amount", "512000000uatom"), event("message", "sender", gov),
				event("transfer", "recipient", someone, "amount", "9uatom"), event("message", "sender", pool),
				event("active_proposal", "proposal_id", "3", "proposal_result", "proposal_passed"),
				event("transfer", "recipient", someone, "amount", "4uatom"), event("message", "sender", pool),
			}}}
	block, err := LegacyLedgerBlock(h, nil)
	require.NoError(t, err)
	require.Equal(t, []ledger.Row{
		{Phase: ledger.PhaseEnd, SubIndex: 0, Category: "unbonding_complete", Amount: "40", Denom: "uatom", To: someone, Validator: "v", Source: "event"},
		{Phase: ledger.PhaseEnd, SubIndex: 1, Category: "gov_deposit_refund", Amount: "512000000", Denom: "uatom", From: gov, To: someone, Source: "event"},
		{Phase: ledger.PhaseEnd, SubIndex: 2, Category: "income", Amount: "9", Denom: "uatom", From: pool, To: someone, Trigger: "community_pool_spend", Source: "event"},
		{Phase: ledger.PhaseEnd, SubIndex: 3, Category: "commission", Amount: "4", Denom: "uatom", From: pool, To: someone, Trigger: "commission",
			Detail: `{"cause":"validator_removed"}`, Source: "event"},
	}, block.Rows)
	var kept []string
	for _, e := range block.Events {
		kept = append(kept, fmt.Sprintf("%d/%d %s", e.Phase, e.Index, e.Type))
	}
	require.Equal(t, []string{"1/5 slash", "1/6 block_evidence", "3/0 complete_unbonding", "3/1 transfer", "3/2 message", "3/3 transfer", "3/4 message", "3/5 active_proposal", "3/6 transfer", "3/7 message"}, kept)
	require.Equal(t, [][2]string{{"json", `{"type":"x"}`}}, block.Events[1].Attributes)
	require.Equal(t, [][2]string{{"amount", "40uatom"}, {"validator", "v"}, {"delegator", someone}}, block.Events[2].Attributes)

	h.Results.EndBlockEvents = []abci.Event{event("transfer", "recipient", someone, "amount", "9uatom"), event("message", "sender", collector)}
	_, err = LegacyLedgerBlock(h, nil)
	require.ErrorContains(t, err, "block coin movement not recognized")
	h.Results.EndBlockEvents = []abci.Event{event("complete_unbonding", "validator", "v", "delegator", someone)}
	_, err = LegacyLedgerBlock(h, nil)
	require.ErrorContains(t, err, "unbonding completion without one amount")
	// An unbonding that returned nothing has an empty amount. It gets no
	// row, its event is kept, and it does not hold the height.
	h.Results.EndBlockEvents = []abci.Event{event("complete_unbonding", "amount", "", "validator", "v", "delegator", someone),
		event("complete_unbonding", "amount", "7uatom", "validator", "v", "delegator", someone)}
	block, err = LegacyLedgerBlock(h, nil)
	require.NoError(t, err)
	require.Equal(t, []ledger.Row{{Phase: ledger.PhaseEnd, SubIndex: 0, Category: "unbonding_complete", Amount: "7", Denom: "uatom", To: someone, Validator: "v", Source: "event"}}, block.Rows)
	require.Equal(t, "3/0 complete_unbonding", fmt.Sprintf("%d/%d %s", block.Events[2].Phase, block.Events[2].Index, block.Events[2].Type))
	require.Equal(t, [][2]string{{"amount", ""}, {"validator", "v"}, {"delegator", someone}}, block.Events[2].Attributes)
	for _, broken := range [][]string{{"amount", "", "validator", "v"}, {"amount", "", "delegator", someone}, {"amount", "0uatom", "validator", "v", "delegator", someone},
		{"amount", "1uatom,2stake", "validator", "v", "delegator", someone}} {
		h.Results.EndBlockEvents = []abci.Event{event("complete_unbonding", broken...)}
		_, err = LegacyLedgerBlock(h, nil)
		require.ErrorContains(t, err, "unbonding completion without one amount")
	}
	h.Results.EndBlockEvents = []abci.Event{event("transfer", "recipient", someone, "amount", "9uatom")}
	_, err = LegacyLedgerBlock(h, nil)
	require.ErrorContains(t, err, "without a recipient or its sender")
}

type intakeDelegate struct {
	Delegator, Validator []byte
	Amount               intakeCoin
}

func (intakeDelegate) marker() {}

func TestLegacyLedgerBlockKeepsAStakingAmountWithoutDenomOnlyInAFailure(t *testing.T) {
	cdc := amino.NewCodec()
	cdc.RegisterInterface((*intakeMsg)(nil), nil)
	cdc.RegisterConcrete(intakeDelegate{}, "cosmos-sdk/MsgDelegate", nil)
	cdc.RegisterConcrete(intakeTx{}, "cosmos-sdk/StdTx", nil)
	a, b := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20)
	raw, err := cdc.MarshalBinaryLengthPrefixed(intakeTx{Msgs: []intakeMsg{intakeDelegate{a, b, intakeCoin{Amount: "5"}}}, Fee: intakeFee{Amount: []intakeCoin{{"uatom", "7"}}, Gas: 200000}})
	require.NoError(t, err)
	input := &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: "cosmoshub-3", Height: 9, Time: time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)}, Data: tm.Data{Txs: tm.Txs{raw}}}}
	decode := func(json string) (*capture.Height, []LegacyInclusion) {
		var results rpc.CustomBlockResults
		require.NoError(t, tmjson.Unmarshal([]byte(json), &results))
		items, err := DecodeLegacyBlock(input, &results)
		require.NoError(t, err)
		return &capture.Height{Block: input, Results: &results, Hash: strings.Repeat("AB", 32), Proposer: "AA11"}, items
	}
	// The handler refused the denom after the message began, so the fee was kept.
	block, err := LegacyLedgerBlock(decode(`{"height":"9","results":{"deliver_tx":[{"code":102,"codespace":"staking","log":"[{\"msg_index\":0,\"success\":false,\"log\":\"\",\"events\":[{\"type\":\"message\",\"attributes\":[{\"key\":\"action\",\"value\":\"delegate\"}]}]}]"}]}}`))
	require.NoError(t, err)
	require.Equal(t, uint32(102), block.Inclusions[0].Code)
	require.Equal(t, "7uatom", block.Inclusions[0].Fee)
	require.True(t, block.Inclusions[0].FeeCharged)
	require.Empty(t, block.Rows)

	// The chain could not have executed it, so a success is held, not classified.
	_, err = LegacyLedgerBlock(decode(`{"height":"9","results":{"deliver_tx":[{"code":0,"log":"[{\"msg_index\":0,\"success\":true,\"log\":\"\",\"events\":[{\"type\":\"delegate\",\"attributes\":[{\"key\":\"validator\",\"value\":\"someone\"},{\"key\":\"amount\",\"value\":\"5\"}]},{\"type\":\"message\",\"attributes\":[{\"key\":\"module\",\"value\":\"staking\"},{\"key\":\"sender\",\"value\":\"payer\"},{\"key\":\"action\",\"value\":\"delegate\"}]}]}]"}]}}`))
	require.ErrorContains(t, err, "legacy transaction 0 held: staking amount without a denom in a successful transaction")
}

type intakeParty struct {
	Address []byte
	Coins   []intakeCoin
}
type intakeMultiSend struct{ Inputs, Outputs []intakeParty }

func (intakeMultiSend) marker() {}

// A reward row names the delegator. The coins went to the withdraw address,
// which the row records when it is another address, and the rows must add up
// to what the distribution account paid. A multi send with several payers
// gets a row for each payer.
func TestLegacyLedgerBlockAccountsForEveryCoinAMessageMoved(t *testing.T) {
	cdc := amino.NewCodec()
	cdc.RegisterInterface((*intakeMsg)(nil), nil)
	cdc.RegisterConcrete(intakeDelegate{}, "cosmos-sdk/MsgDelegate", nil)
	cdc.RegisterConcrete(intakeMultiSend{}, "cosmos-sdk/MsgMultiSend", nil)
	cdc.RegisterConcrete(intakeTx{}, "cosmos-sdk/StdTx", nil)
	a, b, c := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20), bytes.Repeat([]byte{3}, 20)
	delegator, other, third := sdk.AccAddress(a).String(), sdk.AccAddress(b).String(), sdk.AccAddress(c).String()
	validator := sdk.ValAddress(b).String()
	pool := authtypes.NewModuleAddress("distribution").String()
	type attribute struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	type logEvent struct {
		Type       string      `json:"type"`
		Attributes []attribute `json:"attributes"`
	}
	event := func(kind string, pairs ...string) logEvent {
		e := logEvent{Type: kind}
		for i := 0; i < len(pairs); i += 2 {
			e.Attributes = append(e.Attributes, attribute{pairs[i], pairs[i+1]})
		}
		return e
	}
	build := func(msg intakeMsg, events ...logEvent) (*capture.Height, []LegacyInclusion) {
		raw, err := cdc.MarshalBinaryLengthPrefixed(intakeTx{Msgs: []intakeMsg{msg}, Fee: intakeFee{Amount: []intakeCoin{{"uatom", "7"}}, Gas: 200000}})
		require.NoError(t, err)
		input := &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: "cosmoshub-3", Height: 9, Time: time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)}, Data: tm.Data{Txs: tm.Txs{raw}}}}
		log, err := json.Marshal([]map[string]any{{"msg_index": 0, "success": true, "log": "", "events": events}})
		require.NoError(t, err)
		body, err := json.Marshal(map[string]any{"height": "9", "results": map[string]any{"deliver_tx": []map[string]any{{"code": 0, "log": string(log)}}}})
		require.NoError(t, err)
		var results rpc.CustomBlockResults
		require.NoError(t, tmjson.Unmarshal(body, &results))
		items, err := DecodeLegacyBlock(input, &results)
		require.NoError(t, err)
		return &capture.Height{Block: input, Results: &results, Hash: strings.Repeat("AB", 32), Proposer: "AA11"}, items
	}
	delegate := intakeDelegate{a, b, intakeCoin{"uatom", "5"}}
	staked := []logEvent{event("delegate", "validator", validator, "amount", "5"),
		event("message", "module", "staking", "sender", delegator, "action", "delegate")}
	payout := func(to, amount string) []logEvent {
		return []logEvent{event("transfer", "recipient", to, "amount", amount), event("message", "sender", pool)}
	}
	categories := func(block ledger.Block) (out []string) {
		for _, row := range block.Rows {
			out = append(out, fmt.Sprintf("%s %s from %q to %q %s", row.Category, row.Amount, row.From, row.To, row.Detail))
		}
		return out
	}

	block, err := LegacyLedgerBlock(build(delegate, append(payout(delegator, "3uatom"), staked...)...))
	require.NoError(t, err)
	require.Equal(t, []string{`reward 3 from "" to "` + delegator + `" `, `delegate 5 from "` + delegator + `" to "" `}, categories(block))

	block, err = LegacyLedgerBlock(build(delegate, append(payout(other, "3uatom"), staked...)...))
	require.NoError(t, err)
	require.Equal(t, []string{`reward 3 from "" to "` + delegator + `" {"paid_to":"` + other + `"}`, `delegate 5 from "` + delegator + `" to "" `}, categories(block))

	_, err = LegacyLedgerBlock(build(delegate, append(append(payout(delegator, "3uatom"), payout(other, "2uatom")...), staked...)...))
	require.ErrorContains(t, err, "the distribution account paid two addresses in one message")

	several := intakeMultiSend{Inputs: []intakeParty{{a, []intakeCoin{{"uatom", "4"}}}, {b, []intakeCoin{{"uatom", "6"}}}},
		Outputs: []intakeParty{{c, []intakeCoin{{"uatom", "10"}}}}}
	block, err = LegacyLedgerBlock(build(several, event("message", "action", "multisend")))
	require.NoError(t, err)
	require.Equal(t, []string{`transfer 10 from "" to "` + third + `" `, `transfer 4 from "` + delegator + `" to "" `, `transfer 6 from "` + other + `" to "" `}, categories(block))
	require.Equal(t, []int{0, 1, 2}, []int{block.Rows[0].SubIndex, block.Rows[1].SubIndex, block.Rows[2].SubIndex})

	one := intakeMultiSend{Inputs: several.Inputs[:1], Outputs: []intakeParty{{c, []intakeCoin{{"uatom", "4"}}}}}
	block, err = LegacyLedgerBlock(build(one, event("message", "action", "multisend")))
	require.NoError(t, err)
	require.Equal(t, []string{`transfer 4 from "` + delegator + `" to "` + third + `" `}, categories(block))
}

type intakeUndelegate struct {
	Delegator, Validator []byte
	Amount               intakeCoin
}

func (intakeUndelegate) marker() {}

type intakeRedelegate struct {
	Delegator, Source, Destination []byte
	Amount                         intakeCoin
}

func (intakeRedelegate) marker() {}

// Hub 3 removed an unbonded validator inside the message that took its last
// stake and paid its commission to the operator's account, with a transfer
// that looks like a reward. Only a verdict from the chain state lets that
// payment be stored as commission. Without one, or when the verdict and the
// payments disagree, the height is held.
func TestLegacyLedgerBlockSplitsTheCommissionOfARemovedValidator(t *testing.T) {
	cdc := amino.NewCodec()
	cdc.RegisterInterface((*intakeMsg)(nil), nil)
	cdc.RegisterConcrete(intakeDelegate{}, "cosmos-sdk/MsgDelegate", nil)
	cdc.RegisterConcrete(intakeUndelegate{}, "cosmos-sdk/MsgUndelegate", nil)
	cdc.RegisterConcrete(intakeRedelegate{}, "cosmos-sdk/MsgBeginRedelegate", nil)
	cdc.RegisterConcrete(intakeTx{}, "cosmos-sdk/StdTx", nil)
	a, b, c := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20), bytes.Repeat([]byte{3}, 20)
	delegator, operator, third := sdk.AccAddress(a).String(), sdk.AccAddress(b).String(), sdk.AccAddress(c).String()
	validator, destination := sdk.ValAddress(b).String(), sdk.ValAddress(c).String()
	pool := authtypes.NewModuleAddress("distribution").String()
	type attribute struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	type logEvent struct {
		Type       string      `json:"type"`
		Attributes []attribute `json:"attributes"`
	}
	event := func(kind string, pairs ...string) logEvent {
		e := logEvent{Type: kind}
		for i := 0; i < len(pairs); i += 2 {
			e.Attributes = append(e.Attributes, attribute{pairs[i], pairs[i+1]})
		}
		return e
	}
	// The chain wrote the events of one message merged by type: every bank
	// payer in one message event before the handler's own, every payment in
	// one transfer event.
	run := func(msg intakeMsg, verdict, action string, staking logEvent, payments ...string) ([]string, error) {
		raw, err := cdc.MarshalBinaryLengthPrefixed(intakeTx{Msgs: []intakeMsg{msg}, Fee: intakeFee{Amount: []intakeCoin{{"uatom", "7"}}, Gas: 200000}})
		require.NoError(t, err)
		input := &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: "cosmoshub-3", Height: 9, Time: time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)}, Data: tm.Data{Txs: tm.Txs{raw}}}}
		var senders, transfers []string
		for i := 0; i < len(payments); i += 2 {
			senders = append(senders, "sender", pool)
			transfers = append(transfers, "recipient", payments[i], "amount", payments[i+1])
		}
		events := []logEvent{event("message", append(senders, "module", "staking", "sender", delegator, "action", action)...)}
		if len(transfers) > 0 {
			events = append(events, event("transfer", transfers...))
		}
		log, err := json.Marshal([]map[string]any{{"msg_index": 0, "success": true, "log": "", "events": append(events, staking)}})
		require.NoError(t, err)
		body, err := json.Marshal(map[string]any{"height": "9", "results": map[string]any{"deliver_tx": []map[string]any{{"code": 0, "log": string(log)}}}})
		require.NoError(t, err)
		var results rpc.CustomBlockResults
		require.NoError(t, tmjson.Unmarshal(body, &results))
		items, err := DecodeLegacyBlock(input, &results)
		require.NoError(t, err)
		if verdict != "" {
			attached, err := ResolveLegacyRemovals(items, map[string]string{"cosmoshub-3/9/0/0": verdict, "cosmoshub-3/8/0/0": "not removed, two archives"})
			if err != nil {
				return nil, err
			}
			require.Equal(t, 1, attached)
		}
		block, err := LegacyLedgerBlock(&capture.Height{Block: input, Results: &results, Hash: strings.Repeat("AB", 32), Proposer: "AA11"}, items)
		var out []string
		for _, row := range block.Rows {
			out = append(out, fmt.Sprintf("%d %s %s to %q validator %q %s %s", row.SubIndex, row.Category, row.Amount, row.To, row.Validator, row.Trigger, row.Detail))
		}
		return out, err
	}
	unbond := event("unbond", "validator", validator, "amount", "5", "completion_time", "2020-01-23T00:00:00Z")
	leave := intakeUndelegate{a, b, intakeCoin{"uatom", "5"}}
	own := intakeUndelegate{b, b, intakeCoin{"uatom", "5"}}
	removed := "removed with commission 8uatom, two archives"
	commission := ` commission 8 to "` + operator + `" validator "` + validator + `" commission {"cause":"validator_removed"}`
	principal := func(sub int, who string) string {
		return fmt.Sprintf(`%d undelegate 5 to %q validator %q  {"completion_time":"2020-01-23T00:00:00Z"}`, sub, who, validator)
	}

	// A delegator takes the last stake: the reward is the delegator's, the
	// second payment is the operator's commission.
	rows, err := run(leave, removed, "begin_unbonding", unbond, delegator, "3uatom", operator, "8uatom")
	require.NoError(t, err)
	require.Equal(t, []string{`0 reward 3 to "` + delegator + `" validator "` + validator + `" undelegate `, principal(1, delegator), "2" + commission}, rows)
	// The same on one archive only is stored the same way.
	_, err = run(leave, "removed with commission 8uatom, one archive", "begin_unbonding", unbond, delegator, "3uatom", operator, "8uatom")
	require.NoError(t, err)

	// The operator takes its own last stake: both payments go to one address.
	rows, err = run(own, removed, "begin_unbonding", unbond, operator, "3uatom", operator, "8uatom")
	require.NoError(t, err)
	require.Equal(t, []string{`0 reward 3 to "` + operator + `" validator "` + validator + `" undelegate `, principal(1, operator), "2" + commission}, rows)
	// With no reward due, the only payment is the commission.
	rows, err = run(own, removed, "begin_unbonding", unbond, operator, "8uatom")
	require.NoError(t, err)
	require.Equal(t, []string{principal(0, operator), "1" + commission}, rows)
	// A removed validator that had no commission pays only the reward.
	rows, err = run(own, "removed with no commission, two archives", "begin_unbonding", unbond, operator, "3uatom")
	require.NoError(t, err)
	require.Equal(t, []string{`0 reward 3 to "` + operator + `" validator "` + validator + `" undelegate `, principal(1, operator)}, rows)
	rows, err = run(own, "not removed, two archives", "begin_unbonding", unbond, operator, "3uatom")
	require.NoError(t, err)
	require.Len(t, rows, 2)

	// A redelegation pays the reward of the validator it leaves, the
	// commission, then the reward of the validator it joins.
	move := intakeRedelegate{a, b, c, intakeCoin{"uatom", "5"}}
	moved := event("redelegate", "source_validator", validator, "destination_validator", destination, "amount", "5", "completion_time", "2020-01-23T00:00:00Z")
	rows, err = run(move, removed, "begin_redelegate", moved, delegator, "3uatom", operator, "8uatom", delegator, "2uatom")
	require.NoError(t, err)
	require.Len(t, rows, 4)
	require.Equal(t, []string{`0 reward 3 to "` + delegator + `" validator "` + validator + `" redelegate `,
		`1 reward 2 to "` + delegator + `" validator "` + destination + `" redelegate `, "3" + commission}, []string{rows[0], rows[1], rows[3]})
	require.Contains(t, rows[2], "2 redelegate 5 to ")

	// Held: no verdict, a verdict that says the validator stayed, a second
	// payee that is not the operator, another amount, a commission that is
	// not the last payment of an undelegation, a payment too many, two
	// payments of a redelegation that both fit.
	unexplained, mismatch := "the distribution account paid more often than the rewards of the message", "the removal verdict does not match what the distribution account paid"
	for _, refused := range []struct {
		name, want string
		rows       []string
		err        error
	}{
		{name: "no verdict", want: unexplained},
		{name: "no verdict, one address", want: unexplained},
		{name: "no verdict, redelegation", want: unexplained},
		{name: "not removed", want: unexplained},
		{name: "no commission", want: unexplained},
		{name: "another payee", want: mismatch},
		{name: "another amount", want: mismatch},
		{name: "commission not last", want: mismatch},
		{name: "a payment too many", want: mismatch},
		{name: "commission missing", want: mismatch},
		{name: "two fit", want: "two payments of a redelegation fit the commission"},
	} {
		switch refused.name {
		case "no verdict":
			refused.rows, refused.err = run(leave, "", "begin_unbonding", unbond, delegator, "3uatom", operator, "8uatom")
		case "no verdict, one address":
			refused.rows, refused.err = run(own, "", "begin_unbonding", unbond, operator, "3uatom", operator, "8uatom")
		case "no verdict, redelegation":
			refused.rows, refused.err = run(move, "", "begin_redelegate", moved, delegator, "3uatom", operator, "8uatom", delegator, "2uatom")
		case "not removed":
			refused.rows, refused.err = run(leave, "not removed, two archives", "begin_unbonding", unbond, delegator, "3uatom", operator, "8uatom")
		case "no commission":
			refused.rows, refused.err = run(own, "removed with no commission, two archives", "begin_unbonding", unbond, operator, "3uatom", operator, "8uatom")
		case "another payee":
			refused.rows, refused.err = run(leave, removed, "begin_unbonding", unbond, delegator, "3uatom", third, "8uatom")
		case "another amount":
			refused.rows, refused.err = run(leave, removed, "begin_unbonding", unbond, delegator, "3uatom", operator, "9uatom")
		case "commission not last":
			refused.rows, refused.err = run(own, removed, "begin_unbonding", unbond, operator, "8uatom", operator, "3uatom")
		case "a payment too many":
			refused.rows, refused.err = run(leave, removed, "begin_unbonding", unbond, delegator, "3uatom", delegator, "1uatom", operator, "8uatom")
		case "commission missing":
			refused.rows, refused.err = run(leave, removed, "begin_unbonding", unbond, delegator, "3uatom")
		case "two fit":
			refused.rows, refused.err = run(intakeRedelegate{b, b, c, intakeCoin{"uatom", "5"}}, removed, "begin_redelegate", moved, operator, "8uatom", operator, "8uatom")
		}
		require.ErrorContains(t, refused.err, "held: "+refused.want, refused.name)
		require.Empty(t, refused.rows, refused.name)
	}
	// A message that pays two addresses and leaves no validator is held as
	// before, and a verdict cannot be attached to it.
	stake := intakeDelegate{a, b, intakeCoin{"uatom", "5"}}
	staked := event("delegate", "validator", validator, "amount", "5")
	_, err = run(stake, "", "delegate", staked, delegator, "3uatom", operator, "8uatom")
	require.ErrorContains(t, err, "the distribution account paid two addresses in one message")
	_, err = run(stake, removed, "delegate", staked, delegator, "3uatom", operator, "8uatom")
	require.ErrorContains(t, err, "removal verdict for a message that leaves no validator")
	for _, text := range []string{"removed", "removed with commission 8uatom", "removed with commission 0uatom, two archives", "removed with commission 8stake, two archives",
		"removed with commission 8uatom,1uatom, two archives", "not removed, three archives", ""} {
		_, err := ResolveLegacyRemovals([]LegacyInclusion{{Identity: "cosmoshub-3/9/0", Tx: &legacy.Tx{Messages: make([]legacy.Message, 1)}}}, map[string]string{"cosmoshub-3/9/0/0": text})
		require.ErrorContains(t, err, "legacy removal verdict not recognized", text)
	}
}

func TestLegacyAllServingEntryPointsBlocked(t *testing.T) {
	for _, chain := range []string{"cosmoshub-1", "cosmoshub-2", "cosmoshub-3"} {
		cfg := config.IndexConfig{}
		cfg.Probe.ChainID = chain
		// Nil downstream inputs prove rejection happens before parsers or DB work.
		events, err := ProcessRPCBlockResults(cfg, models.Block{}, nil, nil, nil)
		require.ErrorContains(t, err, "legacy serving intake blocked")
		require.Nil(t, events)
		rows, _, err := ProcessRPCTXs(&cfg, nil, nil, nil, nil, nil, nil)
		require.ErrorContains(t, err, "legacy serving intake blocked")
		require.Nil(t, rows)
		events, err = ProcessRPCBlockResults(config.IndexConfig{}, models.Block{Chain: models.Chain{ChainID: chain}}, nil, nil, nil)
		require.ErrorContains(t, err, "legacy serving intake blocked")
		require.Nil(t, events)
	}
	events, err := ProcessRPCBlockResults(config.IndexConfig{}, models.Block{}, &rpc.CustomBlockResults{Legacy: &rpc.LegacyBlockEvidence{}}, nil, nil)
	require.ErrorContains(t, err, "legacy serving intake blocked")
	require.Nil(t, events)
}

func TestLegacyAdapterEvidenceFailsClosed(t *testing.T) {
	input := &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: "cosmoshub-3", Height: 9}, Data: tm.Data{Txs: tm.Txs{intakeRaw(t, "cosmoshub-3")}}}}
	fresh := func() rpc.CustomBlockResults {
		var results rpc.CustomBlockResults
		require.NoError(t, tmjson.Unmarshal([]byte(`{"height":"9","results":{"deliver_tx":[{"code":0}]}}`), &results))
		return results
	}
	for _, mutate := range []func(*rpc.CustomBlockResults){func(r *rpc.CustomBlockResults) { r.Height = 10 }, func(r *rpc.CustomBlockResults) { r.TxsResults = nil }, func(r *rpc.CustomBlockResults) { r.TxsResults[0] = nil }, func(r *rpc.CustomBlockResults) { r.Legacy = nil }, func(r *rpc.CustomBlockResults) { r.Legacy.RawJSON = nil }, func(r *rpc.CustomBlockResults) { r.Legacy.Dialect = "tendermint-0.30-0.31" }, func(r *rpc.CustomBlockResults) { r.Legacy.CodeEvidence[0] = "" }, func(r *rpc.CustomBlockResults) { r.Legacy.CodeEvidence[0] = "omitted-zero-omitempty" }} {
		results := fresh()
		mutate(&results)
		items, err := DecodeLegacyBlock(input, &results)
		require.Error(t, err)
		require.Empty(t, items)
	}
	require.Error(t, LegacyIntakeAdmission())
}

func TestLegacyFeeNotChargedWhenTheAnteHandlerRefusedTheFee(t *testing.T) {
	refused := `{\"codespace\":\"sdk\",\"code\":5,\"message\":\"insufficient funds to pay for fees; 1uatom < 2uatom\"}`
	other := `{\"codespace\":\"sdk\",\"code\":5,\"message\":\"insufficient account funds; 1uatom < 2uatom\"}`
	for chain, json := range map[string]string{
		"cosmoshub-1": `{"height":"9","results":{"DeliverTx":[{"code":5,"codespace":"sdk","log":"` + refused + `"},{"code":5,"codespace":"sdk","log":"` + other + `"}]}}`,
		"cosmoshub-2": `{"height":"9","results":{"DeliverTx":[{"code":5,"codespace":"sdk","log":"` + refused + `"},{"code":5,"codespace":"sdk","log":"` + other + `"}]}}`,
		"cosmoshub-3": `{"height":"9","results":{"deliver_tx":[{"code":5,"codespace":"sdk","log":"` + refused + `"},{"code":5,"codespace":"sdk","log":"` + other + `"}]}}`,
	} {
		t.Run(chain, func(t *testing.T) {
			raw := intakeRaw(t, chain)
			input := &coretypes.ResultBlock{Block: &tm.Block{Header: tm.Header{ChainID: chain, Height: 9}, Data: tm.Data{Txs: tm.Txs{raw, raw}}}}
			var results rpc.CustomBlockResults
			require.NoError(t, tmjson.Unmarshal([]byte(json), &results))
			items, err := DecodeLegacyBlock(input, &results)
			require.NoError(t, err)
			require.Len(t, items, 2)
			require.Empty(t, items[0].Messages)
			require.Equal(t, "not charged", items[0].Fee.Charge)
			require.Equal(t, "unknown", items[1].Fee.Charge)
		})
	}
}
