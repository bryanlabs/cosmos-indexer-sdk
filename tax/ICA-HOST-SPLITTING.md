# ICA host multi-message evidence

`icaHostEvents` separates inner evidence before classifying a committed packet.
It never hands the complete packet log to each inner message. Missing,
contradictory, unsupported or non-unique evidence remains a
`ClassificationIncompleteError`, not a successful empty classification.

## Proofs implemented

- Ordered events: supported staking, withdrawal, commission, redeem and direct
  send handlers have matching terminal events. Every economic event must belong
  to a completed inner operation, with no trailing remainder. Direct sends cannot
  swallow another handler's withdrawals or transfers.
- Merged historical logs: the existing legacy handler/sender trace proves
  transfer boundaries. Staking auto-withdrawals reconcile ordered distribution
  payments and validator identities. Explicit zero withdrawals are retained.
- Legacy Exec wrappers: flatten one level, prove each leaf's handler boundary,
  then pass the partition to the authz parser with local child indices. These
  indices are internal routing, not fabricated chain evidence; stored logs are
  unchanged.
- Indexed Exec plus body-only siblings: exactly one outer Exec owns the tagged
  records, including nested leaf-first indices. Every untagged bank record must
  reconcile to an explicit sibling send. Multiple outer Execs with reused child
  indices are unresolved.
- Redeem-only historical batches: distinct record identities, matching share
  sends, principal returns, burns, per-validator distribution withdrawals and
  owner payouts prove the partition. Retained bank debits and credits reconcile
  transfers, burns and bank-only native restaking legs. Record withdrawals are
  excluded from the redeemer's own income; the owner's complete payout remains
  income only once. Unmatched positive amounts are never treated as zero.
- Send/MultiSend-only packets retain body-only attribution, including real
  bank logs. Each retained bank event kind reconciles as a complete record
  multiset to the explicit payloads. SDK 0.47 InputOutputCoins emits output
  transfers without sender; SendCoins includes sender. Extra, altered or missing
  records within a retained kind and non-bank economic events veto the shortcut.
  Entirely absent event kinds retain historical body-only compatibility.
- Legacy outer bookkeeping strips only MsgRecvPacket action and its immediately
  following sender (SDK 0.47 baseapp createEvents), plus ibc_channel module
  markers. Inner signer records are preserved, including equal-valued signers.
- Other event-independent packets without bank events retain body-only
  classification, but unexplained economic events veto that shortcut.

## Fixtures and acceptance

`testdata/ica-multi-redeem-legacy-sanitized.json` preserves the structure of a
retained eight-redeem packet. Addresses, validators, record IDs, denominations,
channels and positive amounts are replaced consistently; zero amounts remain
zero. No original packet data, wallet identities or transaction metadata is
included. Tests reconstruct synthetic valid module/validator/record identities.

Regressions cover old/new LSM namespaces, ordered/merged reward equivalence,
multiple denominations, owner payouts versus internal record withdrawals,
nested indexed Execs with a sibling send, legacy Execs, automatic and zero
withdrawals, repeated record ambiguity, missing terminals, trailing transfers,
corrupted rewards/payouts/principal/bank events/burns, contradictory execution
results and input immutability.

This code is not proof that the entire retained packet inventory is now
classified. Legacy mixed redeem/non-redeem packets, nested legacy Execs whose
leaf boundaries cannot be proved, multiple indexed outer Execs, unsupported
terminal/event layouts and any unreconciled bank evidence deliberately remain
unresolved. Inventory-wide read-only replay/skip review must enumerate every
remaining shape, including non-2026 years, before closing OPS-818's required
multi-inner gate. No historical replay, production write, deployment or report
methodology change is performed by this change.
