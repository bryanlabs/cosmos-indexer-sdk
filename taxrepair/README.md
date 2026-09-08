# Historical staking reward correction

`taxrepair` uses the production reward parser to replace incorrect historical tax
classifications. It defaults to dry-run. It never broadcasts a chain transaction,
changes balances, modifies fees, or alters the stored transaction/event evidence.

## Why this exists

`coin_received` includes delegated principal received by staking pools. Counting
those amounts as staking income overstated rewards. Actual delegator rewards come
from `withdraw_rewards`, including automatic withdrawals during delegation,
undelegation, and redelegation. A redelegation can withdraw from both validators.

Production disabled storage of `messages.message_bytes`. The tool decodes them
when present; otherwise it reconstructs standard staking message identity from
its stored action/sender event and invokes the same parser. Ordered events and
attributes are preserved. For authz without bytes it preserves non-reward tax
records, classifies each actual withdrawal independently, and identifies the
staking trigger from scoped operation events. Unidentified authz triggers are
labelled `authz`, not guessed. Mixed LSM or legacy authz logs without delegator
identity fail closed and require original transaction bodies.

## Safety

- Successful transactions only, pinned upper message-ID snapshot.
- Keyset batches with bulk event and existing-row reads.
- Optional wallet candidate IDs materialized once from existing events and
  transaction signers. This is a database-scoped repair, not independent proof of
  chain completeness. Reconcile the affected wallets against archive transactions.
- Apply requires a NEW backup file. Existing paths are never overwritten.
- Every changed message's original rows, expected rows, message bytes when
  available, and ordered log are written, fsynced, and read back before mutation.
- Each apply batch locks parent messages, verifies the original rows have not
  changed, atomically replaces only that batch's tax rows, then rereads and checks
  all canonical fields. Unchanged messages are not written.
- Tax row IDs can change. No table may reference them; canonical identity is
  `(message_id, sub_index)`. Verify this schema prerequisite before running.
- Empty results delete stale classifications. Idempotent reruns change zero rows.
- A 60-second statement timeout and 3-second lock timeout bound database work.
- Failure stops immediately. Earlier committed batches remain corrected and are
  backed up. Resume using the last printed committed message ID and the SAME
  upper snapshot bound, with a new backup path.
- Store apply backups on a persistent volume and retain a separate local copy.

## Run

Connect with `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`, and `DB_PASS` environment
variables. Do not put passwords in command arguments or logs.

```sh
go build -o /tmp/taxrepair ./taxrepair
/tmp/taxrepair --addresses ADDRESS1,ADDRESS2 --backup /safe/new-plan.jsonl
```

Review the plan, aggregate amounts, and archive reconciliation before applying.
Run the reviewed nullable-column migration in `schema.sql` before applying.
Deploy the corrected live indexer so it cannot create more incorrect rows.

```sh
/tmp/taxrepair --apply --addresses ADDRESS1,ADDRESS2 --backup /persistent/new-backup.jsonl
/tmp/taxrepair --addresses ADDRESS1,ADDRESS2
```

Omit `--addresses` for the database-wide pass. `--batch` defaults to 500 (maximum
5,000). `--from-message-id` is exclusive and `--to-message-id` inclusive. Totals
printed are for CHANGED reward rows, not the entire database. Read current totals
and compare exports separately. Invalidate cached reports only after their
historical data has been corrected, by deploying the new methodology version.

## Verification

```sh
DOCKER_HOST=unix:///Users/danb/.orbstack/run/docker.sock go test -count=1 ./taxrepair
```

Integration tests use a disposable local PostgreSQL container, never production.
They verify principal removal, two-validator reward splits, timestamp/height/hash
preservation, body-free reconstruction, dry-run behavior, backup protection,
concurrent-write rejection, stale-row deletion, and idempotence.
