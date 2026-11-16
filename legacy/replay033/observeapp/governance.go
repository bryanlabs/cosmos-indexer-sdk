package observeapp

import (
	"bytes"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/gov"
)

// Queue order is exactly the two pinned EndBlocker iterators followed by each
// keeper's deposit iterator. It supplies identities absent from escrow burns.
func (app *GaiaApp) governanceDeposits(ctx sdk.Context) []gov.Deposit {
	ctx = quiet(ctx)
	var out []gov.Deposit
	for _, it := range []sdk.Iterator{app.govKeeper.InactiveProposalQueueIterator(ctx, ctx.BlockHeader().Time), app.govKeeper.ActiveProposalQueueIterator(ctx, ctx.BlockHeader().Time)} {
		for ; it.Valid(); it.Next() {
			var id uint64
			app.cdc.MustUnmarshalBinaryLengthPrefixed(it.Value(), &id)
			deposits := app.govKeeper.GetDeposits(ctx, id)
			for ; deposits.Valid(); deposits.Next() {
				var d gov.Deposit
				app.cdc.MustUnmarshalBinaryLengthPrefixed(deposits.Value(), &d)
				out = append(out, d)
			}
			deposits.Close()
		}
		it.Close()
	}
	return out
}
func (app *GaiaApp) governanceSend(a, b sdk.AccAddress, c sdk.Coins) func() {
	o := &app.observer
	if !o.active || o.phase != "end_block" {
		return func() {}
	}
	if !bytes.Equal(a, gov.DepositedCoinsAccAddr) || len(o.deposits) == 0 {
		app.fail()
		return func() {}
	}
	d := o.deposits[0]
	o.deposits = o.deposits[1:]
	label := "deposit_refund"
	if bytes.Equal(b, gov.BurnedDepositCoinsAccAddr) {
		label = "deposit_burn"
	} else if !bytes.Equal(b, d.Depositor) {
		app.fail()
	}
	if !c.IsEqual(d.Amount) {
		app.fail()
	}
	return app.cause(label, "", d.Depositor.String())
}
