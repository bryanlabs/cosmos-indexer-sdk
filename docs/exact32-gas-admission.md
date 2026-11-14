# Private exact32 gas evidence admission

This is an offline validation-only exception at the real result-difference gate
(`cmd/legacy-replay-import.admitStatusGas` -> `replayrecords.AdmitDifferences`).
Default admission remains tags-only. No production policy, tax acceptance, data
migration, live enable, replay runtime change or serving API change is included.
`--approved-gas-profile` is incompatible with `--write` and export inputs.

The only loadable profile is SHA256
`11b863d30db5b1f799e5d3bdd1b49325e5a2b1637252e5c33df0e44aedf5c236`.
It is private mode 0600 and deliberately absent from this repository. There is
no caller-selected trusted hash, environment approval switch or +1021 waiver.
`scripts/freeze_exact_gas_profile.py` produces an exclusive private artifact from
the four fixed reviewed inputs. It cannot approve a new artifact. The source
constant and this document must be reviewed and frozen together if that artifact
is ever intentionally replaced.

## Evidence and limits

The profile binds cosmoshub-2, SDK v0.34.10, Tendermint v0.31.11, IAVL v0.12.1,
signed diagnostic source 09a9e29, diagnostic image c9405f56, completed native range
400001..482090 and unexecuted header 482091. It includes original complete 3387
marker-line bytes, retained native gas NDJSON, all 31 original raw block/result
envelopes, all 32 ordered signed transactions and their signed fees, exact native
and cached gas pairs, ordered source-attested non-gas response projections and
actual native validator updates. The entire raw profile is SHA-pinned before
strict decoding, including retained raw inputs and review evidence references.
Canonical signed int64 decoding rejects booleans, floats, exponents, plus signs,
leading zeros, overflow and negative zero before lexical information is lost.

Full native DeliverTx raw bytes were NOT retained. Data, log and ordered tag
equality are source-attested by the completed signed run's comparison markers.
Empty Info and successful Codespace are additionally constrained by the pinned
SDK constructors. These are not two independently retained raw JSON responses.
The completion, state, results, parent and validator proofs are attributed to
the reviewed completed run. This code does not claim a new physical whole-header
or database proof. The genesis max gas is not described as a fresh measurement
of the resumed checkpoint's consensus parameters. Actual completed native
outcomes plus exact headroom and all-position capped gas accounting are required.

The financial observer is a DIFFERENT explicitly pinned accepted lineage:
source 3e087ab429b75f3b6204ce475908025f6c1bdcfe, image
sha256:656638fa97f3792c9ba3cddb4b7f11703da5025d51b22163cc0036dd280781bd.
Observer Git tree and SDK go.mod/go.sum Git blob IDs are bound separately.
Uninstrumented diagnostic image c940 cannot be supplied as the recorder image.
These proof fields are checked inputs, not independent runtime attestation of a
future recording: its actual runtime/source receipt still must be verified by
the private proof owner before supplying it. No future image/version is approved.

## What can pass, and what cannot

Only the exact32 gas differences can pass the difference gate with this profile,
its exact marker file and matching explicit recording lineage. The resulting
receipt names `native_gas_used_diagnostic` separately. Cached GasUsed, raw cached
envelopes and original discrepancy bytes are never edited or regenerated.
Affected cache files are compared to complete retained raw envelopes at intake,
and decoded transaction bytes and raw result objects are checked again.

This does NOT produce native movement records. The ordinary complete-height
stream parser and `core.LegacyReplayBlock` still require all ante fees, native
principal/cause evidence, message execution and committed/aborted reconciliation.
There is no override of those gates. Actual private tests admit the exact gas
packet and then refuse the actual affected transaction without movement records.
No financial ledger or tax result is accepted by a gas-only proof.

Unlisted/33rd, missing, duplicate, reordered, failed, near-limit, late-out-of-gas,
changed non-gas, unknown-field, different-source/runtime/range and incomplete
completion cases remain refused. Historical v0.31 ResultsHash authenticates only
ordered Code/Data. Actual pinned-library tests demonstrate gas/tags exclusion;
independent semantic tests refuse their mutation instead of relying on that root.

## Tests

Public tests are synthetic. Private tests are opt-in via
`EXACT_GAS_PRIVATE_PROFILE` and `EXACT_GAS_PRIVATE_CACHE`, with the latter holding
byte-preserving copies of the 62 reviewed gzip members. These variables choose
input locations, NOT trusted digests. Private mutation tests intentionally rebind
a test-only internal fixture digest so semantic validators actually execute.
No public fixture includes private identities, account values or signed bytes.
Failed logs, first helper source and bounded copied inputs are retained privately.
