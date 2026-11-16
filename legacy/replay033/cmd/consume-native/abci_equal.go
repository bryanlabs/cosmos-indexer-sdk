package main

import (
	"github.com/gogo/protobuf/proto"
	abci "github.com/tendermint/tendermint/abci/types"
)

// sameABCIResponses compares complete responses using the pinned protocol types,
// not their JSON representation. In Tendermint v0.31.0-dev0-fix0 the response
// data, tag keys/values and validator public-key data are proto3 bytes: nil and
// empty encode the same value. gogo/protobuf v1.1.1 Equal covers every declared
// field and unknown wire bytes, preserves repeated-field order and message
// presence, and ignores only generated bookkeeping. Do not use this comparison
// for store values or before-images, where absent and present-empty differ.
func sameABCIResponses(begin abci.ResponseBeginBlock, deliver []*abci.ResponseDeliverTx, end abci.ResponseEndBlock, wantBegin *abci.ResponseBeginBlock, wantDeliver []*abci.ResponseDeliverTx, wantEnd *abci.ResponseEndBlock) bool {
	if !proto.Equal(&begin, wantBegin) || !proto.Equal(&end, wantEnd) || len(deliver) != len(wantDeliver) {
		return false
	}
	for i := range deliver {
		if !proto.Equal(deliver[i], wantDeliver[i]) {
			return false
		}
	}
	return true
}
