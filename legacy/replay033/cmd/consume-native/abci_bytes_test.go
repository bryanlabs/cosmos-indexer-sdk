package main

import "testing"

// Exercise each protocol byte location in both directions, including keys.
// Each accepts only empty/absent equivalence, never an actual nonempty change.
func TestABCIProtocolByteLocations(t *testing.T) {
	locations := map[string]func(*history) *[]byte{
		"begin.key":            func(h *history) *[]byte { return &h.Begin.Tags[0].Key },
		"begin.value":          func(h *history) *[]byte { return &h.Begin.Tags[0].Value },
		"deliver.data":         func(h *history) *[]byte { return &h.Deliver[0].Data },
		"deliver.tag.key":      func(h *history) *[]byte { return &h.Deliver[0].Tags[0].Key },
		"deliver.tag.value":    func(h *history) *[]byte { return &h.Deliver[0].Tags[0].Value },
		"end.tag.key":          func(h *history) *[]byte { return &h.End.Tags[0].Key },
		"end.tag.value":        func(h *history) *[]byte { return &h.End.Tags[0].Value },
		"end.validator.pubkey": func(h *history) *[]byte { return &h.End.ValidatorUpdates[0].PubKey.Data },
	}
	for name, field := range locations {
		t.Run(name, func(t *testing.T) {
			a, b := copyABCI(syntheticABCI()), copyABCI(syntheticABCI())
			*field(&a), *field(&b) = nil, []byte{}
			if !sameHistoryABCI(a, b) || !sameHistoryABCI(b, a) {
				t.Fatal("protocol empty bytes differ")
			}
			*field(&b) = []byte{99}
			if sameHistoryABCI(a, b) || sameHistoryABCI(b, a) {
				t.Fatal("nil/nonempty bytes accepted")
			}
			*field(&a) = []byte{}
			if sameHistoryABCI(a, b) || sameHistoryABCI(b, a) {
				t.Fatal("empty/nonempty bytes accepted")
			}
		})
	}
}

func TestABCIUnknownFieldsCompared(t *testing.T) {
	locations := map[string]func(*history) *[]byte{
		"begin":                func(h *history) *[]byte { return &h.Begin.XXX_unrecognized },
		"begin.tag":            func(h *history) *[]byte { return &h.Begin.Tags[0].XXX_unrecognized },
		"deliver":              func(h *history) *[]byte { return &h.Deliver[0].XXX_unrecognized },
		"deliver.tag":          func(h *history) *[]byte { return &h.Deliver[0].Tags[0].XXX_unrecognized },
		"end":                  func(h *history) *[]byte { return &h.End.XXX_unrecognized },
		"end.tag":              func(h *history) *[]byte { return &h.End.Tags[0].XXX_unrecognized },
		"end.validator":        func(h *history) *[]byte { return &h.End.ValidatorUpdates[0].XXX_unrecognized },
		"end.pubkey":           func(h *history) *[]byte { return &h.End.ValidatorUpdates[0].PubKey.XXX_unrecognized },
		"end.consensus":        func(h *history) *[]byte { return &h.End.ConsensusParamUpdates.XXX_unrecognized },
		"end.block_size":       func(h *history) *[]byte { return &h.End.ConsensusParamUpdates.BlockSize.XXX_unrecognized },
		"end.evidence":         func(h *history) *[]byte { return &h.End.ConsensusParamUpdates.Evidence.XXX_unrecognized },
		"end.validator_params": func(h *history) *[]byte { return &h.End.ConsensusParamUpdates.Validator.XXX_unrecognized },
	}
	for name, field := range locations {
		t.Run(name, func(t *testing.T) {
			a, b := copyABCI(syntheticABCI()), copyABCI(syntheticABCI())
			*field(&b) = []byte{0x78, 1}
			if sameHistoryABCI(a, b) || sameHistoryABCI(b, a) {
				t.Fatal("unknown wire field change accepted")
			}
		})
	}
}
