package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/types"
)

// Gas details are diagnostic evidence, never an alternate admission policy.
// The existing complete comparison, state and validator checks stay unchanged.
type gasDifferenceDetail struct {
	Height          int64  `json:"height"`
	TxIndex         int    `json:"tx_index"`
	TxSHA256        string `json:"tx_sha256"`
	NativeCode      uint32 `json:"native_code"`
	CachedCode      uint32 `json:"cached_code"`
	NativeGasWanted int64  `json:"native_gas_wanted"`
	CachedGasWanted int64  `json:"cached_gas_wanted"`
	NativeGasUsed   int64  `json:"native_gas_used"`
	CachedGasUsed   int64  `json:"cached_gas_used"`
}

func gasCanonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved, nil
	}
	parent := filepath.Dir(absolute)
	if parent == absolute {
		return absolute, nil
	}
	resolved, err := gasCanonicalPath(parent)
	return filepath.Join(resolved, filepath.Base(absolute)), err
}

func validateGasDestination(path, home, profile, movements string) error {
	gasPath, err := gasCanonicalPath(path)
	if err != nil {
		return fmt.Errorf("cannot resolve private gas evidence destination")
	}
	homePath, err := gasCanonicalPath(home)
	if err != nil || gasPath == homePath || strings.HasPrefix(gasPath, homePath+string(os.PathSeparator)) {
		return fmt.Errorf("gas evidence must be outside the replay home")
	}
	for _, other := range []string{profile, movements} {
		if other == "" {
			continue
		}
		resolved, err := gasCanonicalPath(other)
		if err != nil || resolved == gasPath {
			return fmt.Errorf("gas evidence must not share another output path")
		}
	}
	return nil
}

func writeGasDifferences(w io.Writer, height int64, txs types.Txs, cached *cachedResults, native []*abci.ResponseDeliverTx) error {
	if cached == nil || len(txs) != len(native) || len(cached.deliver) != len(native) {
		return fmt.Errorf("gas diagnostics require complete cached and native transaction results")
	}
	encoder := json.NewEncoder(w)
	for i, result := range native {
		other := cached.deliver[i]
		if result == nil || other == nil {
			return fmt.Errorf("gas diagnostics require non-nil transaction results")
		}
		if result.GasWanted == other.GasWanted && result.GasUsed == other.GasUsed {
			continue
		}
		detail := gasDifferenceDetail{
			Height: height, TxIndex: i, TxSHA256: hex.EncodeToString(txs[i].Hash()),
			NativeCode: result.Code, CachedCode: other.Code,
			NativeGasWanted: result.GasWanted, CachedGasWanted: other.GasWanted,
			NativeGasUsed: result.GasUsed, CachedGasUsed: other.GasUsed,
		}
		if encoder.Encode(detail) != nil {
			return fmt.Errorf("cannot write private gas diagnostic evidence")
		}
	}
	return nil
}
