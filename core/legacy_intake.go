package core

import (
	"bytes"
	"fmt"

	"github.com/DefiantLabs/cosmos-indexer/legacy"
	"github.com/DefiantLabs/cosmos-indexer/legacy/evidence"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	abci "github.com/cometbft/cometbft/abci/types"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// LegacyInclusion is an offline intake candidate, never a serving Tx row.
// Identity is chain/height/index; Hash is evidence, not a uniqueness constraint.
// Original Amino messages and explicit derived SDK forms coexist in Tx.
type LegacyInclusion struct {
	Identity, Hash    string
	Tx                *legacy.Tx
	Result            *abci.ResponseDeliverTx
	RawResultEnvelope []byte
	CodeEvidence      string
	Tags              []rpc.LegacyTag
	// Messages holds the evidence of each message that began executing, in
	// order. It is shorter than Tx.Messages when execution stopped early.
	Messages  []evidence.Message
	Execution string
	Fee       LegacyFeeEvidence
	// Removals holds, by message index, what the chain state says about a
	// message that took stake away from a validator. It is empty until
	// ResolveLegacyRemovals fills it.
	Removals            map[int]LegacyRemoval
	ClassificationReady bool
	Issues              []string
}
type LegacyFeeEvidence struct {
	Declared sdk.Coins
	Payer    sdk.AccAddress
	// charged means the ante handler finished, which commits the fee: either
	// the transaction succeeded, or it failed after a message began executing.
	// unknown means a failure that left no execution marker, where an ante
	// rejection (no fee) and a panic during execution (fee kept) look the
	// same. not charged means the ante handler provably stopped: it ran out
	// of gas at a point only it reaches, or it refused the transaction because
	// the payer could not cover the fee. Its fee deduction was discarded. The
	// signed fee is retained, never converted to zero.
	Charge string
	// Evidence says how Charge is known, in words fit to store beside it.
	Evidence string
}

// DecodeLegacyBlock is an explicit offline adapter. It does not synthesize a
// cosmosTx.Tx, invoke protobuf decoding, run tax classifiers or write a DB.
// Chain identity and result cardinality are checked before any candidates return.
func DecodeLegacyBlock(block *coretypes.ResultBlock, results *rpc.CustomBlockResults) ([]LegacyInclusion, error) {
	if err := ValidateBlockTxResults(block, results); err != nil {
		return nil, err
	}
	if results.Legacy == nil || len(results.Legacy.CodeEvidence) != len(results.TxsResults) || len(results.Legacy.TxTags) != len(results.TxsResults) {
		return nil, fmt.Errorf("missing legacy execution evidence")
	}
	chain := block.Block.ChainID
	if _, err := legacy.InclusionKey(chain, block.Block.Height, 0); err != nil {
		return nil, err
	}
	if len(results.Legacy.RawJSON) == 0 || (chain == "cosmoshub-3" && results.Legacy.Dialect != "tendermint-0.32") || (chain != "cosmoshub-3" && results.Legacy.Dialect != "tendermint-0.30-0.31") {
		return nil, fmt.Errorf("legacy result dialect does not match chain era")
	}
	for _, evidence := range results.Legacy.CodeEvidence {
		if evidence != "explicit" && !(evidence == "omitted-zero-omitempty" && results.Legacy.Dialect == "tendermint-0.30-0.31") {
			return nil, fmt.Errorf("unsupported legacy execution code evidence")
		}
	}
	var output []LegacyInclusion
	for i, raw := range block.Block.Txs {
		tx, err := legacy.Decode(chain, raw)
		if err != nil {
			return nil, fmt.Errorf("legacy transaction %d: %w", i, err)
		}
		id, err := legacy.InclusionKey(chain, block.Block.Height, i)
		if err != nil {
			return nil, err
		}
		result := results.TxsResults[i]
		item := LegacyInclusion{Identity: id, Hash: tendermintHashToHex(raw.Hash()), Tx: tx, Result: result, RawResultEnvelope: bytes.Clone(results.Legacy.RawJSON), CodeEvidence: results.Legacy.CodeEvidence[i], Tags: results.Legacy.TxTags[i], Execution: "failed", Issues: append([]string(nil), tx.Issues...)}
		item.Fee = LegacyFeeEvidence{Declared: tx.Fee, Charge: "unknown"}
		if tx.SignersComplete && len(tx.Signers) > 0 {
			item.Fee.Payer = bytes.Clone(tx.Signers[0])
		}
		messages, evidenceErr := evidence.For(chain, tx, result, item.Tags)
		if evidenceErr != nil {
			item.Issues = append(item.Issues, "legacy message evidence unattributed: "+evidenceErr.Error())
		}
		item.Messages = messages
		switch {
		case len(item.Fee.Payer) == 0:
			item.Issues = append(item.Issues, "fee payer unresolved")
		case result.Code == 0:
			item.Fee.Charge, item.Fee.Evidence = "charged", "the transaction succeeded"
		case len(messages) > 0:
			item.Fee.Charge, item.Fee.Evidence = "charged", "it failed after a message began executing"
		case evidence.AnteOnlyLocation(evidence.OutOfGasLocation(result)):
			item.Fee.Charge, item.Fee.Evidence = "not charged", "it ran out of gas before the fee was taken"
		case evidence.AnteFeeRejection(result):
			item.Fee.Charge, item.Fee.Evidence = "not charged", "the payer could not cover the fee"
		default:
			item.Issues = append(item.Issues, "failed legacy transaction left no execution marker; fee commitment needs gas or state evidence")
		}
		if result.Code == 0 {
			item.Execution = "succeeded"
		}
		// Successful legacy reward and staking messages need era-specific event/tag
		// reconciliation, not the modern event classifier's empty-output shortcut.
		item.Issues = append(item.Issues, "legacy economic event classification not yet reconciled")
		output = append(output, item)
	}
	return output, nil
}

func IsLegacyHubChain(chain string) bool {
	return chain == "cosmoshub-1" || chain == "cosmoshub-2" || chain == "cosmoshub-3"
}

// LegacyIntakeAdmission is intentionally not configurable. A decoder candidate
// is not permission to write into the serving schema or mark coverage complete.
func LegacyIntakeAdmission() error {
	return fmt.Errorf("legacy serving intake blocked: inclusion-scoped DB identity; per-hub genesis balances/delegations/vesting; terminal-to-genesis per-address reconciliation; failed-fee commitment; legacy economic classification; historical daily balances; report routing")
}
