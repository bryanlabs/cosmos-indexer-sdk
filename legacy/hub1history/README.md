# Hub1 private raw-history adapter

Offline preparation only. This is not a promoter, native replay, observer,
financial classifier, SQL merger, migration, publication path, or new actor.
Priority: Accuracyfirst then speed (current user).

The shared atomic CLI and its isolated raw-stage mode belong to the Hub2 owner
in the separate `cosmos-tax-hub2-private-promotion` worktree. This adapter does
not edit or duplicate that CLI. Its output uses the shared projection format:
`table`, `columns`, `rows`, and an exact file `path`/`sha256` pin.

## Representation

Two existing table shapes are projected, `blocks` and `block_events`. The
receiver maps them to its isolated `raw_blocks` and `raw_block_events`, not
legacy financial tables. Thus later financial imports do not have to delete,
overwrite, adopt, or collide with raw evidence or existing Hub3/4 rows.

* Every native height retains its complete original JSONL line as base64.
  Headers, nanosecond timestamps, signed transactions, failed results, all typed
  Begin/Deliver/End fields, native roots, unknown fields and scalar encodings
  survive exactly. All source anchors and original completion markers survive.
* Complete movement recordings and cached raw block/result archives are stored
  in ordered base64 chunks of the **original compressed/artifact bytes**.
  Unknown prefixes, rich share fields, failed/aborted attempts, repeated writes,
  module state and original raw results are not narrowed to the Hub2 schema.
  Completion events bind path, SHA256, byte count and chunk count. Supporting
  source metadata and original boundary/terminal artifacts are retained too.
* Signed transactions and execution results are already present as raw evidence.
  Derived `tx_inclusions` are explicitly deferred, not filled with a false
  `fee_charged` boolean for an unknown charge. Derived taxable rows are also
  deferred, not fabricated as zero or converted from shares without native
  exchange context. Status events and receipts explicitly say tax is incomplete.
  This absence is not a claim that no transactions or taxable activity occurred.
* Canonical migration is 500000. Historical heights 500001..500015 remain a
  separate complete component using its original closed-reference proof. No
  500016 header is invented. Its diagnostic export does not replace the
  canonical opening, and intermediate zero-height validation payouts never
  become historical income.

No financial completeness or production admission is asserted by these tools.
The already-passed all-account/all-coin boundary check is not a request for
another approval. Raw staging need not wait for the independent derived
financial reconciliation, but it must preserve all original evidence.

## Tools and safety

`prepare_inputs.py` verifies the existing local closed capture pins, every cache
archive member, original source status and canonical proof hashes. It writes a
new sanitized input manifest. It never opens a native database or calls an actor.

`adapter.py` checks every segment's complete history, gzip trailer, signed-byte
cardinality, chain, release, parent/commit links, nine-store identities, closure
and component seams before emitting anything. It rechecks input digests while
reading and after projection. It reuses original native proofs, it does not
independently recompute native consensus/root acceptance. Output directories
must be new, disjoint from sources, and private. Failures leave partial attempts
intact with no success receipt. Optional progress files contain only counts,
latest-window rates and finish times. Outputs may be large, about 58 GB for the
current full retained scope, and must never be printed.

`verify_projection.py` reads the **entire** emitted files, verifies file digests,
restores every raw native line and artifact byte, checks native raw digests,
ordered chunks, every source artifact pin, closure counts and transaction totals.
It writes a separate private success receipt only after a complete pass.

`canonicalize.py` normalizes event JSON only within a proved restricted domain:
fixed root keys, int32 values, and arrays of ASCII text pairs. Unsupported values
fail rather than being normalized approximately. All keys and values survive.
It retains regularly spaced samples and every event shape for actual PostgreSQL
comparison, and records exact input/output hashes.

`sqlcanon/` is a **read-only** helper for the existing loopback private fixture
at port 25671. Credentials are loaded internally from the explicit existing
fixture file and never logged. It rejects other ports, non-loopback hosts,
fallbacks, and non-test admin databases. Catalog discovery and all actual row
coercions run in read-only transactions. It creates no database, schema, table,
role, ACL, SQL row, or actor. It canonicalizes all block rows through the actual
`legacy.blocks` composite type and checks all supplied event samples through
`legacy.block_events`. Native nanoseconds remain exact in raw evidence; only
separate typed block timestamps use the actual schema's microsecond cast.
It never connects to production. Its outputs and repeated confirmations are
exclusive new files, not rewrites of failed attempts.

The raw-stage receiver must compare these typed block times using actual
PostgreSQL coercion, not exact nanosecond equality against native headers.
The shared owner was informed through the safe status file, and retains the
native raw timestamps unchanged.

## Current private verification

On 2026-11-15, full original and PostgreSQL-normalized output both passed complete
byte restoration for 500015 blocks, 500023 raw history lines, 20228 signed
transactions, 713857 evidence rows and 43 source artifacts. All component seams
matched. All 500015 typed block rows were canonicalized by actual read-only
PostgreSQL, and 179 deterministic event samples matched its exact JSON format.
No production or fixture DDL/DML was run. One timed-out projection attempt and
all source attempts were preserved.

The current input manifest and current status are in
`/Users/danb/.cache/cosmos-tax-hub1-recording-recovery-20261115/`:

* `hub1-history-adapter.inputs.safe.json`
* `hub1-history-adapter.current.safe.json`
* `raw-projection-pgcanonical-attempt1/projection.safe.json`
* `hub1-history-adapter.canonical-full-roundtrip.safe.json`
* `hub1-history-adapter.pg-readonly-full-samples-v2.safe.json`

Actual atomic SQL fixture import/rollback/preservation acceptance and any
production staging remain the shared owner's work, not claims of this adapter.
No native consumer/recorder/monitor, source capture, job, pod, PVC or existing
financial runtime file was edited or signaled.

Tests run locally without credentials: Python unittest discovery in this
folder, and Go tests for `./legacy/hub1history/sqlcanon`. Tests cover lossless
unknown fields and failed records, source-byte restoration, terminal proof-mode
isolation, bad seams, truncated/corrupt sources, source mutation, exclusive paths,
canonicalization-domain refusal, output tampering and the fixed fixture boundary.
