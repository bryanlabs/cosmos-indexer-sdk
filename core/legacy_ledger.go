package core

import (
	"bytes"
	"encoding/json"
	"fmt"

	sdkmath "cosmossdk.io/math"
	"github.com/DefiantLabs/cosmos-indexer/config"
	indexertx "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	"github.com/DefiantLabs/cosmos-indexer/legacy/evidence"
	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	abci "github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
)

// Hub 3 module accounts. Coins the chain moves between two of them at the
// start or end of a block (minting, fee collection, validators entering or
// leaving the bonded set) belong to no wallet and are not kept.
var (
	legacyGovAccount          = authtypes.NewModuleAddress("gov").String()
	legacyDistributionAccount = authtypes.NewModuleAddress("distribution").String()
	legacyModuleAccounts      = func() map[string]bool {
		out := map[string]bool{}
		for _, name := range []string{"gov", "distribution", "mint", "fee_collector", "bonded_tokens_pool", "not_bonded_tokens_pool"} {
			out[authtypes.NewModuleAddress(name).String()] = true
		}
		return out
	}()
	// Paid per validator on every block. They allocate to validators, not to
	// accounts, and would be over a billion rows.
	legacyPerBlockEvents = map[string]bool{"mint": true, "proposer_reward": true, "commission": true, "rewards": true, "liveness": true}
	// Messages whose rows take their amount from the signed message. Every
	// other Hub 3 row takes it from the events of the result.
	legacyAmountFromMessage = map[string]bool{"cosmos-sdk/MsgSend": true, "cosmos-sdk/MsgMultiSend": true,
		"cosmos-sdk/MsgCreateValidator": true, "cosmos-sdk/MsgFundCommunityPool": true}
)

// LegacyLedgerBlock turns one decoded cosmoshub-3 height into what the
// legacy ledger keeps: the block, every inclusion with its signers and its
// fee verdict, the tax rows the serving classifier gives for each executed
// message (with the address a reward was paid to when that is not the
// delegator, and a row for each payer of a multi send with several payers),
// the coins the chain itself paid to accounts at the end of the block, and
// the block events later steps need. It is offline only and
// admits nothing into the serving path.
//
// A height is all or nothing. If any transaction in it has a decoder issue,
// a message without a projection, evidence that does not line up, a fee
// whose charge is unknown, a row without an amount, reward rows that do not
// add up to what the distribution account paid, or a block level coin
// movement that is not recognized, the whole height is refused, so a stored
// height is never partly classified. Messages of a failed transaction
// changed no state and get no rows. Hub 1 and Hub 2 are refused: their tags
// need their own classifier.
func LegacyLedgerBlock(h *capture.Height, items []LegacyInclusion) (ledger.Block, error) {
	if h == nil || h.Block == nil || h.Block.Block == nil || h.Results == nil {
		return ledger.Block{}, fmt.Errorf("legacy ledger block missing")
	}
	block := h.Block.Block
	if block.ChainID != "cosmoshub-3" {
		return ledger.Block{}, fmt.Errorf("legacy tax rows are only built for cosmoshub-3")
	}
	if len(items) != len(block.Txs) {
		return ledger.Block{}, fmt.Errorf("legacy inclusions do not cover the block")
	}
	out := ledger.Block{Chain: block.ChainID, Height: block.Height, Time: block.Time, Hash: h.Hash, Proposer: h.Proposer}
	parser := &tax.Parser{ID: "legacy-hub-three"}
	for i, item := range items {
		refuse := func(reason string) (ledger.Block, error) {
			return ledger.Block{}, fmt.Errorf("legacy transaction %d held: %s", i, reason)
		}
		if item.Tx == nil || item.Result == nil {
			return refuse("incomplete inclusion")
		}
		if len(item.Tx.Issues) != 0 || !item.Tx.SignersComplete || len(item.Tx.Signers) == 0 {
			return refuse("decoder issue")
		}
		if item.Fee.Charge != "charged" && item.Fee.Charge != "not charged" {
			return refuse("fee charge unknown")
		}
		executed := item.Result.Code == 0
		if executed && len(item.Messages) != len(item.Tx.Messages) {
			return refuse("message evidence does not line up")
		}
		inclusion := ledger.Inclusion{Index: i, Hash: item.Hash, Code: item.Result.Code, Codespace: item.Result.Codespace,
			CodeEvidence: item.CodeEvidence, GasWanted: item.Result.GasWanted, GasUsed: item.Result.GasUsed,
			Fee: item.Fee.Declared.String(), FeePayer: item.Fee.Payer.String(), FeeCharged: item.Fee.Charge == "charged",
			FeeEvidence: item.Fee.Evidence, Memo: ledger.Text(item.Tx.Memo), Raw: bytes.Clone(block.Txs[i])}
		seen := map[string]bool{}
		for _, signer := range item.Tx.Signers {
			if address := signer.String(); !seen[address] {
				seen[address] = true
				inclusion.Signers = append(inclusion.Signers, address)
			}
		}
		for j, message := range item.Tx.Messages {
			if message.Derived == nil || message.DerivedTypeURL == "" {
				return refuse("message without a projection")
			}
			if !executed {
				continue
			}
			if message.AmountWithoutDenom {
				return refuse("staking amount without a denom in a successful transaction")
			}
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
			paid, payee, err := legacyDistributionPayout(events)
			if err != nil {
				return refuse(err.Error())
			}
			income := sdk.NewCoins()
			for sub, row := range rows {
				if row.Amount == "" || row.Denom == "" {
					return refuse("tax row without an amount")
				}
				source, earned := "event", row.Category == string(tax.CategoryReward) || row.Category == string(tax.CategoryCommission)
				if legacyAmountFromMessage[message.AminoName] && !earned {
					source = "message"
				}
				detail := row.Detail
				if earned {
					amount, ok := sdkmath.NewIntFromString(row.Amount)
					if !ok || !amount.IsPositive() {
						return refuse("tax row without an amount")
					}
					income = income.Add(sdk.NewCoin(row.Denom, amount))
					// A reward row names the delegator who earned it. When the
					// chain paid the coins to another address, keep that too.
					if payee != row.ToAddr {
						if detail != "" {
							return refuse("reward row already carries a detail")
						}
						detail = `{"paid_to":"` + payee + `"}`
					}
				}
				out.Rows = append(out.Rows, ledger.Row{Phase: ledger.PhaseTx, TxIndex: i, MsgIndex: j, SubIndex: sub,
					Category: row.Category, Amount: row.Amount, Denom: row.Denom, Asset: row.Asset, From: row.FromAddr, To: row.ToAddr,
					Validator: row.ValidatorAddress, Trigger: row.RewardTrigger, Detail: detail, TxHash: item.Hash, Source: source})
			}
			if income.String() != paid.String() {
				return refuse("reward and commission rows differ from what the distribution account paid")
			}
			// The serving classifier gives a multi send with several payers
			// rows for what each address received and none for what each
			// payer paid. Add one row per payer so every address has its side.
			if multi, ok := message.Derived.(*banktypes.MsgMultiSend); ok && len(multi.Inputs) > 1 {
				sub := len(rows)
				for _, input := range multi.Inputs {
					for _, coin := range input.Coins {
						if !coin.IsPositive() {
							continue
						}
						out.Rows = append(out.Rows, ledger.Row{Phase: ledger.PhaseTx, TxIndex: i, MsgIndex: j, SubIndex: sub,
							Category: string(tax.CategoryTransfer), Amount: coin.Amount.String(), Denom: coin.Denom, From: input.Address,
							TxHash: item.Hash, Source: "message"})
						sub++
					}
				}
			}
		}
		out.Inclusions = append(out.Inclusions, inclusion)
	}
	begin, _, err := legacyBlockPhase(ledger.PhaseBegin, h.Results.BeginBlockEvents)
	if err != nil {
		return ledger.Block{}, err
	}
	for k, raw := range h.Evidence {
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			return ledger.Block{}, fmt.Errorf("legacy block evidence is not JSON")
		}
		begin = append(begin, ledger.Event{Phase: ledger.PhaseBegin, Index: len(h.Results.BeginBlockEvents) + k, Type: "block_evidence",
			Attributes: [][2]string{{"json", compact.String()}}})
	}
	end, paid, err := legacyBlockPhase(ledger.PhaseEnd, h.Results.EndBlockEvents)
	if err != nil {
		return ledger.Block{}, err
	}
	out.Events = append(begin, end...)
	out.Rows = append(out.Rows, paid...)
	return out, nil
}

// legacyDistributionPayout sums what the distribution account paid while one
// message ran and names the address it paid. The events carry the payer on
// each transfer. One message pays one address: the withdraw address of its
// delegator or of its validator. A second address means the message also
// removed a validator and paid out its commission, which the classifier
// cannot tell from a reward, so it is refused.
func legacyDistributionPayout(events []abci.Event) (sdk.Coins, string, error) {
	paid, payee := sdk.NewCoins(), ""
	for _, e := range events {
		if e.Type != "transfer" {
			continue
		}
		recipient, sender := "", ""
		for _, a := range e.Attributes {
			switch a.Key {
			case "recipient":
				recipient, sender = a.Value, ""
			case "sender":
				sender = a.Value
			case "amount":
				if sender != legacyDistributionAccount {
					continue
				}
				coins, err := sdk.ParseCoinsNormalized(a.Value)
				if err != nil || recipient == "" {
					return nil, "", fmt.Errorf("distribution payout unreadable")
				}
				if payee != "" && payee != recipient {
					return nil, "", fmt.Errorf("the distribution account paid two addresses in one message")
				}
				payee = recipient
				paid = paid.Add(coins...)
			}
		}
	}
	return paid, payee, nil
}

// legacyBlockPhase reads the begin or end block events of a Hub 3 height.
// Hub 3 wrote a coin movement as a transfer event (recipient, amount)
// followed by a message event holding the sender. A movement between two
// module accounts is dropped. At the end of a block the governance account
// refunds deposits, the distribution account pays community pool spends and
// the commission of a removed validator, and finished unbondings return
// their coins; each becomes a tax row. Any other
// movement refuses the height. Events other than the per validator
// allocations of every block are kept.
func legacyBlockPhase(phase int, events []abci.Event) ([]ledger.Event, []ledger.Row, error) {
	var kept []ledger.Event
	var rows []ledger.Row
	keep := func(index int) {
		event := ledger.Event{Phase: phase, Index: index, Type: events[index].Type, Attributes: [][2]string{}}
		for _, a := range events[index].Attributes {
			event.Attributes = append(event.Attributes, [2]string{ledger.Text([]byte(a.Key)), ledger.Text([]byte(a.Value))})
		}
		kept = append(kept, event)
	}
	for i := 0; i < len(events); i++ {
		if legacyPerBlockEvents[events[i].Type] {
			continue
		}
		if phase == ledger.PhaseEnd && events[i].Type == "complete_unbonding" {
			var amount, delegator, validator string
			for _, a := range events[i].Attributes {
				switch a.Key {
				case "amount":
					amount = a.Value
				case "delegator":
					delegator = a.Value
				case "validator":
					validator = a.Value
				}
			}
			coins, err := sdk.ParseCoinsNormalized(amount)
			if err != nil || delegator == "" || validator == "" || len(coins) != 1 || !coins[0].IsPositive() {
				return nil, nil, fmt.Errorf("legacy unbonding completion without one amount, its delegator and its validator")
			}
			rows = append(rows, ledger.Row{Phase: phase, SubIndex: len(rows), Category: string(tax.CategoryUnbondingComplete),
				Amount: coins[0].Amount.String(), Denom: coins[0].Denom, To: delegator, Validator: validator, Source: "event"})
			keep(i)
			continue
		}
		if events[i].Type != "transfer" {
			keep(i)
			continue
		}
		var recipient, amount, sender string
		for _, a := range events[i].Attributes {
			switch a.Key {
			case "recipient":
				recipient = a.Value
			case "amount":
				amount = a.Value
			}
		}
		if i+1 < len(events) && events[i+1].Type == "message" && len(events[i+1].Attributes) == 1 && events[i+1].Attributes[0].Key == "sender" {
			sender = events[i+1].Attributes[0].Value
		}
		if recipient == "" || sender == "" {
			return nil, nil, fmt.Errorf("legacy block transfer without a recipient or its sender")
		}
		if legacyModuleAccounts[sender] && legacyModuleAccounts[recipient] {
			i++
			continue
		}
		category, trigger, detail := "", "", ""
		switch {
		case phase == ledger.PhaseEnd && sender == legacyGovAccount:
			category = string(tax.CategoryGovDepositRefund)
		case phase == ledger.PhaseEnd && sender == legacyDistributionAccount:
			// Governance ran before staking at the end of a block. A payout
			// before a proposal passes is that proposal spending from the
			// community pool. Any other is staking removing a validator
			// that had no stake left, which pays out its commission.
			category, trigger, detail = string(tax.CategoryCommission), "commission", `{"cause":"validator_removed"}`
			for _, later := range events[i+1:] {
				if later.Type != "active_proposal" {
					continue
				}
				for _, a := range later.Attributes {
					if a.Key == "proposal_result" && a.Value == "proposal_passed" {
						category, trigger, detail = string(tax.CategoryIncome), tax.TriggerCommunityPoolSpend, ""
					}
				}
			}
		default:
			return nil, nil, fmt.Errorf("legacy block coin movement not recognized")
		}
		coins, err := sdk.ParseCoinsNormalized(amount)
		if err != nil {
			return nil, nil, fmt.Errorf("legacy block transfer amount unreadable")
		}
		for _, coin := range coins {
			if !coin.IsPositive() {
				continue
			}
			rows = append(rows, ledger.Row{Phase: phase, SubIndex: len(rows), Category: category, Amount: coin.Amount.String(), Denom: coin.Denom,
				From: sender, To: recipient, Trigger: trigger, Detail: detail, Source: "event"})
		}
		keep(i)
		keep(i + 1)
		i++
	}
	return kept, rows, nil
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
			items[i].Fee.Evidence = "the payer's account sequence"
		case verdict:
		default:
			return fmt.Errorf("legacy fee verdict disagrees with the block results")
		}
	}
	return nil
}
