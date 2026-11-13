package ledger_test

import (
	"context"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema/pgtest"
	"github.com/stretchr/testify/require"
)

func TestReplayPGSourcesRoundtripConfinedAndImmutable(t *testing.T) {
	db := pgtest.OpenRetained(t)
	findings, e := schema.Confined(db.Importer)
	require.NoError(t, e)
	require.Empty(t, findings)
	before := pgtest.Snapshot(t, db.Owner, "public")
	conn := importer(t, db)
	ctx := context.Background()
	b := sample("cosmoshub-2", 9)
	b.Rows[1].Source = "state"
	b.Rows[2].Source = "state"
	b.Rows = append(b.Rows, ledger.Row{Phase: ledger.PhaseExport, SubIndex: 0, Category: "commission", Amount: "13", Denom: "uatom", To: "owner", Validator: "validator", Trigger: "zero_height_export", Detail: `{"paid_to":"destination"}`, Source: "export_payout"})
	count, e := ledger.Write(ctx, conn, []ledger.Block{b})
	require.NoError(t, e)
	require.Equal(t, 1, count)
	saved, e := ledger.Load(ctx, conn, b.Chain, []int64{9})
	require.NoError(t, e)
	require.Len(t, saved, 1)
	require.Equal(t, b.Rows, saved[0].Rows)
	snapshot := pgtest.Snapshot(t, db.Owner, "legacy")
	count, e = ledger.Write(ctx, conn, []ledger.Block{b})
	require.NoError(t, e)
	require.Zero(t, count)
	changed := b
	changed.Rows = append([]ledger.Row(nil), b.Rows...)
	changed.Rows[1].Amount = "8"
	_, e = ledger.Write(ctx, conn, []ledger.Block{changed})
	require.ErrorIs(t, e, ledger.ErrStoredDiffers)
	pgtest.RequireUntouched(t, snapshot, pgtest.Snapshot(t, db.Owner, "legacy"))
	pgtest.RequireUntouched(t, before, pgtest.Snapshot(t, db.Owner, "public"))
}
func TestReplayPGPhaseSourceGuardsAndHub3Unchanged(t *testing.T) {
	db := pgtest.OpenRetained(t)
	conn := importer(t, db)
	ctx := context.Background()
	before := pgtest.Snapshot(t, db.Owner, "public")
	for _, tc := range []struct {
		name, chain, source string
		phase               int
		category            string
	}{
		{"state on hub3", "cosmoshub-3", "state", ledger.PhaseTx, "reward"},
		{"export source in tx", "cosmoshub-2", "export_payout", ledger.PhaseTx, "reward"},
		{"state in export", "cosmoshub-2", "state", ledger.PhaseExport, "reward"},
		{"message in export", "cosmoshub-2", "message", ledger.PhaseExport, "reward"},
		{"export transfer", "cosmoshub-2", "export_payout", ledger.PhaseExport, "transfer"},
		{"state begin", "cosmoshub-2", "state", ledger.PhaseBegin, "reward"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := sample(tc.chain, 9)
			b.Rows = []ledger.Row{{Phase: tc.phase, Category: tc.category, Amount: "1", Denom: "uatom", Source: tc.source, Validator: "validator", To: "owner", Trigger: "zero_height_export"}}
			_, e := ledger.Write(ctx, conn, []ledger.Block{b})
			require.Error(t, e)
		})
	}
	_, e := ledger.Write(ctx, conn, []ledger.Block{sample("cosmoshub-3", 9)})
	require.NoError(t, e)
	stored, e := ledger.Load(ctx, conn, "cosmoshub-3", []int64{9})
	require.NoError(t, e)
	require.Equal(t, sample("cosmoshub-3", 9).Rows, stored[0].Rows)
	// Existing Hub3 later-derived state remains unowned by this importer.
	require.NoError(t, db.Owner.Exec(`INSERT INTO legacy.taxable_events (chain_id,height,phase,tx_index,msg_index,sub_index,category,amount,denom,"timestamp",amount_source) VALUES ('cosmoshub-3',9,3,0,0,1,'unbonding_complete','5','uatom',now(),'state')`).Error)
	legacyBefore := pgtest.Snapshot(t, db.Owner, "legacy")
	count, e := ledger.Write(ctx, conn, []ledger.Block{sample("cosmoshub-3", 9)})
	require.NoError(t, e)
	require.Zero(t, count)
	stored, e = ledger.Load(ctx, conn, "cosmoshub-3", []int64{9})
	require.NoError(t, e)
	require.Equal(t, sample("cosmoshub-3", 9).Rows, stored[0].Rows)
	pgtest.RequireUntouched(t, legacyBefore, pgtest.Snapshot(t, db.Owner, "legacy"))
	pgtest.RequireUntouched(t, before, pgtest.Snapshot(t, db.Owner, "public"))
}
