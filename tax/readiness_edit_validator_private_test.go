package tax

import (
	"encoding/json"
	"errors"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
	staking "github.com/cosmos/cosmos-sdk/x/staking/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadinessActualStoredAuthzEditValidatorPrivate(t *testing.T) {
	root := os.Getenv("READINESS_CAUSE_ROOT")
	if root == "" {
		t.Skip("retained private input required")
	}
	b, err := os.ReadFile(filepath.Join(root, "exact-message-response.private.json"))
	if err != nil {
		t.Fatal("retained source unavailable")
	}
	var f struct {
		MessageID    int64  `json:"message_id"`
		MessageBytes []byte `json:"message_bytes"`
		ParserError  string `json:"parser_error"`
		Events       []struct {
			Type       string                        `json:"type"`
			Attributes []struct{ Key, Value string } `json:"attributes"`
		} `json:"events"`
	}
	if json.Unmarshal(b, &f) != nil || f.MessageID != 27907375 {
		t.Fatal("wrong retained source")
	}
	var msg authz.MsgExec
	if msg.Unmarshal(f.MessageBytes) != nil {
		t.Fatal("stored outer decode failed")
	}
	if len(msg.Msgs) != 1 || msg.Msgs[0].TypeUrl != "/cosmos.staking.v1beta1.MsgEditValidator" {
		t.Fatal("unexpected inner structure")
	}
	log := &txlog.LogMessage{}
	for _, e := range f.Events {
		v := txlog.LogMessageEvent{Type: e.Type}
		for _, a := range e.Attributes {
			v.Attributes = append(v.Attributes, txlog.Attribute{Key: a.Key, Value: a.Value})
		}
		log.Events = append(log.Events, v)
	}
	if authzMovedCoins(log) {
		t.Fatal("actual source contains economic events")
	}
	data, err := (&Parser{}).ParseMessage(&msg, log, cfgForTests())
	if os.Getenv("READINESS_EXPECT_BASELINE") == "1" {
		if err == nil || err.Error() != f.ParserError {
			t.Fatal("actual source did not reproduce exact persisted parser error")
		}
		return
	}
	if err != nil {
		t.Fatal("actual retained edit-validator remains refused")
	}
	if data == nil || len((*data).([]TaxableEvent)) != 0 {
		t.Fatal("actual metadata-only message fabricated financial rows")
	}
}

func TestReadinessAuthzEditValidatorPrivateSafety(t *testing.T) {
	edit := func() *codectypes.Any { return safetyUncached(t, &staking.MsgEditValidator{ValidatorAddress: val}) }
	parse := func(msgs []*codectypes.Any, log *txlog.LogMessage) ([]TaxableEvent, error) {
		d, e := (&Parser{}).ParseMessage(&authz.MsgExec{Grantee: other, Msgs: msgs}, log, cfgForTests())
		if d == nil {
			return nil, e
		}
		return (*d).([]TaxableEvent), e
	}
	t.Run("non_economic", func(t *testing.T) {
		rows, e := parse([]*codectypes.Any{edit()}, &txlog.LogMessage{})
		if e != nil || len(rows) != 0 {
			t.Fatal("no-op not recognized")
		}
	})
	for _, kind := range []string{"coin_received", "coin_spent", "transfer", "burn", "coinbase"} {
		t.Run(kind, func(t *testing.T) {
			l := &txlog.LogMessage{Events: []txlog.LogMessageEvent{ev(kind, [2]string{attributeAuthzMsgIndex, "0"})}}
			_, e := parse([]*codectypes.Any{edit()}, l)
			var incomplete *ClassificationIncompleteError
			if !errors.As(e, &incomplete) {
				t.Fatal("coin event incorrectly waived")
			}
		})
	}
	t.Run("unknown_sibling", func(t *testing.T) {
		_, e := parse([]*codectypes.Any{edit(), {TypeUrl: "/private.unknown.Msg", Value: []byte{1}}}, &txlog.LogMessage{})
		if e == nil {
			t.Fatal("unknown sibling waived")
		}
	})
	t.Run("malformed_edit", func(t *testing.T) {
		_, e := parse([]*codectypes.Any{{TypeUrl: "/cosmos.staking.v1beta1.MsgEditValidator", Value: []byte{0xff}}}, &txlog.LogMessage{})
		if e == nil {
			t.Fatal("malformed edit waived")
		}
	})
	t.Run("economic_sibling", func(t *testing.T) {
		send := safetyUncached(t, &bank.MsgSend{FromAddress: del, ToAddress: other, Amount: sdk.NewCoins(sdk.NewInt64Coin(fixtureNativeDenom, 5))})
		rows, e := parse([]*codectypes.Any{edit(), send}, &txlog.LogMessage{})
		if e != nil || len(rows) != 1 || rows[0].FromAddr != del || rows[0].ToAddr != other || rows[0].Amount != "5" {
			t.Fatal("economic sibling lost")
		}
	})
	t.Run("metadata_does_not_make_unknown_supported", func(t *testing.T) {
		_, e := parse([]*codectypes.Any{{TypeUrl: "/private.unknown.Msg"}}, &txlog.LogMessage{})
		if e == nil || !strings.Contains(e.Error(), "unknown inner") {
			t.Fatal("unknown type fabricated acceptance")
		}
	})
}
