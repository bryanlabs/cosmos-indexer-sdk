# Private lossless commission result writer

Branch `fix/commission-lossless-private-writer`, based on `6754a0d`.
This changes private result persistence and its explicit readers only. It does
not establish accounting acceptance, change public serving, or authorize rollout.

## Actual failure and proof boundary

Native failed logs report SQLSTATE 54000, `total size of jsonb array elements
exceeds the maximum of 268435455 bytes`. PostgreSQL JSONB containers use a
28-bit JEntry offset/length bound. The existing snapshot writes `run_json`,
`rows_json` and `evidence_json` as JSONB in one INSERT. The failed serialized
native payload was not saved and is not available. Therefore its exact failing
column is not established, and the original validator needs a source-pinned
recomputation before the repair is proven on that financial result.

The isolated PostgreSQL 16 test creates synthetic evidence with two complete
134,217,728-byte archive response strings. Its original 268,435,691-byte JSON
fails with the real 54000 when inserted into the unchanged old `evidence_json`
column. The repaired writer saves it, verifies every byte on reassembly,
decodes both full responses, and exercises the offline export reader. This is
a native limit and codec proof, not a reproduction of the unavailable payload.

## Storage contract

* Fields at or below 1 MiB retain the existing JSONB representation.
* Larger fields retain the original JSON bytes, gzip-compressed and stored in
  ordered, at most 1 MiB `bytea` chunks in
  `tax_commission_flow_payload_chunks`.
* `tax_commission_flow_payloads` is the complete index: original source/build
  and identity, codec, raw/encoded SHA-256, raw/encoded byte counts, and chunk
  count. The index, chunks, generation, and original snapshot row commit in
  the SAME transaction. Duplicate saves are INSERT conflicts, never updates.
* The original JSONB column contains a JSON STRING marker with field and raw
  SHA-256. Direct legacy struct/array decoders fail loudly, rather than silently
  accepting an empty object. Never audit raw marker exports as complete data.
* Readers verify index provenance, marker, codec, all chunk identities,
  consecutive ordinals, chunk sizes/counts, chunk SHA-256, encoded length/hash,
  gzip framing/trailing bytes, and raw length/hash. Partial output from a
  streaming reader is untrusted until its returned error is nil.
* `LoadCommissionFlowVersion` provides complete materialized fields, with the
  same typed JSON semantics. `ReadCommissionFlowPayload` offers bounded-chunk
  streaming. Snapshot reuse verifies all chunked fields before success, with
  the original source/build/window/status guards unchanged.
* Compression and DB transfer are chunk-bounded. The original collector still
  constructs and JSON-marshals each full result. This is not an end-to-end
  bounded-memory collector rewrite. The convenience materializing loader also
  needs memory proportional to the requested result.
* Writer SQL logging is discarded for the result transaction, so driver errors
  cannot dump enormous private JSON parameters into native logs.

## Explicit private offline export

`taxcommissionexport` is a new, separate read-only executable. It does not call
migrations or the collector. It sets PostgreSQL
`default_transaction_read_only=on`, limits the pool to one connection, and uses
an explicit whole-export deadline. It requires DB_HOST, DB_NAME, DB_USER and
DB_PASS, plus explicit version/build/window/membership and a private new output
path. Never put credentials in command arguments or public logs.

Its flags are `--version`, `--build`, `--chain`, `--from`, `--as-of`,
`--validators-file`, `--output`, and `--timeout`. `--as-of` is the native exclusive
end day, not the audit context's inclusive result day. Membership is a private
JSON array of exact validator identities, not inferred from whatever rows exist.

Exports resolve and verify chunked fields BEFORE including them. JSON strings
preserve their exact original bytes after outer decoding, and are supported by
`scripts/commission-proof-audit.py`. Failed statuses remain present for the
audit to assess. No generic source compatibility override exists.

Files are exclusive, mode 0600. A `.partial` file is retained on failure and is
never promoted as complete. On success an exclusive hard link publishes the
complete output, so existing outputs cannot be overwritten. Stdout contains
only an aggregate count and export SHA-256. Keep partial and complete files
private. The offline audit remains financially_accepted=false.

## Tests and isolated staging profile

Unit tests run without a database. PostgreSQL tests opt in only with
`COSMOS_TAX_PAYLOAD_TEST_DSN`; they refuse DDL unless current_database() begins
`commission_payload_writer_`. The oversized proof separately requires
`COSMOS_TAX_PAYLOAD_HUGE_TEST=1`. Never point these tests at production or a
financial database. Fixture writes contain synthetic strings and identities.

The fixture connection uses one connection, statement_timeout=90s,
lock_timeout=2s, work_mem=4MB, max_parallel_workers_per_gather=0 and jit=off.
The proof uses the EXISTING isolated `agents/stmt2-testpg` 6-CPU/8-GiB cgroup,
with both PostgreSQL and the native Go test client inside that hard limit.
Go adds GOMAXPROCS=4 and GOMEMLIMIT=2GiB. GOMEMLIMIT is a soft runtime goal,
NOT a 2-GiB process cgroup. No resources on existing Pods were changed.
Artifacts use an owner-specific mode-0700 directory under the fixture's bounded
80-GiB `/data` mount, not huge stdout captures or an unbounded root `/tmp`.

The final actual cgroup proof at 15:43 UTC includes all new Go/PG tests and
the explicit export reader. The oversized test passed in 11.79s; sampled client
high-water RSS was 2,322,026,496 bytes, with zero OOM kills and unchanged
memory.max events. The native export CLI also succeeded on the full oversized
fixture; wrong-build and existing-output controls refused publication. After
that CLI, memory PSI avg60 was 3.14 and avg10 was 0.03, so transient reclaim
stalls occurred. This is not a concurrent-reader or production-load gate pass.
Test binary SHA-256:
`041029ccea1050bbd331df726f38b5744520a54ff299d65fe8e711aea6836cb4`.
Export binary SHA-256:
`b30209f97ed019cc8b6eda8623d2a9b7501ccfa4f2245fc103079652a59cd9ab`.
Receipts: `/Users/danb/.cache/cosmos-tax-tmp/commission-lossless-writer-20261114/`.

The full legacy Go suite was attempted. Five pre-existing Docker-backed tests
could not start because this worker has no Docker socket. No Docker daemon,
new Pod, or production action was introduced to bypass that environment limit.
The remaining local Go suite, all opt-in isolated private PG tests, and offline
audit tests are reported separately. A focused Opus review found the old
raw-SQL audit export incompatible with markers; the verified read-only export
above fixes it, and the same review's follow-up reported no defects.

Go tests cover exact JSON bytes and typed semantics, pseudorandom multi-chunk
payloads, duplicate writes and immutable originals, transaction rollback,
missing/extra/reordered/corrupt chunks, index lengths/hashes/codec/build,
failed compression sinks, strict source guards, and actual oversized JSONB.
Audit compatibility and explicit provenance/membership failure cases are also
covered. No synthetic success is financial acceptance.

## Before any real private rerun

1. Keep original sources, native logs, failed snapshots and process state.
2. Review this branch and its exact binary/image fingerprints as a new source.
3. Verify destination migrations only on an explicitly approved private output
   database. None of this work applies production DDL or starts a collector.
4. Keep financial-source connections read-only, with the previously required
   private read-only snapshot, memory, timeout and metadata-batch guards intact.
5. Use a NEW output version with exact current source/build identity. Do not
   reinterpret stored errors as success or relax original provenance checks.
6. Recompute the actual failed validator under the applicable authority, then
   verify all raw lengths, hashes, typed evidence and accounting separately.
7. Export through the verified reader, retain originals, and pass the complete
   explicit membership to the offline audit. No partial coverage waiver.

A successful isolated codec proof does not clear source readiness, evidence,
conservation, native-cash, or public-promotion requirements.
