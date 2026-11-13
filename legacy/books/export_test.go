package books

import "math/big"

// NewTestChange builds a genesis change for tests. Outside tests only
// LoadProp29 makes one.
func NewTestChange(name string, pool int64, nextGenesisSHA256 string, recoveries ...Recovery) *GenesisChange {
	return &GenesisChange{Chain: Prop29Chain, Name: name, Repo: "example.org/repo", Tag: "v1", Path: "file.go", SHA256: "0c0c",
		PoolStateSHA256: "0d0d", NextGenesisSHA256: nextGenesisSHA256, recoveries: recoveries, pool: big.NewInt(pool)}
}

// Recoveries lists the entries of a change for tests.
func (c *GenesisChange) Recoveries() []Recovery { return c.recoveries }
