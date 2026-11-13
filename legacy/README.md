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
  (the `legacy` schema below; not read by the API).
- Per-hub genesis balances, delegations and vesting imports
  (`cmd/legacy-genesis`).
- Terminal-to-next-genesis per-address reconciliation (`cmd/legacy-close`,
  Hub 3 only).
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

The Postgres tests (`legacy/schema`, `legacy/genesis`, `legacy/ledger`,
`legacy/books`) need a private throwaway Postgres on loopback and skip
without one. They refuse port 15432.

```
LEGACY_STORE_TEST_ADMIN_DSN="host=127.0.0.1 port=<port> user=<user> password=<password> dbname=postgres sslmode=disable" \
GOPROXY=off GOMAXPROCS=2 go test -p 1 ./legacy/... -count=1
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

## Import

`core.LegacyLedgerBlock` turns a decoded cosmoshub-3 height into what the
`legacy` schema keeps: the block, every inclusion with its signers and its
fee verdict, the tax rows the serving classifier gives for each executed
message, and the block events that say something happened to an account or
a validator. A height with any unresolved transaction is refused whole.
Beyond the classifier's rows it adds:

- the address a reward was paid to, in the row's detail as `paid_to`, when
  the withdraw address is not the delegator (the row still names the
  delegator);
- one row per payer of a multi send with several payers (the classifier
  gives only the rows of the receivers);
- at the end of a block: a row for each completed unbonding, each refunded
  governance deposit, each community pool spend, and the commission paid out
  when a validator with no stake left is removed.

It refuses a message whose reward and commission rows do not add up to what
the distribution account paid while the message ran.

`cmd/legacy-import` runs cache, decode, classify and store for one explicit
range of heights through `legacy/ledger`. It refuses a connection that can
reach anything outside the `legacy` schema, claims its range in
`legacy.import_ranges` (no two importers on one range, no overlapping
ranges), checks that each block names the block before it as its parent,
stores heights in batches, and resumes where it stopped. A stored height is
never rewritten: a rerun that reads something different from what is stored
stops. It prints a progress line and aggregate counts only. Hub 1 and Hub 2
are refused until their tags have a classifier.

```
LEGACY_IMPORT_DSN="host=<host> port=<port> user=legacy_importer password=<password> dbname=<name> sslmode=disable" \
legacy-import --cache /path/to/cache --chain cosmoshub-3 --from 1 --to 5200790 --workers 8 --resolutions verdicts.txt
```

`--resolutions <file>` applies fee verdicts written by `legacy-seqcheck`.
The range is done only when every height in it is stored; a held height
leaves it failed and the command exits 1.

## Books close

`cmd/legacy-close` checks that the stored history of a chain explains the
balances the next chain started with (`legacy/books`). For every address it
takes the spendable balance in the chain's genesis file, applies every
stored row and every charged fee up to the export height, and compares the
result with the spendable balance in the next chain's genesis file. Module
accounts are left out and counted. It prints counts and totals only, and
with `--record` replaces the chain's `liquid` lines in
`legacy.reconciliation`, one per address, closed or open. It supports a
chain whose export paid nothing into balances, which is Hub 3; Hub 1 and
Hub 2 need their restart payout rows first.

```
LEGACY_IMPORT_DSN="... user=legacy_importer ..." legacy-close --chain cosmoshub-3 --record
```

The Hub 4 genesis file was not a plain copy of the Hub 3 state: the program
that wrote it carried out governance proposal 29. It emptied seven
fundraiser wallets, credited seven recovery wallets with a fixed amount each
and left the rest in the community pool. No Hub 3 transaction exists for
any of it, so without help those fourteen wallets never close.
`--genesis-change <file>` names a copy of the public source file that made
the change (`github.com/cosmos/gaia` `v4.0.0` `app/prop29.go`). Only the
file with the sha256 recorded in `legacy/books/genesischange.go` is
accepted, and no wallet or amount of the list is written in this
repository. The list is closed and is taken whole or not at all. It is
accepted only when all of this holds: for every source wallet the next
genesis holds nothing and the history gives more than nothing; for every
recovery wallet the difference equals the listed amount exactly; no source
held less than its recovery was given; and the seven source balances less
the seven listed amounts come to exactly 131,859,276,000 uatom. That last
number is the change in the community pool between the Hub 3 state after
the export height and the Hub 4 genesis file. It is recorded next to the
file's sha256 with the sha256 of its two pieces of evidence, and the close
does not run with the list at all unless the Hub 4 genesis file loaded in
the database is the one it was read from. The public program gives no share
per source (it pays the listed amounts out of one sum), so the remainder is
checked as one sum.

Accepted, the fourteen wallets are counted as closing with the Proposal 29
genesis change, on their own line and with their own status in
`legacy.reconciliation`, never as closing exactly. Refused, the result is
the one a close without the list gives, and the command exits 1.

With `--record` and the list accepted, the change is also stored as
fourteen rows in `legacy.taxable_events`, once: at the export height, in
the phase of the state export, dated at the time of the block at the export
height (the last Hub 3 block; the Hub 4 genesis file carries the Hub 3
genesis time, not a start time of its own), category
`prop29_genesis_recovery`, amount source `genesis`. A source wallet gets one
row out for its whole balance, a recovery wallet one row in for the listed
amount. Neither is classified as income or as a sale, both say they need
review, and each names the other wallet, the part left to the community
pool and the evidence, and says that the movement was made by the genesis
file of cosmoshub-4 and took effect on that chain. The rows move no balance
in the close itself.

```
LEGACY_IMPORT_DSN="... user=legacy_importer ..." legacy-close --chain cosmoshub-3 --genesis-change prop29.go --record
```

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
counting. A signature is checked against the public key in the transaction,
or against the key the account already has on record when the transaction
carries none or carries one that is not the signer's (the chain ignored a
key in the transaction once the account had one). `Tx.SignDoc` rebuilds the bytes each era signed (sorted Amino JSON
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
sub_index). `legacy.block_events` keeps the begin and end block events
later steps need (slashes, completed unbondings and redelegations, proposals
that ended, duplicate vote evidence). What the chain recorded is written
once: a trigger refuses any update, delete or truncate on the block,
inclusion, block event and genesis tables, for every role.

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
