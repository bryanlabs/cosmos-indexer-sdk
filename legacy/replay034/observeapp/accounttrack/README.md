# Sparse raw account evidence, Cosmos SDK v0.34.10

This package is permanently installed by observeapp; sparse fences are opt-in
through SetSparseAccounting(true). See ../SPARSE-ACCOUNTING.md for integration
and differential validation. The primitive itself never decodes accounts,
reconciles coins, emits records, scans accounts, writes observer state, changes
gas meters, flushes caches on behalf of the caller, or refuses a native mutation.
Its errors contain no underlying error text, keys, or values.

## Integration contract

1. Construct `tracker := accounttrack.NewTracker()` before BaseApp construction.
   Prepend the constructor option installing
   `app.SetCMS(tracker.WrapCommitMultiStore(store.NewCommitMultiStore(db)))`.
   Preserve all existing options in their original relative order. Reject any
   later option substituting an unobserved CMS. Do not retain writable raw
   backing-store aliases. No SDK or dependency changes are needed.
2. `tracker.Bind(app.keyAccount)` using the exact mounted StoreKey capability,
   before accessing stores, mounting/loading, or running the app. Bind once.
   Decorators remain installed when recording is disabled.
3. Obtain the raw decorated account view with
   `ctx.MultiStore().GetKVStore(app.keyAccount)`, never `ctx.KVStore(...)`.
   Call `fence, err := tracker.OpenFence(view)` before the scoped execution.
   All currently open fences capture a copied first before-image in their own
   view before every originating account Set/Delete, even in discarded caches.
4. Before discarding a phase/ante cache, call
   `before, after, err := fence.CloseFence(extraRawAccountKeys)` on its fence.
   Supply **every** account key referenced by the independent movement ledger,
   including keys with no writes. Keys are binary strings in the returned maps;
   a present nil value denotes absence. A never-written extra key has identical
   before/after values. No data returned with an error is usable evidence.
5. Independently decode raw account values with the pinned keeper codec. Verify
   account identity against the raw key, reject unsupported/malformed values,
   and reconcile all denominations against independently expected records.
   A dirty key is evidence to check, not authority to invent a movement. This
   package deliberately accepts opaque malformed *values* for caller decoding.
6. Close child fences before native cache disposal; parent fences stay pinned
   to their own accepted view. Close transaction/block/export fences on their
   actual accepted views. Independently reject changed ante view identity,
   preserve aborted/retained records and late gas ambiguity checks. Poll
   `tracker.Error()` through the existing private observer failure latch and
   refuse commit there. Commit/load with any open fence latches an error but
   still delegates the original native operation once.

Fences may overlap and close in any order. A closed fence cannot be reused.
While fences are active, cache wrappers retain sparse pending account keys so
that a later native flush captures first images for fences opened after the
original mutation. Preflight reads happen before native Write acquires its
cache mutex. Pending keys propagate to parent caches on successful flush.

While no fence is active, wrappers retain only a constant-size dirty flag, not
key copies. If such a cache (or an ancestor receiving its pending writes) is
later flushed with a fence active, the tracker latches an error rather than
inventing missing before-images. Native Write still executes exactly once.
Open the overall block/export fence before application writes and do not carry
unflushed account writes from recorder-disabled execution into an enabled
scope. The SDK exposes no public dirty-key enumeration that could recover this
case without disabled-mode key tracking or a forbidden full scan.

There is no reset of the permanent failure latch. SDK stores and Tracker are
serial-use, not safe for concurrent execution. `-race` checks the tests but does
not confer thread safety on the SDK.

## View and tracing contracts

All account accessor variants on one multi-store return the same wrapper.
Different cache views have distinct wrappers. Commit/load advances the view
generation and invalidates the root's memoized wrapper. OpenFence rejects stale
root and cache views; callers must not retain SDK views across these boundaries.
Commit stores preserve Committer/Queryable; cache stores preserve native Write
and do not claim Committer. Non-account stores are forwarded unchanged.

Cache construction delegates native CacheMultiStore/CacheWrap/
CacheWrapWithTrace. Native cachemulti Write uses its private stores, so tracking
is intentionally based on originating wrapper mutations, not flush callbacks.
No native write/flush is synthesized, replayed, or split. Tracing setters retain
the actual returned native MultiStore (including cachemulti's value receivers)
and return the decorator with the same account wrapper identity. Native SDK
multi-store CacheWrapWithTrace ignores its supplied arguments; this package
preserves that behavior rather than inventing new tracing configuration.

At the commit root the canonical account wrapper uses native GetKVStore for
execution, including native tracing when enabled, and native GetStore for raw
observer before/after reads. Thus commit/GetStore account accessor variants
share the canonical GetKVStore tracing policy rather than exposing an untraced
mutation escape. Observer images use an iterator bounded to exactly one account
key, never advancing it, rather than cachekv.Get: Get would memoize parent data
in a child execution cache and could change later native reads after a parent
write. Point iterators preserve the full oracle's iteration semantics without
an all-account traversal or newly populating clean execution read-cache entries.
Observer reads have no consensus gas wrapper. Native iterators may themselves
trigger native trace output; this package neither installs a tracer nor
suppresses existing trace behavior.

An inactive tracker adds only permanent wrapper dispatch, the empty-fence and
view-generation checks, and constant-size pending-cache metadata (a prefix
check and dirty flag): no observer shape validation, key/value copying, reads,
or decoding.
No full-state mirror, key cap, sampling, or account iteration is used.

## Validation boundary

Run only from `legacy/replay034`:

`go test -mod=readonly -race ./observeapp/accounttrack`

These isolated tests establish sparse byte capture/decorator behavior, not
application-level coin reconciliation, historical hash equivalence, replay
throughput, or completeness of the observer insertion points. The parent
observeapp integration tests separately exercise refusal/differential behavior;
they do not establish historical completeness or production throughput.
