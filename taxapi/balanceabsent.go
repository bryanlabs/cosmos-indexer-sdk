package taxapi

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// LoadAbsentProofs reads a file of operator|height lines, each a height at
// which two archives answered NotFound for the validator while serving the
// validator set, and checks it against its exact digest.
func LoadAbsentProofs(path, expected string) (map[AbsentAt]bool, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("absent proof file must be a regular non-symlink file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<20 || !correctionHash(expected) || privateSHA(b) != expected {
		return nil, fmt.Errorf("absent proof file digest mismatch")
	}
	out := map[AbsentAt]bool{}
	for _, line := range strings.Fields(string(b)) {
		op, hs, ok := strings.Cut(line, "|")
		h, err := strconv.ParseInt(hs, 10, 64)
		if !ok || op == "" || err != nil || h <= 0 {
			return nil, fmt.Errorf("absent proof file: bad line %q", hashLabel(line))
		}
		out[AbsentAt{op, h}] = true
	}
	return out, nil
}

// UseAbsentProofs lets the ledger walk count a delegation on a validator as
// zero at a proved height where the validator did not exist.
func (s *Server) UseAbsentProofs(absent map[AbsentAt]bool) { s.ledgerAbsent = absent }
