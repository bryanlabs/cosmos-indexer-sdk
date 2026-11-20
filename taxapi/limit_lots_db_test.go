package taxapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

// secondMessageSQL adds the fixture's second message, in the second tx.
const secondMessageSQL = "INSERT INTO messages(id,tx_id,message_type_id,message_index) VALUES(2,2,1,0)"

// Uses the disposable Docker Postgres fixture, never a live tax DB. A maker
// fill in the period finds the sell order the wallet placed before it.
func TestLimitMakerFillLotFromEarlierPeriod(t *testing.T) {
	s, db, wallet, _ := assetReviewFixture(t)
	contract := "book-contract"
	fillDetail := `{"contract":"` + contract + `","maker":"true","review_reason":"` + tax.ReviewLimitMakerSpend + `","review_required":"true"}`
	require.NoError(t, db.Model(&tax.TaxableEvent{}).Where("message_id = 1").Updates(map[string]any{
		"from_addr": contract, apiDenom: "ibc/QUOTE", apiAmount: "8000000", apiCategory: string(tax.CategorySwap), apiRewardTrigger: tax.TriggerLimitFill, apiDetail: fillDetail,
	}).Error)
	start, end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	rows, err := s.rowsFor(apiMainnet, wallet, start, end)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotEmpty(t, semanticReviewReason(rows[0]))

	for _, q := range []string{
		"INSERT INTO blocks(id,chain_id,proposer_cons_address_id,height,time_stamp,tx_indexed) VALUES(2,1,1,25000000,'2025-03-01T00:00:00Z',true)",
		"INSERT INTO txes(id,block_id,hash,code,memo) VALUES(2,2,'ORDERTX',0,'')",
		secondMessageSQL,
	} {
		require.NoError(t, db.Exec(q).Error)
	}
	order := tax.TaxableEvent{
		MessageID: 2, Category: string(tax.CategoryProtocolDeposit), FromAddr: wallet, ToAddr: contract, Amount: "9000000", Denom: apiUatom,
		RewardTrigger: tax.TriggerLimitOrder,
		Detail:        `{"contract":"` + contract + `","open_qty":"9000000","order_id":"7","price":"2000000000000","side":"sell"}`,
		BlockHeight:   25000000, Timestamp: time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC), TxHash: "ORDERTX",
	}
	require.NoError(t, db.Omit(apiMessage).Create(&order).Error)
	rows, err = s.rowsFor(apiMainnet, wallet, start, end)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, r := range rows {
		require.Empty(t, semanticReviewReason(r))
	}
	var paid Row
	for _, r := range rows {
		if r.Direction == apiOut {
			paid = r
		}
	}
	require.Equal(t, apiUatom, paid.Denom)
	require.Equal(t, "4", paid.Amount.String())
}
