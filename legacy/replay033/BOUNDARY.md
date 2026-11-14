# Hub 1 native terminal export component

The default `scan` and `run` commands are unchanged. Nothing here imports
financial records, modifies production, or constitutes complete accounting,
staking-share, genesis, or tax acceptance by itself.

## Required offline source

Use the preserved committed height-500000 checkpoint, never the live replay
home. All original files and failed attempts remain retained. Do not open an
active database with another process. Capture and compare source file hashes
before and after every proof/export. New proof and export files must be
outside the checkpoint, exclusive new files with mode 0600.

`checkpoint-proof --cache "$CACHE" --chain cosmoshub-1 --home "$CHECKPOINT" --out "$PROOF"`

This is the already established Hub 2 independent proof implementation ported
to the pinned Hub 1 SDK module and release. It copies the existing source
under the existing LevelDB lock, opens only its retained private copy, checks
all nine actual loaded persistent roots against commit metadata, then binds
the root, parent block, results hash and validator set to the unexecuted next
header. It never executes the next block or rewrites the original status.
For this boundary require checkpoint/proven height 500000 and next header
500001. A height-500015 tail is not a replacement for the 500000 checkpoint.

## Complete native zero-height export

`replay033 export-zero --home "$CHECKPOINT" --out "$NATIVE_EXPORT" --require-height 500000`

The pinned SDK v0.33.2 `ExportAppStateAndValidators(true, nil)` performs its
native zero-height preparation in check-state caches. The database is opened
read-only under its normal lock; no Commit occurs. The entire app-state JSON
and native validator array are preserved. Validator interface public keys
and power fields use the native amino codec, not standard Go JSON. Metadata
identifies the pinned release, exact source height, pre-export application
root and zero-height mode. The source must already have a regular LOCK file;
even a malformed source must not acquire one from the LevelDB opener.

Errors retain their exclusive partial/empty artifact for diagnosis and return
nonzero. Never treat a partial file as a completed export or delete it to
reuse the name. A retry requires a fresh path and an independent explanation.
The comparator rejects absent/partial JSON and wrong provenance.

## Exact full-genesis comparison

Run `compare_zero_export.py` with `--source-genesis`, `--next-genesis`,
`--native-export`, `--checkpoint-proof` and a fresh `--receipt` beside the
native export. The export's pre-export root must match the independent
500000-to-500001 proof exactly. The comparison receipt binds the root and
raw SHA256 of that proof, so another same-height home cannot be substituted. It runs on
the analysis host; Python is not added to the replay runtime image.

The source and next-genesis raw SHA256 inputs are pinned. The only migration
is the unmodified public SDK v0.34.0 script and its library, vendored under
`testdata/hub1-migration/`, also SHA256 pinned. Sources:

- https://github.com/cosmos/cosmos-sdk/blob/v0.34.0/contrib/export/v0.33.x-to-v0.34.0.py
- https://github.com/cosmos/cosmos-sdk/blob/v0.34.0/contrib/export/lib.py

Every JSON key/value, scalar type and array position is compared. Duplicate
keys and nonfinite tokens fail closed. Numeric values retain arbitrary
precision; equivalent number spelling and object-member ordering do not
create an artificial raw-byte gate. Raw source/export/genesis files are not
normalized or rewritten. Every difference is retained privately by JSON
pointer, kind and complete subtree hashes, without printing financial values.
Only the two public migration arguments, next chain ID and genesis time,
come from next-genesis metadata. No next-genesis account or staking value is
used to construct the candidate.

A successful comparison is one complete native-export/genesis component,
not proof of observed payout classification or a shares-denominated movement
ledger. Those independent boundary and accounting proofs remain necessary.
