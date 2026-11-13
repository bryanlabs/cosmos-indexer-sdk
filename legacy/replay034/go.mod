module github.com/DefiantLabs/cosmos-indexer/legacy/replay034

go 1.13

require (
	github.com/cosmos/cosmos-sdk v0.34.10
	github.com/syndtr/goleveldb v1.0.0
	github.com/tendermint/go-amino v0.14.1
	github.com/tendermint/tendermint v0.31.11
)

replace golang.org/x/crypto => github.com/tendermint/crypto v0.0.0-20180820045704-3764759f34a5
