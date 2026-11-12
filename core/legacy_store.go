package core

import (
	"fmt"

	"github.com/DefiantLabs/cosmos-indexer/config"
	indexertx "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	"github.com/DefiantLabs/cosmos-indexer/legacy/evidence"
	"github.com/DefiantLabs/cosmos-indexer/legacy/store"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
)

// LegacyStoreBlock turns one decoded cosmoshub-3 height into what the offline
// store keeps: every inclusion with its signers, its fee when the chain is
// known to have charged it, and its messages with the tax rows the serving
// classifier gives for them. It is offline only and admits nothing into the
// serving path.
//
// A height is all or nothing. If any transaction in it has a decoder issue, a
// message without a projection, evidence that does not line up, or a fee whose
// charge is unknown, the whole height is refused, so a stored height is never
// partly classified. Messages of a failed transaction changed no state and get
// no rows. Hub 1 and Hub 2 are refused: their tags need their own classifier.
func LegacyStoreBlock(block *coretypes.ResultBlock, items []LegacyInclusion) (store.Block, error) {
	if block == nil || block.Block == nil {
		return store.Block{}, fmt.Errorf("legacy store block missing")
	}
	if block.Block.ChainID != "cosmoshub-3" {
		return store.Block{}, fmt.Errorf("legacy tax rows are only built for cosmoshub-3")
	}
	if len(items) != len(block.Block.Txs) {
		return store.Block{}, fmt.Errorf("legacy inclusions do not cover the block")
	}
	out := store.Block{Chain: block.Block.ChainID, Height: block.Block.Height, Time: block.Block.Time}
	parser := &tax.Parser{ID: "legacy-hub-three"}
	for i, item := range items {
		refuse := func(reason string) (store.Block, error) {
			return store.Block{}, fmt.Errorf("legacy transaction %d held: %s", i, reason)
		}
		if item.Tx == nil || item.Result == nil {
			return refuse("incomplete inclusion")
		}
		if len(item.Tx.Issues) != 0 || !item.Tx.SignersComplete {
			return refuse("decoder issue")
		}
		if item.Fee.Charge != "charged" && item.Fee.Charge != "not charged" {
			return refuse("fee charge unknown")
		}
		executed := item.Result.Code == 0
		if executed && len(item.Messages) != len(item.Tx.Messages) {
			return refuse("message evidence does not line up")
		}
		detail := &store.Detail{}
		seen := map[string]bool{}
		for _, signer := range item.Tx.Signers {
			if address := signer.String(); !seen[address] {
				seen[address] = true
				detail.Signers = append(detail.Signers, address)
			}
		}
		if item.Fee.Charge == "charged" {
			detail.FeePayer = item.Fee.Payer.String()
			for _, coin := range item.Fee.Declared {
				if coin.IsPositive() {
					detail.Fee = append(detail.Fee, store.Coin{Denom: coin.Denom, Amount: coin.Amount.String()})
				}
			}
		}
		for j, message := range item.Tx.Messages {
			if message.Derived == nil || message.DerivedTypeURL == "" {
				return refuse("message without a projection")
			}
			stored := store.Message{TypeURL: message.DerivedTypeURL}
			if executed {
				events, err := evidence.TransferSenders(message.AminoName, item.Messages[j].Events)
				if err != nil {
					return refuse(err.Error())
				}
				log := &indexertx.LogMessage{MessageIndex: j}
				for _, e := range events {
					event := indexertx.LogMessageEvent{Type: e.Type}
					for _, a := range e.Attributes {
						event.Attributes = append(event.Attributes, indexertx.Attribute{Key: a.Key, Value: a.Value})
					}
					log.Events = append(log.Events, event)
				}
				parsed, err := parser.ParseMessage(message.Derived, log, config.IndexConfig{})
				if err != nil || parsed == nil {
					return refuse("classifier did not finish")
				}
				rows, ok := (*parsed).([]tax.TaxableEvent)
				if !ok {
					return refuse("classifier returned an unknown result")
				}
				for _, row := range rows {
					stored.Rows = append(stored.Rows, store.Row{Category: row.Category, Amount: row.Amount, Denom: row.Denom, Asset: row.Asset,
						From: row.FromAddr, To: row.ToAddr, Validator: row.ValidatorAddress, Trigger: row.RewardTrigger, Detail: row.Detail})
				}
			}
			detail.Messages = append(detail.Messages, stored)
		}
		out.Inclusions = append(out.Inclusions, store.Inclusion{Index: i, Hash: item.Hash, Code: item.Result.Code,
			Codespace: item.Result.Codespace, CodeEvidence: item.CodeEvidence, Detail: detail})
	}
	return out, nil
}

// ResolveLegacyFees applies verdicts from the account sequence check to
// failed transactions whose fee charge the block results could not prove.
// Verdicts are keyed by inclusion identity; one for another height is
// ignored. A fee the results already prove is never changed: a verdict that
// disagrees with it is an error.
func ResolveLegacyFees(items []LegacyInclusion, verdicts map[string]string) error {
	for i := range items {
		verdict, ok := verdicts[items[i].Identity]
		if !ok {
			continue
		}
		if verdict != "charged" && verdict != "not charged" {
			return fmt.Errorf("legacy fee verdict not recognized")
		}
		switch items[i].Fee.Charge {
		case "unknown":
			if len(items[i].Fee.Payer) == 0 {
				return fmt.Errorf("legacy fee verdict for a transaction without a payer")
			}
			items[i].Fee.Charge = verdict
		case verdict:
		default:
			return fmt.Errorf("legacy fee verdict disagrees with the block results")
		}
	}
	return nil
}
