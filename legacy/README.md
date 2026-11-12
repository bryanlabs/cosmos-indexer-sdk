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

Every message name seen in the cache has a projection: send, multisend,
delegate, undelegate, redelegate, reward and commission withdrawal,
withdraw-address change, deposit, vote, submit proposal (both field layouts),
create validator, edit validator, unjail and verify invariant. Projections are
separate SDK semantic objects, not the original representation. One case is
deliberately left without one: Hub 1 undelegations and redelegations name an
amount of validator shares, not coins, so the signer is exact and the amount
waits for the validator exchange rate. Proposal content other than text and
community pool spend is kept raw. Threshold multisig public-key addresses
refer to the aggregate account; members are not extra signers. Public-key
address extraction is not cryptographic signature verification.

`legacy/evidence` attributes what the chain recorded to the message that
produced it: the flat tag list of Hub 1 and 2 is split at the action tag each
message wrote, and Hub 3 is read from the per message log. Anything that does
not line up with the decoded messages is refused. `TransferSenders` writes the
payer onto Hub 3 transfers, which that era recorded separately, so the serving
tax classifier can read reward payouts and commission. No projection is sent
through the serving tax parser outside the census yet.

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

Declared fees are not measured balance debits. Every era commits the fee when
the ante handler finishes, before the first message runs, and writes an action
marker when a message starts. A failed transaction with at least one marker is
therefore recorded as fee charged. A failure with no marker (seen only as out
of gas) stays unknown: an ante rejection charges nothing and an out of gas
panic during execution keeps the fee, and the result alone cannot tell them
apart. Missing old fee events do not mean zero fees.

Serving configuration, transaction/search paths, and block-event paths remain
blocked, including empty legacy blocks, until all of:

- Inclusion-scoped database identity, including repeat/cross-chain hashes
  (prototyped offline in `legacy/store`, not wired in).
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
GOPROXY=off GOMAXPROCS=2 go test -p 1 ./legacy/... ./rpc ./config
LEGACY_PRIVATE_FIXTURE_DIR=/path/to/local/fixtures \
LEGACY_RETAINED_CACHE_DIR=/path/to/retained/captures \
GOPROXY=off GOMAXPROCS=2 go test -p 1 ./core -run 'TestLegacy' -count=1
```

The optional original tests print only schema/coverage metadata. No private
fixtures may be committed or attached to review output.

`legacy/store` tests need a private throwaway Postgres on loopback and skip
without one. They refuse port 15432.

```
LEGACY_STORE_TEST_ADMIN_DSN="host=127.0.0.1 port=<port> user=<user> password=<password> dbname=postgres sslmode=disable" \
GOPROXY=off GOMAXPROCS=2 go test -p 1 ./legacy/store -count=1
```

## Census

`cmd/legacy-census` walks cached captures and prints aggregate counts only:
decode results, message types and their status, result dialects and codes,
per message evidence, failed fee evidence, what the serving classifier makes
of Hub 3 projections, and repeated hash counts. It prints no addresses,
amounts, memos or hashes. The remaining work, with the measured numbers, is in
`PATH-TO-INDEXING.md`.

```
legacy-census --root /cache --chains cosmoshub-3:100,cosmoshub-2:0 --workers 1
```

`chain:N` reads N consecutive heights from each 10,000 height directory, and
0 reads every cached height.
