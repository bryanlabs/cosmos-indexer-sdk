# Path to indexing Cosmos Hub 3, 2 and 1

Written 2026-11-12 and rewritten 2026-11-13 on branch
`ops-814-legacy-intake` (OPS-814). The design is decided in the vault note
`Operations/Systems/Cosmos-Tax-Early-Hubs-And-Site-Plan.md` ("the spec":
decisions D1 to D7, work packages 1 to 8). This file is the builder's side
of it: what exists on this branch, what was measured, what ran where, what
is left, and where the measurements disagree with the spec.

"Measured" means a run on real cached heights or the real genesis files.
"Estimate" is arithmetic on a measurement and says which one. "Designed"
means written down and not built.

Three states are kept apart throughout, as the spec asks: downloaded is not
imported, imported is not verified, verified is not accepted for tax. No
serving entry point admits a legacy chain: `core.LegacyIntakeAdmission()`
still refuses, the API reads nothing in the `legacy` schema, and every era's
publish switch is off.

No wallet address, wallet balance, memo or provider address appears here.
Amounts are totals over all wallets.

## Where things stand

| Piece | State |
| --- | --- |
| Amino transaction decoder, Hub 1 to 3 | Built. Every block and result file of all three chains opened and parsed: 0 failures. |
| Legacy block result reader (tags and events) | Built, run on every height of all three chains. |
| Evidence per message, fee charge on failures | Built (`legacy/evidence`). Failures the results cannot prove are settled by the sequence check, and the last two by archive state. |
| Sign bytes and signature verification | Built (`legacy/signbytes.go`). Whole Hub 3 verified. |
| Account sequence check | Built (`legacy/sequence`, `cmd/legacy-seqcheck`). Whole Hub 3 run: all 183,969 accounts end on the sequence the Hub 4 genesis records. Hub 2 and Hub 1 run only on their first ranges. |
| `legacy` schema and the confined importer role (D2) | Built, tested, applied to production on 2026-11-13 with Dan's approval. |
| Genesis loader, all four files | Built and tested. Hub 3 and Hub 4 files loaded in production. Hub 1 and Hub 2 files loaded only in private databases. |
| Hub 3 importer (`cmd/legacy-import`, `legacy/ledger`) | Built. Two full private runs (the second with 0 heights held), then the production run. |
| Validator removal and fee state proofs (`cmd/legacy-removals`) | Built. 224 removal verdicts and 2 fee state verdicts for Hub 3, each proven on two archives. |
| Books close 3 to 4, spendable balance (`cmd/legacy-close`, `legacy/books`) | Built. On the private copy and in production: 183,949 wallets close exactly, 14 close only with the Proposal 29 genesis change, 0 do not close. |
| Delegation and unbonding comparison per wallet (the rest of D3) | Designed, not built. |
| Hub 2 and Hub 1 tax rows, restart payout rows, their closes | Not built. Decoder, signature check and genesis rows exist. |
| API union, report routing, daily balances for the old hubs | Not built, not on this branch. |

## Hub 3: what ran, in order

Every step names the commit it ran on. A later step is proof only of what
its own commit and image did.

### Signature and sequence pass (commit e75f20f)

`legacy-seqcheck --signatures` over heights 1 to 5,200,791, in a private pod
with no route to the production database. None left out. 2,417,960 fee
paying signatures verified. Every one of the 183,969 accounts ends on its
closing sequence: 0 mismatched, 0 contradictions between a sequence and a
fee charge. 2,119 failed transactions had a fee the block results cannot
prove: 2,113 settled as charged, 4 as not charged, 2 left unsettled (no
later signature by the account). The verdict file has 2,117 lines, sha256
`d8aaaa51ab15c91eb4046ad25747253efa6bae0733ad6192643bd8129f2bc82f`, and is
used unchanged, byte for byte, by every import since.

Seven signatures did not verify. They are described under "Not verified".

### Private run 1 (commit 796bc62)

Heights 1 to 5,200,790 into a throwaway Postgres beside the importer, 1 hour
30 minutes 45 seconds at 11 workers. 5,200,333 heights stored and 457 held:
453 because an unbonding completion carried no amount in the results, 2
because the fee charge was still unknown, 2 because the distribution account
paid two addresses inside one message (a validator removed when its last
delegation left). The close, not recorded because the import was incomplete:
183,963 wallets compared, 183,748 close, 215 do not.

The 215 were split with nothing left over:

- 201 fail only because of the 457 held heights. For each, the difference
  equals exactly what its transactions in the held heights move.
- 0 depend on the two heights where run 1 stored a removed validator's
  commission as a reward (the category changes, the amount per wallet does
  not).
- 14 are the Proposal 29 wallets (below).

### Fixes between the runs (commits a2da170 to ed76405)

- An unbonding completion with no amount in the results is stored from the
  unbonding entry the importer carries forward.
- A message that could have removed a validator needs a verdict line saying
  whether it did and whether commission was paid with it. The verdicts come
  from `legacy-removals`, which asks two archives for the validator's state
  before and after the height and writes a line only when both agree. Whole
  chain: 224 lines on 222 heights (214 not removed, 6 removed with no
  commission, 4 removed with commission), sha256
  `32ce5e9a0c5ca83d32e014fe009292d48b6271f4969226e4cca47b99e74398ab`. The
  importer fails a range when such a message has no verdict.
- The two fees still unknown were settled from the payer's account sequence
  before and after the height on two archives: 1 charged, 1 not charged.
  File of 2 lines, sha256
  `28687e56285c72299e72c2665b1ae02b8fa47f7321fd8c8a961fc0ab279ef4a4`.
- A range is tied to the three verdict files it started with; other files
  are refused.

The prover binary inside the final image reproduced both files with every
raw archive answer identical to the first run.

### Private run 2 (commit ed76405, image digest `sha256:fc3598203a05...6761`)

A fresh database beside run 1's. Heights 1 to 5,200,790 in 1 hour 51
minutes: all 5,200,790 stored, 0 held. 224 removal verdicts matched to a
message, 0 messages that could have removed a validator left without one; 2
fee state verdicts matched. Rows: 2,420,082 transactions, 3,627,963 tax
rows, 50,204 block events.

Close, recorded: 183,963 wallets compared (6 module accounts left out),
183,949 close exactly, 14 do not. Over those 14 the Hub 4 genesis holds
1,186,733,484,000 uatom above the history and 1,318,592,760,000 uatom below
it.

Run 1 against run 2, every table, per height: 459 heights differ. 457 are
the heights run 1 held and run 2 stored. 2 are the heights declared before
run 2 started (675,436 and 708,307, commission stored as reward by run 1),
where only the tax rows differ. Differences outside those two sets: 0.
Heights stored by run 1 and missing in run 2: 0. Run 1's own tables hashed
the same before and after run 2.

### The Proposal 29 genesis change (commit 0507c82, image digest `sha256:7e53571afdc8...c92b`)

The 14 wallets have one cause. The program that wrote the Hub 4 genesis file
carried out governance proposal 29: it emptied seven fundraiser wallets,
credited seven recovery wallets with a fixed amount each and left the rest
in the community pool (`github.com/cosmos/gaia` `v4.0.0` `app/prop29.go`,
sha256 `912e0dcf14808028bc09bee726eeb003216aed069ceaffee530db69ae9fc5519`).
No Hub 3 transaction exists for it.

The close accepts that one file as a closed list, whole or not at all, and
only when: every source wallet holds nothing in the next genesis and
something in the history; every recovery wallet's difference equals its
listed amount exactly; and what the sources held less what the recoveries
were given equals 131,859,276,000 uatom. That number was measured
separately: the community pool in the Hub 4 genesis file less the pool in
Hub 3 state after height 5,200,790, read from two archives that returned
identical bytes, equal to all 18 decimals.

Result on run 2's database: 183,949 close exactly, 14 close with the
Proposal 29 genesis change (counted on their own line and with their own
status, never as closing exactly), 0 do not close. 14 rows were added to the
tax rows at the export height, in the export phase, dated at the last Hub 3
block (2021-02-18 05:59:57 UTC). They are classified as neither income nor a
sale and are marked as needing review.

No third import was run for this image. Its importer, prover, schema,
genesis, census and sequence binaries are byte for byte those of the image
run 2 used (the pod prints the six sha256 values before it starts), only the
close binary is new, and the complete close ran on it against the whole
run 2 database.

Observed, not a rule in the public program: in 7 of 7 pairs the recovery
amount is exactly nine tenths of what the source wallet held.

### Production (approved by Dan 2026-11-13 12:41 UTC)

The approval covers the `legacy` schema and role, the importer's Secret, the
Hub 3 and Hub 4 genesis loads, the Hub 3 import and its close. It does not
cover Hub 2 or Hub 1 rows, deleting anything, or anything people see.

All of it ran from the bare-metal manifest
`cluster/apps/cosmos-tax-sdk-backfill/legacy-hub3-production.yaml`, as Jobs
that connect only as `legacy_importer`. Times are UTC on 2026-11-13.

| Step | Job | Image | Ran | Result |
| --- | --- | --- | --- | --- |
| Schema and role | `cosmos-tax-legacy-schema-20261113` | `fc3598...6761` | 12:57:48 to 12:58:00 | Migration of 355 lines, sha256 `c082bda1...95c7`, applied in one transaction. As the importer: 193 statements against everything outside `legacy`, 193 refused. |
| Genesis loads and import | `cosmos-tax-legacy-hub3-import` | `fc3598...6761` | 13:07:03 to 14:58:34 | Hub 3 and Hub 4 genesis files loaded and read back. Range 1 to 5,200,790 done: 5,200,790 heights stored, 0 held, 2,420,082 transactions, 3,627,963 tax rows. |
| Plain close | `cosmos-tax-legacy-hub3-close` | `fc3598...6761` | 17:17:32, 25 s | 183,963 wallets compared, 183,949 close exactly, 14 do not. The next genesis holds 1,186,733,484,000 uatom above the history and 1,318,592,760,000 below it. |
| Close with the Proposal 29 list | `cosmos-tax-legacy-hub3-close-prop29` | `7e5357...c92b` | 17:24:48 to 17:25:18 | 183,949 close exactly, 14 close with the Proposal 29 genesis change, 0 do not. The pool amount the list leaves, 131,859,276,000 uatom, equals the chain state. 14 rows recorded at height 5,200,790. |

The import ran with 4, then 8, then 11 workers, and went back to 8 for the
last 56 minutes when the memory pressure line of the shared database was
crossed. It never paused.

What was compared, and with what:

- The import used the same three verdict files as private run 2, checked by
  sha256 before the first height.
- After the import, a per-table row count and md5 over every row, computed
  with one script on both databases: `blocks`, `tx_inclusions`,
  `taxable_events`, `block_events`, `eras` and the five genesis row tables
  are identical to private run 2. `genesis_files` and `import_ranges` differ
  only in their time columns and are identical without them.
- The plain close printed the same 31 lines as on the private copy (sha256
  `89936524...f557`), and `reconciliation` then had the same 183,963 rows
  and md5 as the private copy after its plain close.
- The close with the list first printed the sha256 of its seven programs
  (six identical to the image the import ran on, the close program the one
  tested on the private copy) and of the list file. It printed the same 33
  lines as on the private copy (sha256 `79198dd1...1e781`). Afterwards
  `taxable_events` (3,627,977 rows) and `reconciliation` have the same row
  counts and md5 as the private copy after its close.
- The importer role was read three times (at the apply, after the import,
  after the closes): it owns nothing, belongs to no group and holds no right
  on any of the 47 tables or 32 sequences outside `legacy`. The tables
  outside `legacy` cannot be compared by checksum, because the live indexer
  writes them all the time.

Evidence is kept on the private run's volume under `/scratch/prod` (the run
log, the close logs and outputs, the table captures of both databases, the
role readings, with `SHA256SUMS` files) beside the private run's own
evidence in `/scratch/out2` and `/scratch/out4`.

State of Hub 3 in production: imported, and the spendable balance of every
wallet closes against the cosmoshub-4 genesis, 14 of them only with the
published Proposal 29 change. It is not verified beyond that (see the end),
not shown on the website, and not accepted for tax.

## Measured numbers

All counts are from `cmd/legacy-census`, which prints aggregates only, or
from the stored tables.

### Decoder coverage on real heights

| | cosmoshub-1 | cosmoshub-2 | cosmoshub-3 |
| --- | --- | --- | --- |
| Heights with a block and a result read | 500,015 | 2,902,002 | 5,200,790 (5,200,791 by the signature pass) |
| Heights with transactions | 14,552 | 462,628 | 1,572,934 |
| Transactions | 20,228 | 662,621 | 2,420,082 |
| Transactions that decode | all | all | all |
| Transactions with every message projected | 19,737 | 662,621 | all but 1 (below) |
| Captures that failed to open or parse | 0 | 0 | 0 |

Hub 1 and Hub 2 are full passes: every result file was opened, including
those of heights with no transactions. The Hub 3 column is what the
production importer's twin stored in run 2, which reads every height.

Hub 1 message types: MsgWithdrawDelegationReward 11,915, MsgDelegate 10,253,
MsgWithdrawValidatorCommission 2,611, MsgVote 446, MsgBeginRedelegate 359,
MsgEditValidator 322, MsgCreateValidator 142, MsgUndelegate 132, MsgMultiSend
58, MsgSend 18, MsgDeposit 18, MsgModifyWithdrawAddress 7, MsgUnjail 7,
MsgSubmitProposal 5. The 491 transactions not fully projected are the 359
redelegations and 132 undelegations, which state their amount in validator
shares, not coins; the signer is known and the amount needs the validator's
exchange rate. 919 transactions failed; 417 of them have a fee the results
alone cannot prove. One transaction carries a public key that is not its
signer's and is handled.

Hub 2 message types: MsgSend 421,629, MsgWithdrawDelegationReward 209,863,
MsgDelegate 150,552, MsgMultiSend 16,721, MsgUndelegate 10,182,
MsgWithdrawValidatorCommission 6,301, MsgBeginRedelegate 4,678, MsgVote 924,
MsgEditValidator 709, MsgModifyWithdrawAddress 375, MsgCreateValidator 321,
MsgDeposit 89, MsgUnjail 42, MsgSubmitProposal 17. 490,904 transactions
succeeded. 1,601 failures have a fee the results alone cannot prove, and 9
are proven not charged from the ante handler's own refusal. 363 transactions
were signed by a multisig key.

Hub 3 message types, from the census pass that ran while the download was
still going (1,802,721 transactions): MsgWithdrawDelegationReward 912,030,
MsgSend 775,879, MsgDelegate 688,500, MsgUndelegate 46,402, MsgMultiSend
30,299, MsgWithdrawValidatorCommission 14,481, MsgBeginRedelegate 5,313,
MsgVote 3,402, MsgEditValidator 401, MsgModifyWithdrawAddress 198,
MsgCreateValidator 174, MsgUnjail 64, MsgDeposit 39, MsgSubmitProposal 20.

Found and fixed by these runs:

- A fee coin with amount 0 and no denom. All three chains accepted it,
  because a fee that is all zero skips both validation and deduction. It
  decodes as a fee of nothing.
- A failure the ante handler returned because the payer could not cover the
  fee is proven not charged, from the error's own wording and form.
- A Hub 3 delegation signed with an amount and no denom, at height 210,960.
  The chain let it into a block and the staking handler then refused it. It
  decodes as signed, its fee is proven charged, and its signature verifies.
  It is the one Hub 3 message the decoder does not project.
- Hub 3 transfer events record the payer apart from the transfer.
  `evidence.TransferSenders` puts it back.
- On Hub 1 and Hub 2 an unjail message was signed as its bare value with no
  type name, unlike every other message. The sign bytes follow that.

### What the tags hold on Hub 1 and Hub 2 (measured)

| Message | Hub 1 | Hub 2, to height 479,159 | Hub 2, from height 479,179 |
| --- | --- | --- | --- |
| Withdraw reward | delegator, validator. No amount. | delegator, validator. No amount. | delegator, validator, `rewards` amount |
| Withdraw commission | validator. No amount. | validator. No amount. | validator, `commission` amount |
| Delegate, undelegate, redelegate | parties only | parties only | parties only. No amount for the rewards they pay out. |
| Send, multisend | (transfers were off) | sender, recipient. Amount is in the message. | same |

The switch is sharp in the stored results: the last reward withdrawal
without an amount is at height 479,159, the first with one at 479,179, there
is none between them, and no height mixes the two. Over the whole of Hub 2,
183,626 successful reward withdrawals carry an amount and 23,499 do not;
4,999 commission withdrawals carry one and 1,281 do not.

Why the amounts appear part way through (verified in the SDK source):
`x/distribution/handler.go` in cosmos-sdk 0.34.0 and 0.34.3 emits no
`rewards` or `commission` tag; 0.34.4 to 0.34.7 do. Tags were not part of
consensus, so whether a stored result has the amount depends on the software
the archive node ran when it executed that block. Both providers return the
same amount-less tags for two early heights, so neither has a copy with
amounts for the early range. A replay of Hub 2 from height 1 with 0.34.4 or
later should record the amount of every explicit withdrawal (unverified: not
run). It would still not record the rewards paid out by a delegate,
undelegate or redelegate, which no version tags.

### State the providers serve (measured 2026-11-12, both providers, read only)

Probed with the raw store query on real account keys taken from the genesis
files (values never printed).

| | Hub 1 | Hub 2 |
| --- | --- | --- |
| Heights that answered with a real record | 10,000, 20,000, 250,000, 490,000, 500,000, and 499,950 and 500,010 | 10,000, 150,000, 1,000,000, 1,400,000, 2,900,000, and 2,901,950 and 2,902,000 |
| Heights that answered "version does not exist" | 1,000, 5,000, 15,000, 499,000, 499,900 | 1,000, 15,000 |
| Latest state height | 500,014 | 2,902,001 |

So both keep state at every multiple of 10,000 and for about the last 100
heights, and state at both export heights (500,000 and 2,902,000) is
readable. Hub 3 serves state at any height: the removal, fee state and
community pool proofs above used it on two archives.

### The four genesis files (measured, `legacy-genesis`)

All four are on the cache volume in `/cache/genesis` with a `SHA256SUMS`
file, and all four match it. Their origin is the `genesis/` folder of the
`cosmos/mainnet` repository on GitHub.

| | cosmoshub-1 | cosmoshub-2 | cosmoshub-3 | cosmoshub-4 |
| --- | --- | --- | --- | --- |
| Accounts | 984 | 1,027 | 25,182 | 183,969 |
| of which vesting | 45 | 45 | 45 | 45 |
| of which module accounts | 0 | 0 | 6 | 6 |
| Delegations | 67 (from the 67 genesis transactions) | 1,112 | 13,838 | 76,776 |
| Accounts with a delegation | 67 | 409 | 7,908 | 57,674 |
| Unbonding entries | 0 | 60 | 841 | 9,033 |
| Validators | 67 | 105 | 204 | 268 |
| Withdraw addresses set | 0 | 0 | 84 | 193 |
| Validators with unpaid rewards / commission | none | none | none | 205 / 189 |
| Unbonding entries keep their creation height | n/a | no (all zero) | no (all zero) | yes (up to 5,200,782) |
| Accounts with a sequence above zero | 0 | 423 | 22,609 | 157,295 |
| Sum of sequences | 0 | 20,297 | 682,908 | 3,102,982 |

Checks the reader makes on each file and all four pass: no account twice,
only uatom, every delegation names a listed validator and a listed account,
the delegations to each validator add up to its `delegator_shares` exactly
(Hub 2, 3 and 4), balances add up to the stated supply (Hub 4, the only file
that states one).

What the files show about the three restarts:

- The cosmoshub-2 and cosmoshub-3 files are zero height exports, taken at
  heights 500,000 and 2,902,000: no validator has unpaid rewards or
  commission, and every delegation starts at height 0 (1,112 of 1,112 and
  13,838 of 13,838). The export paid every delegator's pending rewards and
  every validator's commission into balances, with no transaction.
- The cosmoshub-4 file is not. 205 validators still carry unpaid rewards and
  189 unpaid commission, and unbonding entries keep their heights. Nothing
  was paid out at the third restart, and the close 3 to 4 confirms it:
  183,949 wallets close with no payout row at all.
- Sequences carry across every export. The differences are the number of
  fee paying signatures each hub must contain: about 20,300 on Hub 1,
  662,600 on Hub 2 and 2,420,100 on Hub 3.
- The 67 genesis transactions of Hub 1 are self delegations made before
  height 1. They are in no block, so the loader turns them into validator
  and delegation rows and takes them off the liquid balance.

### Do the earlier handovers change balances in code, like Proposal 29?

Checked from the public sources and the genesis files: no. Neither the Hub 1
to 2 script (cosmos-sdk v0.34.0 `contrib/export/v0.33.x-to-v0.34.0.py`) nor
the Hub 2 to 3 migration (cosmos-sdk v0.37.4, `x/genaccounts/legacy/v0_36`
and `x/genutil/legacy/v036`) names an address or an account amount. The
first changes only settings, the chain id and the start time. The second
removes the two old governance holding accounts, adds six module accounts
and copies every other account with its coins unchanged. Both handovers do
change balances, by one general rule: the zero height export pays out all
unwithdrawn rewards and commission.

Not verified: that neither genesis file was edited by hand after the public
tools ran. Only a per wallet close can show that, and only if the restart
payout is proven on its own (see the Hub 2 design note below).

## Chain boundaries

| Chain | Cached heights | Export taken at | SDK | Result form |
| --- | --- | --- | --- | --- |
| cosmoshub-1 | 1 to 500,015 | 500,000 | 0.33.x | tags |
| cosmoshub-2 | 1 to 2,902,002 | 2,902,000 | 0.34.x | tags |
| cosmoshub-3 | 1 to 5,200,791 | 5,200,790 | 0.37.4 | events plus a per message log |

Hub 1 and Hub 2 ran on for a few blocks after the state that became the
next genesis was taken. Nothing that happened in those blocks reached the
next chain. The design is to store them like any other block and give them
no tax rows; whether they hold transactions was not counted separately. The
last Hub 3 block, 5,200,791, is in the cache and is empty; the first
cosmoshub-4 block has the same height number. The Hub 3 import and close
stop at 5,200,790.

A block is identified by chain and height, never by height alone. Rows are
ordered by era (1 to 4), height, phase, transaction index, message index
and sub index.

## Storage: the `legacy` schema (D2). Built, in production

`legacy/schema/schema.sql` is the whole change. `legacy-schema --print`
writes it: 355 lines, sha256
`c082bda1b214f209b774ac7b3bc22dcedbcb451140d2fe2452a7501e1d0e95c7`, the
same in every image used for a proof. It touches no existing table.

| Table | Key | Holds |
| --- | --- | --- |
| `legacy.eras` | chain_id | era number, first height, export height, whether that export paid out rewards, and the publish switch (default off) |
| `legacy.blocks` | (chain_id, height) | time, block hash, proposer, transaction count |
| `legacy.tx_inclusions` | (chain_id, height, tx_index) | hash (indexed, may repeat), code, fee, fee payer, whether the fee was charged and the evidence for that, memo, signers, raw bytes |
| `legacy.taxable_events` | (chain_id, height, phase, tx_index, msg_index, sub_index) | the value columns of the serving tax row, plus `amount_source` |
| `legacy.block_events` | chain, height and position | begin and end block events that say something happened to an account or a validator |
| `legacy.genesis_files` | chain_id | checksum and row counts of the file that was loaded |
| `legacy.genesis_accounts` | (chain_id, address) | account number, sequence, kind, liquid, delegated, unbonding and the vesting fields |
| `legacy.genesis_delegations` | (chain_id, delegator, validator) | shares, and tokens at the validator's rate |
| `legacy.genesis_unbondings` | (chain_id, delegator, validator, entry) | creation height, completion time, initial and remaining balance |
| `legacy.genesis_validators` | (chain_id, operator) | tokens, shares, status, unpaid rewards and commission |
| `legacy.genesis_withdraw_addresses` | (chain_id, delegator) | where that delegator's rewards are paid |
| `legacy.reconciliation` | (chain_id, address, component) | expected, computed, difference, status |
| `legacy.balance_days` | (chain_id, address, day) | holdings at the end of the day (empty; not built) |
| `legacy.import_ranges` | (chain_id, from_height) | range, status, counts, resume height, build, the verdict files it started with |

Phase orders the rows of one height: 0 genesis (height 0), 1 begin block, 2
transaction, 3 end block, 4 a change made by the state export or by the next
genesis. Rows outside phase 2 carry transaction index 0 and message index 0.

How the schema keeps the promise "the import cannot change a row people
already see":

- The importer connects as `legacy_importer`, which is granted usage of the
  schema, select on its tables, insert on the tables that hold what the
  chains recorded, and insert, update and delete on the three working
  tables (`reconciliation`, `balance_days`, `import_ranges`). It is a
  member of no role and is granted nothing else.
- `schema.Confined` reads the catalog as the connected role and lists every
  way that role could reach outside the schema or rewrite a written-once
  table. Every legacy writer calls it before its first write and refuses to
  run on any finding.
- A trigger on the block, inclusion, event and genesis tables refuses
  update, delete and truncate for every role, the owner included.

Tested on private Postgres 15.8 and 16.4: the migration and a second run of
it leave every row, row version, column and trigger of the serving schema
unchanged; the statements the importer must not be able to run are each
denied; the same transaction hash is stored on two chains; a connection that
can reach the serving tables is refused before any write.

Proven in production at the apply: as the importer, 193 statements against
the 47 tables and views outside `legacy` (select, insert, update and delete
on each, create table in every schema, create schema, update and delete
inside the written-once legacy tables) were all refused. The catalog, read
as the owner, shows the role with 0 rights on 47 relations and 32 sequences
outside `legacy`, owning nothing and member of nothing. Like every role in
that database it may create session temporary tables, which vanish with the
session.

One consequence to know: the importer cannot change or remove a tax row it
wrote. Replacing estimated rows with exact ones later is therefore a
reviewed step run by the owner, not something an importer Job can do.

Undo, while no era is published: `DROP SCHEMA legacy CASCADE` and `DROP ROLE
legacy_importer` as the owner remove everything this work added and nothing
else.

## The close against the next genesis (D3)

Built, for the spendable balance of Hub 3 (`legacy/books`): for every
address, the liquid balance in genesis 3, plus every stored row and every
charged fee up to the export height, must equal the liquid balance in
genesis 4, to the uatom. One line per address goes to
`legacy.reconciliation`, closed or open. An address that does not close is
listed with its difference and is never passed.

What the spendable balance close proves and does not: it proves every
movement of liquid coins of every wallet, including reward and commission
amounts, delegations out, completed unbondings back in and fees. It does not
compare delegation shares per delegator and validator, or open unbonding
entries, with the next genesis. A wrong delegation amount would show in the
liquid balance, so the exposure is narrow, but that comparison is designed
and not built:

| Component | Rule | State |
| --- | --- | --- |
| Sequence | closing sequence minus opening sequence equals the fee paying transactions signed | Built, whole Hub 3 passes |
| Liquid | opening liquid plus stored movements and fees equals the next opening liquid | Built, Hub 3 result above |
| Delegations | per delegator and validator, shares after every delegate, undelegate and redelegate equal the next opening shares | Not built. Needs the validator's rate at each height, which moves when it is slashed |
| Unbonding | open entries equal the next genesis entries by completion time and balance | Not built. The importer already carries entries forward to pay completions; comparing the open ones at the end is the missing step |

### Restart payouts, Hub 1 to 2 and Hub 2 to 3 (designed, not built)

The export withdrew every delegator's pending rewards and every validator's
commission into balances. One synthetic row per receiving address and kind,
in the export phase at the export height, dated at that block's time.

The spec defines the amount as the next genesis balance less the balance at
the export height. Taken that way the payout is a residual and would absorb
any unexplained difference, which is what was ruled out for Proposal 29. So
the payout needs its own evidence: the archive's pending rewards per
delegation and commission per validator at the export height, summed per
receiving address under the export's own rounding. The close then requires
balance at the export height plus that computed payout to equal the next
genesis balance exactly, and a wallet with any remainder stays not
verified. On Hub 2 rewards go to the withdraw address, which has to be
followed from MsgModifyWithdrawAddress (375 on Hub 2) because the genesis
files carry none; on Hub 1 withdraw addresses were disabled and the
delegator is paid. Both export heights are inside what the archives answer.
The two governance holding accounts removed by the Hub 2 to 3 migration and
the six module accounts it adds are left out of the close and counted.

## Reward amounts the results do not hold (D4)

Measured: on Hub 1 nothing carries a reward amount. On Hub 2 explicit
withdrawals carry one only from height 479,179 on, and the rewards paid out
by a delegate, undelegate or redelegate never do. Hub 1 undelegations and
redelegations are stated in validator shares, not coins.

In the order the spec sets, with what the probes add:

1. Import everything exact: transfers, delegation principal, fees,
   withdrawals that carry an amount.
2. The liquid difference at the close, less the separately proven restart
   payout, is the exact total of an address's amount-less payouts on that
   chain.
3. The 10,000 height checkpoints are usable. The balance of an address at
   two neighbouring checkpoints bounds its amount-less payouts inside that
   window (about 19 hours). A window with one such payout gives its exact
   amount. Windows with several keep an exact total and an inexact split,
   stored as `window_allocated`.
4. A replay with pruning off makes the rest exact (package 8).
5. Shares to coins on Hub 1: the validator's tokens per share from state,
   1.0 unless that validator was slashed before.

Step 2 and the rule above pull against each other and the order matters: the
restart payout must be proven first from archive state, or the remainder in
step 2 is not evidence of anything.

## What is left, in order

### Hub 3

1. Delegation and unbonding comparison against genesis 4. Estimate 2 to 4
   days, unmeasured.
2. Daily balances (`legacy.balance_days`), the API union and report routing
   (packages 4 to 6 of the spec). Not on this branch.
3. Review of the 14 Proposal 29 rows and the decision how reports show them.
4. Turning the era's publish switch on. That changes what people see and is
   its own approval.

### Hub 2, then Hub 1 (package 7). None of this writes production yet, and none is approved

1. A classifier that reads tags: transfers and delegation principal from
   the message, reward and commission amounts from the tag where it exists.
   Estimate 2 to 3 days.
2. Withdraw address tracking from height 1, and the Hub 1 share conversion.
   Estimate 1 day.
3. The whole hub signature and sequence pass. It must read Hub 2 from
   height 1 without a gap, because the account numbers of accounts created
   during Hub 2 are in no genesis file and are found from the order of
   creation (verified only through height 149,999). Estimate under 1 hour of
   run time for Hub 2 from the Hub 3 rate.
4. Restart payout proof from archive state at 2,902,000 and 500,000 (above),
   and the payout rows. About 13,800 delegation and 200 validator reads on
   Hub 2 and about 1,100 and 100 on Hub 1 (the counts in the next genesis
   files), at the archives' allowed rate. Estimate 2 to 3 days with the
   rounding.
5. Checkpoint windows for the amount-less payouts. Estimate 2 to 3 days.
6. The same private run, comparison and close as Hub 3, then a production
   import on the image that closed. Import run time under 1 hour for Hub 2
   and minutes for Hub 1, from the Hub 3 rate per transaction.

All estimates are unmeasured.

### Package 8: replay. Not started

Needs a peer or a data folder from a provider. The message to send is in
the spec, section 5.

## Where this differs from the spec

1. No restart payout at Hub 3 to 4. The cosmoshub-4 genesis keeps unpaid
   rewards, commission and unbonding heights, so it was not a zero height
   export, and 183,949 wallets close with no payout.
2. The Hub 4 genesis did change 14 balances in code (Proposal 29). The spec
   does not know of it. It is handled as a closed, pinned list with its own
   count, never as a general adjustment.
3. Exports were taken at 500,000 and 2,902,000; the chains ran a few blocks
   longer. Blocks after the export are stored and give no tax rows.
4. The restart payout must be proven from archive state, not taken as the
   difference between the two balances (above).
5. A payout row needs the withdraw address and the reward and commission
   split, so "one row per address" is one row per receiving address and
   kind.
6. `amount_source` needs `message` and `genesis`.
7. Checkpoint state every 10,000 heights exists on both early hubs, so most
   of what D4 leaves to a replay can be bounded per window first.
8. The schema has six tables the spec does not list (`eras`,
   `genesis_files`, `genesis_unbondings`, `genesis_validators`,
   `genesis_withdraw_addresses`, `block_events`), and the importer cannot
   rewrite tax rows.
9. The block results alone do not settle Hub 3. Three verdict files made
   from signatures and from archive state are inputs of the import, pinned
   by sha256: 2,117 fee verdicts, 224 validator removal verdicts, 2 fee
   state verdicts.
10. The import is one Job over one range with a worker count that can be
    changed while it runs, not one Job per disjoint range.
11. Hub 1 has 491 undelegate and redelegate messages in shares, not 11, and
    20,228 transactions, not 439. The spec quotes the first small sample.

## Steps that write production data

Done with approval: the schema and role, the Hub 3 and Hub 4 genesis rows,
the Hub 3 import, its close lines and the 14 Proposal 29 rows, all in
`legacy`. Still to come and each its own approval: Hub 2 and Hub 1 rows,
payout rows, and turning an era's switch on. Every other step reads the
cache, an archive or a private database.

## Not verified

- Delegation shares and open unbonding entries per wallet against genesis
  4. Only the spendable balance is closed.
- The seven signatures the whole hub pass reported as not verifying. A
  second look at every signer of a failure with an unproven fee (2,121), with
  the sequence the stored history gives each: 2,116 verify at that
  sequence; 5 verify only at an older one (4 one behind, 1 thirteen behind).
  Those 5 are single message transactions the chain itself refused with
  "signature verification failed" (4 sends, 1 undelegation), signed with the
  signer's own key, so a stale sequence and not a wrong key. All 5 are
  stored with the fee not charged and carry no tax row. The other 2 of the
  7 verify once the whole history is stored; which two lines of the first
  pass they were was not traced.
- The removal and fee state verdicts rest on two archives agreeing. No
  third source was asked.
- Whether a Hub 2 or Hub 3 genesis file was edited by hand after the public
  tools ran.
- The whole hub signature pass on Hub 2 and Hub 1. Account numbers of
  accounts created during Hub 2 are found by search and are verified only
  through height 149,999.
- A replay with 0.34.4 or later giving Hub 2 withdrawal amounts from
  height 1.
- Whether every slash can be dated and sized from block evidence alone.
- Whether the blocks after the Hub 1 and Hub 2 export heights hold
  transactions.
- The fee and removal verdict files were produced by the commit named with
  each; the whole hub signature pass was not rerun on the final image. The
  final image's import accepted the file byte for byte and closed.
- Every estimate above.
