# Collector checkpoint and bounded-memory SAVE, WIP

NOT READY FOR ACTIVATION. No live collector, publisher, guard, source, resource,
Job, Pod or PVC was changed. Main review and independent cross-review are
required before any activation. This branch contains implementation, not a
launch authorization or financial acceptance.

## Checkpoint implementation

`commissionflows_checkpoint.go` writes mode-0600, hash-chained, create-if-absent
read records inside a mode-0700 directory. Records and directory entries are
fsynced. Deterministic engine replay restores discovery order, heap, seen set,
cursor and pools without repeatedly serializing the growing graph. Successful
opening, events, stop, IBC and closing reads are checkpointed. A failed attempt
never saves an immutable result. Corrupt, torn, mismatched or divergent prefixes
are rejected, not repaired or treated as complete. Limits are 64 MiB per read
record and 100,000 records; exceeding either fails closed, not by dropping data.
Very large individual wallet reads may therefore still require a chunked reader.

The production-facing adapter is programmatic only. There are no activation
flags or environment switches. It isolates each root's loader, receipt journals
and archive caches. Receipt deltas retain original projection/correction
snapshot identifiers and exact archive bytes. Replay also reconstructs archive
header/holdings caches. Ordinary collection has checkpoint delta tracking off.
The prior Opus review reproduced a time-zone identity defect; UTC identity and
request normalization now have regression tests.

## Source epoch: candidate protocol, not deployed proof

The existing private authz seal binds one reviewed original transaction and its
related rows. Correction receipts bind their reviewed source inputs. Inspection
of those checks did not prove that every arbitrary discovered wallet, receiver,
label, memo heuristic and channel read is immutable across separate snapshots.
This is not a claim that a stronger global freeze is always required.

The scratch adapter offers PostgreSQL exported/imported MVCC as a conservative
candidate. A separately surviving read-only transaction exports the snapshot;
each root imports it before querying any inputs. Its database identity and
actual executable bytes are bound into the journal. All DB reads, including
receivers and labels, use the imported root transaction. An expired export or
changed database fails before replay. Live-node channel fallback is rejected
because it is not covered by that DB epoch. Exact-height archive reads and
existing private source guards remain in place.

UNPROVEN: a real PostgreSQL integration run, independently surviving export
owner across collector termination, production channel configuration, archive
canonicality/equivalence under interruption, live memo-derived label creation
timestamps, and the admission/freshness policy
that makes the chosen pinned source epoch authoritative rather than stale.
An MVCC token alone is not a seal proving current source completeness. No owner
was started. Long-lived snapshots can retain database history; their operational
cost and H2 impact have not been qualified. Losing the owner requires a fresh
attempt, not reuse under a new token. This candidate must not be activated as if
those missing proofs were already satisfied.

## Memory-safe final SAVE

The retained SOURCE page-later-two OOM audit places the seventh case after
43,945 wallets and 3,678,512 queued events, at saving_started with no capture
attempt. The exact allocating stack is unproven, but the old versioned save
marshaled whole run/rows/evidence fields into memory before capture.

`commissionflows_streamjson.go` now streams the supported typed JSON shapes,
including large escaped response strings, without whole-container encoder
buffers. Unsupported shapes/tags/custom marshalers fail closed.
`commissionflows_spool.go` streams JSON, hashes and gzip to private fsynced
spools, then publishes the capture manifest only after every field is durable.
Small inline values are capped at 1 MiB. Large SQL payloads are read one 1 MiB
compressed chunk at a time. Raw and encoded hashes are checked before metadata
acceptance. Chunks, payload metadata and owning result remain in one existing
SQL transaction. A late failure rolls it back. This versioned SAVE path is wired
in code but has not been deployed or run on the original large source case.
It does not eliminate the already-resident traversal graph or final evidence
collections. No claim that the original 8 GiB case is now safe is established.

## Tests and limits

Executed with GOMAXPROCS=2 and go -p 1:

- Checkpoint boundary/repeated resume, cyclic/duplicate discovery, source/code/
  settings/receiver changes, corruption/torn files and fail-closed completion.
- Actual subprocess SIGTERM after committed reads, and deadline interruption.
- Exact synthetic canonical facts, final read-chain hash and private evidence
  equivalence through 18 interruption boundaries, including closing reads.
- Actual production receipt-adapter restore and incremental delta tests.
- SQL-mocked epoch export/import failure, database mismatch and no-save tests.
- Streaming JSON byte parity, escaping, write faults, unsupported/cyclic values.
- Spool corruption/mode/symlink/truncation, serialization-before-SQL failure,
  SQL chunk/metadata rollback and atomic success, bounded 64 MiB receipt tests.

These are synthetic and SQL-mocked, not production or real PostgreSQL proofs.
A complete package run was attempted and failed on five unrelated tests that
require the absent Docker socket. The package excluding those five passed;
48 environment-dependent tests were skipped. The non-Docker run recorded 400
passing test/subtest actions, and targeted checkpoint/SAVE race tests passed.
See the handoff artifact for exact
commands and counts. No large original private source case was rerun.

## Full queue and retained scope

The exact corrected-flow scope remains 324 original members. The capacity
owner's 22:11 readback is 31 saved, 293 unfinished, five physical collectors.
Three retained source-error members remain unfinished. LATER-S02 is 41 assigned,
one saved, 40 unfinished; the current retry covers one and the other 39 are NOT
queued. This branch does not claim to have restored or launched that queue.

The eventual queue repair must derive every pending queue by exact original
membership minus verified reusable successful outputs, preserve original order
and every old failed/large-root member, and assert disjointness and union=324.
An error receipt is not completion. Keep the original six-slot concurrency and
all fail-closed guards. Preparing and activating executable queues is still a
handoff blocker, coordinated with capacity owner bdcb34c3, not duplicated here.

The separate SOURCE page-later-two batch is 108 members: six saved (five
reconciled, one drift), seventh missing, 101 unstarted. Those six are not added
to the corrected-flow count and are not financial acceptance. No retry or
relabeling of their immutable generation was made. A changed binary requires a
new reviewed generation and explicit cross-version policy.
