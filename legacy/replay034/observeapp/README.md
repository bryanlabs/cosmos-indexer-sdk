# Pinned Gaia balance observer

This package derives the four production Go files in `cmd/gaia/app` from
`github.com/cosmos/cosmos-sdk v0.34.10`. `PROVENANCE.json` identifies their
original and derived SHA-256 hashes, the upstream sources consulted for cache
semantics, and the copied Apache-2.0 `LICENSE`. No newer SDK is substituted.

## API and integration contract

* `NewGaiaApp` has exactly the pinned constructor signature and returns `*GaiaApp`.
* `SetRecorder(io.Writer)` has no return. A nil writer leaves observation off.
  Configure before BeginBlock or ExportObserved. Changing it during an active
  block, or after a latched error, fails closed.
* `RecorderError() error` returns only a fixed, scrubbed error. Check before AND
  after every Commit. No observer diagnostic logs addresses, balances or SDK errors.
* Standard ABCI BeginBlock, DeliverTx, EndBlock and Commit methods are wrapped.
  Accounting errors latch and Commit returns without calling BaseApp.Commit.
* `ExportObserved() (json.RawMessage, []tmtypes.GenesisValidator, error)` calls the
  real `ExportAppStateAndValidators(true, nil)`. Use a separate app opened on a
  read-only DB, once. It changes the check-state cache only, never calls Commit,
  and emits records only after the complete export and reconciliation succeed.

The package is serial like BaseApp, not goroutine safe. Initial genesis account
allocation and genesis transactions are the unobserved baseline. Optional
recording starts with BeginBlock, not InitChain. Without a recorder all execution
uses the original contexts, keepers and handlers without observer snapshots.

## What is reconciled

Full account balances are read before and after begin-block, end-block, each
message, ante, each transaction and zero-height export. The entire block is also
reconciled immediately before Commit. Snapshot reads use independent infinite
transaction AND block gas meters. Mutators always receive the original context.

The bank wrapper measures only balances at the addresses named in an actual
bank mutator call, labeled with its operation and the enclosing message or phase.
It does not manufacture movements from phase residuals. Reconciliation compares
those observed changes against full account-state differences. Direct bypasses
of bank accounting stop the run. Direct ante fees are separately expected from
the signed StdTx fee, labeled `ante_fee`, and checked against account state.

SDK v0.34.10 BaseApp commits the successful ante cache before executing messages.
All messages share one later cache, written only when all succeed. A successful
first message followed by a failed second message produces aborted attempted
movements, while a successful ante fee remains committed. A failed ante discards
its whole cache, including a fee already written for a valid first signer when a
later signer fails. Tests cover both cases.

Bank observations compare account-substore identity to the active accounting
scope. Discarded child caches used by crisis/distribution invariant checks are
excluded. This matters especially during zero-height export: its preliminary
invariant check simulates commission and reward withdrawals in a discarded cache.
Only the actual export loops produce payout observations. In this exact pinned
production app, CacheContext occurs only in the crisis handler, distribution
invariants and distribution queriers, all discarded. This exclusion is not a
general-purpose nested-cache observer: future committed child caches would require
explicit write tracking, particularly for net-zero movements. The payout labels are
`validator_commission/bank.add` and `delegator_reward/bank.add`, with actual
validator/delegator identity and the actual withdrawal destination. Each actual
withdrawal call also gets an accounting marker, even if it pays nothing.

### Known fail-closed boundary

Pinned BaseApp writes a successful message cache before its deferred block-gas
charge. That charge can panic and produce a nonzero response after message writes
have occurred. This observer does not equate that response with a committed
movement: when handlers all returned successfully but the final response failed, it
latches an error regardless of balance delta, refuses the entire block commit,
and emits nothing. Handler panic/error detection preserves ordinary failed-message
rollback accounting. A later missing route after successful handlers is also
conservatively rejected. Dedicated tests prove post-write failure with both
nonzero and exactly cancelling message movements. This is balance evidence,
not a general execution-write trace.

## Private JSONL schema

Every record has `kind`, `height`, zero-based `tx` and `message` indices, `phase`,
`label`, `status`, and numeric ABCI `code`. An index of -1 means outside that scope.
End-block and whole-height records have both indices -1. Transaction accounting
markers have message -1. `code` is meaningful for message, ante and transaction
accounting markers; other markers use zero. Message markers interrupted by an SDK
panic can have zero code; the transaction marker carries the recovered response.

* `kind=movement`: `address` is the actual account, `denom` the exact denomination,
  and `amount` a signed arbitrary-precision integer string. No float conversion.
  Export payouts additionally contain `validator` and, for delegation rewards,
  `delegator`. Owner and withdrawal destination are not conflated.
* `kind=accounting`: scope reconciliation completed. Covers begin/end block,
  every invoked ante/message, every DeliverTx including decoding failures, each
  actual export withdrawal, and the whole successful export. No arithmetic.
* `kind=height_completed`: final marker for every committed height, including
  empty blocks. No arithmetic.
* `kind=export_completed`: successful full zero-height export, including one with
  no payouts. No arithmetic.

Only movement records participate in reconciliation. Marker address/denom/amount
fields are empty strings. Ordering is deterministic; within a bank call movements
are ordered by account then denomination.

`status=committed` means retained in a committed block, `aborted` means attempted
cache changes or message accounting discarded by a failed transaction/ante, and
`exported` means successful zero-height export cache changes, not a chain commit.
A committed transaction accounting marker means reconciliation completed for the
retained state, NOT that its transaction succeeded. Check its ABCI code. Aborted
observations are never counted as committed movements.

All block JSONL is buffered until the underlying DB commit succeeds. Accounting
failure emits none of that block's buffer. An output error/short write latches a
scrubbed error; it cannot roll back an already committed DB version. io.Writer
cannot provide atomic output or fsync. The caller must use a private exclusive
file, fsync after commit, check RecorderError, and stop on error. A partial final
batch must not be accepted as complete without its final completion marker.
Export output failure similarly returns no export artifact and a latched error.

## Verification

From `legacy/replay034`:

```
go test ./observeapp -count=1
go test -race ./...
go vet ./...
go build -o /tmp/replay034-recorder-build .
```

Synthetic tests include aborted multi-message cache rollback, retained successful
ante fees, early and partial-write failed ante, direct and unscoped delta
mismatches, pre-commit buffering, empty height markers, error/short writer handling,
post-write block-gas failure, actual zero-height commission/reward payout identity,
withdrawal destination override, persistent-cache isolation, zero-payout export,
export write failure, and pinned upstream transaction-gas/state-hash/full-export
equivalence with observation enabled and disabled. They use no historical or
production state and do not print private fixture data.
