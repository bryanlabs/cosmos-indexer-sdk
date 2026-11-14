package taxapi

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

const basisPacket = `{"ibc_generation":"v1","packet_sequence":"23","packet_src_port":"transfer","packet_src_channel":"channel-7"}`

func TestNativeRefundBasisGateAcrossYearsAndLaterSpend(t *testing.T) {
	dec := time.Date(2025, 12, 31, 12, 0, 0, 0, time.UTC)
	jan := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	for _, trigger := range []string{ibcTimeoutRefund, ibcErrorAckRefund} {
		t.Run(trigger, func(t *testing.T) {
			events := []tax.TaxableEvent{
				{Category: apiReward, ToAddr: apiWallet, Amount: "100000000", Denom: apiUatom, Timestamp: dec.Add(-24 * time.Hour)},
				{Category: "ibc_out", FromAddr: apiWallet, Amount: "100000000", Denom: apiUatom, Detail: basisPacket, Timestamp: dec},
				{Category: string(tax.CategoryProtocolWithdrawal), ToAddr: apiWallet, Amount: "100000000", Denom: apiUatom, Detail: basisPacket, RewardTrigger: trigger, Timestamp: jan},
				{Category: apiTransfer, FromAddr: apiWallet, Amount: "100000000", Denom: apiUatom, Timestamp: jan.Add(24 * time.Hour)},
			}
			for _, end := range []time.Time{dec.Add(6 * time.Hour), jan.Add(12 * time.Hour), jan.AddDate(1, 0, 0)} {
				r := ibcBasisReadiness(events, end)
				require.False(t, r.ExportReady, "must not certify original disposal, returned inventory or later real spend")
				require.Contains(t, r.ReviewRequired, ibcRefundBasisRequired)
			}
			require.True(t, ibcBasisReadiness(events, dec.Add(-time.Hour)).ExportReady)
			// No packet/owner matching by quantity, denom or adjacent dates.
			events[2].Detail = strings.Replace(basisPacket, `"23"`, `"24"`, 1)
			require.True(t, ibcBasisReadiness(events, dec.Add(6*time.Hour)).ExportReady)
			events[2].Detail = basisPacket
			events[2].ToAddr = testOtherWallet
			require.True(t, ibcBasisReadiness(events, dec.Add(6*time.Hour)).ExportReady)
		})
	}
}

func TestV2RefundBasisIdentityAndClientScope(t *testing.T) {
	at := time.Date(2025, 12, 31, 12, 0, 0, 0, time.UTC)
	key := `{"ibc_generation":"v2","packet_source_client":"08-wasm-1","packet_destination_client":"08-wasm-2","packet_sequence":"23"}`
	events := []tax.TaxableEvent{
		{Category: "ibc_out", FromAddr: apiWallet, Detail: key, Timestamp: at},
		{Category: string(tax.CategoryProtocolWithdrawal), ToAddr: apiWallet, Detail: key, RewardTrigger: ibcErrorAckRefund, Timestamp: at.Add(48 * time.Hour)},
	}
	require.False(t, ibcBasisReadiness(events, at.Add(time.Hour)).ExportReady)
	events[1].Detail = strings.Replace(key, "08-wasm-2", "08-wasm-3", 1)
	require.True(t, ibcBasisReadiness(events, at.Add(time.Hour)).ExportReady)
}

func TestRefundSemanticBlockCannotBeClearedByDecisionsOrTags(t *testing.T) {
	row := semanticClaimRow()
	row.Category, row.Denom, row.RewardTrigger, row.Detail = string(tax.CategoryProtocolWithdrawal), apiUatom, ibcTimeoutRefund, basisPacket
	row.Amount, row.PriceUSD, row.ValueUSD = decimal.NewFromInt(100), decimal.NewFromInt(2), decimal.NewFromInt(200)
	for _, decisions := range []AssetDecisions{
		{row.AssetIdentity.RecordID: {Mode: AssetDecisionExclude}},
		{DecisionKeySpamPolicy: {Mode: AssetDecisionExclude}},
		{DecisionKeyDenomPrefix + apiClaimToken: {Mode: AssetDecisionOverride, Token: "ATOM", UnitValueUSD: "2"}},
	} {
		rows, err := ApplyAssetDecisions([]Row{row}, decisions)
		require.NoError(t, err)
		for _, tag := range []string{TagSpam, TagInternalTransfer, TagIncome, TagProtocolDeposit} {
			rows[0].UserTag = tag
			require.False(t, readinessForRows(rows).ExportReady)
			require.Contains(t, readinessForRows(rows).ReviewRequired, ibcRefundBasisRequired)
			var out strings.Builder
			require.ErrorAs(t, WriteCSV(&out, apiGeneric, rows), &reportReadinessRequired{})
			require.Empty(t, out.String())
		}
	}
}

func TestFinancialFingerprintFramingAndDeletion(t *testing.T) {
	digest := func(rows ...[2]string) string {
		h := sha256.New()
		for _, row := range rows {
			writeFinancialFingerprintRow(h, row[0], row[1])
		}
		return fmt.Sprintf("%x", h.Sum(nil))
	}
	base := [2]string{financialEventKind, `{"denom":"uatom","amount":"100"}`}
	fee := [2]string{"fee", `{"amount":"1","payer_address_id":1}`}
	require.NotEqual(t, digest(base), digest())
	require.NotEqual(t, digest(base, fee), digest(base))
	require.NotEqual(t, digest(base, fee), digest(base, [2]string{"fee", `{"amount":"1","payer_address_id":2}`}))
	require.NotEqual(t, digest([2]string{financialEventKind, "ab"}, [2]string{financialEventKind, "c"}), digest([2]string{financialEventKind, "a"}, [2]string{financialEventKind, "bc"}))
}

func TestMatchedRefundRestoresOriginalLots(t *testing.T) {
	dec := time.Date(2025, 12, 31, 12, 0, 0, 0, time.UTC)
	jan := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	pair := func() []tax.TaxableEvent {
		return []tax.TaxableEvent{
			{ID: 1, Category: apiReward, ToAddr: apiWallet, Amount: "100000000", Denom: apiUatom, Timestamp: dec.AddDate(0, -6, 0)},
			{ID: 2, Category: "ibc_out", FromAddr: apiWallet, Amount: "100000000", Denom: apiUatom, Detail: basisPacket, Timestamp: dec},
			{ID: 3, Category: string(tax.CategoryProtocolWithdrawal), ToAddr: apiWallet, Amount: "100000000", Denom: apiUatom, Detail: basisPacket, RewardTrigger: ibcTimeoutRefund, Timestamp: jan},
		}
	}
	for _, end := range []time.Time{dec.Add(6 * time.Hour), jan.AddDate(1, 0, 0)} {
		require.True(t, ibcBasisReadiness(pair(), end).ExportReady)
	}
	events := pair()
	reconcileIBCRefunds(events, matchIBCRefunds(events))
	require.Equal(t, string(tax.CategoryProtocolDeposit), events[1].Category)
	require.Equal(t, ibcRefundedSend, events[1].RewardTrigger)
	require.Equal(t, ibcRefundReconciled, events[2].RewardTrigger)
	require.Equal(t, apiReward, events[0].Category)

	// The returned coins keep the reward lot's date and cost: a later sale is
	// long term against the reward basis, and nothing is disposed of at the send.
	rows := []Row{
		{Time: events[0].Timestamp, Category: apiReward, Direction: "in", Symbol: "ATOM", Amount: decimal.NewFromInt(100), PriceUSD: decimal.NewFromInt(5)},
		{Time: events[1].Timestamp, Category: events[1].Category, Direction: apiOut, Symbol: "ATOM", Amount: decimal.NewFromInt(100), PriceUSD: decimal.NewFromInt(9)},
		{Time: events[2].Timestamp, Category: events[2].Category, Direction: "in", Symbol: "ATOM", Amount: decimal.NewFromInt(100), PriceUSD: decimal.NewFromInt(9)},
		{Time: jan.AddDate(0, 6, 0), Category: apiTransfer, Direction: apiOut, Symbol: "ATOM", Amount: decimal.NewFromInt(100), PriceUSD: decimal.NewFromInt(8)},
	}
	lines := Build8949(rows)
	require.Len(t, lines, 1)
	require.True(t, lines[0].CostBasis.Equal(decimal.NewFromInt(500)))
	require.True(t, lines[0].LongTerm)

	for name, change := range map[string]func([]tax.TaxableEvent) []tax.TaxableEvent{
		"different amount": func(e []tax.TaxableEvent) []tax.TaxableEvent { e[2].Amount = "99000000"; return e },
		"different denom":  func(e []tax.TaxableEvent) []tax.TaxableEvent { e[2].Denom = "ibc/ABC"; return e },
		"refund before send": func(e []tax.TaxableEvent) []tax.TaxableEvent {
			e[2].Timestamp = dec.Add(-time.Hour)
			return e
		},
		"second send for the packet": func(e []tax.TaxableEvent) []tax.TaxableEvent {
			extra := e[1]
			extra.ID = 4
			return append(e, extra)
		},
		"second refund for the packet": func(e []tax.TaxableEvent) []tax.TaxableEvent {
			extra := e[2]
			extra.ID = 5
			return append(e, extra)
		},
		"refund without a row id": func(e []tax.TaxableEvent) []tax.TaxableEvent { e[2].ID = 0; return e },
	} {
		t.Run(name, func(t *testing.T) {
			events := change(pair())
			require.Empty(t, matchIBCRefunds(events))
			r := ibcBasisReadiness(events, jan.AddDate(1, 0, 0))
			require.False(t, r.ExportReady)
			require.Contains(t, r.ReviewRequired, ibcRefundBasisRequired)
		})
	}
}
