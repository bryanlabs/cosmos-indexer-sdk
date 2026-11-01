# Cosmos Hub 2025 historical intake

The target year is heights **23763459–29114835**, inclusive, or 5,351,377
blocks. Archive headers verified both UTC boundaries and their neighboring
blocks on 2026-11-01. The first January block is 00:00:04.587503692Z; the last
December block is 23:59:59.087526315Z. This is a target, not a completeness claim.

## Pipeline and accounting

`taxindexer` writes generic blocks, transactions, fees, messages and ordered
events to Postgres, then runs `tax/parser.go` for economic records. `taxapi`
prices those records and serves income, validator statements, books, exports
and forms to the platform `/tax` routes. Reward recognition defaults to claims;
principal movements are not staking income. Cross-chain acquisition basis and
unknown asset identity are not fabricated merely because an event is indexed.

Interchain Security provider receipt/allocation events are protocol accounting,
not a second wallet income recognition. Actual distribution withdrawals can
contain consumer-chain IBC denominations. The provider module's v28 removal and
v29 store deletion do not erase historical transactions. ICS721 NFT bridging,
Interchain Security and liquid/interchain staking products are separate things.

## Historical versions

Release dates in `gaia_versions.go` are CHANGELOG dates, not activation dates.
Verified upgrade block timestamps differ from proposal voting/release dates.

| Gaia line | Effective dependencies | Historical concern |
| --- | --- | --- |
| v21, carry-in | SDK 0.50.9-lsm, ibc-go 8.5.1, ICS 6.3.0 | SDK staking/distribution LSM namespaces |
| v22 | SDK 0.50.11-lsm, ibc-go 8.5.2, ICS 6.4.0 | Same legacy LSM wire shapes |
| v23 | SDK declaration 0.50.13 replaced with 0.50.11-lsm; ibc-go 10, ICS 7 | Both IBC generations and old LSM |
| v24, height 26131400 | Transitional SDK fork, ibc-go 10.1.1 | LSM migrates to `gaia.liquid.v1beta1` |
| v25, height 26440900 | SDK 0.53, wasmd 0.60.1 | x/liquid continues; it did not originate in v25 |

`LegacyLSMMsgTypes` separately registers the nine old SDK message identities.
The tests exercise all wire shapes, original-byte retention, mixed authz,
aggregate legacy reward settlement and principal/income separation. Registering
only the new Gaia namespace misses actual early-2025 messages. Historical
release entries remain explicitly unreviewed until the broader audit finishes.

## Evidence and request safety

Historical intake enables `[flags] index-tx-message-raw` and message events.
Unknown messages retain their original URL, index, bytes and ordered events as
opaque evidence. They are not silently dropped or classified by guessing.
An upsert with raw capture disabled cannot erase existing bytes. A nonzero fee
without a resolvable payer defers its block instead of creating an empty address.

The optional archive HTTP guard serializes actual HTTP calls across workers,
sets a timeout and total request budget, disables redirects and transparent
reused-connection retries, and exits nonzero on HTTP 403/429 or exhausted budget.
It requires bounded input and zero RPC retries. Default live behavior is
unchanged. A stopped run retains committed progress but is not complete.

The 2026-11-01 public CC Validators probe stopped on a 429 after 46 requests
paced at least 1.1 seconds apart. Earlier short probes had succeeded. Published
2025 operator terms state 5 requests/second per client IP, with higher throughput
available upon request; that old SLA is not proof of a current unrestricted
allocation. No dedicated Clemens endpoint was verified in available records.
Polypore timed out twice. Do not rotate endpoints or retry around a throttle.

Crucially, `tx_search` returned zero transactions at 24180999 and 26131399,
although block bodies contained two transactions each. For the bounded intake,
`skip-block-by-height-rpc-request=true` bypasses transaction search and uses
block bytes plus `block_results`. The decoder requires matching transaction and
result counts. Independent verification must compare those sources, not merely
count messages already stored in our own database.

## Contract discovery

Historical REST at height 29114835 returned 380 code IDs across four exhausted
pages, versus 36 at the first January block. A bounded 60-request investigation
sampled 28 code-specific contract lists (234 addresses), 20 contract-info
responses and ten histories. All response heights matched the requested height.
This is not a complete contract or execution census.

Missing candidates include earlier Skip entry point
`cosmos1zvesudsdfxusz06jztpph4d3h5x6veglqsspxns2v2jqml9nhywshhfp5j`
and paired adapter
`cosmos198plfkpwzpxxrlpvprhfmdkcf3frpa7kvduq9cw8lh02mm327tgqhh3s55`,
created at 25140966 and 25140957. The entry point's code hash matches an already
registered Skip entry point. DAO staking, Polytone, vesting, CosmoCrow escrow,
a self-labeled DEX and LocalMoney candidates also exist. On-chain labels alone
are not audited protocol identity, taxable treatment or proof of user activity.

Do not project current Hydro Hub contracts back into 2025: the official 2025
Hydro configuration used Neutron. Drop's ATOM instance also has Neutron
contracts. Hub IBC/ICA effects and remote-chain protocol activity need separate
attribution. Existing `FirstSeenHeight` values are production-index observations,
not deployment dates. Unknown `claim` semantics, IBC error/refund handling and
cross-chain execution attribution remain audit items before year-wide reporting
can be certified.

## Acceptance and continuation

Start with an explicitly listed, low-rate canary spanning release boundaries,
including the archive search gaps, then the January 1 tranche (23763459–23778060).
Require exact expected block/transaction counts, original message identities and
raw bytes, ordered events, zero failed blocks and no duplicates. A job exiting
successfully alone is insufficient. Keep the live tip indexer unchanged.

Do not launch the whole year merely because a canary works. Validate storage
headroom from measured bytes per block, confirm archive throughput, inventory
unknowns, and publish remaining gaps. Reports generated while the year is
partial must not be described as complete; saved previews may need resync.
The deployment, scripts and progress runbook live in bare-metal
`cluster/apps/cosmos-tax-sdk-backfill/`; OPS-671 remains the broader genesis
backfill/coverage ticket.

## Sources

- Gaia tagged `go.mod`: v21.0.1, v22.0.0, v23.0.0 and v24.0.0.
- SDK v0.50.11-lsm `proto/cosmos/{staking,distribution}/v1beta1/tx.proto`.
- Gaia v24 liquid proto and ICS v7 provider events/distribution implementation.
- https://forum.cosmos.network/t/milestone-report-cosmoshub-archive-node/16304
- Official Hydro deployment configuration pinned to 2025 commit
  `50e9a5a20c501ef9a431be1928181cb16af83b32`.
- Public-chain preflight artifacts retained separately in the infra evidence
  directory for 2026-11-01; no credentials belong in this repository.
