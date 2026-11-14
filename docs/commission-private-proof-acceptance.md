# Private commission proof acceptance

Execution completion is not financial acceptance. Neither this proof nor a
subsequent full private generation publishes totals, replaces serving rows,
repairs financial source rows, or authorizes statement stage 2.

The current seven-validator Job and every snapshot, including errors, must be
retained. Do not restart it, overwrite its version, resume the intentionally
stopped defective tracers, or omit a validator to make a generation complete.

## Seven checks before the next full private generation

1. Verify the exact seven expected validators, build digest, chain and dates
   against the actual immutable snapshots. Account for every error separately.
   A successful engine status is evidence of execution only.
2. Compare commission origins by month with independently read, chain-scoped
   native source rows. Every month and amount must agree. Check both allocation
   rules against those origins, not just the grand total.
3. Require exact per-origin FIFO conservation. Record every pro-rata decimal
   residual explicitly. The corrected pool retains positive small remainders;
   do not round old residuals away or alter old results to make them pass.
4. Require known opening and closing holdings and no unresolved destinations.
   Approximate or height-unbound ledger reconstructions must fall through to
   exact archive reads in private mode. Each wallet records its opening and
   closing source. Retained archive responses must survive SHA-256 verification;
   the auditor links the wallet, height and day boundary to its raw receipts.
   Exact heights, chain and pagination are also checked by the strict runtime.
5. Review actual wallet-level overdraw and drift against native cash/source
   receipts. The engine's per-wallet drift tolerance is not acceptance of cash
   accounting. A prior wallet list or matching aggregate is not proof of the
   current wallet set. Legacy snapshots without wallet accounting evidence
   cannot alone settle current wallet-level residuals. New private outputs
   retain each comparison without performing extra source reads.
6. Verify source and serving preservation fingerprints. Keep missing-ledger
   findings separate from tracer defects. Do not repair source rows, narrow
   asset history/readiness, change a busy-wallet cutoff to conceal drift, or discard debits.
7. For a correction, retain the original failed or discrepant result, test the
   exact offending retained input and relevant whole-wallet read, run regression
   and append-only PostgreSQL tests, obtain independent review, and pin a new
   signed build. Coordinate the exact new Job and target-specific monitoring
   objects with the parent before a fresh diff and ship. No duplicate scheduler.

These checks protect attribution, original basis, source coverage and immutable
evidence. Unexplained material cash differences remain an accounting blocker,
not a reason to label a reconciled status financially accepted. Source gaps
may be classified without repairing them, but attribution affected by them
must remain explicitly unaccepted. No automatic public promotion follows.

## Continuation fixes

A retained GATA dependency used the complete voucher trace on the outbound row
and its exact ICS-20 SHA-256 denomination on the refund row. Native debit and
refund receipts establish the same owner and coin. Matching normalizes only a
complete valid trace to its hash, preserving owner, packet, amount, chronology,
uniqueness and native-asset distinctions. It does not match by base denom or
remove non-native coverage. Clearing this one wallet does not prove GATA's
route membership or full accounting.

Pro-rata pool removal discarded positive remainders below a small threshold.
The new pool retains them, keeping every commission origin. The old output is
preserved, its residuals reported, and no exact causal decomposition of old
aggregate residuals is inferred merely from the regression reproduction.

Payer fee reads resolve the unique address to its numeric payer before planning.
For whole UTC-day windows on one chain, independently located day boundaries
add a redundant height range while retaining the original timestamp predicates.
Unproved or incomplete boundaries, multiple chain aliases and intraday windows
keep the unbounded-height query. Failed transactions and fee-only transactions
remain included. Validate performance changes against the exact offending input
and compare all ordered fee rows within the same read-only snapshot, not just
counts or totals. The native snapshot deadline is unchanged.

`scripts/commission-proof-audit.py` is an offline, read-only aggregate verifier
for privately exported snapshots. It never returns financial acceptance. Its
context file pins version/build/chain/from_day/as_of and expected validators;
`as_of` is the inclusive result date, unlike the native Job's exclusive flag.
