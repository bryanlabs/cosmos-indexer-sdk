package taxapi

import (
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func saleRow(day, minute int, price string) tax.TaxableEvent {
	return flowRow(day, minute, apiNFTSale, flowRowsSeller, flowRowsBuyer, price, "uatom", "")
}

// A seller is paid the price less the marketplace fee and the royalty: 39.2
// of a 40 ATOM sale and 44 of a 50 ATOM one. The trace credits what the chain
// paid, so the wallet ends with the chain's balance.
func TestFlowNFTSaleCreditsWhatTheChainPaid(t *testing.T) {
	rows := []tax.TaxableEvent{
		saleRow(2, 0, "40000000"),
		saleRow(3, 1, "50000000"),
	}
	credited := saleCredits([]flowReceipt{
		{MessageID: rows[0].MessageID, Value: "39200000uatom"},
		{MessageID: rows[1].MessageID, Value: "44000000uatom,5ibc/ABC"},
	}, "uatom")
	l := newFakeFlowLedger()
	for _, e := range rows {
		fe, ok := flowEventFromRow(e, flowRowsSeller, "uatom")
		require.True(t, ok)
		l.rows[flowRowsSeller] = append(l.rows[flowRowsSeller], fe)
	}
	l.rows[flowRowsSeller] = netSaleProceeds(l.rows[flowRowsSeller], flowRowsSeller, credited)
	l.commission(flowAt(1, 0), flowRowsSeller, 10000000)
	tr := runFlowTest(t, l, flowRowsSeller)
	// Chain: 10 + 39.2 + 44 = 93.2 ATOM. The prices would give 100.
	require.Equal(t, "93200000", tr.Wallets[flowRowsSeller].simulated().String())
}

func TestFlowNFTSaleNetting(t *testing.T) {
	in := func(msg uint, category, from, to, amount string) flowEvent {
		return flowEvent{Key: flowKey{Message: msg}, Category: category, Dir: "in", From: from, To: to, Amount: base(amount)}
	}
	out := func(msg uint, amount string) flowEvent {
		return flowEvent{Key: flowKey{Message: msg}, Category: apiNFTSale, Dir: apiOut, From: flowRowsSeller, To: flowRowsBuyer, Amount: base(amount)}
	}
	events := []flowEvent{
		// 1: two sales in one message share 90 of 100 by price.
		in(1, apiNFTSale, flowRowsSeller, flowRowsBuyer, "30"),
		in(1, apiNFTSale, flowRowsSeller, flowRowsBuyer, "70"),
		// 2: a refund row in the same message is credited on its own.
		in(2, apiNFTSale, flowRowsSeller, flowRowsBuyer, "50"),
		in(2, string(tax.CategoryProtocolWithdrawal), flowRowsAuction, flowRowsSeller, "20"),
		// 3: no credit recorded, the price stays.
		in(3, apiNFTSale, flowRowsSeller, flowRowsBuyer, "40"),
		// 4: the buyer's side is left alone.
		out(4, "60"),
		// 5: three sales split 100 by price, the last takes the rounding.
		in(5, apiNFTSale, flowRowsSeller, flowRowsBuyer, "1"),
		in(5, apiNFTSale, flowRowsSeller, flowRowsBuyer, "1"),
		in(5, apiNFTSale, flowRowsSeller, flowRowsBuyer, "1"),
	}
	got := netSaleProceeds(events, flowRowsSeller, map[uint]decimal.Decimal{1: base("90"), 2: base("66"), 4: base("1"), 5: base("100")})
	var amounts []string
	for _, e := range got {
		amounts = append(amounts, e.Amount.String())
	}
	require.Equal(t, []string{"27", "63", "46", "20", "40", "60", "33", "33", "34"}, amounts)
}

// The sale proceeds read against real tables: the seller's credit in each
// sale message, native coin only, and nothing for a sale that credited
// someone else or for a row where the wallet bought.
func TestFlowSaleProceedsPostgres(t *testing.T) {
	s := flowPGServer(t)
	seller := flowPGReceiver1
	for _, sql := range []string{
		"CREATE TABLE message_event_types (id bigint, type text)",
		"CREATE TABLE message_events (id bigint, message_id bigint, message_event_type_id bigint, index bigint)",
		"CREATE TABLE message_event_attribute_keys (id bigint, key text)",
		"CREATE TABLE message_event_attributes (id bigint, message_event_id bigint, message_event_attribute_key_id bigint, value text, index bigint)",
		"INSERT INTO message_event_types VALUES (1,'coin_received')",
		"INSERT INTO message_event_attribute_keys VALUES (1,'receiver'),(2,'amount')",
		// 601: fee to the market, the rest to the seller. 602: the seller is
		// paid in two events plus a non-native coin. 603: only the buyer's
		// purchase, paid to someone else.
		"INSERT INTO message_events VALUES (1,601,1,0),(2,601,1,1),(3,602,1,0),(4,602,1,1),(5,603,1,0)",
		"INSERT INTO message_event_attributes VALUES " +
			"(1,1,1,'" + flowPGReceiver2 + "',0),(2,1,2,'800000uatom',1)," +
			"(3,2,1,'" + seller + "',0),(4,2,2,'39200000uatom',1)," +
			"(5,3,1,'" + seller + "',0),(6,3,2,'40000000uatom',1)," +
			"(7,4,1,'" + seller + "',0),(8,4,2,'5ibc/ABC,4000000uatom',1)," +
			"(9,5,1,'" + flowPGReceiver2 + "',0),(10,5,2,'7000000uatom',1)",
	} {
		require.NoError(t, s.db.Exec(sql).Error, sql)
	}
	sale := func(msg uint, from, to string) tax.TaxableEvent {
		return tax.TaxableEvent{MessageID: msg, Category: apiNFTSale, Amount: "50000000", Denom: "uatom", FromAddr: from, ToAddr: to}
	}
	got, err := s.flowSaleProceeds(seller, []tax.TaxableEvent{sale(601, seller, flowPGReceiver2), sale(602, seller, flowPGReceiver2), sale(603, flowPGReceiver2, seller)})
	require.NoError(t, err)
	amounts := map[uint]string{}
	for m, v := range got {
		amounts[m] = v.String()
	}
	require.Equal(t, map[uint]string{601: "39200000", 602: "44000000"}, amounts)
}
