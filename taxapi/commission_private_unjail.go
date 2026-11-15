package taxapi

// This extension is opt-in to a new private profile digest. The ordinary parser,
// public readiness gate, original errors, fees and financial rows are unchanged.
import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cosmos/cosmos-sdk/x/authz"
	slashing "github.com/cosmos/cosmos-sdk/x/slashing/types"
)

const privateUnjailSchema = "commission-readonly-authz-projection-v2"

type privateUnjailSource struct {
	MessageID, ErrorID, TxID, Height int64
	SourceSHA, TxSHA                 string
}

var privateUnjailSources = [...]privateUnjailSource{
	{28657858, 10404, 19312696, 22944818, "9df9dfa0ed6e7fd46e264cd0b8b99d1f4af569f34184645a90dc903db39a1746", "4335abc702a40fbf4b6048f8dc67f01ea8ecfba5c5a20be6204c119f8d700dbf"},
	{29441609, 10535, 19837518, 22958406, "44c81d620b072ab16c902439ce6819992f623250b1864c5aed43226116acfd71", "019f321866b97d168b717bbd408f156859b1cfb3439974d93b3940695596c0fc"},
}

func (p *CommissionPrivateProfile) validSchema() bool {
	switch p.Schema {
	case "commission-readonly-authz-projection-v1":
		return len(p.UnjailSourcesSHA256) == 0
	case privateUnjailSchema:
		return len(p.UnjailSourcesSHA256) == 2 && p.UnjailSourcesSHA256[0] == privateUnjailSources[0].SourceSHA && p.UnjailSourcesSHA256[1] == privateUnjailSources[1].SourceSHA
	}
	return false
}

func privateUnjailSourceSQL(source privateUnjailSource) string {
	return strings.Replace(privateProfileSourceSQL,
		"m.id=27907375 AND e.id=10262 AND t.id=18802568 AND b.height=27991521",
		fmt.Sprintf("m.id=%d AND e.id=%d AND t.id=%d AND b.height=%d", source.MessageID, source.ErrorID, source.TxID, source.Height), 1)
}

func validatePrivateUnjailSource(raw []byte, expected privateUnjailSource) error {
	if privateSHA(raw) != expected.SourceSHA {
		return fmt.Errorf("private unjail source seal mismatch")
	}
	var source struct {
		MessageBytes []byte `json:"message_bytes"`
		MessageType  string `json:"message_type"`
		Message      struct {
			ID   int64 `json:"id"`
			TxID int64 `json:"tx_id"`
		} `json:"message"`
		Error struct {
			ID    int64
			Error string
		} `json:"error"`
		Tx struct {
			ID   int64
			Hash string
			Code int64
		} `json:"tx"`
		Block       struct{ Height int64 }  `json:"block"`
		TaxRows     []json.RawMessage       `json:"tax_rows"`
		AllErrors   []json.RawMessage       `json:"all_errors"`
		AllMessages []json.RawMessage       `json:"all_messages"`
		Events      []struct{ Type string } `json:"events"`
	}
	if json.Unmarshal(raw, &source) != nil || source.MessageType != "/cosmos.authz.v1beta1.MsgExec" ||
		source.Message.ID != expected.MessageID || source.Message.TxID != expected.TxID || source.Error.ID != expected.ErrorID || source.Tx.ID != expected.TxID || source.Block.Height != expected.Height || source.Tx.Code != 0 ||
		privateSHA(source.MessageBytes) != "7658239812f1a2232dd5aa0ec0d37ff707c51a2c6910c72d33f96ec6b28b681c" ||
		privateSHA([]byte(source.Error.Error)) != "7175e4cf1c44d38320e528f13372aaad8e29d87d1c02f09a9b3b0e83e9fb1cea" || privateSHA([]byte(source.Tx.Hash)) != expected.TxSHA ||
		len(source.TaxRows) != 0 || len(source.AllErrors) != 1 || len(source.AllMessages) != 1 || len(source.Events) != 1 || source.Events[0].Type != "message" {
		return fmt.Errorf("private unjail original message/error/effects mismatch")
	}
	var outer authz.MsgExec
	if outer.Unmarshal(source.MessageBytes) != nil || len(outer.Msgs) != 1 || outer.Msgs[0] == nil || outer.Msgs[0].TypeUrl != "/cosmos.slashing.v1beta1.MsgUnjail" {
		return fmt.Errorf("private unjail requires exactly one reviewed inner message")
	}
	var inner slashing.MsgUnjail
	if inner.Unmarshal(outer.Msgs[0].Value) != nil || inner.ValidatorAddr == "" {
		return fmt.Errorf("private unjail decode failed")
	}
	// MsgUnjail changes validator jail status, never emits a monetary projection.
	// The exact sealed events and zero tax rows above independently bind that fact.
	return nil
}

func (p *CommissionPrivateProfile) privateParserScope() (string, int64) {
	if p.Schema == privateUnjailSchema && p.validSchema() {
		return "JOIN (SELECT DISTINCT m.tx_id FROM message_parser_errors mpe JOIN messages m ON m.id=mpe.message_id WHERE (mpe.id,m.id,m.tx_id) IN ((10262,27907375,18802568),(10404,28657858,19312696),(10535,29441609,19837518))) parser_scope ON parser_scope.tx_id=t.id", 3
	}
	return "JOIN (SELECT DISTINCT m.tx_id FROM message_parser_errors mpe JOIN messages m ON m.id=mpe.message_id WHERE mpe.id=10262 AND m.id=27907375 AND m.tx_id=18802568) parser_scope ON parser_scope.tx_id=t.id", 1
}
