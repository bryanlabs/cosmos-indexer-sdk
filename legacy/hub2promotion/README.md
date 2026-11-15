# Atomic Hub1/Hub2 promotion

Dan approved independent Hub1 and Hub2 live loads on Nov15 15:49: “yes, load
hub one hub two and production when their balance checks pass”. This replaces
old approval-packet requirements. Complete financial source coverage and strict
all-account/all-denomination balance equality still must actually pass.

`cmd/legacy-approved-load` is the shared production adapter, with only static
cosmoshub-1 and cosmoshub-2 scopes. It uses the original tested atomic core, not
the private global-empty importer. `ApprovedLoad` binds the actual canonical
native export and next genesis. The final financial acceptance binds every SQL
projection, source/component, build/binary, genuine genesis and canonical payout
hash/count. No new approval signature or receipt service is required.

Hub2 additionally uses `cmd/hub2-migrate036`, an isolated dependency module that
calls the official SDK v0.37.4 v036 migration. The real migration removes two
old governance base accounts from the transformed state and creates six module
accounts with coins calculated from the exported fee, staking, governance and
distribution state. Original files and SQL evidence are never deleted. The
adapter recomputes and verifies the pinned transformation, then demands zero
missing accounts, extra accounts and denomination mismatches against the actual
retained Hub3 genesis. It never waives differences or compares the later tail.
The financial report also binds `migrated_export_sha` and `migration_binary_sha`.

Live operations are `validate`, `scope-sql`, coordinated `install-scope`, `load`
and read-only `audit`, with an explicit pinned manifest, private0600 connection
file and fresh physical runtime identity. No environment DSN fallback. Scope
installation first verifies the actual server identity in its transaction.
Do not install production metadata while the Auth export-cache rebuild is active;
coordinate credentials with that owner afterward. Actual loading remains blocked
until full source and balance checks pass, not until another approval packet.
No serving or publication change is supplied here. The existing
`cmd/hub2-private-rehearsal`, `legacy/schema`, decoder and `legacy/ledger` are
unchanged. This package never calls the old batch-committing ledger writer.

Priority: accuracy first, then speed. Do not sacrifice accounting correctness,
existing coverage or evidence. Equally, do not turn that priority into endless
preparation, artificial approval loops or monitoring without implementation.

## Entry points and evidence

`cmd/hub2-load-plan` is offline. It requires an absolute manifest path and an
independently pinned SHA256. Default admission requires the entire 1..2902002
chain, canonical migration/payout at 2902000, and distinct historical tail
2902001..2902002. `--fixture-only` accepts only the visibly synthetic <=100-height
profile. Neither mode opens a database or returns production-ready status.

`MergeFixture` accepts only that bounded synthetic profile in a specifically
named private fixture database. `MergePrivateScale` is separate, accepts only
the retained accepted 1.4M SQL prefix in a private database, and requires ZERO
canonical/migration/tail claims and ZERO export payout rows. It is a SQL scale
test, not another native replay or a whole-chain financial proof. Both use the
same atomic core. There is no production-enable switch or credential fallback.

`ExportProjection` reads an already accepted private SQL staging database in one
READ ONLY snapshot, binds its actual database/OID/system identity, and writes
NEW exclusive retained files. It does not decode or replay native inputs.

Complete admission checks every fixed table/column/count/file SHA and streams
the actual full projection. Typed, nonfixture full-range source/module/store/
check/component envelopes must retain and bind the independent owners' original
proof artifacts. A final financial receipt must be independently owner-attested
with an OpenSSH SSHSIG under namespace `hub2-complete-financial-v1`, verified
against the ED25519 authority independently checked on the signed base. No
private key or signing operation occurs in admission. A new authority requires
a reviewed code change. Its signed artifact inventory binds every underlying
file hash/path to its actual source/module/store/check/component scopes and role.
Shared all9 proof artifacts are allowed only when the owner explicitly attests
those scopes. Genesis, source wrappers, projection and package metadata cannot
substitute for native-proof artifacts. Do not impose artificial uniqueness on
genuinely shared accepted native proof, and never sign synthetic full evidence.
Format-only test fixtures use an ephemeral untrusted key, not the real signer.
The signed financial body must be EXACT `FinancialReceipt.CanonicalBytes()`
(JSON from the typed Go value). This rejects repeated/merged map keys, alternate
case spellings, unknown hidden claims and representation ambiguity. Review can
render that immutable file, but must never rewrite the signed bytes. The signer
attests the entire BuildIdentity SHA as well as source/binary and all projections.
Sibling envelopes and native build inputs may be retained with role `input`, but
cannot themselves fill native/source-proof roles.
Envelopes bind genesis, native provenance, source, build,
seams and canonical/tail policy. The final financial seal additionally binds
the complete component list and every SQL projection SHA, not just the payout.
The initial-genesis and seam checks bind the accepted initial/last roots and
exact component list; native image/gas checks bind their actual pinned profile.
Build receipts must identify an actual clean Go binary whose embedded VCS
revision matches its signed source commit. The execution owner still verifies
the Git signature. Native binaries, immutable image and gas profile are pinned.
`BuildReceipt.ImageDigest` names the accepted native-source image; the new PG/
promotion runtime image is separately pinned in the eventual deployment packet.
These contracts do not create financial acceptance: real final owner seals and
full SQL staging are still missing, and synthetic metadata cannot pass complete
admission. Do not wrap unaccepted data in an envelope and call it proof.

Tail tax rows are preserved as actual Hub2 history. They are not omitted or
reclassified as migrated Hub3 opening balances. This package performs NO Hub3
insert or migration. Only the genuine financially accepted canonical phase4
payout may enter once, at the canonical height. Intermediate zero-height
exports remain validation artifacts, never income. Old `books.Close` is not
used as whole-module/all-coin acceptance.

## Database boundary

`Scope.SQL` creates NEW uniquely named roles/schema/ten simple insert views and
one static no-argument SECURITY DEFINER lock helper. It fails on existing names;
it never replaces existing schema or triggers. It grants legacy schema USAGE
and SELECT on the exact14 existing relations for verification. A nonlogin view
owner holds SELECT/INSERT on the ten base targets. A separate nonlogin lock
owner holds UPDATE on14 base relations and on10 scope views only to take locks.
The authenticated loader has SELECT/INSERT only on CHECK OPTION Hub2 views,
read-only legacy verification access, no base DML/CREATE/ownership/memberships,
and no callable alternate user-defined definer. Owner roles cannot log in or
be assumed by the loader. Every role/view/helper definition, owner, ACL and
relationship is checked and pinned. No production scope objects exist yet.

PG16 PUBLIC TEMP is real. PostgreSQL cannot deny it to just this role. Nothing
revokes global PUBLIC rights or claims TEMP is absent. The core fixes pg_catalog first in search_path, qualifies every relation and
user-defined object, and explicitly qualifies critical projection/identity casts. Hostile temp-shadow tests are
required. The function checks detect newly granted user-defined execution paths;
this is not a claim that arbitrary future administrator-granted PUBLIC powers
can be denied per-role without changing global privileges.

One pinned physical session holds advisory340310 plus an identity-derived
Hub2-specific key. One REPEATABLE READ transaction holds14 SHARE ROW EXCLUSIVE
base-table locks in fixed order, then ROW EXCLUSIVE locks on all10 scope views.
The latter prevent view rewrites/INSTEAD OF triggers during streaming, without
granting UPDATE to the loader. Lock acquisition is bounded. Ordinary public
SELECT/append activity continues; unrelated public row counts are not frozen.
Synchronous commit is explicitly ON.

A SELECT-invoked helper can establish RR before its internal LOCK. The code does
not pretend otherwise: the authoritative baseline is taken by a separate fresh
READ ONLY auditor AFTER all locks. RR must match that full-row/xmin/ctid baseline
and the retained manifest fingerprints before any insert, or refuse. A fresh
READ ONLY auditor repeats actual catalog/role/function confinement after the
last insert, because RR catalog SQL alone could miss concurrent grants/DDL.
Every auditor/export/unknown-outcome READ ONLY transaction pins pg_catalog first,
UTC, bounded lock and statement timeouts. Catalog digest functions are explicitly
pg_catalog-qualified, even against exact-type public overloads.

All chunks remain in the SAME transaction. No upsert, conflict-ignore, adoption,
UPDATE, DELETE, era/publication/reconciliation/balance/import-range change, or
chunk commit occurs. All13 preexisting Hub2 cohorts conflict, even equal rows.
Before COMMIT, the ten exact full projections,13 cohort counts, full14 protected
physical/content fingerprints, eras, genesis and final source hashes must match.

## Failure and recovery

Precommit failures roll back only this attempt's NEW rows, never delete evidence.
Lost sessions never reconnect to continue the transaction. Unknown COMMIT is
terminal and never auto-retried. `AuditOutcome` uses a fresh READ ONLY connection
and reports `exact`, `empty` or `conflict` against the sealed package and protected
baseline. It never adopts, retries, repairs or writes. A committed wrong result
has no automatic reversal. Keep it unpublished and preserve evidence; any
recovery needs separate explicit approval and a NEW isolated restore target.

Actual tests use ONLY unique `pgtest.OpenRetained` databases on the established
private PG16 helper. Missing private fixture configuration FAILS, never skips.
All nine required groups include populated H3/H4/all14 protected content,
13 preexisting cohorts, every view/base-DML/public/role boundary, identity/source
negatives, contention/public concurrency, late/last-table/cancel/session death,
unknown COMMIT outcomes, source/canonical/tail negatives, and pg_dump/restore
into a NEW retained private database with exact14 schema/ACL/rows. Extra tests
cover stale RR races, late grants/definers, view DDL, inherited TEMP, durability,
source export and actual backend peak-memory/WAL-upper-bound measurement.

Run the exact `-race -count=1` package targets with a private0600 fixture env
file and NEW0700 retained `HUB2_PROMOTION_ARTIFACT_ROOT` plus persistent TMPDIR.
No DSN belongs in shell output, logs, Git or an approval packet.

## Historical private implementation requirements (approval now superseded)

The independent final complete financial/canonical seals and full final SQL
projection; one populated-destination 1.4M SQL scale proof and measurements;
explicitly reviewed full-root private scale Job diff before any large launch;
reviewed signed reproducible binary; refreshed production destination and
concurrency inventory; actual production backup and exact isolated restore;
The old Dan-YES-for-a-new-packet requirement is superseded by the Nov15 approval.
Schema/ACL installation is coordinated with Auth, while serving/publication,
native/commission changes and offsite work remain separate. Never claim complete
production acceptance until actual source coverage, balances and physical SQL
readback pass.
