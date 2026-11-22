# Private commission source corrections

The optional `commission-native-source-corrections-v1` manifest incorporates
already-proven native effects into the commission reader, not the production
financial source. The bounded authority comprises the six retained corrected
wallet windows, their fee transaction union and six native reward credits.
The union is explicit and deduplicated by native transaction; authority counts
are validated separately, not added on an assumption of disjointness.

This is not whole-fee-universe, all-wallet, current-lot or public acceptance.
Neither an empty intersection for one observed wallet nor a completed private
collection certifies other wallets or financial accuracy.

## Loading and immutable identity

`taxledger` requires both `--commission-source-corrections` and
`--commission-source-corrections-sha256`, along with the existing exact private
classification profile and private version/build/window options. The source
manifest's window and base profile must match that generation. The version
must contain both `-profile` followed by the base profile's first twelve hash
characters and `-sources` followed by the source manifest's first twelve.

The stored profile identity hashes both complete profile digests. A result
cannot be reused under a different correction manifest, build or date window.
`taxcommissionexport` takes the same source-correction flags as the collector;
its existing `--private-profile` flags still identify the base profile.
Neither tool promotes a result or changes serving tables.

Manifests are bounded regular, non-symlink files. The loader rejects a wrong
hash, unknown fields, trailing data, unknown amounts/refunds, duplicate native
identities, unbound memberships and missing BANK overlap proof declarations.
Native proof provenance and the separately reviewed offline normalizer are
pinned in the private manifest and retained alongside the source fixtures.
Private fixtures, addresses and native amounts are not committed to this repo.

## Actual trace-read integration

Corrections are evaluated inside `readReportInput`'s existing repeatable-read,
read-only snapshot after its original readiness and wallet reads. They never
write fees, taxable events, classifications, balances, labels or ledger rows.

For a known fee transaction, the original indexed payer fee is deferred, then
its actual source transaction, native hash, height, code and time are checked.
The full generic fee source must contain only the proven indexed/actual actors
and expected nominal coin vector. Unsupported fee grants fail closed. Native
charged minus native refund must equal the retained net expense. Only the
actual charged actor receives that net fee; the incorrect indexed actor does
not. Receipts distinguish a genuinely removed original actor fee from a proved
absent original fee row. The actor's exact source event fingerprint must still reproduce the
retained no-BANK-double-count footprint. A changed source is an error, not an
excuse to reapply an old correction or guess zero.

Native headers retain nanoseconds while PostgreSQL timestamps retain
microseconds. Matching follows the existing archive contract, a difference
strictly below one microsecond. Derived flows use the actual validated indexed
timestamp, not an invented higher-precision order. Crossing a read boundary is
rejected. A corrected fee resolves the actual indexed outer message0 ID and
is ordered immediately before that message, after earlier source-ordered
messages in the same block. Its negative subrank is a derived ante-fee phase,
not a fabricated indexed ID. Other fees keep their original ordering. The
currently pinned bounded authority contains no nonzero refunds; arithmetic
validation alone is not a claim of native chronology for another authority's
nonzero refund phases.

For a missing reward, the real indexed message ID is resolved from the exact
native transaction and outer-message index/type. No missing indexed ID is
fabricated. The original source fingerprint for that message must still
match. Added native credits follow their retained native event order. Their
flow subranks are explicitly derived, not represented as native/indexed IDs.
The adapter allows this only after proving existing native same-message rows
are positive non-commission reward credits, which commute under both FIFO
and pro-rata. A debit, commission origin or changed footprint refuses the
correction instead of inventing chronology.

Other wallet events and fees remain unchanged. The public/default reader has
no correction profile and follows its original strict readiness path.

## Evidence and tests

Each successful wallet-period read emits a source-profile receipt recording
either bounded consumed native effects or a genuinely observed empty
intersection. Applied effects retain proof hashes and the actual source IDs
resolved during that read, with the exact validated indexed timestamp and
derived flow key retained for offline reproduction. A failed or rolled-back snapshot publishes no
consumption receipts. Each validator trace owns a separate journal, including
when collector workers run concurrently.

The source manifest and read receipts are included in immutable private
commission evidence. A real collection is still needed to measure affected
page roots and financial acceptance. There is no precondition that unrelated
all-tax/current-lot work finish before tracing a wallet.

Focused tests cover native fee replacement, incorrect payer removal including
failed fees without a stored outcome, exact source changes, refund arithmetic,
BANK duplication, native message resolution, source fingerprint changes,
rollback, generation reuse/export identity, per-trace isolation and both origin
rules driven by the adapter's actual converted output. Optional retained-six
fixture tests use the exact accepted source inputs and native companions.
Those fixture tests use SQL mocks for runtime identity lookup, explicitly not
live source queries or completed page traces. Their receipts do not claim a
measured number of affected page roots.
