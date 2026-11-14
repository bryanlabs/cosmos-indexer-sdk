# Private native033 bounded financial consumer

This is a quarantine proof path, not a ledger importer or general v3 admission.
The original FULL recorder default and strict Hub2 financial admission remain
unchanged. In particular, this does not make unsupported 033 records acceptable
to `legacy/replayrecords.Stream` or `core.LegacyReplayBlock` by relabeling them.
No SQL, API, public result, source financial row, or financial book is written.

## Actual running-source capture

`capture_bounded.py --directory <new-private-directory> --through 1000` reads
only the owned range-0 producer. It preserves the real source target 200000 and
its in-progress native status. The current implementation deliberately supports
genesis-prefix bounded captures only. It does not pretend to accept the later
two checkpoint-started ranges or the terminal suffix yet.

Original compressed chunks are retained independently for coin records through
`height_completed(1000)` and history through native height 1001. These are
fragments of growing gzip streams, not falsely closed gzip members. The selected
original uncompressed bytes, line count, byte count, and exact complete-height
boundary are separately hashed. A partial height, missing before-image, changed
source, wrong common boundary, valid footer without completion, or EOF alone
cannot prove a bounded component. Capture never changes, pauses, or restarts the
producer and never opens its LevelDB.

The bounded cache contains exact raw block/results pairs through the independent
next header plus the genuine genesis. Producer binary/image, actual Pod identity,
native status, cache member bytes, and compressed source fragments are bound.
Captures made before the post-capture Pod check was added require the separately
retained same-container continuity observation, including container ID, immutable
image ID, start time and zero restart counts. That is not a forged old observation.

## Independent streaming proof

Build `./cmd/consume-native` in this pinned module, then run:

`consume_bounded.py --directory <captured-source> --native-helper <binary> --receipt <new-private-result>`

The Python consumer checks every original frame. Complete all-nine-store anchors,
every phase's full-state commitments/deltas, every successful source Set/Delete,
cache parent/child writes, repeated and net-zero writes, attempted states, exact
transaction inputs and full native results remain present. Raw unknown prefixes
and present empty bytes are not projected away. Dirty keys propagate through
cache parents so actual commit replay includes successful transaction writes and
net-zero deletes with an explicit absent before-image.

The native helper is independent of the observed producer implementation. It:

* Independently runs unmodified upstream native InitChain on the pinned genuine
  genesis and compares every raw opening key in all nine stores. A diagnostic
  private genesis commit makes those values queryable. No historical block or
  export/payout keeper is executed.
* Applies complete phase patches to new in-memory native stores with explicit
  before/after existence and byte checks, then decodes native all-coin accounts,
  staking validators/delegations/shares, unbondings/redelegations, fee collection,
  mint/pool supply, community pool and every outstanding reward liability.
* Reconciles actual bank movements, charged fees against original native signers
  and the complete signed vector, principal and share before/after objects, and
  native exact mint/fee/community/outstanding conservation. The native first
  block retains mint in fee collection because distribution allocates only above
  height one. It is not a general accounting waiver.
* Independently replays the committed raw source writes in sorted native cache
  order into IAVL and compares all nine native store commitments and the combined
  application commitment at EVERY height. A snapshot SHA or producer label is
  insufficient.
* Checks the native raw block/header/time, exact original transaction bytes and
  full cached ABCI result, then authenticates each commit/result against its
  independently captured next header. No gas, epoch or metadata waiver exists.

Transaction disposition follows actual complete nine-store cache flush evidence,
not final ABCI code alone. A late native block-gas error can follow a successful
message flush. Aborted attempted writes remain evidence but are not paid income.
The phase chain must begin from its actual predecessor, and committed transaction
state must equal exactly its retained ante/message state.

Native history retains only current/next height in memory; transaction snapshots
and phase versions are discarded after each complete height. There are no full
state dumps per height. Native IAVL uses the pinned SDK's default pruning of old
versions. All output is private, bounded, and incrementally written.

## Tests and limits

`test_consume_bounded.py` includes actual source-frame, changed-byte, truncation,
missing anchor, phase, key/prefix, duplicate/gap, result, fake completion and
classification controls. Optional read-only accepted canonical fixtures prove
that substituting 500015 for canonical migration height 500000 is refused.
`actual_bounded_controls.py` mutates actual fee/share/principal, native result,
all-store root/application root and before-image inputs, requiring the specific
native refusal, not just any nonzero exit or missing completion.

The actual first 1000 heights include 239 transactions and nonzero discarded
message writes. This bounded proof does not close ALL1..500015. Final closure
still requires normal closed 200000/400000/500000 sources and seams, complete
checkpoint-started consumer contexts, genuine opening, retained canonical 500000
export/payout evidence, and the separately validated exact 500001..500015 suffix.
The suffix is not a Hub2 opening; zero-height validation payouts are never
ordinary income. There is no accepted source proof for 500016. Full books/tax and
financial admission remain false until that complete independent path exists.
