package books

import (
	"math/big"
	"testing"
)

// NewTestChange builds a genesis change for tests. Outside tests only
// LoadProp29 makes one.
func NewTestChange(name string, pool int64, nextGenesisSHA256 string, recoveries ...Recovery) *GenesisChange {
	return &GenesisChange{chain: Prop29Chain, name: name, repo: "example.org/repo", tag: "v1", path: "file.go", sha256: "0c0c",
		poolState: "0d0d", nextGenesis: nextGenesisSHA256, recoveries: recoveries, pool: big.NewInt(pool)}
}

// Recoveries lists the entries of a change for tests.
func (c *GenesisChange) Recoveries() []Recovery { return c.recoveries }

// DepositAccounts names the two governance deposit accounts of a chain.
func DepositAccounts(t *testing.T, chain string) (string, string) {
	held, burned, ok := depositAccounts(chain)
	if !ok {
		t.Fatal("the chain kept deposits in a module account")
	}
	return held, burned
}
