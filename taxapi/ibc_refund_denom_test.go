package taxapi

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/stretchr/testify/require"
)

func TestRefundExactVoucherTraceAlias(t *testing.T) {
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	for _, trace := range []string{"transfer/channel-536/uusdc", "transfer/channel-1/transfer/channel-536/uusdc", "transfer/channel-1/factory/creator/token"} {
		hash := fmt.Sprintf("ibc/%X", sha256.Sum256([]byte(trace)))
		for _, reverse := range []bool{false, true} {
			for _, trigger := range []string{ibcTimeoutRefund, ibcErrorAckRefund} {
				send, refund := trace, hash
				if reverse {
					send, refund = refund, send
				}
				events := []tax.TaxableEvent{
					{ID: 1, Category: "ibc_out", FromAddr: apiWallet, Denom: send, Amount: "1970058", Detail: basisPacket, Timestamp: at},
					{ID: 2, Category: string(tax.CategoryProtocolWithdrawal), ToAddr: apiWallet, Denom: refund, Amount: "1970058", Detail: basisPacket, Timestamp: at.Add(time.Minute), RewardTrigger: trigger},
				}
				pairs := matchIBCRefunds(events)
				require.Equal(t, map[uint]string{1: ibcRefundRoleSend, 2: ibcRefundRoleReturned}, pairs)
				require.True(t, ibcBasisReadiness(events, at.Add(time.Hour)).ExportReady)
				reconcileIBCRefunds(events, pairs)
				require.Equal(t, string(tax.CategoryProtocolDeposit), events[0].Category)
				require.Equal(t, ibcRefundReconciled, events[1].RewardTrigger)
				require.Equal(t, send, events[0].Denom, "evidence stays byte-for-byte unchanged")
				require.Equal(t, refund, events[1].Denom)
			}
		}
	}
}

// Optional offline replay of retained private input. No database or node access.
func TestRefundRetainedEvidence(t *testing.T) {
	path := os.Getenv("COSMOS_TAX_REFUND_EVIDENCE")
	if path == "" {
		t.Skip("set COSMOS_TAX_REFUND_EVIDENCE to a retained private diagnostic")
	}
	var input struct{ Events []tax.TaxableEvent }
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &input))
	require.NotEmpty(t, input.Events)
	before, err := json.Marshal(input.Events)
	require.NoError(t, err)
	readiness := ibcBasisReadiness(input.Events, time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC))
	require.True(t, readiness.ExportReady)
	require.Zero(t, readiness.PendingSemanticReviews)
	after, err := json.Marshal(input.Events)
	require.NoError(t, err)
	require.Equal(t, before, after, "identity matching must not mutate retained source")
}

func TestRefundVoucherAliasNeverWeakensIdentity(t *testing.T) {
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	trace := "transfer/channel-536/uusdc"
	hash := fmt.Sprintf("ibc/%X", sha256.Sum256([]byte(trace)))
	pair := func() []tax.TaxableEvent {
		return []tax.TaxableEvent{
			{ID: 1, Category: "ibc_out", FromAddr: apiWallet, Denom: trace, Amount: "1970058", Detail: basisPacket, Timestamp: at},
			{ID: 2, Category: string(tax.CategoryProtocolWithdrawal), ToAddr: apiWallet, Denom: hash, Amount: "1970058", Detail: basisPacket, Timestamp: at.Add(time.Minute), RewardTrigger: ibcTimeoutRefund},
		}
	}
	for name, change := range map[string]func([]tax.TaxableEvent) []tax.TaxableEvent{
		"base denom":       func(e []tax.TaxableEvent) []tax.TaxableEvent { e[1].Denom = "uusdc"; return e },
		"different route":  func(e []tax.TaxableEvent) []tax.TaxableEvent { e[0].Denom = "transfer/channel-537/uusdc"; return e },
		"different amount": func(e []tax.TaxableEvent) []tax.TaxableEvent { e[1].Amount = "1"; return e },
		"different packet": func(e []tax.TaxableEvent) []tax.TaxableEvent {
			e[1].Detail = `{"ibc_generation":"v1","packet_sequence":"24","packet_src_port":"transfer","packet_src_channel":"channel-7"}`
			return e
		},
		"different owner":  func(e []tax.TaxableEvent) []tax.TaxableEvent { e[1].ToAddr = testOtherWallet; return e },
		"earlier refund":   func(e []tax.TaxableEvent) []tax.TaxableEvent { e[1].Timestamp = at.Add(-time.Minute); return e },
		"duplicate send":   func(e []tax.TaxableEvent) []tax.TaxableEvent { extra := e[0]; extra.ID = 3; return append(e, extra) },
		"duplicate refund": func(e []tax.TaxableEvent) []tax.TaxableEvent { extra := e[1]; extra.ID = 3; return append(e, extra) },
	} {
		t.Run(name, func(t *testing.T) { require.Empty(t, matchIBCRefunds(change(pair()))) })
	}
	for _, denom := range []string{"uatom", "factory/creator/token", "transfer/not-a-channel/uusdc", "ibc/not-a-hash", ""} {
		require.Equal(t, denom, ibcRefundDenom(denom))
	}
}
