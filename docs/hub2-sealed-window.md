# Hub2 sealed-window financial component

This host-only validation component advances the already running accepted 3e
recording, without replaying a native block, opening a database, writing a ledger,
changing the gas profile, or claiming an entire historical chain is accepted.

## Concrete boundary

The reconciled component is every complete height 400001..482090, including every
transaction/message/asset, failed transaction/aborted attempt, signed ante fee,
native cause and principal record. It calls the existing production
`reconcileReplayHeight` and unchanged `core.LegacyReplayBlock`. The original
cached GasUsed/raw responses remain canonical. Separately attributed gas
metadata is admitted only by the existing exact SHA-pinned profile and its exact
3387 original marker bytes. A different/missing/extra marker refuses.

The core gate reconciles one complete native-causal height at a time. It does
not need, initialize or pretend to reconstruct an opening liquid/share ledger.
This component therefore does not seed genesis height0 as height400000 and does
not establish books/genesis/export/full-chain/tax acceptance. The complete
original movement prefix through400000 is retained when the source is long1;
no prior context or aborted evidence is silently dropped.

## Source sealing, not fake Job completion

The first adapter is intentionally pinned to the actual original long1 JobUID,
signed 3e observer source and 656 image. Its original first height remains1,
its target remains2902000, and it remains Running. The additional flag
`--sealed-window-receipt` is validation only, requires the exact approved gas
profile and is incompatible with write/export options BEFORE input/DB opening.
Ordinary status completion guards are unchanged. The derived 400001..482090
scope exists only in memory and is never written as the original runner status.

The original native schema has these five available input streams:

1. movements.ndjson, native complete-height batches written after Commit then
   fsynced by the driver;
2. validator-updates.jsonl, genesis and sparse actual updates fsynced BEFORE
   Commit;
3. result-differences.txt, appended AFTER movement sync and all native checks;
4. durable run.log, whose successful report follows those operations;
5. status.json, an atomic rename of the actual original runState each30seconds.

`export.json` and `export-movements.ndjson` do not exist until the complete
2902000 source run has finished. They are not represented as sealed, invented
or bypassed. This per-height component does not consume a zero-height export.

A successful original status with committed>=482091 and verified=committed-1
>=482090, matching its exact timestamped durable report, establishes a candidate
prefix. A height counter alone does not. Original Job/Pod raw API responses
before/after must show the same JobUID/PodUID/spec, accepted runtime digest,
ready/no-restarts and original2902000 command, with the original Job still live.

The extractor counts raw bytes under LC_ALL=C up to the actual482090 completion
marker. It copies only that exact bounded movement prefix, not a file which
will keep growing toward2902000. It independently parses the copy. For every
source prefix it retains exact byte offset, raw SHA, inode, original observed
file sizes and two remote prefix checksums matching local readback. Partial
captured tails in sparse streams remain separate raw artifacts; they are never
called sealed or normalized. Immutable original status bytes are copied, never
shortened. Concurrent atomic status replacement can conservatively refuse a
single attempt; all failed artifacts remain intact.

The selected complete window is byte-extracted from the retained movement and
discrepancy prefixes. All heights and ordering/markers are independently checked
again by the Go loader before callbacks. SHA-rebound mutations of selected
records still must equal the original source prefix. All required original
bytes remain private and outside Git.

## Cached inputs and honest state-proof limits

All raw block/result gzip members400000..482091 are copied losslessly into an
exclusive private cache. Every compressed member must match a fresh actual
remote file checksum, its copied manifest and Go readback before financial
callbacks. Missing/extra/duplicate/reordered members refuse. Entire raw inputs
are retained. Identity, every parent link, and all four actual boundary
header/AppHash/raw-block hashes are checked, without editing envelopes.

The successful original native driver checks AppHash against the following
cached header, Code/Data historical results roots, validators, proposer, parent
and result comparisons. Its original successful status/report is source-attested
evidence of those native checks. This component does NOT invent independent
retained per-height native AppHashes, full native DeliverTx response bytes,
checkpoint consensus parameters or a fresh loaded-root proof. Original482091
WAS executed by the ongoing source Job, unlike the separate completed gas
control's unexecuted482091. The extractor executes no blocks. Both facts are
explicit in its cache binding.

## Bounded execution and hold

Build the host CLI from signed source. No container build/deployment is needed.
Before capture, the runner requires its own clean G-signed source tree, rebuilds
the CLI into a new private path with identical build flags and requires its SHA
to equal the supplied binary. `/bin/true` cannot become an apparent financial
proof. The command/result bind the signed consumer source and binary receipt.
Private executable files are0700; nonexecutable evidence files are0600.
The runner makes one attempt, has no retry/sleep/poll/schedule and creates no
Kubernetes resources. Every new private path is exclusive, directories0700 and
files0600. stdout contains counts/booleans only; detailed raw evidence stays
private. Failed attempts are not overwritten or deleted.

Run `scripts/hub2_sealed_window.py` with an exclusive `--out`, the pinned private
`--profile` and built `--cli`. `--readiness-only` explicitly reports candidate
availability without capture. Requested validation with unavailable source exits2
and writes readiness.json, not a success/component-result. Refusals exit1.
Only an actual completed core invocation exiting0 writes component_validated
true. Exact readiness also requires all five stable prefixes, complete records,
exact3387 marker bytes, all raw cached members and core held0, not just a native
height or an ETA.

The existing parent-owned10minute check may invoke the ready runner later.
This implementation does not alter that helper or create a second watcher.

## Parallel recording handoff

Parent subsequently authorized a separate owner to create new3e range recordings
from saved native checkpoints. That implementation and its overlap proof are NOT
owned here. The current adapter will refuse a different Job. Switching the
financial proof to the new400001..600000 source requires its actual Job/Pod UID,
original full target/first height, native400000 checkpoint/copy bindings and
owner's actual sealed raw-overlap packet against long1, not a guessed identity
or an unexecuted overlap. Preserve the original long1 reference. A checkpoint-
started source is never represented as a genesis-started source, and its whole
600000 Job must never be relabeled complete at482090. Narrow adapter work follows
that actual packet; no native replay or old accepted metadata proof is rerun.
