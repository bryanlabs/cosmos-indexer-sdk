// Package evidence attributes what a Cosmos Hub 1 to 3 chain recorded for a
// transaction to the messages that produced it.
package evidence

import (
	"encoding/json"
	"fmt"

	"github.com/DefiantLabs/cosmos-indexer/legacy"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	abci "github.com/cometbft/cometbft/abci/types"
)

// Message is what the chain recorded for one executed message.
// Hub 1 and Hub 2 recorded a flat tag list for the whole transaction; Hub 3
// recorded events per message inside the result log. Exactly one of Tags and
// Events is used, by era.
type Message struct {
	Index   int
	Success bool
	Tags    []rpc.LegacyTag
	Events  []abci.Event
}

// The slashing handler of Hub 1 and Hub 2 wrote its own action tag inside the
// unjail message's tags. It is the only handler that did.
const unjailedAction = "validator-unjailed"

// For attributes a transaction's recorded tags or events
// to the messages that produced them, and fails instead of guessing.
//
// Every era wrote an action marker when it began executing a message and
// stopped at the first failing one. So the number of markers is the number of
// messages that began executing, and at least one marker proves the ante
// handler finished, which is when the fee was committed. A result with no
// marker proves nothing either way: an ante rejection charges no fee, while
// an out of gas panic during execution charges it and discards its markers.
func For(chain string, tx *legacy.Tx, result *abci.ResponseDeliverTx, tags []rpc.LegacyTag) ([]Message, error) {
	if tx == nil || result == nil {
		return nil, fmt.Errorf("legacy message evidence needs a transaction and its result")
	}
	var evidence []Message
	var err error
	if chain == "cosmoshub-3" {
		evidence, err = fromLog(chain, tx, result.Log)
	} else {
		evidence, err = fromTags(chain, tx, tags)
	}
	if err != nil {
		return nil, err
	}
	if result.Code == 0 {
		if len(evidence) != len(tx.Messages) {
			return nil, fmt.Errorf("successful legacy transaction lacks evidence for every message")
		}
		return evidence, nil
	}
	if len(evidence) > 0 {
		evidence[len(evidence)-1].Success = false
	}
	return evidence, nil
}

func fromTags(chain string, tx *legacy.Tx, tags []rpc.LegacyTag) ([]Message, error) {
	var evidence []Message
	pos := 0
	for i, msg := range tx.Messages {
		if pos == len(tags) {
			break
		}
		name := legacy.ActionName(chain, msg.AminoName)
		if name == "" {
			return nil, fmt.Errorf("legacy message has no known action name")
		}
		if string(tags[pos].Key) != "action" || string(tags[pos].Value) != name {
			return nil, fmt.Errorf("legacy tags do not follow message order")
		}
		pos++
		start := pos
		for pos < len(tags) {
			if string(tags[pos].Key) == "action" && !(name == "unjail" && string(tags[pos].Value) == unjailedAction) {
				break
			}
			pos++
		}
		evidence = append(evidence, Message{Index: i, Success: true, Tags: tags[start:pos]})
	}
	if pos != len(tags) {
		return nil, fmt.Errorf("legacy tags continue past the last message")
	}
	return evidence, nil
}

func fromLog(chain string, tx *legacy.Tx, log string) ([]Message, error) {
	var entries []struct {
		MsgIndex *int  `json:"msg_index"`
		Success  *bool `json:"success"`
		Events   []struct {
			Type       string `json:"type"`
			Attributes []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"attributes"`
		} `json:"events"`
	}
	// A failure outside message execution writes an error object or plain
	// text, not the per-message array. That is no evidence, not bad evidence.
	if json.Unmarshal([]byte(log), &entries) != nil {
		return nil, nil
	}
	if len(entries) > len(tx.Messages) {
		return nil, fmt.Errorf("legacy log has more entries than messages")
	}
	var evidence []Message
	for i, entry := range entries {
		if entry.MsgIndex == nil || *entry.MsgIndex != i || entry.Success == nil {
			return nil, fmt.Errorf("legacy log entries are not in message order")
		}
		if !*entry.Success && i != len(entries)-1 {
			return nil, fmt.Errorf("legacy log continues after a failed message")
		}
		name := legacy.ActionName(chain, tx.Messages[i].AminoName)
		if name == "" {
			return nil, fmt.Errorf("legacy message has no known action name")
		}
		item := Message{Index: i, Success: *entry.Success}
		marked := false
		for _, e := range entry.Events {
			event := abci.Event{Type: e.Type}
			for _, a := range e.Attributes {
				if e.Type == "message" && a.Key == "action" && a.Value == name {
					marked = true
				}
				event.Attributes = append(event.Attributes, abci.EventAttribute{Key: a.Key, Value: a.Value})
			}
			item.Events = append(item.Events, event)
		}
		if !marked {
			return nil, fmt.Errorf("legacy log entry does not name its message")
		}
		evidence = append(evidence, item)
	}
	return evidence, nil
}
