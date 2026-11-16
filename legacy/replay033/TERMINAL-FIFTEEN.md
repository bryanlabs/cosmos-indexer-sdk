# Closed Hub 1 terminal fifteen-height component

This is an explicit, private, source-pinned proof mode for the actual retained
cosmoshub-1 heights 500001 through 500015. Ordinary strict replay still requires
the unexecuted next header. No 500016 header is generated, waived, or inferred.
No public promotion, financial import, ledger repair, or SQL is implemented.

## What is independently trusted

The original completed normal replay checkpoint has 92 files, committed target
500015, zero cached result differences, and an empty `complete` marker. Its
original status honestly verifies the next-header chain only through 500014.
That status is not upgraded, edited, or called a terminal consensus certificate.

The exact original files were captured by byte copy, without a LevelDB open,
with complete before/after remote file manifests and equal local hashes. The
compiled manifest pin covers every relative filename and file byte, including
status, tail, validator history, LOCK, LOG, MANIFEST, journal, and all tables.
Framing is sorted relative filename, NUL, lowercase SHA256, newline. Its digest
is `417424c8075ea3dcfd166eddaad8f5012c2510797abeca7f4f0864d399f0fd86`.
The retained whole capture SHA256 is
`32e6d66108205625e2521d0f9afe7b0dbe934574b34f75a8ed80bd4c00eb2281`.
The complete genesis, terminal block capture, and terminal native ABCI result
capture have separate compiled exact-byte pins in `terminal.go`. These are
actual acquired source hashes, not hashes copied from a claimed handoff.

The terminal reference is thus a pinned independent closed replay reference,
not independent consensus signatures for a nonexistent successor header.
Changing any original file, even a nominally nonfinancial byte, fails closed.

## Native proof path

`run` accepts `--closed-terminal-reference`, `--terminal-reference-copy` and
`--terminal-proof` only for chain cosmoshub-1, exact target500015, comparison
mode, and a private start at500000. Next-header mode is mutually exclusive.
All writable paths are checked for overlap, including symlink ancestors,
before any directory creation, recorder marker, database open, or output.

The supplied reference is never database-opened. Its existing lock is acquired
shared while making an exclusive new byte copy at the explicit retained copy
path. Source and copy manifests must match the compiled pin. Only that copy
is opened read-only. Every actual loaded native IAVL root is compared against
its committed metadata, and the native MultiStoreProof recomputes the combined
root. The reference preserves all nine raw stores and all256 first-byte query
partitions, complete native header/block, complete typed ABCI results, and
original status/tail. Present empty values and unknown keys are retained.

Every new native block still validates parent, native before-state, prior
results, validators, proposer, last-commit slots and evidence. Complete native
ABCI results must match the actual cached result before commit. Final native
commit/root, complete validator history, last block/result hash and every raw
ending-store query response must equal the independent pinned closed reference.
Root equality alone cannot seal an observed financial recording.

The terminal coin stream ends with `observation_closed_terminal_completed`,
phase `segment_closed_pinned_native_reference`. History has the distinct
`closed_terminal_history_completed` marker. The final standalone proof and
status flags are written only after recorder sealing and successful evidence
closure. Acceptance also requires successful process exit receipts and the
independent complete stream verifier, never a completion-looking partial file.
`state_hash_verified_through` remains500014; the distinct
`closed_reference_verified_through` is500015. Both proof modes are explicit.
All private copies, partial outputs and failed attempts are retained.

## Whole financial verification

Two NEW independent private copies of the accepted500000 checkpoint execute
the actual fifteen heights, one untouched Gaia and one lossless observer.
This does not repeat the accepted500000 export or its payout proof.
Both new ending checkpoints produce complete native SDK500015 zero-height
exports. That export is novel diagnostic evidence, not a next-chain opening.
Terminal export requires the same closed reference and all-nine raw binding.

`TestTerminalNativeAuditDecode` independently decodes all16 untouched-Gaia
source/committed all-store states with native amino. Then, separately,
`TestTerminalNativeFinancialContext` reads those raw states into NEW in-memory
native stores and uses native read APIs/pure decimal math with each actual
cached header. No block, keeper transition or source database is re-executed.
It retains all accounts/all coins, validators/delegations/shares, unbondings,
redelegations, pool, fee state, native minter/provision, commission and outstanding
reward context. Empty raw values become present non-nil empty native bytes.

`verify_terminal_fifteen.py` compares every native/observed response and full
raw audit, all16 native states, every observer phase/delta and complete source
Set/Delete/cache-write journal, all15 completed heights, explicit terminal
seals, and the entire typed native/observed export. The financial context is
bound to exact raw native input lines and actual header hashes. The actual
cached tail has zero transactions. It verifies unchanged all-coin accounts,
staking principal/shares and fees, and all15 exact native minter/pool supply
transitions. Any invented fee, principal/share transaction or paid-income row
in that actual empty-transaction tail is refused, not classified from a signed
fee or a residual. Deferred commission/reward state is retained, not called a
paid bank payout. Hypothetical terminal export payouts remain explicitly
separate from ordinary committed history and tax acceptance remains false.

Unit controls cover each CLI guard independently, source byte and path aliases,
missing last height, wrong root/result/header, raw keys/unknown prefixes/query
identity, phase/mode substitution, invented fees/principal/shares/income,
complete typed export keys/types/values/array order, and default proof isolation.
Original default Go/native/helper coverage remains required, unchanged in scope.

## Actual chain boundary, without losing the tail

The existing complete native500000 export to official Hub2 genesis comparison
is retained and reused, not re-executed. The new verifier actually reads and
hash-binds the original five artifacts: source and next genesis, native export,
500000-to500001 native checkpoint proof, and complete zero-difference migration
receipt. It checks their exact source height, root, next height and full-genesis
comparison links. The canonical migration is500000. Recording all15 retained
later Hub1 blocks is still mandatory, but their terminal500015 diagnostic export
must not replace that migration source or be imported as new income. The old
migration comparator continues to reject any export source other than500000.

## Activation boundary

This source does not alter the e999 prefix release, shared manifests, existing
Jobs, sources, claims, observers or control loops. Use only a new immutable
image and fresh resources for any eventual terminal Job. Main must read the
fresh complete root diff and activation before any such cluster launch.
