# Exact sparse accounting integration

Sparse accounting is optional. `NewGaiaApp` still defaults to the full-snapshot
oracle. Call `SetSparseAccounting(true)` before an active block or export, then
configure the recorder normally. Both `run` and `export-zero` accept
`-sparse-account-checks=true`; the flag defaults to false and requires
`-coin-records`. Each CLI selects the mode before attaching its recorder.
Use separate private evidence destinations to compare exact FULL/SPARSE record
bytes and export artifacts. This does not replace or block the signed `9d3cd16`
full-snapshot historical rehearsal.
Changing modes during an observation, with buffered records, or after a latched
failure is refused. Genesis initialization remains the unobserved baseline.

## Permanent store boundary

The first BaseApp option installs the accounttrack CommitMultiStore decorator.
Existing options configure that installed store. A final constructor option
rejects replacement of the CMS by comparing the pinned SDK's private CMS identity
using reflection, without unsafe access. The actual mounted account StoreKey is
bound before stores are mounted or loaded. Writable raw CMS aliases and direct
replacement through the embedded BaseApp are unsupported escape hatches, not a
supported observer API. Sparse fences reject foreign, gas-wrapped and stale views.

With sparse accounting disabled no fences are opened. Full snapshots, bank
observations, causal labels, principal records and the original execution calls
remain unchanged. No SDK fork, synthetic cache flush, extra commit, image build
or deployment is part of this integration.

## Exact evidence and independent ledger

Each begin/end phase, ante, message, transaction, whole block and export pins its
own raw account-store view. Before-images are captured before mutations; closing
the fence reads the same view using exact single-key bounded iterators. Unlike
cachekv.Get, these do not populate child execution read caches and therefore do
not make later native reads stale after parent writes. They match the full
oracle's iteration semantics without traversing all accounts. Every movement
account is added independently to
the key union, including phantom record-only accounts. Ante includes signed fee
keys before applying the original partial-abort rule. A nonzero returned ante
context must retain the original account-store view.

Raw values decode through the app codec into auth.Account, matching the pinned
keeper. The decoded address must equal the bytes in its account key. Malformed
keys, values, unsupported codec representations, invalid coin sets or movement
identities fail closed. Every denomination is reconciled. Metadata-only writes,
creation, deletion and discarded speculative writes are not inferred income.

The existing exact movement arithmetic, attempted/retained status filtering,
late block-gas ambiguity refusal, private failure latch and precommit refusal
remain in force. All export fences close even on export failure or panic. An
export still changes only its check cache and never commits.

## Deferred and disabled caches

As documented in accounttrack/README.md, caches written while no fence is open
retain only an unknown-pending flag, not keys or balances. Flushing such a cache
while observation is active fails closed, even if its net coin change happens to
be zero. This is not repaired by scanning accounts or guessing its mutations.
Finish or abandon disabled speculative work before recording. Normal InitChain
writes are the baseline of the first block, not an unexpected active flush.

No fences may survive commit/load. Stale stores cannot become fresh views by
opening them late. Recorder disable/re-enable is supported between completed
blocks. An actual all-account export or module operation still performs its own
necessary full traversal; sparse reconciliation adds no all-account scan,
sampling or key cap.

## Validation and provenance

SPARSE-PROVENANCE.json adds hashes for this integration and retains the original
upstream/causal provenance in PROVENANCE.json without rewriting its historical
hashes. Full-snapshot tests remain intact. New sparse integration tests compare
private records, decisions, responses, gas, roots and export artifacts using
synthetic state only. Counter tests measure full snapshot calls and decoded sparse
account images, not elapsed-time claims or historical throughput.

Run from legacy/replay034 with an isolated copy of this module's go.mod/go.sum:

    GOWORK=off go test -mod=readonly -modfile=/tmp/cosmos-tax-sparse-integration.mod -race ./...
    GOWORK=off go vet -mod=readonly -modfile=/tmp/cosmos-tax-sparse-integration.mod ./...

The CLI regression suite compares omitted, explicit false and explicit true
export modes byte-for-byte and confirms the source checkpoint root is unchanged.
It also checks that sparse-without-recording fails before checkpoint creation.

Historical private-stream differential verification is not performed here. A
synthetic pass is not evidence of historical completeness or a production speedup.
