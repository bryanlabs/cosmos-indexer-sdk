# Explicit suspected-spam asset review

Methodology v9 keeps suspected-spam and identity-mismatch receipts visible with
their reported labels, original denomination, amount, wallet, transaction memo,
and available source-chain trace. Suspicion is not a finding of fraud or a tax
classification. No token identity, valuation, or exclusion is preselected.

## Stable per-record decisions

A flagged preview row exposes `asset_identity.record_id`:

```
mainnet|<wallet>|<message_id>|<sub_index>:<source-fingerprint>
```

Copy the supplied ID; do not construct one from ticker names. The fingerprint
binds the decision to the original hash, denomination, amount, timestamp,
category, counterparties, memo, trace, and displayed source identity evidence,
so changed source facts invalidate an old decision rather than silently reusing it. Decisions apply only
to matching flagged records in the requested wallets and date range. Unknown IDs,
invalid fields and conflicting modes are rejected. Original indexed records are
never updated by this feature.

The optional `asset_decisions` query parameter is a JSON object. The web
application uses the camel-case `assetDecisions` request-body field and forwards
it to the SDK. Keys come in three kinds, and the most specific one wins for each
receipt:

| Key | Reaches | Override shape |
| --- | --- | --- |
| `<record_id>` | that one receipt | totals: `quantity`, `value_usd`, `cost_basis_usd`, `acquired_date` |
| `denom:<raw_denom>` | every flagged receipt of that token in the report | `token` and `unit_value_usd`; each receipt is priced quantity times unit, basis equals value, acquisition date is the receipt day |
| `policy:spam` | every receipt flagged with reason `spam_memo` and no more specific decision | exclude only |

`spam_policy=valueless` is query shorthand that inserts `policy:spam` as an
exclude decision; `spam_policy=ask` (the default) inserts nothing. A decision a
rule produced is materialized per receipt and carries `applied_from` with the
key that produced it, so the audit CSV and the preview show whether a person
decided the receipt or a rule did. `applied_from` is server-set and rejected on
input. An unknown record ID is rejected (400) because it is stale or foreign. An
idle `denom:` or `policy:` key is accepted and listed in
`unmatched_decision_keys` of `/asset-review`, so a remembered rule can be sent
with every report.

Each entry is one of:

```json
{"mode":"exclude"}
```

```json
{
  "mode":"override",
  "token":"MYTOKEN",
  "quantity":"2",
  "value_usd":"20",
  "cost_basis_usd":"6",
  "acquired_date":"2026-05-01"
}
```

```json
{"mode":"override","token":"MYTOKEN","unit_value_usd":"0.5"}
```

The override token is free text, not an ATOM/USDC preset or registry lookup.
Quantity is positive. USD value and basis are separate TOTAL amounts for the
record, both manually entered and nonnegative. Explicit zero is valid; omission
is not zero. The acquisition date must be valid and no later than the receipt.
Plain decimals are bounded to 38 digits and 18 decimal places. At most 500
decisions and 256 KiB of decision JSON are accepted; narrow the report range
when necessary. No excess rows or decisions are silently discarded.

## Reason codes and groups

Every flagged receipt carries `asset_identity.reason_codes` next to the
human-readable `reasons`: `spam_memo`, `label_mismatch`, `unverified_factory`,
or the fallback `identity_mismatch` when none of the three specific signals
fired. The texts of the first three are part of the record fingerprint and
never change; a new signal gets a new code. `/asset-review` returns
`reason_glossary` (code, title, text, explainer, suggested treatment) so a site
or an agent can explain a flag without hardcoding it, and `groups`: one entry
per raw denomination with the reported label, token name, source chain,
receipt and pending counts, first and last date, total quantity, reason codes,
a sample memo, the record IDs, the `denom:` decision key, and the denom-level
decision when one is in force. A person decides per group; the API still
accepts per-receipt decisions for exceptions.

## Preview and export boundaries

- `/report-job` is an internal preview transport, restricted to `summ` or
  `cryptotaxcalculator`. Its JSON contains a preview CSV with dedicated metadata
  columns, not a tax-software download. Pending and user-excluded rows remain.
- `/asset-review` provides readiness and audit evidence for `addresses`, `chain`,
  `start`, `end`, and optional decisions. It returns HTTP 409 with the pending
  count until every flagged receipt is reviewed, and HTTP 200 when resolved.
- `/events`, `/income`, `/8949`, `/schedule-d`, `/990t` and delegator summaries
  validate the same decisions. Tax downloads return HTTP 409 while any required
  review is pending. Invalid decisions return HTTP 400.
- The web application gates export, forms, statements, evidence bundles and
  public events on this readiness check. Backend tax export functions also gate,
  so disabling a button is not the sole control.
- Report cache keys hash canonical decisions as well as the query/methodology.
  Cached previews also recheck a streamed fingerprint of exact persisted financial
  events, transaction memos and payer-side fees across the wallets' full history.
  Historical corrections, inserts, deletions and same-amount fee payer changes
  invalidate previews even when their blockchain timestamps are old. The digest
  is checked before and after computation; changes during computation fail closed.
  An override cannot contaminate the undecided report or another decision set.
  Failed jobs remain terminal until explicit regeneration/resync, not an endless
  automatic retry loop.

## IBC refund basis readiness

Timeout and error-ack refunds are principal returns, not new FMV acquisitions.
Packet-linked restoration of original FIFO lots is not implemented. Authoritative
calculations and cached previews therefore return HTTP 409 with
`readiness.review_required=["ibc_refund_basis_reconciliation_required"]` for
an affected wallet. No tax CSV, positions or phantom disposal result is returned.
Token identity, spam/value decisions and user tags cannot clear this blocker.

The gate reads full persisted chain/wallet IBC history, including refunds after
the requested end date, to block a matching earlier send and later inventory or
real spends. Packet identity uses source port/channel/sequence for v1 and source
and destination clients/sequence for v2. Known unrelated packets, wallets and
chains are not linked by amount, token or timestamp. Legacy outbound rows can use
exact retained `send_packet` evidence. Missing or ambiguous legacy identity in
a wallet with refunds adds `ibc_outbound_packet_identity_required` until replay
restores the evidence. The gate does not certify unindexed refunds or off-chain
basis and does not perform the missing lot reconciliation.

## Semantic and parser/decode readiness

Token identity and value review is separate from economic classification review.
Unknown contract claims retain observed transfer legs, `unknown_claim` as the
source trigger, and `review_required="true"` with
`review_reason="unknown_claim_semantics"` in detail. These rows are not certified
as income or returned principal. Asset exclusions, price overrides, denom rules,
spam policy and user tags do not resolve this semantic blocker. Actual classifier
review and successful reindexing are required; no tax-policy override is inferred.

`/asset-review` returns `semantic_reviews` plus a `readiness` object containing
`export_ready`, `parser_decode_complete`, `pending_asset_reviews`,
`pending_semantic_reviews`, `parser_errors`, `decode_errors` and `scope`. Its
`pending` count sums asset and semantic review obligations, so a row with both
issues counts twice. `/report-job` still reports `status="ready"` when its preview
CSV is computed, with `preview_only=true` and the separate readiness object.
The preview's `Taxable` column is `unknown` for unresolved semantic rows.

Retained message parser errors and failed-message/transaction decode evidence
block downloads and cached previews with HTTP 409 JSON, `status="blocked"`,
`readiness` and an explanatory `error`, never a stale prior CSV. Checks run before
cache reuse and again after background computation. Wallet attribution uses
retained signers, fee payers, classified legs, validator addresses or exact
message-event values, scoped to chain and period. Unattributable failed
transactions appear in `/coverage` parser/decode counts rather than blocking
every unrelated wallet. Counts represent affected transactions, not each nested
parser exception. Archive completeness, unsupported opaque payload coverage,
missing prices, basis and off-chain history require separate checks.

Cached report storage adds `readiness_json`, covered by the existing `ReportJob`
AutoMigrate. Methodology v9 and a readiness generation in the report key invalidate
old previews; semantic review markers also participate in current fingerprints,
including claims denominated in the native token.

## Effects of explicit choices

Exclusion sets an explicit user-supplied zero price, value, and basis while
preserving the original quantity and immutable source identity evidence. It keeps
`Excluded=true`, so the receipt is omitted from financial calculations, FIFO, and
native-asset totals. It remains in previews and the Generic CSV audit export with
its metadata and zero treatment. Other tax-software import formats continue to
omit it because they may auto-value unsupported assets. Exclusion does not delete
an on-chain record or erase an actual transaction fee.

Override applies the user's token, quantity and total USD value. Its separate
manual basis and acquisition date feed FIFO. The original asset evidence remains
attached, and basis derived from user input is labelled as user-supplied, not
chain-verified. An override does not change the original transaction category or
physical balance snapshots.

The Generic CSV has explicit manual value/basis/date/treatment columns and
retains excluded rows at explicit zero value and basis. For an explicitly excluded
identity mismatch only, its `asset` display uses the trimmed issuer token name
when it is not `ATOM` or `uatom`; an empty or ATOM-like unverified name becomes
`UNVERIFIED (denom: <full raw denom>)`. This is a Generic CSV display aid only:
issuer names are not unique asset identity and do not establish native-ATOM
equivalence. The original reported label, raw denomination, preview evidence,
and record ID remain unchanged. User override tickers remain authoritative,
including an intentional `ATOM` override. Manual source CSV values preserve
sub-cent precision; currency-formatted tax PDFs may round for presentation.
Other formats retain the decision in their existing description or note where
supported.
CoinTracker's fixed quantity-only import has no metadata/value/basis columns;
retain the review audit and enter basis in the destination software if necessary.
The evidence ZIP includes `asset-review-decisions.json`, and the preview can
separately download a review audit even while reviews remain pending.

Saved-report choices use owner-scoped JSONB storage. Saving a decision in a saved
report updates only that owner's report query, not a global asset registry. A new
unsaved report keeps decisions in its active query; use Save report to retain it.
Reset removes the explicit decision and blocks exports again until re-reviewed.

## Verification

`assetreview_http_test.go` runs a disposable PostgreSQL/HTTP fixture covering
pending gates, retained previews, explicit exclusion, arbitrary token overrides,
manual basis/date FIFO, exact zero values, source immutability, unknown-ID
rejection, cache separation, and (`TestDenomAndPolicyDecisionsFanOutToReceipts`)
denom and policy fan-out, precedence, `spam_policy` shorthand, groups, and the
reason glossary. Frontend Node/SSR tests verify no selected mode
or default token, escaped memos, metadata integrity, and explicit manual values.
