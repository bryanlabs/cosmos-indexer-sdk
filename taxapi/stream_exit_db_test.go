package taxapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

// Uses the disposable Docker Postgres fixture, never a live tax DB. A stream
// exit in the period finds a subscription made before it.
func TestStreamExitLotFromEarlierPeriod(t *testing.T) {
	s, db, wallet, _ := assetReviewFixture(t)
	contract := "stream-contract"
	exitDetail := `{"contract":"` + contract + `","review_reason":"stream_spend_not_in_message","review_required":"true","spent":"7000000","stream_id":"4"}`
	require.NoError(t, db.Model(&tax.TaxableEvent{}).Where("message_id = 1").Updates(map[string]any{
		"from_addr": contract, apiCategory: string(tax.CategorySwap), apiRewardTrigger: tax.TriggerStreamExit, apiDetail: exitDetail,
	}).Error)
	start, end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	rows, err := s.rowsFor(apiMainnet, wallet, start, end)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotEmpty(t, semanticReviewReason(rows[0]))

	for _, q := range []string{
		"INSERT INTO blocks(id,chain_id,proposer_cons_address_id,height,time_stamp,tx_indexed) VALUES(2,1,1,25000000,'2025-03-01T00:00:00Z',true)",
		"INSERT INTO txes(id,block_id,hash,code,memo) VALUES(2,2,'SUBTX',0,'')",
		"INSERT INTO messages(id,tx_id,message_type_id,message_index) VALUES(2,2,1,0)",
	} {
		require.NoError(t, db.Exec(q).Error)
	}
	sub := tax.TaxableEvent{
		MessageID: 2, Category: string(tax.CategoryProtocolDeposit), FromAddr: wallet, ToAddr: contract, Amount: "9000000", Denom: apiUatom,
		RewardTrigger: tax.TriggerStreamSubscribe, Detail: `{"contract":"` + contract + `","stream_id":"4"}`,
		BlockHeight: 25000000, Timestamp: time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC), TxHash: "SUBTX",
	}
	require.NoError(t, db.Omit(apiMessage).Create(&sub).Error)
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
	require.Equal(t, "7", paid.Amount.String())
}
