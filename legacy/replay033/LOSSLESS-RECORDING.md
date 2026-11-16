# Private lossless Hub 1 financial recording

FULL iterator/account observation and the unobserved upstream Gaia constructor
remain the defaults. This is a private source recorder, not v3 financial
admission, a tax ledger, a balance repair, or an importer.

`run` and `export-zero` accept `--lossless-recording=true` only with
`--coin-records`. `--compress-evidence=true` adds reversible gzip framing to coin
records and optional `--native-history`. Neither flag changes native execution,
integer/decimal arithmetic, scalar types, row order, or native result comparison.

## Complete state and source writes

All nine persistent stores are observed, including every unknown prefix/key and
present empty value. Complete initial/final anchors, original per-phase complete
SHA-256/key-count commitments and all raw before/after changes are retained.
They reconstruct every original FULL phase image, including attempted states.
No financial-key projection, sampling, value normalization or key cap exists.

The first native image is a complete iterator anchor. An exact same view advances
using write revisions. Switching views reconstructs from the complete canonical
commit anchor and every candidate write, including discarded speculative writes.
Dense changes use the same complete native iterator. Commit captures the actual
root before advancing the generation. Load invalidates the anchor. Borrowed raw
images are immutable. Neither bounded reads nor full iteration memoizes native
execution `Get` values or consumes execution gas.

Supplemental `native_write` records retain every successful source Set/Delete,
including repeated and net-zero writes, with full raw before/after bytes and
native view/parent identity. `native_cache_write` records retain actual successful
cache flushes with their native phase. These records supplement, not replace, the
original FULL phase records. Transaction message disposition follows observed
flushes of all nine native stores, not merely the final ABCI code. A late native
block-gas error can follow successful message writes. Partial flush observation
fails closed. An ante failure and a discarded message cache remain distinct.

Sparse account checks decode the union of all raw account writes and independent
movement/fee-payer keys using native SDK 0.33 account types. Every denomination is
reconciled. Metadata and unrelated raw account-store keys remain in native raw
state, never invented transfers. The default FULL checks remain unchanged.

## Native history and completeness

`--native-history` requires `--compare-results=true` and
`--verify-target-next-header=true`. Complete cached ABCI equality is required
before Commit even when coin recording is not selected. Compact history retains
the source genesis hash, source native anchor, every native header/time, signed
transaction bytes and complete typed Begin/Deliver/End results. All nine actual
committed store roots are read only from the NEW replay's own DB metadata and
recombined by the pinned SDK's unmodified MultiStoreProof implementation.

History is complete only with its terminal `history_completed` record, written
after actual next-header/result/validator verification. A valid gzip trailer is
not completion. Coin recordings independently require their existing completed
segment/export marker. Every successful height flushes compression before fsync;
the final close is checked before reporting success. Failed/truncated attempts
are retained and inadmissible, never silently resumed.

The strict CLI does not fabricate a missing next header. A terminal height whose
next header is unavailable still needs separately proved native terminal binding;
this implementation does not claim that binding from a label or a signed fee.

## Proof boundaries

Bounded actual FULL-versus-lossless comparisons must include every original
record byte, complete typed all-nine-store audit, native headers/results/roots,
whole typed export, source preservation and source-write/rollback semantics.
Supplemental journal records are checked separately, not discarded as financial
noise. Missing native keys, changed fee/share/result/raw-state/anchor data and
incorrect rollback evidence must be refused. Synthetic nonzero-write rollback
and retained post-write failure tests prove supported mechanics, not presence of
those cases in a particular historical window.

Retained payout fixtures may exercise reconstruction without executing payout
keepers or repeating an accepted terminal export. A state hash match alone never
accepts whole-history financial accounting. All accepted heights, terminal typed
exports and an actual independent financial prover remain required.
