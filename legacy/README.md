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
`--empty-stride N` reads the result of a height with no transactions only at
multiples of N, which makes a pass over every transaction about three times
cheaper. Without it every result is read.

## Offline import

`core.LegacyStoreBlock` turns a decoded cosmoshub-3 height into store rows:
signers, the fee when the chain is known to have charged it, messages, and
the tax rows the serving classifier gives for them. A height with any
unresolved transaction is refused whole. `cmd/legacy-import` runs cache,
decode, classify and store end to end. It accepts only a loopback database
whose name starts with `legacy_`, refuses port 15432, and prints aggregate
counts only. Hub 1 and Hub 2 are refused until their tags have a classifier.

```
LEGACY_IMPORT_DSN="host=127.0.0.1 port=<port> user=<user> password=<password> dbname=legacy_<name> sslmode=disable" \
legacy-import --cache /path/to/cache --create-schema --from 1000000 --to 1001999
```

Pass `--create-schema` once, alone, on an empty private database. Later
runs, and parallel ranges, only check that the inclusion table and the guard
are present. `--resolutions <file>` applies fee verdicts written by
`legacy-seqcheck`.

## Account sequence check

`cmd/legacy-seqcheck` counts, per signer, the cached transactions that got
past the fee and signature check, and compares them with the account
sequences in the hub's genesis file and the next hub's. Over a whole hub
(`--whole-hub`, every height present) an account with no unproven failure
must match exactly, and for the others the difference says how many unproven
failures paid their fee. `--out` writes the settled ones as
`chain/height/index verdict` lines, with no address. On a partial range it
only confirms that no account is counted past its closing sequence. It
prints aggregate counts only.

```
legacy-seqcheck --cache /path/to/cache --chain cosmoshub-3 --from 1 --to 5200791 \
  --opening genesis.json --closing genesis.cosmoshub-4.json.gz --whole-hub --out verdicts.txt
```

`--signatures` follows each account one transaction at a time instead of
counting. `Tx.SignDoc` rebuilds the bytes each era signed (sorted Amino JSON
of the chain id, fee, memo, messages, account number and sequence) and
`VerifySignature` checks them against the public key. A fee paying
transaction whose signature verifies at a sequence fixes the account there,
and an unproven failure between two fixed points is settled by the count
alone, so `--out` writes verdicts for a partial range too. The range has to
start at height 1 on Hub 2, where account numbers of new accounts are found
from the order of creation. With `--whole-hub` and every height read it also
compares every account's last sequence with the next hub's genesis.

```
legacy-seqcheck --signatures --workers 16 --cache /path/to/cache --chain cosmoshub-3 --from 1 --to 5200791 \
  --opening genesis.json --closing genesis.cosmoshub-4.json.gz --whole-hub --out verdicts.txt
```

## Legacy schema and genesis load

Hub 1 to 3 history is stored in its own Postgres schema, `legacy`, written by
a role that has no right on anything else (`legacy/schema`). Rows are keyed
by chain first: `legacy.blocks` by (chain_id, height), `legacy.tx_inclusions`
by (chain_id, height, tx_index) with a hash that may repeat, and
`legacy.taxable_events` by (chain_id, height, phase, tx_index, msg_index,
sub_index). What the chain recorded is written once: a trigger refuses any
update, delete or truncate on those tables, for every role.

`schema.Confined` reads the catalog and lists every way the connected role
could reach outside the schema or rewrite a written-once table. Every legacy
writer calls it before its first write and refuses to run on any finding.

```
legacy-schema --print                 # the whole migration, for review
LEGACY_ADMIN_DSN="host=127.0.0.1 port=<port> user=<user> password=<password> dbname=legacy_<name> sslmode=disable" \
legacy-schema --apply                 # private loopback database only
LEGACY_DSN="... user=legacy_importer ..." legacy-schema --check
```

The migration creates the role without a password. Set one separately.

`cmd/legacy-genesis` loads the opening state of a hub from its genesis file
(`legacy/genesis`): accounts with liquid, delegated and unbonding amounts,
delegations valued at the validator's rate, unbonding entries, validators
with unpaid rewards and commission, and withdraw addresses. It checks the
file against a `SHA256SUMS` list, refuses a file whose parts disagree
(delegations that do not add up to a validator's shares, balances that do
not add up to the stated supply), loads in one database transaction, then
reads every stored row back and compares it with the file. A second run
with the same file writes nothing. It prints counts only.

```
legacy-genesis --file genesis.cosmoshub-2.json --sums SHA256SUMS --check-only
LEGACY_DSN="... user=legacy_importer ..." legacy-genesis --file genesis.cosmoshub-2.json --sums SHA256SUMS
```

The Postgres tests need a private server and skip without one:

```
LEGACY_STORE_TEST_ADMIN_DSN="host=127.0.0.1 port=<port> user=<user> password=<password> dbname=postgres sslmode=disable" \
go test ./legacy/schema/... ./legacy/genesis/...
```
