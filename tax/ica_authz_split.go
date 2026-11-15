package tax

import (
	"fmt"
	"strconv"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
)

// Legacy Execs have no root event boundary, but their leaf handlers close in
// packet order. Flatten one level for that proof, then preserve the proven
// partition with local child indices when passing it back to the authz parser.
// The stored source events are never rewritten or labelled as chain indices.
func icaLegacyExecSubLogs(inners []sdk.Msg, log *txlog.LogMessage) ([]*txlog.LogMessage, error) {
	if authzIndexed(log) {
		return nil, fmt.Errorf("indexed log is not a legacy proof")
	}
	flat := []sdk.Msg{}
	ranges := make([][2]int, len(inners))
	hasExec := false
	for i, m := range inners {
		start := len(flat)
		if exec, ok := m.(*authz.MsgExec); ok {
			hasExec = true
			for _, a := range exec.Msgs {
				child, err := unpackAuthzInner(a)
				if err != nil {
					return nil, err
				}
				flat = append(flat, child)
			}
		} else {
			flat = append(flat, m)
		}
		ranges[i] = [2]int{start, len(flat)}
		if len(flat) > maxAuthzMessages {
			return nil, fmt.Errorf("flattened ICA budget exceeded")
		}
	}
	if !hasExec {
		return nil, fmt.Errorf("no legacy Exec")
	}
	leaves, err := legacyAuthzSubLogs(flat, log)
	if err != nil || !icaLegacyTerminals(flat, leaves) {
		leaves, err = icaLegacyStakingSubLogs(flat, log)
	}
	if err != nil || !icaLegacyTerminals(flat, leaves) {
		return nil, fmt.Errorf("legacy Exec leaf boundaries unproven")
	}
	subs := make([]*txlog.LogMessage, len(inners))
	for i, m := range inners {
		r := ranges[i]
		if _, ok := m.(*authz.MsgExec); !ok {
			subs[i] = leaves[r[0]]
			continue
		}
		sub := &txlog.LogMessage{MessageIndex: i}
		for j := r[0]; j < r[1]; j++ {
			for _, e := range leaves[j].Events {
				attrs := append([]txlog.Attribute(nil), e.Attributes...)
				attrs = append(attrs, txlog.Attribute{Key: attributeAuthzMsgIndex, Value: strconv.Itoa(j - r[0])})
				sub.Events = append(sub.Events, txlog.LogMessageEvent{Type: e.Type, Attributes: attrs})
			}
		}
		subs[i] = sub
	}
	return subs, nil
}

func icaAuthzIndexPath(root *authz.MsgExec, indices []string) bool {
	if len(indices) > maxAuthzDepth {
		return false
	}
	current := root
	for i := len(indices) - 1; i >= 0; i-- {
		n, err := strconv.Atoi(indices[i])
		if err != nil || n < 0 || n >= len(current.Msgs) {
			return false
		}
		child, err := unpackAuthzInner(current.Msgs[n])
		if err != nil {
			return false
		}
		nested, isExec := child.(*authz.MsgExec)
		if i == 0 {
			return !isExec
		}
		if !isExec {
			return false
		}
		current = nested
	}
	return false
}

// One indexed MsgExec can be separated from body-only siblings: its tagged
// records belong to it, while every untagged bank record must match an explicit
// sibling send. Two Execs reuse their child indices, so this proof cannot assign
// them uniquely and must refuse rather than combine their rewards.
func icaIndexedExecSubLogs(inners []sdk.Msg, log *txlog.LogMessage) ([]*txlog.LogMessage, error) {
	execIndex := -1
	var exec *authz.MsgExec
	subs := make([]*txlog.LogMessage, len(inners))
	type bankKey struct{ kind, who, other, amount string }
	expected := map[bankKey]int{}
	for i, inner := range inners {
		subs[i] = &txlog.LogMessage{MessageIndex: i}
		switch m := inner.(type) {
		case *authz.MsgExec:
			if execIndex != -1 {
				return nil, fmt.Errorf("multiple Execs share child indices")
			}
			execIndex, exec = i, m
		case *banktypes.MsgSend:
			expected[bankKey{string(CategoryTransfer), m.FromAddress, m.ToAddress, m.Amount.String()}]++
			expected[bankKey{eventCoinSpent, m.FromAddress, "", m.Amount.String()}]++
			expected[bankKey{eventCoinReceived, m.ToAddress, "", m.Amount.String()}]++
		default:
			if !authzNoTaxEffect[sdk.MsgTypeURL(inner)] {
				return nil, fmt.Errorf("non-body-only Exec sibling")
			}
		}
	}
	if execIndex < 0 || !authzIndexed(log) {
		return nil, fmt.Errorf("no single indexed Exec")
	}
	seen := map[bankKey]int{}
	tagged := false
	for _, ev := range log.Events {
		if ev.Type == eventMessage {
			continue
		}
		first := ""
		switch ev.Type {
		case string(CategoryTransfer):
			first = attributeRecipient
		case eventCoinSpent:
			first = attributeSpender
		case eventCoinReceived:
			first = attributeReceiver
		case eventWithdrawRewards, eventWithdrawCommission:
			first = attributeAmount
		case string(CategoryDelegate), eventUnbond:
			first = attributeValidator
		case string(CategoryRedelegate):
			first = "source_validator"
		case string(CategoryRedeemShares):
			first = attributeDelegator
		case eventWithdrawTokenizeShareReward:
			first = attributeWithdrawAddress
		case eventBurn:
			first = "burner"
		default:
			return nil, fmt.Errorf("unassignable Exec event")
		}
		// Split only at the event's opening field, not at repeated authz indices:
		// a nested Exec legitimately appends several leaf-first indices to a record.
		var records [][]txlog.Attribute
		for _, a := range ev.Attributes {
			if a.Key == first {
				records = append(records, nil)
			}
			if len(records) == 0 {
				return nil, fmt.Errorf("malformed Exec event record")
			}
			records[len(records)-1] = append(records[len(records)-1], a)
		}
		for _, attrs := range records {
			indices := []string{}
			keys := map[string]bool{}
			for _, a := range attrs {
				if a.Key == attributeAuthzMsgIndex {
					if a.Value == "" {
						return nil, fmt.Errorf("empty Exec child index")
					}
					indices = append(indices, a.Value)
					continue
				}
				if keys[a.Key] {
					return nil, fmt.Errorf("duplicate Exec record field")
				}
				keys[a.Key] = true
			}
			if len(indices) > 0 {
				if !icaAuthzIndexPath(exec, indices) {
					return nil, fmt.Errorf("exec child index path is not proven")
				}
				subs[execIndex].Events = append(subs[execIndex].Events, txlog.LogMessageEvent{Type: ev.Type, Attributes: attrs})
				tagged = true
				continue
			}
			a := attrMap(txlog.LogMessageEvent{Attributes: attrs})
			k := bankKey{kind: ev.Type, amount: a[attributeAmount]}
			switch ev.Type {
			case string(CategoryTransfer):
				k.who, k.other = a[attributeSender], a[attributeRecipient]
			case eventCoinSpent:
				k.who = a["spender"]
			case eventCoinReceived:
				k.who = a["receiver"]
			default:
				return nil, fmt.Errorf("untagged non-send economic event")
			}
			seen[k]++
			if seen[k] > expected[k] {
				return nil, fmt.Errorf("untagged bank evidence is not a sibling send")
			}
		}
	}
	if !tagged {
		return nil, fmt.Errorf("no tagged execution evidence")
	}
	// Some SDK layouts omit bank debit/credit events. Transfer evidence is still
	// mandatory for every sibling; if either bank event kind is present, all of
	// that kind must reconcile, not just the convenient subset.
	present := map[string]bool{}
	for k := range seen {
		present[k.kind] = true
	}
	for k, n := range expected {
		if k.kind == string(CategoryTransfer) || present[k.kind] {
			if seen[k] != n {
				return nil, fmt.Errorf("missing sibling send evidence")
			}
		}
	}
	return subs, nil
}
