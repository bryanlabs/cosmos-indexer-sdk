package observeapp

import (
	"bytes"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/libs/log"
)

func TestSparseAnteReturnedViewIdentity(t *testing.T) {
	for _, mode := range []string{"same", "zero", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			app, _, _, _ := fixture(t, false)
			app.SetSparseAccounting(true)
			var out bytes.Buffer
			app.SetRecorder(&out)
			begin(app, 1)
			ctx := app.NewContext(false, abci.Header{})
			child, _ := ctx.CacheContext()
			wrapped := app.observeAnte(func(in sdk.Context, _ sdk.Tx, _ bool) (sdk.Context, sdk.Result, bool) {
				switch mode {
				case "zero":
					return sdk.Context{}, sdk.Result{}, true
				case "foreign":
					return child, sdk.Result{}, true
				default:
					return in, sdk.Result{}, true
				}
			})
			wrapped(ctx, auth.StdTx{}, false)
			if mode != "foreign" {
				app.EndBlock(abci.RequestEndBlock{Height: 1})
			}
			app.Commit()
			must(t, (app.RecorderError() != nil) == (mode == "foreign"), "ante must keep its pinned account view")
			if mode == "foreign" {
				must(t, out.Len() == 0 && app.LastBlockHeight() == 0, "foreign ante view cannot commit")
			}
		})
	}
}

func TestSparseWriterFailuresRemainPrivateAndLatched(t *testing.T) {
	for _, short := range []bool{false, true} {
		app, _, _, _ := fixture(t, false)
		app.SetSparseAccounting(true)
		app.SetRecorder(badWriter{short: short})
		begin(app, 1)
		app.EndBlock(abci.RequestEndBlock{Height: 1})
		app.Commit()
		must(t, app.LastBlockHeight() == 1 && app.RecorderError() != nil, "output failure follows commit and latches")
		must(t, !bytes.Contains([]byte(app.RecorderError().Error()), []byte("sensitive")), "sparse output error is private")
		before := app.LastCommitID()
		app.SetRecorder(nil)
		app.SetSparseAccounting(false)
		app.BeginBlock(abci.RequestBeginBlock{Header: abci.Header{Height: 2}})
		app.Commit()
		must(t, app.LastCommitID().Version == before.Version && bytes.Equal(app.LastCommitID().Hash, before.Hash), "latched error cannot be disabled to continue")
	}
}

func TestSparseZeroPayoutExportAndOutputFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		app, db, _, _ := fixture(t, false)
		begin(app, 1)
		app.EndBlock(abci.RequestEndBlock{Height: 1})
		app.Commit()
		exported := NewGaiaApp(log.NewNopLogger(), db, nil, true, 0)
		exported.SetSparseAccounting(true)
		var out bytes.Buffer
		if fail {
			exported.SetRecorder(badWriter{})
		} else {
			exported.SetRecorder(&out)
		}
		before := exported.LastCommitID()
		raw, _, err := exported.ExportObserved()
		must(t, exported.LastCommitID().Version == before.Version && bytes.Equal(exported.LastCommitID().Hash, before.Hash), "zero export never commits")
		if fail {
			must(t, err != nil && raw == nil && exported.RecorderError() != nil, "sparse export output failure returns no artifact")
			continue
		}
		must(t, err == nil && len(raw) > 0, "sparse zero payout export succeeds")
		completed := false
		for _, r := range rows(t, &out) {
			must(t, r.Kind != "movement", "zero payout export invents no movement")
			completed = completed || r.Kind == "export_completed"
		}
		must(t, completed, "sparse zero payout export completion")
	}
}
