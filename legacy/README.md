# Offline Hub1-3 Amino intake candidate

This is new candidate code based on documented historical source commit
`4d51ab7e9982fa95199d76cddf08eed198186677`. The retained build log corroborates
the `91ee97c4...` image digest, but clean build-context provenance is unproven.
This candidate is not claimed equivalent to that image.

`Decode(chainID, raw)` reads the original length-prefixed Amino StdTx, retaining
transaction bytes, original type name/prefix, every field, per-message bytes,
fee bytes, signature/public-key bytes, ordered message signers, and memo bytes.
It never decodes Amino as protobuf or invents a protobuf transaction. Unknown
message prefixes and unsupported message schemas remain explicit opaque
candidates with issues. Unsupported fee fields and malformed envelopes error.

The known message projections cover send/multisend, delegation, coin-valued
undelegation/redelegation, reward/commission withdrawal, withdraw-address
changes, deposit, and unjail. Projections are separate SDK semantic objects,
not the original representation. Share-valued old staking variants and other
unmodeled types stay unsupported, not classified as no-effect. No projection
is sent through the serving tax parser yet. Threshold multisig public-key
addresses refer to the aggregate account; members are not extra signers.
Public-key address extraction is not cryptographic signature verification.

`core.DecodeLegacyBlock` adapts matching block/results into offline candidates
keyed by chain/height/transaction index, not hash. Original result JSON and
ordered byte tags coexist with normalized events. Tendermint 0.30/0.31
PascalCase responses omit zero-valued code (`omitempty`); the evidence records
that serialization rule. Tendermint 0.32 snake_case responses require explicit
code. An entirely empty old result is insufficient execution evidence and is
rejected. Unknown nested result keys, duplicate keys, malformed/null codes, and
mixed dialects fail closed. Old validator-update `{type,data}` public keys are
explicitly projected into the modern oneof, never decoded by silently dropping
those old fields. Tags never masquerade as modern economic events.

Declared fees are not measured balance debits. A successful DeliverTx commits
ante fees; a failure code alone cannot distinguish ante failure from message
failure. Failed transaction fee commitment stays unknown until era-specific
ante/state evidence resolves it. Missing old fee events do not mean zero fees.

Serving configuration, transaction/search paths, and block-event paths remain
blocked, including empty legacy blocks, until all of:

- Inclusion-scoped database identity, including repeat/cross-chain hashes.
- Per-hub genesis balances, delegations and vesting imports.
- Terminal-to-next-genesis per-address reconciliation.
- Failed-fee commitment and era-specific economic event classification.
- Historical daily balances and report routing.

No serving schema migration is included. Existing protected 76 inclusion
issues, 29 skips, and 21 wallet comparisons are not touched. This decoder is
not completion of OPS-814; its checkboxes remain pending.

The enqueue opt-in is `base.terminal-archive-height`. It must match end-block
and the real RPC head. Default/follow behavior keeps the head safety margin.
It does not turn on legacy persistence or use a fabricated latest+1 height.

## Tests

All provider access is disabled for these tests. Private payloads stay outside
the repository. Synthetic Amino regression transactions use go-amino itself,
not protobuf; sanitized historical result fixtures live in rpc/testdata.

```
GOPROXY=off GOMAXPROCS=2 go test -p 1 ./legacy ./rpc ./config
LEGACY_PRIVATE_FIXTURE_DIR=/path/to/local/fixtures \
LEGACY_RETAINED_CACHE_DIR=/path/to/retained/captures \
GOPROXY=off GOMAXPROCS=2 go test -p 1 ./core -run 'TestLegacy' -count=1
```

The optional original tests print only schema/coverage metadata. No private
fixtures may be committed or attached to review output.
