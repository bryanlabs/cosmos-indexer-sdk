package taxapi

import (
	"encoding/base64"

	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
)

// hubFailedTxEventsFrom is the first cosmoshub-4 block (Gaia v7, SDK 0.45)
// whose failed transactions carry the ante handler's events. From Gaia v6
// (block 8695000) to here a failed transaction has no events at all, whether
// or not its fee was charged: a message that failed after the ante handler
// passed still paid the whole declared fee, while one the ante handler
// turned away (a wrong sequence, say) paid nothing. Both were read from the
// chain: block 8695006 collected the fees of its four failed staking
// transactions, and block 9600000 collected none of its three wrong-sequence
// ones.
const hubFailedTxEventsFrom = 10085397

const (
	eventCoinReceived = "coin_received"
	eventCoinSpent    = "coin_spent"
	attrReceiver      = "receiver"
	attrSpender       = "spender"
	codespaceSDK      = "sdk"
	attrMode          = "mode"
)

type rpcEvent struct {
	Type       string    `json:"type"`
	Attributes []rpcAttr `json:"attributes"`
}

// rawTxInfo is what a transaction's bytes and result say about its fee.
type rawTxInfo struct {
	Codespace  string
	Fee        sdk.Coins
	FeeGranter string
	FeeDecoded bool
	// Reincluded is a failed copy of bytes another inclusion ran or paid the
	// fee for. The signers' sequences allow only one inclusion past the ante
	// handler, so this copy was turned away and paid nothing.
	Reincluded bool
	// Failed is the result of a failed transaction that carries no events,
	// which is what settles its fee before Gaia v7.
	Failed *failedTxResult
}

// decodeTxFee reads the declared fee and fee granter from a transaction's
// bytes.
func decodeTxFee(raw []byte) (sdk.Coins, string, bool) {
	var tx txtypes.TxRaw
	if err := tx.Unmarshal(raw); err != nil {
		return nil, "", false
	}
	var auth txtypes.AuthInfo
	if err := auth.Unmarshal(tx.AuthInfoBytes); err != nil || auth.Fee == nil {
		return nil, "", false
	}
	fee := sdk.NewCoins()
	for _, c := range auth.Fee.Amount {
		if c.Amount.IsNil() || c.Amount.IsNegative() || sdk.ValidateDenom(c.Denom) != nil {
			return nil, "", false
		}
		fee = fee.Add(c)
	}
	return fee, auth.Fee.Granter, true
}

// eventKV decodes one attribute, undoing the base64 encoding of older
// CometBFT releases.
func eventKV(a rpcAttr) (string, string) {
	switch a.Key {
	case attrReceiver, attrSpender, apiAmount, attrMode, "sender", "recipient":
		return a.Key, a.Value
	}
	k, err1 := base64.StdEncoding.DecodeString(a.Key)
	v, err2 := base64.StdEncoding.DecodeString(a.Value)
	if err1 == nil && err2 == nil {
		switch string(k) {
		case attrReceiver, attrSpender, apiAmount, attrMode, "sender", "recipient":
			return string(k), string(v)
		}
	}
	return a.Key, a.Value
}

func feeCollectorAddress() string {
	addr, err := sdk.Bech32ifyAddressBytes(sdk.GetConfig().GetBech32AccountAddrPrefix(), authtypes.NewModuleAddress(authtypes.FeeCollectorName))
	if err != nil {
		panic(err)
	}
	return addr
}

// preEventStats counts how the scan settled failed transactions' fees on
// blocks before failed transactions had events.
type preEventStats struct {
	Blocks            int            `json:"blocks"`
	BlocksWithFailed  int            `json:"blocks_with_failed"`
	ResolvedBlocks    int            `json:"resolved_blocks"`
	UnresolvedBlocks  int            `json:"unresolved_blocks"`
	FailedCharged     int            `json:"failed_charged"`
	FailedUncharged   int            `json:"failed_uncharged"`
	FailedUnknown     int            `json:"failed_unknown"`
	UnresolvedHeights []int64        `json:"unresolved_heights,omitempty"`
	UnresolvedReasons map[string]int `json:"unresolved_reasons,omitempty"`
}

func (p *preEventStats) add(o preEventStats) {
	p.Blocks += o.Blocks
	p.BlocksWithFailed += o.BlocksWithFailed
	p.ResolvedBlocks += o.ResolvedBlocks
	p.UnresolvedBlocks += o.UnresolvedBlocks
	p.FailedCharged += o.FailedCharged
	p.FailedUncharged += o.FailedUncharged
	p.FailedUnknown += o.FailedUnknown
	p.UnresolvedHeights = append(p.UnresolvedHeights, o.UnresolvedHeights...)
	for k, v := range o.UnresolvedReasons {
		if p.UnresolvedReasons == nil {
			p.UnresolvedReasons = map[string]int{}
		}
		p.UnresolvedReasons[k] += v
	}
}

// anteOnlyFailure is a failure only the ante handler raises, which
// charges nothing: a wrong account sequence, or bytes that do not decode.
func anteOnlyFailure(code uint32, codespace string) bool {
	return codespace == codespaceSDK && (code == 32 || code == 2)
}

// Unresolved reasons of the pre-v7 settlement.
const (
	preEventLogUnsettled = "a failed transaction's result does not settle its fee"
	preEventReincluded   = "a failed copy of a transaction that ran elsewhere reads as charged"
)

// settlePreEventBlock settles the fee of each failed transaction of a
// mainnet block before Gaia v7, where a failed transaction carries no events
// whether or not it paid, from where its result says it failed
// (preCoinEventsCharge): a message failure paid the declared fee and a
// rejection before the messages paid nothing. One the result does not settle
// is left unknown. A copy of bytes another inclusion ran was turned away, so
// one whose result reads as charged is a contradiction and is left unknown
// too. A failed transaction's fee grant is read from its bytes.
func settlePreEventBlock(height int64, b scannedBlock) preEventStats {
	st := preEventStats{Blocks: 1}
	failed, unknown := 0, 0
	reasons := map[string]int{}
	for i := range b.txs {
		t := &b.txs[i]
		info := b.raw[i]
		if t.Code == 0 || info.Failed == nil {
			continue
		}
		failed++
		if t.Granter == "" && info.FeeDecoded {
			t.Granter = info.FeeGranter
		}
		charged, ok := preCoinEventsCharge(*info.Failed)
		switch {
		case !ok:
			reasons[preEventLogUnsettled]++
		case charged != "" && info.Reincluded:
			ok = false
			reasons[preEventReincluded]++
		}
		switch {
		case !ok:
			t.Rejected, t.Charged, t.ChargeKnown = false, "", false
			unknown++
			st.FailedUnknown++
		case charged == "":
			t.Rejected, t.Charged, t.ChargeKnown = true, "", true
			st.FailedUncharged++
		default:
			t.Rejected, t.Charged, t.ChargeKnown = false, charged, true
			st.FailedCharged++
		}
	}
	if failed == 0 {
		return st
	}
	st.BlocksWithFailed++
	if unknown > 0 {
		st.UnresolvedBlocks++
		st.UnresolvedHeights = append(st.UnresolvedHeights, height)
		st.UnresolvedReasons = reasons
		return st
	}
	st.ResolvedBlocks++
	return st
}
