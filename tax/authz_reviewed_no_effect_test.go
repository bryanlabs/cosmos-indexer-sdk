package tax

import (
	"strings"
	"testing"

	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	slashing "github.com/cosmos/cosmos-sdk/x/slashing/types"
)

// The governance authority on the Hub uploads and migrates 08-wasm light
// client code through an authz grant: an outer MsgExec by the grantee wraps a
// MsgExec whose inner message is the 08-wasm one. Five such mainnet execs
// (heights 25030377 to 28600694, four MsgStoreCode and one
// MsgMigrateContract) were parser errors until these types decoded.
func ibcWasmNestedExec(t *testing.T, url string, body []byte) *authz.MsgExec {
	t.Helper()
	inner := &codectypes.Any{TypeUrl: url, Value: body}
	nested := safetyUncached(t, &authz.MsgExec{Grantee: other, Msgs: []*codectypes.Any{inner}})
	return &authz.MsgExec{Grantee: other, Msgs: []*codectypes.Any{nested}}
}

func ibcWasmBodies() map[string][]byte {
	checksum := strings.Repeat("\x11", 32)
	return map[string][]byte{
		TypeURLIBCWasmStoreCode:       append(fieldBytes(1, del), fieldBytes(2, "\x00asm\x01\x00\x00\x00")...),
		TypeURLIBCWasmMigrateContract: append(append(append(fieldBytes(1, del), fieldBytes(2, "08-wasm-1369")...), fieldBytes(3, checksum)...), fieldBytes(4, "{}")...),
		TypeURLIBCWasmRemoveChecksum:  append(fieldBytes(1, del), fieldBytes(2, checksum)...),
	}
}

func TestIBCWasmMessagesDecode(t *testing.T) {
	for url, body := range ibcWasmBodies() {
		m, err := unpackAuthzInner(&codectypes.Any{TypeUrl: url, Value: body})
		if err != nil {
			t.Fatalf("%s: %v", url, err)
		}
		raw, err := m.(interface{ Marshal() ([]byte, error) }).Marshal()
		if err != nil || string(raw) != string(body) {
			t.Fatalf("%s did not keep its bytes", url)
		}
		if got := m.(interface{ XXX_MessageName() string }).XXX_MessageName(); "/"+got != url {
			t.Fatalf("%s names itself %s", url, got)
		}
	}
	mig, _ := unpackAuthzInner(&codectypes.Any{TypeUrl: TypeURLIBCWasmMigrateContract, Value: ibcWasmBodies()[TypeURLIBCWasmMigrateContract]})
	if c := mig.(*MsgIBCWasmMigrateContract); c.Signer != del || c.ClientID != "08-wasm-1369" {
		t.Fatalf("migrate fields: %+v", c.ibcWasmMsg)
	}
}

// The events these transactions emitted: message events and the upload or
// migration, nothing that moves coins.
func ibcWasmLog(kind string) *txlog.LogMessage {
	return &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("message", [2]string{"action", typeURLAuthzExec}),
		ev(kind, [2]string{"checksum", "11"}),
		ev("message", [2]string{attributeModule, "ibc_client"}),
	}}
}

func TestAuthzIBCWasmCodeOpsHaveNoTaxEffect(t *testing.T) {
	kinds := map[string]string{
		TypeURLIBCWasmStoreCode:       "store_wasm_code",
		TypeURLIBCWasmMigrateContract: "migrate_contract",
		TypeURLIBCWasmRemoveChecksum:  "message",
	}
	for url, body := range ibcWasmBodies() {
		msg := ibcWasmNestedExec(t, url, body)
		data, err := (&Parser{}).ParseMessage(msg, ibcWasmLog(kinds[url]), cfgForTests())
		if err != nil || len((*data).([]TaxableEvent)) != 0 {
			t.Fatalf("%s through nested authz was not a clean no-op: %v / %v", url, data, err)
		}
		moved := ibcWasmLog(kinds[url])
		moved.Events = append(moved.Events, ev(eventCoinReceived, [2]string{attributeReceiver, del}, [2]string{attributeAmount, "5" + fixtureNativeDenom}))
		if _, err := (&Parser{}).ParseMessage(msg, moved, cfgForTests()); err == nil || !strings.Contains(err.Error(), "unsupported inner "+url) {
			t.Fatalf("%s with coin movement must stay unresolved: %v", url, err)
		}
	}
}

// Mainnet heights 25138809 to 25294772: a grantee uploaded CosmWasm code
// through authz. The upload emits store_code and moves nothing.
func TestAuthzCosmWasmStoreCodeHasNoTaxEffect(t *testing.T) {
	store := safetyUncached(t, &wasmtypes.MsgStoreCode{Sender: del, WASMByteCode: []byte("\x00asm\x01\x00\x00\x00")})
	msg := &authz.MsgExec{Grantee: other, Msgs: []*codectypes.Any{store}}
	log := &txlog.LogMessage{Events: []txlog.LogMessageEvent{
		ev("message", [2]string{"action", typeURLAuthzExec}),
		ev("store_code", [2]string{"code_id", "1"}),
	}}
	data, err := (&Parser{}).ParseMessage(msg, log, cfgForTests())
	if err != nil || len((*data).([]TaxableEvent)) != 0 {
		t.Fatalf("delegated store code was not a clean no-op: %v / %v", data, err)
	}
	log.Events = append(log.Events, ev("coin_spent", [2]string{attributeSpender, del}, [2]string{attributeAmount, "5" + fixtureNativeDenom}))
	if _, err := (&Parser{}).ParseMessage(msg, log, cfgForTests()); err == nil || !strings.Contains(err.Error(), "unsupported inner /cosmwasm.wasm.v1.MsgStoreCode") {
		t.Fatalf("store code with coin movement must stay unresolved: %v", err)
	}
}

// The delegated path decides from the same reviewed list as the top level, so
// a type reviewed there (MsgUnjail, MsgOptIn, ...) cannot stay a parser error
// when a grantee sends it.
func TestAuthzNoEffectMatchesReviewedList(t *testing.T) {
	if len(authzNoTaxEffect) != len(NoTaxEffectTypeURLs) {
		t.Fatalf("authz list has %d types, reviewed list %d", len(authzNoTaxEffect), len(NoTaxEffectTypeURLs))
	}
	for _, url := range NoTaxEffectTypeURLs {
		if !authzNoTaxEffect[url] {
			t.Fatalf("%s is reviewed but not skipped under authz", url)
		}
	}
	unjail := safetyUncached(t, &slashing.MsgUnjail{ValidatorAddr: val})
	msg := &authz.MsgExec{Grantee: other, Msgs: []*codectypes.Any{unjail}}
	data, err := (&Parser{}).ParseMessage(msg, &txlog.LogMessage{}, cfgForTests())
	if err != nil || len((*data).([]TaxableEvent)) != 0 {
		t.Fatalf("delegated unjail was not a clean no-op: %v / %v", data, err)
	}
}
