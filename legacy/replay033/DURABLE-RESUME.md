# Scratch integrated closed-checker checkpoint protocol

This is scratch engineering, not permission to start a financial range, modify
original attempts, accept books/tax data, or use an unreviewed slash candidate.
The frozen consumer, source roots, Jobs, Pods, PVCs and original databases are
untouched. No original process is signalled. `movement_journal.py` does not exist
under this checkout's `legacy/`; the exact production Journal import is
`consume_bounded.py -> terminal_journal.Journal`, subclassed by `CommitJournal`.

## Publication and restoration

An explicit preparation pass validates the original one-member gzip sources
with `closed_lines`, including compressed SHA256, footer, exact original raw
bytes/hash/count, and completion marker. It writes line-aligned chunks (8 MiB
by default, a single record may exceed this) and an exclusive source manifest.
A fresh run rechecks the derivative concatenation against the original raw seal.
Resume never seeks inside gzip or replays an old compressed prefix. It pins the
exact derivative manifest and checks each chunk's size/hash before use.
Preparation is an additional private derivative, not an original-source change.
It must be budgeted and approved before any real large-range preparation.

After `height_completed`, the Python phase and independent Set/Delete/cache
journal are closed. Python sends a checkpoint barrier after the queued native
commit. The native helper acknowledges only after verifying the real native
IAVL commitment, raw cache/ABCI results and actual next-header join, then syncing
an exclusive snapshot of its entire underlying committed DB, working phase and
transient stores, counters, original anchor, prior hash, and facts output. The
facts prefix has a durable length and SHA256, not merely a LevelDB directory.

Python snapshots its complete consumer, journal, history window and both source
cursors using typed non-executable JSON with alias preservation. It hashes all
native/Python checkpoint files, writes and fsyncs the common seal, renames the
generation, then atomically replaces and fsyncs `CURRENT`. Only `CURRENT` is a
publication point. Partial or orphan generations are retained but never used.
The seal binds source capture/derivative manifest, helper binary, imported local
Python source files, native command Go files, Go module files and semantic
configuration. Output paths and checkpoint timing are operational, not semantic.

Restore validates the pointer, seal, complete member coverage and every hash
before launching native code. Different code/source/config, torn state and
changed Python/native members fail closed. Native restore rebuilds a NEW private
DB from the exact stored DB key/value records and verifies the restored commit;
it does not execute a historical block or reopen the original DB. Facts restore
requires an exact checkpoint prefix before truncating an uncommitted scratch
output tail. Prefer a new exclusive output directory when restoring so prior
attempt artifacts are retained. The current source chunk is hashed before seek.

## Interval and limits

The default interval is 120 seconds, checked only at completed heights. The
maximum lost work is that interval plus an indivisible height and an in-progress
checkpoint write. A slow indivisible height cannot be split by this protocol.
Full DB and Python-state snapshots are deliberately conservative, not an
incremental performance claim. Their real-range size, duration and memory cost
remain unmeasured until Main authorizes an isolated dead-attempt copy or real
candidate run under the existing authority. Thus a production minutes-of-loss
bound is NOT established by tiny fixtures alone. No original source scan or
range launch is part of this scratch work.

All Go tests/builds are nice19, GOMAXPROCS=2, `-p 1`, serialized through the
existing `hub1-go-tests.lock`, with GOPROXY=off and GOTOOLCHAIN=local. H2 is not
split, displaced or stopped.

## Tests

`test_resume_integration.py` uses a three-height synthetic native033 lossless
fixture made by `resume_fixture_test.go`, including actual native InitChain,
BeginBlock, EndBlock, commits and raw cache evidence. Only original-producer
identity and the historical genesis pin have test-only provenance adapters.
The financial checker, phase consumer, independent Journal, native subprocess,
commit roots and next-header validation are not mocked. The integration runner
kills ONLY its own new scratch child process group after a durable checkpoint,
restores, and compares final receipt/facts bytes with the uninterrupted run.
It also attempts native/Python tamper, config mismatch and torn-pointer restores.
The safe result file records actual outcomes, not this design's intent.
