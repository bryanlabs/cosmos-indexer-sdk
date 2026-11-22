package taxapi

import (
	"encoding/base64"
	"fmt"

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

// hubGenesisHeight is cosmoshub-4's first block. Below Gaia v6 (block
// 8695000) the fee collector's BeginBlock movements show only as transfer
// events, so the scan reads those instead of coin_received and coin_spent.
const hubGenesisHeight = 5200791

// hubFeeGrantsFrom is the Gaia v5 upgrade block (SDK 0.44), where the
// feegrant module started. Before it a fee granter written into a
// transaction's bytes paid nothing: the fee payer paid.
const hubFeeGrantsFrom = 6910000

const eventTransfer = "transfer"

// maxFeeSubsetSearch bounds the failed transactions of one block whose
// charges are searched combination by combination.
const maxFeeSubsetSearch = 20

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

// feeCollectorBegin is what the fee collector received and sent in a block's
// BeginBlock, in order. In Gaia v6 the mint module pays the block's new coins
// to the fee collector first, then the distribution module takes the fee
// collector's whole balance: the previous block's fees plus those new coins.
type feeCollectorBegin struct {
	received []sdk.Coins
	spent    []sdk.Coins
	// receivedFirst is set when the first receipt came before the first
	// spend.
	receivedFirst bool
	err           error
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

// readFeeCollectorBegin collects the fee collector's coin_received and
// coin_spent events from a block's BeginBlock events (older results) or the
// BeginBlock part of its finalize events (newer nodes serving old blocks).
//
// Blocks before Gaia v6 carry no coin events. There the fee collector's
// movements are read from transfer events: what it was sent (the minted
// coins) and what it sent (to the distribution module). A block whose
// BeginBlock has any coin event for the fee collector is read from those
// alone, since Gaia v6 and later emit a transfer event for each send too.
func readFeeCollectorBegin(begin, finalize []rpcEvent) feeCollectorBegin {
	out := readFeeCollectorCoins(begin, finalize)
	if out.err != nil || len(out.received) > 0 || len(out.spent) > 0 {
		return out
	}
	return readFeeCollectorTransfers(begin, finalize)
}

// readFeeCollectorTransfers reads the fee collector's BeginBlock transfer
// events, in order, for blocks with no coin events.
func readFeeCollectorTransfers(begin, finalize []rpcEvent) feeCollectorBegin {
	var out feeCollectorBegin
	fc := feeCollectorAddress()
	events := begin
	fromFinalize := len(begin) == 0
	if fromFinalize {
		events = finalize
	}
	for _, e := range events {
		if e.Type != eventTransfer {
			continue
		}
		var sender, recipient, amount, mode string
		for _, a := range e.Attributes {
			k, v := eventKV(a)
			switch k {
			case "sender":
				sender = v
			case "recipient":
				recipient = v
			case apiAmount:
				amount = v
			case attrMode:
				mode = v
			}
		}
		if fromFinalize && mode != "BeginBlock" {
			continue
		}
		if sender != fc && recipient != fc {
			continue
		}
		if sender == recipient {
			out.err = fmt.Errorf("fee collector transfer to itself")
			return out
		}
		coins, err := sdk.ParseCoinsNormalized(amount)
		if err != nil {
			out.err = fmt.Errorf("fee collector transfer amount %q: %w", amount, err)
			return out
		}
		if recipient == fc {
			if len(out.spent) == 0 && len(out.received) == 0 {
				out.receivedFirst = true
			}
			out.received = append(out.received, coins)
		} else {
			out.spent = append(out.spent, coins)
		}
	}
	return out
}

// readFeeCollectorCoins reads the fee collector's BeginBlock coin_received
// and coin_spent events, in order.
func readFeeCollectorCoins(begin, finalize []rpcEvent) feeCollectorBegin {
	var out feeCollectorBegin
	fc := feeCollectorAddress()
	events := begin
	fromFinalize := len(begin) == 0
	if fromFinalize {
		events = finalize
	}
	for _, e := range events {
		if e.Type != eventCoinReceived && e.Type != eventCoinSpent {
			continue
		}
		var who, amount, mode string
		for _, a := range e.Attributes {
			k, v := eventKV(a)
			switch k {
			case attrReceiver, attrSpender:
				who = v
			case apiAmount:
				amount = v
			case attrMode:
				mode = v
			}
		}
		if fromFinalize && mode != "BeginBlock" {
			continue
		}
		if who != fc {
			continue
		}
		coins, err := sdk.ParseCoinsNormalized(amount)
		if err != nil {
			out.err = fmt.Errorf("fee collector %s amount %q: %w", e.Type, amount, err)
			return out
		}
		if e.Type == eventCoinReceived {
			if len(out.spent) == 0 && len(out.received) == 0 {
				out.receivedFirst = true
			}
			out.received = append(out.received, coins)
		} else {
			out.spent = append(out.spent, coins)
		}
	}
	return out
}

// collectedFees is the fees the previous block paid the fee collector, read
// from this block's BeginBlock: what the distribution module took less the
// new coins the mint module paid in just before it.
func (b feeCollectorBegin) collectedFees() (sdk.Coins, error) {
	if b.err != nil {
		return nil, b.err
	}
	switch {
	case len(b.spent) > 1 || len(b.received) > 1:
		return nil, fmt.Errorf("fee collector moved %d times out and %d times in", len(b.spent), len(b.received))
	case len(b.spent) == 0 && len(b.received) == 0:
		return sdk.NewCoins(), nil
	case len(b.spent) == 0:
		return nil, fmt.Errorf("fee collector received new coins but paid nothing out")
	case len(b.received) == 0:
		return b.spent[0], nil
	case !b.receivedFirst:
		return nil, fmt.Errorf("fee collector paid out before receiving the new coins")
	}
	fees, neg := b.spent[0].SafeSub(b.received[0]...)
	if neg {
		return nil, fmt.Errorf("fee collector paid out %s, less than the %s it received", b.spent[0], b.received[0])
	}
	return fees, nil
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
	SubsetResolved    int            `json:"subset_resolved_blocks"`
	CleanMismatch     int            `json:"clean_block_mismatches"`
	UnresolvedHeights []int64        `json:"unresolved_heights,omitempty"`
	MismatchHeights   []int64        `json:"clean_mismatch_heights,omitempty"`
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
	p.SubsetResolved += o.SubsetResolved
	p.CleanMismatch += o.CleanMismatch
	p.UnresolvedHeights = append(p.UnresolvedHeights, o.UnresolvedHeights...)
	p.MismatchHeights = append(p.MismatchHeights, o.MismatchHeights...)
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

// resolvePreEventFees decides which failed transactions of a block paid their
// fee, from the fees the block collected. Every successful transaction paid
// its whole declared fee, and so did each failed one the ante handler let
// through; the rest paid nothing. It accepts only an exact, unique answer.
func resolvePreEventFees(txs []nodeTx, raw []rawTxInfo, collected sdk.Coins) ([]bool, string) {
	charged := make([]bool, len(txs))
	paid := sdk.NewCoins()
	var candidates []int
	for i, t := range txs {
		info := raw[i]
		if !info.FeeDecoded {
			if t.Code != 0 && (anteOnlyFailure(t.Code, info.Codespace) || info.Reincluded) {
				continue
			}
			return nil, "undecodable transaction"
		}
		switch {
		case t.Code == 0:
			paid = paid.Add(info.Fee...)
		case anteOnlyFailure(t.Code, info.Codespace), info.Reincluded, info.Fee.IsZero():
		default:
			candidates = append(candidates, i)
		}
	}
	target, neg := collected.SafeSub(paid...)
	if neg {
		return nil, "collected less than the successful fees"
	}
	sum := func(mask uint64) sdk.Coins {
		s := sdk.NewCoins()
		for j, i := range candidates {
			if mask&(1<<uint(j)) != 0 {
				s = s.Add(raw[i].Fee...)
			}
		}
		return s
	}
	all := uint64(1)<<uint(len(candidates)) - 1
	if len(candidates) < 64 && sum(all).String() == target.String() {
		for _, i := range candidates {
			charged[i] = true
		}
		return charged, ""
	}
	if len(candidates) > maxFeeSubsetSearch {
		return nil, "too many failed transactions to search"
	}
	found, matches := uint64(0), 0
	for mask := uint64(0); mask < all; mask++ {
		if sum(mask).String() == target.String() {
			found = mask
			matches++
			if matches > 1 {
				return nil, "ambiguous combination"
			}
		}
	}
	if matches == 0 {
		return nil, "no combination matches"
	}
	for j, i := range candidates {
		charged[i] = found&(1<<uint(j)) != 0
	}
	return charged, "subset"
}

// settlePreEventBlock marks each failed transaction of a block from before
// failed transactions had events as charged its declared fee, rejected, or
// unknown, using the fees the block collected (read from the next block).
// A failed transaction's fee grant is read from its bytes, from Gaia v5 on.
func settlePreEventBlock(height int64, b scannedBlock, next feeCollectorBegin) preEventStats {
	st := preEventStats{Blocks: 1}
	failed := 0
	for i, t := range b.txs {
		if t.Code != 0 {
			failed++
			if b.txs[i].Granter == "" && b.raw[i].FeeDecoded && height >= hubFeeGrantsFrom {
				b.txs[i].Granter = b.raw[i].FeeGranter
			}
		}
	}
	collected, err := next.collectedFees()
	var charged []bool
	reason := ""
	if err != nil {
		reason = "fee collector: " + err.Error()
	} else {
		charged, reason = resolvePreEventFees(b.txs, b.raw, collected)
	}
	if failed == 0 {
		if charged == nil {
			st.CleanMismatch++
			st.MismatchHeights = append(st.MismatchHeights, height)
		}
		return st
	}
	st.BlocksWithFailed++
	if charged == nil {
		st.UnresolvedBlocks++
		st.UnresolvedHeights = append(st.UnresolvedHeights, height)
		st.UnresolvedReasons = map[string]int{reason: 1}
		for i := range b.txs {
			if b.txs[i].Code == 0 {
				continue
			}
			// A failure only the ante handler raises, or a zero fee, paid
			// nothing whatever the block collected.
			if anteOnlyFailure(b.txs[i].Code, b.raw[i].Codespace) || b.raw[i].Reincluded || (b.raw[i].FeeDecoded && b.raw[i].Fee.IsZero()) {
				b.txs[i].Rejected, b.txs[i].Charged = true, ""
				st.FailedUncharged++
				continue
			}
			b.txs[i].Rejected, b.txs[i].Charged, b.txs[i].ChargeUnknown = false, "", true
			st.FailedUnknown++
		}
		return st
	}
	st.ResolvedBlocks++
	if reason == "subset" {
		st.SubsetResolved++
	}
	for i := range b.txs {
		if b.txs[i].Code == 0 {
			continue
		}
		if charged[i] {
			b.txs[i].Rejected, b.txs[i].Charged = false, b.raw[i].Fee.String()
			st.FailedCharged++
		} else {
			b.txs[i].Rejected, b.txs[i].Charged = true, ""
			st.FailedUncharged++
		}
	}
	return st
}
