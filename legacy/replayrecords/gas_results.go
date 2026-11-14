package replayrecords

import (
	"encoding/hex"

	"github.com/cometbft/cometbft/crypto/merkle"
	amino "github.com/tendermint/go-amino"
)

// historicalGasResultsRoot is the v0.31.11 Code/Data-only projection, NOT a
// modern canonical result. Private all31 tests cross-check against the actual
// pinned-library roots calculated by the independent review. Pinned-library
// tests in replay034 additionally demonstrate the excluded fields explicitly.
func historicalGasResultsRoot(results []gasResponse) (string, error) {
	codec := amino.NewCodec()
	leaves := make([][]byte, len(results))
	for i, r := range results {
		if r.Code < 0 || r.Code > 1<<32-1 {
			return "", invalid
		}
		projected := struct {
			Code uint32
			Data []byte
		}{uint32(r.Code), r.Data}
		raw, e := codec.MarshalBinaryBare(projected)
		if e != nil {
			return "", invalid
		}
		leaves[i] = raw
	}
	return hex.EncodeToString(merkle.HashFromByteSlices(leaves)), nil
}
