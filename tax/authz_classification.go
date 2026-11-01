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
		if !supported {
			state.issues = append(state.issues, fmt.Sprintf("authz %s: unsupported inner %s", innerPath, a.TypeUrl))
			continue
		}
		sub := eventsForAuthzIndex(log, i)
		out = append(out, classifyWithState(inner, sub, state, depth+1, innerPath)...)
	}
	return out
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
	for _, custom := range []map[string]sdk.Msg{GaiaLiquidMsgTypes(), LegacyLSMMsgTypes(), IBCChannelV2MsgTypes(), TokenFactoryMsgTypes()} {
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

// SDK authz appends its index after the inner handler's events. A nested Exec
// therefore has leaf-first indices. Match and remove only the last index, never
// any matching attribute, or cousins with the same index can leak into a leaf.
func authzSubLog(log *txlog.LogMessage, idx int) *txlog.LogMessage {
	sub := &txlog.LogMessage{}
	if log == nil {
		return sub
	}
	for _, ev := range log.Events {
		last := -1
		for i, a := range ev.Attributes {
			if a.Key == attributeAuthzMsgIndex {
				last = i
			}
		}
		if last < 0 || ev.Attributes[last].Value != strconv.Itoa(idx) {
			continue
		}
		copyEvent := ev
		copyEvent.Attributes = append([]txlog.Attribute(nil), ev.Attributes[:last]...)
		copyEvent.Attributes = append(copyEvent.Attributes, ev.Attributes[last+1:]...)
		sub.Events = append(sub.Events, copyEvent)
	}
	return sub
}
