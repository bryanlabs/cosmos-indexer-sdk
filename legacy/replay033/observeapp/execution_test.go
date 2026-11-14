package observeapp

import (
	"bytes"
	"encoding/json"
	sdk "github.com/cosmos/cosmos-sdk/types"
	abci "github.com/tendermint/tendermint/abci/types"
	"reflect"
	"testing"
)

func decodedNativeJSON(t *testing.T, b []byte) interface{} {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	var value interface{}
	if decoder.Decode(&value) != nil {
		t.Fatal("native typed JSON decode failed")
	}
	return value
}
func TestNativeABCITransactionSerializationKeepsPinnedJSONPBTypes(t *testing.T) {
	app, _, _, _ := fixture(t, false)
	app.observer.active = true
	response := abci.ResponseDeliverTx{GasWanted: 70000, GasUsed: 12345, Code: 2, Log: "synthetic-native-log"}
	app.execution("transaction", "native_abci", []byte{1, 2}, &response, response.Code)
	raw, e := json.Marshal(&response)
	must(t, e == nil, "pinned native jsonpb encoding")
	records := app.observer.pending
	must(t, len(records) == 1, "native response evidence")
	must(t, reflect.DeepEqual(decodedNativeJSON(t, records[0].After), decodedNativeJSON(t, raw)), "all native response fields and JSON scalar types preserved")
}
func TestNativeKeeperErrorsKeepIdentityAndEntireResult(t *testing.T) {
	app, _, key, _ := fixture(t, false)
	app.observer.active = true
	owner := sdk.AccAddress(key.PubKey().Address()).String()
	validator := sdk.ValAddress(key.PubKey().Address()).String()
	restore := app.cause("synthetic_native_keeper", validator, owner)
	defer restore()
	failure := sdk.ErrUnknownRequest("synthetic native keeper detail")
	app.keeperResult("end_block", "native_complete_unbonding", nil, failure)
	must(t, len(app.observer.pending) == 1, "native keeper error recorded")
	row := app.observer.pending[0]
	expected, e := json.Marshal(failure.Result())
	must(t, e == nil, "native error result encoding")
	must(t, row.Validator == validator && row.Delegator == owner && row.Code == uint32(failure.Result().Code) && reflect.DeepEqual(decodedNativeJSON(t, row.After), decodedNativeJSON(t, expected)), "native keeper code, identity and full result retained")
}
