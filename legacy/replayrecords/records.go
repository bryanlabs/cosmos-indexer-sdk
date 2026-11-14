// Package replayrecords admits private observe-only replay evidence.
package replayrecords

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
)

const Release034 = "cosmos-sdk v0.34.10 (gaia), tendermint v0.31.11, iavl v0.12.1"

type Record struct {
	Validator string `json:"validator,omitempty"`
	Delegator string `json:"delegator,omitempty"`
	Kind      string `json:"kind"`
	Code      uint32 `json:"code"`
	Height    int64  `json:"height"`
	Tx        int    `json:"tx"`
	Message   int    `json:"message"`
	Phase     string `json:"phase"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	Address   string `json:"address"`
	Denom     string `json:"denom"`
	Amount    string `json:"amount"`
}

// Proof binds exact record bytes to the pinned release and the next-header
// verified range. A final own Commit is never a state hash proof.
type Proof struct {
	Chain                      string               `json:"chain"`
	Release                    string               `json:"release"`
	From                       int64                `json:"from"`
	Through                    int64                `json:"through"`
	VerifiedThrough            int64                `json:"state_hash_verified_through"`
	SHA256                     string               `json:"records_sha256"`
	Export                     bool                 `json:"zero_height_export,omitempty"`
	ResultDifferencesSHA256    string               `json:"result_differences_sha256,omitempty"`
	ResultDifferencesAdmission *DifferenceAdmission `json:"result_differences_admission,omitempty"`
	GasDiagnostic              *GasAdmission        `json:"gas_diagnostic,omitempty"`
	FinancialRecording         *GasRecording        `json:"financial_recording,omitempty"`
}
type Height struct {
	Number  int64
	Records []Record
}

var invalid = fmt.Errorf("replay evidence incomplete or inconsistent")

// Stream first validates the entire segment and digest without retaining heights.
// Only then does it revisit one height at a time. The caller must retain immutable
// input evidence for both passes. Failed/aborted operations remain in Records.
func Stream(r io.ReadSeeker, p Proof, visit func(Height) error) error {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := scan(r, p, nil); err != nil {
		return err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return scan(r, p, visit)
}

// Read is for small fixtures. Full-chain callers must use Stream.
func Read(r io.Reader, p Proof) ([]Height, error) {
	var out []Height
	err := scan(r, p, func(h Height) error { out = append(out, h); return nil })
	if err != nil {
		return nil, err
	}
	return out, nil
}
func scan(r io.Reader, p Proof, visit func(Height) error) error {
	if p.Chain != "cosmoshub-2" || p.Release != Release034 || p.From < 1 || p.Through < p.From || p.VerifiedThrough < p.Through || len(p.SHA256) != 64 || (p.Export && p.From != p.Through) {
		return invalid
	}
	if _, e := hex.DecodeString(p.SHA256); e != nil {
		return invalid
	}
	hash := sha256.New()
	br := bufio.NewReader(io.TeeReader(r, hash))
	current := Height{Number: p.From}
	begin, end := 0, 0
	for {
		// ReadSlice bounds memory even for a malicious unterminated line.
		var line []byte
		for {
			part, e := br.ReadSlice('\n')
			if len(line)+len(part) > 1<<20 {
				return invalid
			}
			line = append(line, part...)
			if e == bufio.ErrBufferFull {
				continue
			}
			if e == io.EOF && len(line) == 0 {
				break
			}
			if e != nil {
				return invalid
			}
			break
		}
		if len(line) == 0 {
			break
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		var x Record
		if dec.Decode(&x) != nil {
			return invalid
		}
		var extra interface{}
		if dec.Decode(&extra) != io.EOF {
			return invalid
		}
		if x.Height != current.Number || x.Height > p.Through || (x.Status != "committed" && x.Status != "aborted" && !(p.Export && x.Status == "exported")) {
			return invalid
		}
		if p.Export && (x.Phase != "zero_height_export" || x.Status != "exported" || x.Tx != -1 || x.Message != -1) {
			return invalid
		}
		switch x.Kind {
		case "movement", "principal":
			amount, ok := new(big.Int).SetString(x.Amount, 10)
			zeroAnteFee := !p.Export && x.Kind == "movement" && x.Phase == "ante" && x.Label == "ante_fee" && x.Amount == "0"
			if !ok || (amount.Sign() == 0 && x.Kind != "principal" && !zeroAnteFee) || amount.String() != x.Amount || x.Address == "" || (x.Denom == "" && !zeroAnteFee) || x.Code != 0 {
				return invalid
			}
			if x.Kind == "principal" && (p.Export || x.Phase != "message" || amount.Sign() < 0 || x.Validator == "" || x.Delegator == "") {
				return invalid
			}
			if !p.Export && x.Phase != "ante" && x.Phase != "message" && x.Phase != "begin_block" && x.Phase != "end_block" {
				return invalid
			}
		case "accounting":
			if x.Address != "" || x.Amount != "" || x.Denom != "" {
				return invalid
			}
			if x.Phase == "begin_block" && x.Status == "committed" {
				begin++
			}
			if x.Phase == "end_block" && x.Status == "committed" {
				end++
			}
			if p.Export {
				begin++
				break
			}
			if x.Phase != "begin_block" && x.Phase != "end_block" && x.Phase != "message" && x.Phase != "ante" && x.Phase != "transaction" {
				return invalid
			}
		case "height_completed":
			if p.Export || x.Status != "committed" || x.Phase != "block" || x.Tx != -1 || x.Message != -1 || x.Code != 0 || begin != 1 || end != 1 {
				return invalid
			}
		case "export_completed":
			if !p.Export || x.Code != 0 || begin < 1 {
				return invalid
			}
		default:
			return invalid
		}
		// Position checks apply even to aborted evidence.
		if !p.Export {
			switch x.Phase {
			case "ante", "transaction":
				if x.Tx < 0 || x.Message != -1 {
					return invalid
				}
			case "message":
				if x.Tx < 0 || x.Message < 0 {
					return invalid
				}
			default:
				if x.Tx != -1 || x.Message != -1 {
					return invalid
				}
			}
		}
		current.Records = append(current.Records, x)
		if x.Kind == "height_completed" || x.Kind == "export_completed" {
			if visit != nil {
				if err := visit(current); err != nil {
					return err
				}
			}
			current = Height{Number: current.Number + 1}
			begin, end = 0, 0
		}
	}
	if len(current.Records) != 0 || current.Number != p.Through+1 || hex.EncodeToString(hash.Sum(nil)) != p.SHA256 {
		return invalid
	}
	return nil
}
