package tax

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	"github.com/cosmos/gogoproto/proto"
)

const (
	maxAuthzDepth    = 16
	maxAuthzMessages = 1024
)

// ClassificationIncompleteError prevents a partially understood MsgExec from
// being persisted as a complete economic classification. The ordinary parser
// error table retains this diagnostic; partial rows are returned for inspection.
// Coverage consumers must treat these errors as unresolved, not as empty income.
type ClassificationIncompleteError struct{ Issues []string }

func (e *ClassificationIncompleteError) Error() string {
	return "tax: classification incomplete: " + strings.Join(e.Issues, "; ")
}

type classificationState struct {
	issues []string
	inners int
}

func classifyAuthz(m *authz.MsgExec, log *txlog.LogMessage, state *classificationState, depth int, path string) []TaxableEvent {
	if depth >= maxAuthzDepth {
		state.issues = append(state.issues, fmt.Sprintf("authz %s: nesting limit %d", path, maxAuthzDepth))
		return nil
	}
	var out []TaxableEvent
	subs, err := authzSubLogs(m, log)
	if err != nil {
		state.issues = append(state.issues, fmt.Sprintf("authz %s: %v", path, err))
	}
	for i, a := range m.Msgs {
		state.inners++
		if state.inners > maxAuthzMessages {
			state.issues = append(state.issues, "authz inner message budget exceeded")
			break
		}
		innerPath := strconv.Itoa(i)
		if path != "" {
			innerPath = path + "/" + innerPath
		}
		inner, err := unpackAuthzInner(a)
		if err != nil {
			state.issues = append(state.issues, fmt.Sprintf("authz %s: %v", innerPath, err))
			continue
		}
		supported := false
		for _, url := range MessageTypeURLs {
			if a.TypeUrl == url {
				supported = true
				break
			}
		}
		sub := subs[i]
		if !supported && authzNoTaxEffect[a.TypeUrl] && !authzMovedCoins(sub) {
			continue
		}
		if !supported {
			state.issues = append(state.issues, fmt.Sprintf("authz %s: unsupported inner %s", innerPath, a.TypeUrl))
			continue
		}
		out = append(out, classifyWithState(inner, sub, state, depth+1, innerPath)...)
	}
	return out
}

// Delegated governance votes, reward-address changes and grant management move
// no coins. They are skipped only when the inner message's own events confirm
// that.
var authzNoTaxEffect = map[string]bool{
	"/cosmos.gov.v1beta1.MsgVote":                        true,
	"/cosmos.gov.v1beta1.MsgVoteWeighted":                true,
	"/cosmos.gov.v1.MsgVote":                             true,
	"/cosmos.gov.v1.MsgVoteWeighted":                     true,
	"/cosmos.distribution.v1beta1.MsgSetWithdrawAddress": true,
	"/cosmos.staking.v1beta1.MsgEditValidator":           true,
	"/cosmos.authz.v1beta1.MsgGrant":                     true,
	"/cosmos.authz.v1beta1.MsgRevoke":                    true,
	"/cosmos.feegrant.v1beta1.MsgGrantAllowance":         true,
	"/cosmos.feegrant.v1beta1.MsgRevokeAllowance":        true,
}

func authzMovedCoins(log *txlog.LogMessage) bool {
	for _, ev := range log.Events {
		switch ev.Type {
		case eventCoinReceived, "coin_spent", string(CategoryTransfer), "burn", "coinbase":
			return true
		}
	}
	return false
}

// Decode each Any independently. InterfaceRegistry.UnpackAny recursively unpacks
// all children of MsgExec, so one unknown child would discard its known siblings.
// Concrete protobuf unmarshalling does not run that all-or-nothing unpack hook.
func unpackAuthzInner(a *codectypes.Any) (sdk.Msg, error) {
	if a == nil {
		return nil, fmt.Errorf("nil inner Any")
	}
	if m, ok := a.GetCachedValue().(sdk.Msg); ok && m != nil {
		return m, nil
	}
	var m sdk.Msg
	for _, custom := range []map[string]sdk.Msg{GaiaLiquidMsgTypes(), LegacyLSMMsgTypes(), IBCChannelV2MsgTypes(), TokenFactoryMsgTypes(), ProviderMsgTypes(), LiquidityMsgTypes()} {
		if prototype, ok := custom[a.TypeUrl]; ok {
			m = reflect.New(reflect.TypeOf(prototype).Elem()).Interface().(sdk.Msg)
			break
		}
	}
	if m == nil {
		t := proto.MessageType(strings.TrimPrefix(a.TypeUrl, "/"))
		if t == nil || t.Kind() != reflect.Pointer {
			return nil, fmt.Errorf("unknown inner %s", a.TypeUrl)
		}
		var ok bool
		m, ok = reflect.New(t.Elem()).Interface().(sdk.Msg)
		if !ok {
			return nil, fmt.Errorf("not an SDK message: %s", a.TypeUrl)
		}
	}
	if err := proto.Unmarshal(a.Value, m); err != nil {
		return nil, fmt.Errorf("decode %s: %w", a.TypeUrl, err)
	}
	return m, nil
}

// authzSubLogs gives each inner message of m its own events. The SDK tags
// every inner event with authz_msg_index; before it did (the Hub on SDK 0.44
// and 0.45 until about 2023) the log carries no index, and the events of
// several inner messages are separated from the raw log's order instead.
func authzSubLogs(m *authz.MsgExec, log *txlog.LogMessage) ([]*txlog.LogMessage, error) {
	subs := make([]*txlog.LogMessage, len(m.Msgs))
	for i := range subs {
		subs[i] = authzSubLog(log, i)
	}
	if log == nil || len(log.Events) == 0 || authzIndexed(log) {
		return subs, nil
	}
	if len(m.Msgs) == 1 {
		subs[0] = log
		return subs, nil
	}
	inners := make([]sdk.Msg, len(m.Msgs))
	needEvents := false
	for i, a := range m.Msgs {
		inner, err := unpackAuthzInner(a)
		if err != nil {
			return subs, nil
		}
		inners[i] = inner
		if !authzBodyOnly(a.TypeUrl) {
			needEvents = true
		}
	}
	if !needEvents {
		return subs, nil
	}
	split, err := legacyAuthzSubLogs(inners, log)
	if err != nil {
		return subs, fmt.Errorf("log has no authz_msg_index and its inner events cannot be separated: %w", err)
	}
	return split, nil
}

func authzIndexed(log *txlog.LogMessage) bool {
	for _, ev := range log.Events {
		for _, a := range ev.Attributes {
			if a.Key == attributeAuthzMsgIndex {
				return true
			}
		}
	}
	return false
}

// authzBodyOnly reports an inner message classified from its body alone, or
// one that moves no coins, so an untagged log loses nothing for it.
func authzBodyOnly(typeURL string) bool {
	switch typeURL {
	case "/cosmos.bank.v1beta1.MsgSend", "/cosmos.bank.v1beta1.MsgMultiSend":
		return true
	}
	return authzNoTaxEffect[typeURL]
}

// SDK authz appends its index after the inner handler's events. A nested Exec
// therefore has leaf-first indices. Match and remove only the last index, never
// any matching attribute, or cousins with the same index can leak into a leaf.
// A raw log merges every record of one event type into a single event, so an
// event can hold several records, each closed by its own run of indices.
func authzSubLog(log *txlog.LogMessage, idx int) *txlog.LogMessage {
	sub := &txlog.LogMessage{}
	if log == nil {
		return sub
	}
	want := strconv.Itoa(idx)
	for _, ev := range log.Events {
		for _, r := range authzRecords(ev.Attributes) {
			if r.index != want {
				continue
			}
			copyEvent := ev
			copyEvent.Attributes = r.attrs
			sub.Events = append(sub.Events, copyEvent)
		}
	}
	return sub
}

type authzRecord struct {
	index string
	attrs []txlog.Attribute
}

// authzRecords splits an event's attributes at each run of authz_msg_index
// attributes. The last index of a run is this level's and is removed; the rest
// stay for the nested level. An event with one run keeps every other
// attribute, as an unmerged event always has. With several runs, the merged
// message event opens with the MsgExec action, which belongs to no inner
// message.
func authzRecords(attrs []txlog.Attribute) []authzRecord {
	var out []authzRecord
	var ends []int
	start := 0
	for i := 0; i < len(attrs); i++ {
		if attrs[i].Key != attributeAuthzMsgIndex {
			continue
		}
		j := i
		for j+1 < len(attrs) && attrs[j+1].Key == attributeAuthzMsgIndex {
			j++
		}
		out = append(out, authzRecord{index: attrs[j].Value, attrs: append([]txlog.Attribute(nil), attrs[start:j]...)})
		ends = append(ends, j)
		start, i = j+1, j
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		out[0].attrs = append(out[0].attrs, attrs[ends[0]+1:]...)
		return out
	}
	for k := range out {
		kept := out[k].attrs[:0]
		for _, a := range out[k].attrs {
			if a.Key == attributeAction && a.Value == typeURLAuthzExec {
				continue
			}
			kept = append(kept, a)
		}
		out[k].attrs = kept
	}
	return out
}
