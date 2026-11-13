# Hub2 replay importer rehearsal

This command is offline and rehearsal-only. It never migrates schemas, changes
coverage, writes serving tables, contacts a node, or runs a historical replay.
Hub3 continues to use the existing importer and mapper unchanged.

## Inputs and admission

Required flags: `--cache`, `--records`, `--proof`, `--status`.

`--records` names an immutable private JSONL segment produced by the causal
observeapp recorder. Keep the original recorder output, receipt, replay status,
block captures and export artifacts. Never print their contents.

The strict JSON proof fields are `chain` (`cosmoshub-2`), `release` (the exact
`replayrecords.Release034` string), `from`, `through`,
`state_hash_verified_through`, and `records_sha256` (lowercase SHA-256 of the
exact segment bytes). Heights must close contiguously. The status must be the
actual retained replay run status, covering this segment, with the same pinned
release, chain and verified bound. Zero cached-result differences need no extra
file or proof fields, preserving existing synthetic fixtures.

For nonzero differences, provide `--result-differences` naming the original
retained `result-differences.txt` and pin its full raw lowercase SHA-256 in the
proof's optional `result_differences_sha256`. The same pin is required in an
optional export proof. No status field is rewritten, and differences are never
reported as zero. Admission rereads and classifies the entire file, including
older resume lines and lines beyond the imported segment through the actual
status committed height. Only canonical `txN:tags(...)`, `begin:tags(...)` and
`end:tags(...)` clauses in runner order are accepted. Details must be `order`,
a `value key;` list, or a signed nonzero count/key list. Keys must match
`[A-Za-z_][A-Za-z0-9_./:-]{0,255}`. Unsupported key bytes are refused, not
silently skipped. Heights and transaction indexes must be canonical decimals,
unique and ordered. Duplicate heights, clauses and count-delta keys are refused.
Repeated `value key;` entries are valid runner output for different changed tag
occurrences sharing a key; each occurrence is parsed. Lines require a final LF,
not CRLF. Code, data, gas, log,
transaction-count, absent-result and validator-update differences always fail,
even on historical resume lines.

The entire file is bounded to 256 MiB and each line to 1 MiB, with bounded
streaming memory. All heights must be positive and no later than status
`last_height_committed`. The number of lines in status
`first_height_this_run..last_height_committed` must equal its actual
`heights_where_results_differ_from_cache`; earlier lines are separately counted.
The exact full raw file digest must match. The admitted proof gains
`result_differences_admission`, retaining the raw digest, status range and
nonzero count, total/historical/run line counts, parser version and a
`classification_sha256`. This digest hashes the canonical Go JSON of the
admission object with its classification digest field empty. A supplied
admission object is never trusted instead of the raw file and must exactly
match the recomputed object. Both the admitted replay proof and, on the export
height, admitted export proof are stored with the private replay receipt.
Retain the original difference file alongside these receipts. This admission
classifies runner difference labels; it does not independently reexecute Gaia.

The terminal committed record is NOT proven by its own Commit. Replay must
actually read and check the next cached header first. If a recorder run leaves
one final committed but unverified height, retain the whole original file and
create a separately named complete-height prefix segment ending at the verified
bound. Copy the original bytes through that height's completion-marker LF,
without JSON decoding/reencoding, relabeling or dropping individual records.
Pin that prefix's exact bytes in a new proof, and keep the original unverified
tail file byte-for-byte unchanged. Do not rewrite the original or claim its
last height was verified. A terminal chain boundary with no next-header proof
remains held, even if an export artifact exists.

The reader validates all bytes, closure and digest before invoking any consumer,
then revisits one height at a time. No full-chain height slice is retained.
Input files must remain immutable during both passes. Unknown/ambiguous causes
hold the entire height with no rows; later heights are still checked. A nonzero
held count produces a nonzero exit. Output contains only counts and fixed
scrubbed diagnostics, never accounts, amounts, memos, hashes or connection URLs.

Optional `--export-records` and `--export-proof` must be supplied together. The
export proof has `zero_height_export: true`, `from == through` equal to the
replay segment end, an exact export-record digest and the same next-header
verified bound. Only actual completed zero-height export commission/reward
additions are accepted. No residual balance payout inference exists. Export rows
are phase 4 with source `export_payout`; live replay rows use actual keeper
state or reconciled signed message sources. Full recorder evidence, including
aborted attempts, cached result envelopes and block evidence, is retained in
private block-event attributes. Replay receipts are stored alongside it.

## Destination

Without `--write`, the command validates and constructs rows without a database.

Writing requires `LEGACY_REPLAY_IMPORT_DSN` and `--rehearsal-db` to name the same
new database matching `legacy_replay_rehearsal_[a-z0-9_]{8,48}`. A normal operator
must provision that new database and migrate its legacy schema separately.
Connect as the existing confined importer role, not an owner or serving role.
The command verifies its actual database name, schema confinement, an exclusive
advisory lock, and empty legacy evidence/derived tables. Port 15432, including
fallback hosts, is refused before connecting. There is no production override,
resume, update, delete or migration mode. All destination checks (including the
GORM confinement guard), the session lock, and every ledger transaction use one
dedicated SQL connection. Losing that session aborts the importer without pool
reconnection or repeated admission. Cleanup unlocks that same connection and
closes it and its pool.

Each accepted height is inserted atomically. A failure after earlier heights
were stored leaves those insert-only rows intact. Keep that rehearsal database
and rerun into another new named database; do not clean it up or silently resume.
A held height never receives a block row or partial accounting rows.

## Tests

Unit tests cover every pinned Hub2 message family, signed-fee versus actual ante,
transaction code and message positions, aborted evidence, zero/rounded actual
staking principal, two-validator reward causes, actual unbonding completions,
governance burns/refunds, export-only admission and destination guards.

The retained confined-role Postgres tests are `TestReplayPG*` in
`legacy/ledger`. They require `LEGACY_STORE_TEST_ADMIN_DSN` on an explicit
loopback throwaway server, never port 15432. They create private new databases
and roles and retain them, with no DROP cleanup. They prove state/export source
roundtrips, immutable reruns, phase/source guards, unchanged Hub3 behavior and
physically untouched serving rows. `TestRehearsalPG*` in this command also proves
actual command admission/no-resume and fail-closed session loss: only the known
new scratch importer backend is terminated, a second importer acquires its
released lock, and the first cannot reconnect or write. Serving rows and a
separate populated retained database remain physically untouched. Historical captures and real next-header
replay acceptance are separate parent-owned verification, not proven by these
synthetic tests.
