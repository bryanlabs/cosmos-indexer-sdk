# Immutable commission trace output

`taxledger --commission-flows` requires `--commission-output-version`,
`--commission-build`, and an exclusive UTC `--commission-as-of` date. This CLI
path never replaces the serving flow tables. Existing API handlers continue to
read their existing tables until a separately coordinated serving change.

The append-only tables are:

- `tax_commission_flow_generations`: a version's chain, source build and date window.
- `tax_commission_flow_versions`: each validator's run, flow rows and evidence as JSON.

A generation cannot mix builds or windows. Duplicate saves are errors, never
updates. A restarted batch skips successful immutable snapshots only after
checking their build, window and identity. Failed attempts are retained too and
require a new version. The writer checks existing evidence before an expensive
trace.

Versioned loaders keep derived labels, memo heuristics and channel discoveries
in memory, then snapshot them with the result. They do not change shared labels
or channel mappings. Fee outcomes derived while rebuilding holdings stay in
memory rather than being written into the financial ledger.

## Exact historical holdings

An optional `--commission-archive-config` reads a Secret-mounted `probe.toml`.
Its `probe.rpc` must be an operator-configured archive RPC, not user input.
The URL and any customer credential are never included in errors or evidence.

If the normal historical reconstruction cannot supply holdings, the trace asks
that archive for bank balances, delegations and unbonding entries at the exact
indexed end-of-day height. It verifies the Hub chain and both boundary headers,
checks every ABCI response height and code, and follows all pagination. Header
timestamps are compared at PostgreSQL's microsecond precision. Tokens-per-share
samples are never carried forward. Empty state payloads, absent pagination and
JSON-RPC errors are refused. An explicitly finished empty page is valid. An
unknown opening, failed later-hop discovery, unresolved receiver or unavailable
closing fails the versioned trace, rather than silently becoming a reconciled
unresolved fate.

Raw archive response JSON is stored as an opaque string so JSONB cannot rewrite
the bytes covered by its SHA-256. Request data and query paths are retained too.
Both successful and failed private snapshots scope receipts to wallets that
trace actually attempted, not every wallet touched by concurrent workers. This is
source evidence, not a cryptographically verified application proof.

## Acceptance and operations

Reconcile each version's validator set, build, dates, native commission
conservation, unknown openings, unresolved destinations and drift before a
serving change. A complete count alone does not establish accounting accuracy.
Keep before/after class comparisons and classified ledger gaps with the
private evidence. Do not publish aggregate totals before the full accepted run.

Preserve prior Jobs, Pods, output and run receipts. Suspending a Kubernetes Job
removes its active Pods, so it is not an evidence-preserving pause. When a
process must be paused, verify its exact container and host PID and confirm no
write transaction before and after a host-namespace stop signal.
