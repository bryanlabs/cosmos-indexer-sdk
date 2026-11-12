package rpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	abci "github.com/cometbft/cometbft/abci/types"
	tmjson "github.com/cometbft/cometbft/libs/json"
	cmtcrypto "github.com/cometbft/cometbft/proto/tendermint/crypto"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
)

// LegacyBlockEvidence is source evidence, not a modern ABCI event stream.
// Tendermint 0.30/0.31 used PascalCase response names and omitted zero code
// (ResponseDeliverTx's json code tag was omitempty). 0.32's snake_case
// envelope always serialized code. Only the former permits an omitted zero.
type LegacyBlockEvidence struct {
	RawJSON            []byte
	Dialect            string
	CodeEvidence       []string
	TxTags             [][]LegacyTag
	BeginTags, EndTags []LegacyTag
}
type LegacyTag struct{ Key, Value []byte }
type legacyAttribute struct {
	Key   []byte `json:"key"`
	Value []byte `json:"value"`
}
type legacyEvent struct {
	Type       string            `json:"type"`
	Attributes []legacyAttribute `json:"attributes"`
}

// checkedObject rejects unknown keys and duplicate JSON keys at every depth.
// Error text deliberately excludes values from private captures.
func checkedObject(raw []byte, allowed ...string) (map[string]json.RawMessage, error) {
	if err := uniqueJSON(raw); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, fmt.Errorf("legacy result expected object")
	}
	for key := range fields {
		found := false
		for _, name := range allowed {
			if key == name {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unsupported legacy result field")
		}
	}
	return fields, nil
}
func uniqueJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		token, err := d.Token()
		if err != nil {
			return fmt.Errorf("invalid legacy JSON")
		}
		if token == json.Delim('{') {
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return fmt.Errorf("invalid legacy JSON")
				}
				s, ok := key.(string)
				if !ok || seen[s] {
					return fmt.Errorf("duplicate legacy JSON field")
				}
				seen[s] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		}
		if token == json.Delim('[') {
			for d.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing legacy JSON")
	}
	return nil
}
func legacyEvents(raw json.RawMessage) ([]abci.Event, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("invalid legacy events")
	}
	var events []abci.Event
	for _, item := range items {
		f, err := checkedObject(item, "type", "attributes")
		if err != nil {
			return nil, err
		}
		var typ string
		if json.Unmarshal(f["type"], &typ) != nil || typ == "" {
			return nil, fmt.Errorf("invalid legacy event type")
		}
		attrs, err := legacyTags(f["attributes"])
		if err != nil {
			return nil, err
		}
		event := abci.Event{Type: typ}
		for _, attr := range attrs {
			event.Attributes = append(event.Attributes, abci.EventAttribute{Key: string(attr.Key), Value: string(attr.Value)})
		}
		events = append(events, event)
	}
	return events, nil
}
func legacyTags(raw json.RawMessage) ([]LegacyTag, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, fmt.Errorf("invalid legacy attributes")
	}
	var tags []LegacyTag
	for _, item := range items {
		f, err := checkedObject(item, "key", "value")
		if err != nil {
			return nil, err
		}
		var attr legacyAttribute
		if len(f["key"]) == 0 || json.Unmarshal(item, &attr) != nil {
			return nil, fmt.Errorf("invalid legacy byte attribute")
		}
		tags = append(tags, LegacyTag{Key: attr.Key, Value: attr.Value})
	}
	return tags, nil
}

// Legacy ABCI validator keys were {type,data}, not CometBFT's protobuf
// oneof. Decoding them directly into the modern type silently loses the key.
func legacyValidatorUpdates(raw json.RawMessage) ([]abci.ValidatorUpdate, error) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, fmt.Errorf("invalid legacy validator updates")
	}
	var updates []abci.ValidatorUpdate
	for _, item := range items {
		f, err := checkedObject(item, "pub_key", "power")
		if err != nil {
			return nil, err
		}
		key, err := checkedObject(f["pub_key"], "type", "data")
		if err != nil {
			return nil, err
		}
		var typ string
		var data []byte
		var power int64
		if json.Unmarshal(key["type"], &typ) != nil || json.Unmarshal(key["data"], &data) != nil || tmjson.Unmarshal(f["power"], &power) != nil || power < 0 {
			return nil, fmt.Errorf("invalid legacy validator update fields")
		}
		var normalized cmtcrypto.PublicKey
		switch {
		case typ == "ed25519" && len(data) == 32:
			normalized.Sum = &cmtcrypto.PublicKey_Ed25519{Ed25519: data}
		case typ == "secp256k1" && len(data) == 33:
			normalized.Sum = &cmtcrypto.PublicKey_Secp256K1{Secp256K1: data}
		default:
			return nil, fmt.Errorf("unsupported legacy validator key encoding")
		}
		updates = append(updates, abci.ValidatorUpdate{PubKey: normalized, Power: power})
	}
	return updates, nil
}

func (r *CustomBlockResults) unmarshalLegacy(data []byte, _ map[string]json.RawMessage, nested json.RawMessage) error {
	fields, err := checkedObject(data, "height", "results")
	if err != nil {
		return err
	}
	shape, err := checkedObject(nested, "deliver_tx", "begin_block", "end_block", "DeliverTx", "BeginBlock", "EndBlock")
	if err != nil {
		return err
	}
	txKey, beginKey, endKey := "deliver_tx", "begin_block", "end_block"
	dialect := "tendermint-0.32"
	if _, old := shape["DeliverTx"]; old {
		txKey, beginKey, endKey = "DeliverTx", "BeginBlock", "EndBlock"
		dialect = "tendermint-0.30-0.31"
	}
	for key := range shape {
		if key != txKey && key != beginKey && key != endKey {
			return fmt.Errorf("mixed legacy result dialects")
		}
	}
	if _, ok := shape[txKey]; !ok {
		return fmt.Errorf("legacy results missing transaction result array")
	}
	result := CustomBlockResults{Legacy: &LegacyBlockEvidence{RawJSON: bytes.Clone(data), Dialect: dialect}}
	if tmjson.Unmarshal(fields["height"], &result.Height) != nil || result.Height <= 0 {
		return fmt.Errorf("invalid legacy result height")
	}
	var txs []json.RawMessage
	if json.Unmarshal(shape[txKey], &txs) != nil {
		return fmt.Errorf("invalid legacy transaction results")
	}
	for _, raw := range txs {
		allowed := []string{"code", "data", "log", "info", "gasWanted", "gasUsed", "codespace"}
		if dialect == "tendermint-0.32" {
			allowed = append(allowed, "events")
		} else {
			allowed = append(allowed, "tags")
		}
		f, err := checkedObject(raw, allowed...)
		if err != nil {
			return err
		}
		codeEvidence := "explicit"
		if code, ok := f["code"]; !ok {
			if dialect != "tendermint-0.30-0.31" || len(f) == 0 {
				return fmt.Errorf("missing legacy execution code")
			}
			codeEvidence = "omitted-zero-omitempty"
		} else if bytes.Equal(bytes.TrimSpace(code), []byte("null")) {
			return fmt.Errorf("null legacy execution code")
		}
		var wire struct {
			Code      uint32 `json:"code"`
			Data      []byte `json:"data"`
			Log       string `json:"log"`
			Info      string `json:"info"`
			GasWanted int64  `json:"gasWanted"`
			GasUsed   int64  `json:"gasUsed"`
			Codespace string `json:"codespace"`
		}
		if tmjson.Unmarshal(raw, &wire) != nil {
			return fmt.Errorf("invalid legacy transaction result fields")
		}
		events, err := legacyEvents(f["events"])
		if err != nil {
			return err
		}
		tags, err := legacyTags(f["tags"])
		if err != nil {
			return err
		}
		result.TxsResults = append(result.TxsResults, &abci.ResponseDeliverTx{Code: wire.Code, Data: wire.Data, Log: wire.Log, Info: wire.Info, GasWanted: wire.GasWanted, GasUsed: wire.GasUsed, Codespace: wire.Codespace, Events: events})
		result.Legacy.CodeEvidence = append(result.Legacy.CodeEvidence, codeEvidence)
		result.Legacy.TxTags = append(result.Legacy.TxTags, tags)
	}
	for _, key := range []string{beginKey, endKey} {
		raw, exists := shape[key]
		if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		allowed := []string{"validator_updates", "consensus_param_updates"}
		if key == beginKey {
			allowed = nil
		}
		if dialect == "tendermint-0.32" {
			allowed = append(allowed, "events")
		} else {
			allowed = append(allowed, "tags")
		}
		f, err := checkedObject(raw, allowed...)
		if err != nil {
			return err
		}
		events, err := legacyEvents(f["events"])
		if err != nil {
			return err
		}
		tags, err := legacyTags(f["tags"])
		if err != nil {
			return err
		}
		if key == beginKey {
			result.BeginBlockEvents = events
			result.Legacy.BeginTags = tags
			continue
		}
		result.EndBlockEvents = events
		result.Legacy.EndTags = tags
		if updates, ok := f["validator_updates"]; ok {
			result.ValidatorUpdates, err = legacyValidatorUpdates(updates)
			if err != nil {
				return err
			}
		}
		if params, ok := f["consensus_param_updates"]; ok && !bytes.Equal(bytes.TrimSpace(params), []byte("null")) {
			var check cmtproto.ConsensusParams
			d := json.NewDecoder(bytes.NewReader(params))
			d.DisallowUnknownFields()
			if d.Decode(&check) != nil {
				return fmt.Errorf("unsupported legacy consensus update")
			}
			result.ConsensusParamUpdates = &check
		}
	}
	*r = result
	return nil
}
