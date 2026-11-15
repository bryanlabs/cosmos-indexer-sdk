package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	amino "github.com/tendermint/go-amino"
	abci "github.com/tendermint/tendermint/abci/types"
	cmn "github.com/tendermint/tendermint/libs/common"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
)

func syntheticABCI() history {
	tags := []cmn.KVPair{{Key: []byte{1}, Value: []byte{2}}, {Key: []byte{3}, Value: nil}}
	return history{
		Begin: abci.ResponseBeginBlock{Tags: tags},
		Deliver: []*abci.ResponseDeliverTx{
			{Code: 1, Data: []byte{4}, Log: "log", Info: "info", GasWanted: 5, GasUsed: 6, Tags: tags, Codespace: "space"},
			{Code: 2},
		},
		End: abci.ResponseEndBlock{
			Tags:             tags,
			ValidatorUpdates: []abci.ValidatorUpdate{{PubKey: abci.PubKey{Type: "type", Data: []byte{7}}, Power: 8}, {Power: 9}},
			ConsensusParamUpdates: &abci.ConsensusParams{
				BlockSize: &abci.BlockSizeParams{MaxBytes: 10, MaxGas: 11},
				Evidence:  &abci.EvidenceParams{MaxAge: 12},
				Validator: &abci.ValidatorParams{PubKeyTypes: []string{"one", "two"}},
			},
		},
	}
}

func copyABCI(h history) history {
	var out history
	data, err := json.Marshal(h)
	must(err)
	must(json.Unmarshal(data, &out))
	return out
}
func sameHistoryABCI(a, b history) bool {
	return sameABCIResponses(a.Begin, a.Deliver, a.End, &b.Begin, b.Deliver, &b.End)
}

func TestABCICompleteResponseNegativeControls(t *testing.T) {
	a := syntheticABCI()
	if !sameHistoryABCI(a, copyABCI(a)) {
		t.Fatal("identical response rejected")
	}
	controls := map[string]func(*history){
		"begin.tags.key":                     func(h *history) { h.Begin.Tags[0].Key = []byte{99} },
		"begin.tags.value":                   func(h *history) { h.Begin.Tags[0].Value = []byte{99} },
		"begin.tags.nil_nonempty":            func(h *history) { h.Begin.Tags[1].Value = []byte{99} },
		"begin.tags.count":                   func(h *history) { h.Begin.Tags = h.Begin.Tags[:1] },
		"begin.tags.order":                   func(h *history) { h.Begin.Tags[0], h.Begin.Tags[1] = h.Begin.Tags[1], h.Begin.Tags[0] },
		"deliver.count":                      func(h *history) { h.Deliver = h.Deliver[:1] },
		"deliver.order":                      func(h *history) { h.Deliver[0], h.Deliver[1] = h.Deliver[1], h.Deliver[0] },
		"deliver.nil_message":                func(h *history) { h.Deliver[0] = nil },
		"deliver.code":                       func(h *history) { h.Deliver[0].Code++ },
		"deliver.data":                       func(h *history) { h.Deliver[0].Data = []byte{99} },
		"deliver.data.nil_nonempty":          func(h *history) { h.Deliver[1].Data = []byte{99} },
		"deliver.log":                        func(h *history) { h.Deliver[0].Log += "x" },
		"deliver.info":                       func(h *history) { h.Deliver[0].Info += "x" },
		"deliver.gas_wanted":                 func(h *history) { h.Deliver[0].GasWanted++ },
		"deliver.gas_used":                   func(h *history) { h.Deliver[0].GasUsed++ },
		"deliver.codespace":                  func(h *history) { h.Deliver[0].Codespace += "x" },
		"deliver.tags.key":                   func(h *history) { h.Deliver[0].Tags[0].Key = []byte{99} },
		"deliver.tags.value":                 func(h *history) { h.Deliver[0].Tags[0].Value = []byte{99} },
		"deliver.tags.nil_nonempty":          func(h *history) { h.Deliver[0].Tags[1].Value = []byte{99} },
		"deliver.tags.order":                 func(h *history) { d := h.Deliver[0]; d.Tags[0], d.Tags[1] = d.Tags[1], d.Tags[0] },
		"deliver.tags.count":                 func(h *history) { h.Deliver[0].Tags = nil },
		"end.tags.key":                       func(h *history) { h.End.Tags[0].Key = []byte{99} },
		"end.tags.value":                     func(h *history) { h.End.Tags[0].Value = []byte{99} },
		"end.tags.order":                     func(h *history) { h.End.Tags[0], h.End.Tags[1] = h.End.Tags[1], h.End.Tags[0] },
		"end.tags.count":                     func(h *history) { h.End.Tags = nil },
		"end.validators.count":               func(h *history) { h.End.ValidatorUpdates = nil },
		"end.validators.order":               func(h *history) { v := h.End.ValidatorUpdates; v[0], v[1] = v[1], v[0] },
		"end.validators.power":               func(h *history) { h.End.ValidatorUpdates[0].Power++ },
		"end.validators.pubkey.type":         func(h *history) { h.End.ValidatorUpdates[0].PubKey.Type += "x" },
		"end.validators.pubkey.data":         func(h *history) { h.End.ValidatorUpdates[0].PubKey.Data = []byte{99} },
		"end.validators.pubkey.nil_nonempty": func(h *history) { h.End.ValidatorUpdates[1].PubKey.Data = []byte{99} },
		"end.consensus.presence":             func(h *history) { h.End.ConsensusParamUpdates = nil },
		"end.block_size.presence":            func(h *history) { h.End.ConsensusParamUpdates.BlockSize = nil },
		"end.block_size.max_bytes":           func(h *history) { h.End.ConsensusParamUpdates.BlockSize.MaxBytes++ },
		"end.block_size.max_gas":             func(h *history) { h.End.ConsensusParamUpdates.BlockSize.MaxGas++ },
		"end.evidence.presence":              func(h *history) { h.End.ConsensusParamUpdates.Evidence = nil },
		"end.evidence.max_age":               func(h *history) { h.End.ConsensusParamUpdates.Evidence.MaxAge++ },
		"end.validator.presence":             func(h *history) { h.End.ConsensusParamUpdates.Validator = nil },
		"end.validator.pubkey_types.value":   func(h *history) { h.End.ConsensusParamUpdates.Validator.PubKeyTypes[0] = "changed" },
		"end.validator.pubkey_types.count":   func(h *history) { h.End.ConsensusParamUpdates.Validator.PubKeyTypes = nil },
		"end.validator.pubkey_types.order":   func(h *history) { v := h.End.ConsensusParamUpdates.Validator.PubKeyTypes; v[0], v[1] = v[1], v[0] },
		"unknown_wire_bytes":                 func(h *history) { h.Deliver[0].XXX_unrecognized = []byte{0x78, 1} },
	}
	for name, change := range controls {
		t.Run(name, func(t *testing.T) {
			b := copyABCI(a)
			change(&b)
			if sameHistoryABCI(a, b) || sameHistoryABCI(b, a) {
				t.Fatal("changed response accepted")
			}
		})
	}
	if sameABCIResponses(a.Begin, a.Deliver, a.End, nil, a.Deliver, &a.End) || sameABCIResponses(a.Begin, a.Deliver, a.End, &a.Begin, a.Deliver, nil) {
		t.Fatal("missing response accepted")
	}
}

// Make protocol upgrades require a deliberate coverage update rather than
// silently leaving new response fields without negative controls.
func TestABCIProtocolFieldCoverage(t *testing.T) {
	for _, schema := range []struct {
		value  interface{}
		fields string
	}{
		{abci.ResponseBeginBlock{}, "Tags"},
		{abci.ResponseDeliverTx{}, "Code,Data,Log,Info,GasWanted,GasUsed,Tags,Codespace"},
		{abci.ResponseEndBlock{}, "ValidatorUpdates,ConsensusParamUpdates,Tags"},
		{cmn.KVPair{}, "Key,Value"},
		{abci.ValidatorUpdate{}, "PubKey,Power"},
		{abci.PubKey{}, "Type,Data"},
		{abci.ConsensusParams{}, "BlockSize,Evidence,Validator"},
		{abci.BlockSizeParams{}, "MaxBytes,MaxGas"},
		{abci.EvidenceParams{}, "MaxAge"},
		{abci.ValidatorParams{}, "PubKeyTypes"},
	} {
		typ := reflect.TypeOf(schema.value)
		var got []string
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if !strings.HasPrefix(f.Name, "XXX_") {
				got = append(got, f.Name)
			}
		}
		if strings.Join(got, ",") != schema.fields {
			t.Fatalf("protocol field coverage changed: %s", typ.Name())
		}
	}
}

func TestABCIEmptyProtocolBytesOnly(t *testing.T) {
	a := syntheticABCI()
	b := copyABCI(a)
	b.Begin.Tags[1].Value = []byte{}
	b.Deliver[0].Tags[1].Value = []byte{}
	b.Deliver[1].Data = []byte{}
	b.End.Tags[1].Value = []byte{}
	b.End.ValidatorUpdates[1].PubKey.Data = []byte{}
	if !sameHistoryABCI(a, b) || !sameHistoryABCI(b, a) {
		t.Fatal("equivalent protocol empty bytes rejected")
	}
	if equal(a.Begin.Tags[1].Value, b.Begin.Tags[1].Value) {
		t.Fatal("generic JSON equality weakened")
	}
	if equal(raw([]byte(nil)), raw([]byte{})) {
		t.Fatal("before-image absent/empty distinction lost")
	}
}

func scanRetainedHeight(scanner *bufio.Scanner, target int64) (history, error) {
	for scanner.Scan() {
		// Decode each row into a fresh value: omitted proto3 zero fields must
		// never inherit data or reused message pointers from an earlier height.
		var row history
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			return history{}, errors.New("retained history decode failed")
		}
		if row.Height >= target {
			if row.Height == target && row.Kind == "height" {
				return row, nil
			}
			return history{}, errors.New("retained height identity differs")
		}
	}
	return history{}, errors.New("retained height not found within bounded read")
}

func TestABCIHistoryDecodeDoesNotReuseOmittedFields(t *testing.T) {
	rows := `{"kind":"height","height":1235,"begin":{"tags":[{"key":"AQ=="}]},"deliver":[{"code":7,"data":"AQ=="}],"end":{"consensus_param_updates":{"block_size":{"max_bytes":1}}}}
{"kind":"height","height":1236,"begin":{},"deliver":[{}],"end":{}}
`
	h, err := scanRetainedHeight(bufio.NewScanner(strings.NewReader(rows)), 1236)
	if err != nil || !sameHistoryABCI(h, history{Deliver: []*abci.ResponseDeliverTx{{}}}) {
		t.Fatal("retained height inherited earlier fields")
	}
	_, err = scanRetainedHeight(bufio.NewScanner(strings.NewReader(`{"kind":"history_completed","height":1236}`)), 1236)
	if err == nil {
		t.Fatal("completion marker accepted as height response")
	}
}

// Explicitly opt in to the task-authorized bounded read. This does not open a
// database or run native historical/financial execution. Diagnostics are shapes
// and counts only. Stop immediately after decoding retained height 1236.
func TestABCIRetained1236(t *testing.T) {
	root := os.Getenv("HUB1_ABCI_RETAINED_SOURCE")
	if root == "" {
		t.Skip("bounded retained-source check not requested")
	}
	f, err := os.Open(filepath.Join(root, "native-history.source-closed.gz"))
	if err != nil {
		t.Fatal("retained history open failed")
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal("retained history gzip header failed")
	}
	defer z.Close()
	const budget = 8 << 20
	bounded := &io.LimitedReader{R: z, N: budget}
	scanner := bufio.NewScanner(bounded)
	scanner.Buffer(make([]byte, 65536), 1<<20)
	h, err := scanRetainedHeight(scanner, 1236)
	if scanner.Err() != nil || err != nil {
		t.Fatal("retained target not found within bounded read")
	}
	cache := filepath.Join(root, "cache")
	c := amino.NewCodec()
	ctypes.RegisterAmino(c)
	var r ctypes.ResultBlockResults
	if c.UnmarshalJSON(rawCache(cache, 1236, "-block_results.raw.gz"), &r) != nil || r.Height != 1236 || r.Results == nil {
		t.Fatal("cached target decode or identity failed")
	}
	if equal(h.Deliver, r.Results.DeliverTx) && equal(h.Begin, r.Results.BeginBlock) && equal(h.End, r.Results.EndBlock) {
		t.Fatal("old JSON comparator unexpectedly accepted")
	}
	if !sameABCIResponses(h.Begin, h.Deliver, h.End, r.Results.BeginBlock, r.Results.DeliverTx, r.Results.EndBlock) {
		t.Fatal("complete semantic retained comparison failed")
	}
	t.Logf("height=1236 history_uncompressed_read=%d budget=%d old_json_accepts=false semantic_accepts=true financial_execution=false", budget-bounded.N, budget)
}
