# cosmos-indexer-sdk — agent guide

Fork of DefiantLabs `cosmos-indexer` (+ `probe`). The BryanLabs work lives on the
**`tax-layer`** branch, which is what ships — `main` tracks upstream.

## What this is
A Go app that indexes any Cosmos SDK chain into a generalized Transaction/Event
Postgres schema, plus a BryanLabs **tax layer** on top. It is the backend for the
website's **/tax** section and replaced the retired `cosmos-tax-cli`.

## What runs in production (built from this repo)
Image `ghcr.io/bryanlabs/cosmos-tax-sdk` (built from `Dockerfile.tax`), deployed in
k8s namespace `apps` as two trios (mainnet + a parallel `-testnet-*`):
- `cosmos-tax-sdk-indexer` — writes txs/events into Postgres (`taxindexer`).
- `cosmos-tax-sdk-api` — serves the tax HTTP API (Service `:8082`) (`taxapid`).
- `cosmos-tax-sdk-postgres` — the database.

## The BryanLabs tax layer (the part you'll usually touch)
- `taxapi/` — the HTTP API: `server.go` (routes), `assetreview_runtime.go`
  (`/asset-review` readiness/audit, decisions keyed by record ID,
  `denom:<raw>` or `policy:spam`, groups per token and the reason glossary in
  `reviewreasons.go`; see `taxapi/ASSET-REVIEW.md`), `balances.go` (`/balance` = live
  node read via `NODE_REST_API`, persisted to `balance_snapshots`), `form8949.go`,
  `form990t.go` (reports/forms), `stakinginflation.go` (`GET
  /staking/inflation`, INF-748: ATOM minted per day, month or year from every
  cosmoshub-4 block's mint event, where it went, supply, stated vs actual
  inflation and stated, paid, after-commission and real APR; serves the
  embedded `stakingdata/inflation_daily.json`, built from raw block results by
  `scripts/staking-inflation/` (extract, merge with its checks, export; README
  there); replace the file and rebuild to extend it until the daily job writes
  a table). `mystaking.go` (`GET /staking/my-staking`, INF-749): one
  holder's wallets per validator for a period. Rewards are the `/income`
  rows (same filter, `reconciliation` proves the sum), commission paid is an
  estimate from `tax_staking_validator_days`, yield from those days over
  `tax_validator_rates` tokens, votes from `proposal_vote` events (two reads,
  the second with seqscan and hashjoin off) against the node's proposal
  list, commission changes from create and edit validator messages.
  Slashing stays null until the ledger records it. Private: no cache.
- Methodology v8 surfaces (2026-10-15): `tax/catalog.go` (everything the
  indexer watches, served as `GET /catalog`; the website renders it as
  `/tax/coverage`, so a new category, contract or tag belongs here first),
  `taxapi/attestation.go` (validator jurisdictions, on-chain only: the
  scanner reads successful `txes` whose memo starts with
  `bryanlabs-tax:jurisdiction v1 country=XX [entity="..."] [source=https://...]`
  and records one when the signer is the operator account behind a
  cosmosvaloper address; latest block wins, `country=none` revokes, the tx
  hash is the source; `GET /validators/jurisdictions` lists them with the
  validator's current status, `GET /validators/attest/check` validates a memo
  and names the signer; reports take a `JurisdictionFunc` and everything
  without an attestation is `undisclosed`; nothing is ever seeded or matched
  by moniker), `tax/gaia_versions.go` (the reviewed Gaia releases and upcoming changes) with
  `taxapi/chainreleases.go` (the daily watcher that records
  `chain_release_checks` and flags unreviewed releases; `GET /chain/releases`),
  `taxapi/assetreview_runtime.go` `applyUserPrices` (user-entered USD for
  price-missing rows through `price:<denom>@<day>` and `price:<row key>`
  asset decisions, labelled `price_source=user` in every output; see
  `taxapi/ASSET-REVIEW.md`),
  `taxapi/tags.go` (the `tags` query parameter on every export and report,
  vocabulary in `tax/catalog.go`), `taxapi/journal.go` (double-entry USD ledger
  in date order with `price_usd`, `counterparty`, `validator`,
  `validator_country` and `methodology_version` per line),
  `taxapi/positions.go` (open lots, USD rounded to cents, loads the whole
  history so out-of-window `asset_decisions` are accepted),
  `taxapi/validatorstatement.go` (monthly commission and delegator rewards over
  the `idx_tax_validator` partial index; `coverage` reports the index bounds
  and the replay cursor, which skips LSM reward rows the replay never
  rewrites; months that intersect a block gap are `incomplete` too).
  `taxapi/indexcoverage.go` (`GET /index-coverage`: per UTC year block
  range, indexed and expected counts, fully covered dates, gaps; a
  background scanner reads `blocks` in 100k-height buckets with a
  gaps-and-islands query and serves a cached snapshot, 503 until the first
  one; never count `blocks` per request. A bucket with both flags on every
  height is never read again; one settled for `tx_indexed` only rereads its
  event runs (every pass when events are the criterion, hourly otherwise).
  Snapshots publish every 25 bucket reads, so a pass cut off by its timeout
  still publishes; until every bucket has been read the status is `scanning`
  and a period it cannot vouch for is `unknown`. A period that starts before
  genesis and runs past it is judged from genesis and adds `note` /
  `X-Index-Coverage-Note`; a period entirely before genesis is `incomplete`
  with a warning (earlier Hub chains are not in reports yet). Year start
  heights for cosmoshub-4 are hard-coded in `hubYearStarts`; later years are
  found by binary search on indexed blocks. A block counts when
  `tx_indexed`; `TAX_COVERAGE_REQUIRE_BLOCK_EVENTS=true` also requires
  `block_events_indexed`. Report and export routes are wrapped in
  `withPeriodCoverage`, which sets `X-Index-Coverage`
  (`complete|incomplete|unknown`), `X-Index-Coverage-Warning`,
  `X-Index-Coverage-Incomplete-Years` and `X-Index-Coverage-Note`; `/report-job`, `/positions` and
  `/validator-statement` also return `index_coverage` in the body, evaluated
  per response so cached jobs pick up backfill progress. No route clamps
  dates to the index start).
  Earlier Hub chains (cosmoshub-1 to 3, `taxapi/legacyeras.go`): the API
  reads the `legacy` schema (`eras`, `import_ranges`, `reconciliation`,
  `taxable_events`, `tx_inclusions`, `blocks`, `balance_days`) every 5
  minutes (`TAX_LEGACY_ERAS_INTERVAL`, `TAX_LEGACY_ERAS=off` turns it off).
  An era joins reports only when it and every later era have
  `published = true`, so the set in reports is always cosmoshub-3, then 2
  and 3, then 1 to 3. While none is in reports every response is
  byte-for-byte what it was before eras existed, apart from `eras[]` on
  `/index-coverage` (`TestLegacyErasUnpublishedChangeNothingPostgres`).
  Once one is: legacy rows are read with SQL (heights restart on every
  chain, so order is always era, height, phase, tx_index, msg_index,
  sub_index, never height alone), a genesis row is read only on the
  earliest era in reports, rows past an era's export height are dropped,
  events, 8949, journal, previews and balance statements gain a chain id
  column, years and the date floor move to the earliest era, and past-date
  balances on those chains come from `legacy.balance_days` (refused for a
  wallet whose `reconciliation` row is not `closed*`). Commission flows and
  the validator statement stay cosmoshub-4 only. A missing or unreadable
  schema (no USAGE grant) leaves reports as they are and logs once.
  Every handler normalises `chain` through `chainParam` in
  `taxapi/server.go` (`cosmoshub`, `cosmoshub-4` and `mainnet` are the same
  oracle key); never read `q.Get("chain")` raw. `/events?preview=1` emits the preview
  columns the website parses. A Gaia release that changes event shapes gets a
  review entry in `tax/gaia_versions.go`, not a silent parser change.
- `taxapi/validatorstatement.go`: `/validator-statement` reads the validator's
  reward and commission rows once (`readStatementAggregates`, a bitmap scan
  with read-ahead, one snapshot) for the day sums and the distinct
  recipients. No index holds the amount or the recipient, so cost grows with
  the rows read, about 4 seconds per million on production. A period the
  planner sizes above `TAX_STATEMENT_MAX_ROWS` (default 12 million) is
  refused with 422 before any row is read; the database work is cancelled at
  `TAX_STATEMENT_TIMEOUT` (default 90s, under the 120s edge) or when the
  caller goes away, answering 504. At most three statements are read at a
  time. `validatorstatement_pg_test.go` keeps the three former queries word
  for word and pins the single read to them. Stage 2
  (`taxapi/statementsummary*.go`, `taxstatement/`) requires explicit production
  approval: a Job installs/fills derived day totals and monthly
  recipient sets with statement-level invalidation triggers. The API never
  installs or writes them; it mixes trusted, unmarked whole months with live
  rows in one snapshot and counts unique recipients across their union.
  `TAX_STATEMENT_SUMMARY=off` forces live-only reads without deleting anything.
  Preserve the 12M live-row default and all coverage gates. Real-copy proofs
  are opt-in, use newly retained schemas and never rewrite the preserved base.
  DELETE-shaped fixture proofs must roll back.
- `taxapi/coveragerollup.go`, `taxcoverage/` - `GET /coverage` sums a
  rollup instead of counting every message in the period (the old query took
  4 to 7 minutes and up to 10 GB of temp over all history). Three additive
  tables (`tax_coverage_rollup*`, DDL in `CoverageRollupDDL`, created once by
  the owner) hold message counts and classified counts by UTC day, chain,
  type and id chunk, plus transaction counts by day. Only the `taxcoverage`
  CronJob writes them: it extends to the frontier, rebuilds chunks touched by
  new or changed taxable events, recounts the recent chunks, fingerprints
  every chunk's classified count to catch deletes, and rebuilds the oldest
  chunk each run. The API reads them in one read-only snapshot, recounts
  partial days and the unbuilt tail live, caches by frontier and shares one
  query among identical requests. It falls back to the original live query
  only when the rollup is missing, unreadable or more than 1.5M ids behind.
  The coverage statement adds "Transactions scanned" from the same rollup
  (rollup only, 10 s cap, "not counted" otherwise).
  `coveragerollup_pg_test.go` pins rolled counts to the live query.
- `taxapi/stakingtables.go`, `taxstaking/` - the /tax/staking pages
  (INF-748: inflation, staking income, validator income, unbonding) read per
  UTC day staking sums. Four additive tables (`tax_staking_*`, DDL in
  `StakingTablesDDL`, created once by the owner) hold them. `taxstaking
  -seed-merged -seed-byval` loads the archive extraction once (one
  transaction, refuses incomplete or failing input and non-empty tables);
  the daily CronJob runs `taxstaking -refresh`, which adds whole UTC days from
  our node, one transaction per day, and refuses any day with a missing,
  unreadable or mint-less block or a community tax change without a passed
  proposal. Sums are exact (big.Int at 1e18, the extraction's 60 digit
  rounding); the per-block community tax comes from distribution params at
  h-1. `-check FROM TO` recomputes days from the node and compares every
  column. The API reads the tables only when the state row exists, reaches
  the embedded file's last day and every day from the embedded first day is
  present; otherwise it serves the embedded files, so output is identical
  either way (`stakingtables_pg_test.go`). It never writes them. Bare-metal:
  `cluster/apps/cosmos-tax-staking-daily/`.
- `taxapid/main.go` — the API daemon entrypoint.
- `taxindexer/` — the indexing entrypoint; `tax/` — tax computation logic.
- `tax/gaialiquid.go`, `tax/ibcv2.go`, `tax/tokenfactory.go` — hand-written message
  types for modules newer than this build's SDK. See below.
- Upstream machinery (rarely touched): `cmd/`, `core/`, `cosmos/`, `db/`, `indexer/`,
  `parsers/`, `probe/`, `rpc/`, `filter/`.

## Message types the codec does not have

This build links cosmos-sdk v0.47 and ibc-go v7. Cosmos Hub runs modules newer
than both, so their messages are absent from the codec and cannot be decoded.
Upgrading the SDK is not viable, so those messages are declared by hand and
registered under their real type URLs via `RegisterCustomMsgTypesByTypeURLs`
(probe's registry is the same object the decoder uses, so this works).

Covered today: Gaia `x/liquid` and pre-v24 SDK-fork LSM namespaces
(tokenized-share reward income), IBC channel v2 (inbound bridge transfers), and tokenfactory (minted balances, STARS on the
Hub). Field numbers come from each project's own `.proto`; nested Coins that no
taxable event depends on are kept as raw bytes.

Three rules if you add another:

1. **Implement `Marshal`/`Size` returning the bytes you decoded from.** The
   interface registry re-marshals everything it unpacks to cache it back into the
   Any, and gogoproto's reflection marshaller *panics* on hand-written types. The
   `rawMsg` embed in `tax/gaialiquid.go` does this. Round trips also preserve
   fields you chose not to model.
2. **Prefer events over payloads for amounts.** IBC v2 payloads on the Hub arrive
   solidity-ABI encoded from the Ethereum bridge; the `fungible_token_packet`
   event already carries sender/receiver/denom/amount. LSM reward amounts are not
   on the message at all and come from `withdraw_tokenize_share_reward`. Do not
   attribute the internal module-account withdrawal to the owner or count both
   sides of LSM's module-account hop.
3. **Do not treat `ibccallbackerror-` events as committed credits.** Core
   ibc-go v8/v10 re-emits discarded application receive caches with that prefix
   on the type *and* every attribute key. A lower app's `success=true` can precede
   an enclosing middleware error acknowledgement. Require unprefixed app success
   and bank evidence. A separately failed optional callback may leave a transfer
   committed, but that is version/middleware dependent, not guaranteed by the
   prefix. Unreconciled successful receives persist a classification error.

An undecodable message is retained as opaque evidence (`core/opaque.go` and
`core/tx.go`): original type URL, index, bytes and ordered events. It is not
classified by guessing, and does not silently disappear from an indexed block.
A nonzero fee with no resolvable payer defers its block rather than inventing
an address. Raw-disabled upserts must not erase existing raw bytes.

Some `ibc.core.client`, `interchain_security` provider and
`interchain_accounts` controller messages remain opaque. Historical provider
allocation is not a second wallet income event; actual consumer-denominated
reward withdrawals still matter. Governance v1 and v1beta1 deposits are
classified as principal records, not income.

## Who consumes it
The platform mono-app `/tax` section. The web app calls this API via `TAX_SDK_API_URL`
(mainnet events/forms, `cosmos-tax-sdk-api:8082`), `TAX_API_URL` (balances) and
`TAX_API_URL_TESTNET` (`cosmos-tax-sdk-testnet-api:8082`), surfaced to the browser as
`/api/tax/v1/{events,balances}` + `/api/tax/{report,forms,export}`. See
`platform/apps/web/app/tax/AGENTS.md` and `platform/ARCHITECTURE.md` ("The Cosmos SDK
indexer (tax)").

## Backfilling history

The in-cluster Hub fullnode is pruned, so history before the live indexer's
`start-block` needs an archive node. Jobs, the failed-block re-attempt pass, the
gap re-index tool and a completeness verifier live in
`bare-metal/cluster/apps/cosmos-tax-sdk-backfill/` — read that README before
running anything against an archive. Historical 2025 version changes, contract
candidates and acceptance gates are documented in `tax/HISTORICAL-2025.md`.
The optional archive HTTP guard requires bounded input, hard request pacing,
a timeout, budget and zero RPC retries; a 403/429 stops the run. Historical
`tx_search` can omit transactions present in blocks. Prefer block bytes plus
execution results (`skip-block-by-height-rpc-request=true`) and independently
verify their counts before claiming completeness.

## Gotchas
- Token names and suffixes are not asset identity. The Juno tokenfactory `uatom`
  voucher in `taxapi/assetidentity.go` is not native ATOM. Retain its reported
  label and original evidence with a suspected-spam flag. Require an explicit
  per-record exclusion or user-supplied token/value/basis before tax downloads.
  Never infer native ATOM equivalence or a zero value from its label.
- ibc-go v10 serves traces at `/ibc/apps/transfer/v1/denoms/{hash}` with
  `denom.base` and ordered `denom.trace`. The old `denom_traces` route can return
  501. Confirm the route's counterparty chain IDs before trusting token origin.
- Delegator income comes only from `withdraw_rewards` events, never summed
  `coin_received` events (staking pools receive delegated principal). A
  redelegation may auto-withdraw rewards from both source and destination
  validators; preserve each event's validator, denomination, and amount.
- Reclassification must remove stale tax rows even when the new output is empty.
  Use the backed-up, dry-run-first `taxrepair/` CLI for historical correction,
  and bump the report methodology version to avoid reusing stale report caches.
- Validator monikers are current chain REST display labels, not historical
  identities or proof of jurisdiction. The event's operator address is canonical,
  and a jurisdiction exists only as an attestation signed by that operator's
  account (`taxapi/attestation.go`).
- The top-level `README.md` is the **upstream** (generic) doc; the BryanLabs-specific
  code is the `tax*` dirs + `Dockerfile.tax`, on the `tax-layer` branch.
- Build the deployed image from `Dockerfile.tax`, not the plain `Dockerfile`.
- `taxledger --recheck-holes` is read-only by default. Run it as a separate
  Job, review its plan, then use `--apply --backup=/backup/<new-file>.jsonl`
  on the repair-backup PVC to write. The backup is private, contains wallet
  addresses, and records before/after plans durably before each commit.
  Never overwrite an earlier backup. Verify each affected wallet before and
  after; a backup plan alone does not prove its transaction committed.
- Before trusting query results, confirm the indexer is caught up
  (`cosmos-tax-sdk-indexer` logs) *and* that the range you care about is actually
  covered: `cosmos-tax-coverage` reports how many days of the tax year are not
  fully indexed, and the `cosmos-tax` Grafana dashboard shows it.
- `throttling` is a float used as `time.Duration(f)`, so anything below 1
  truncates to zero. Configs that look throttled are not; `rpc-workers` is the
  practical legacy rate control. Guarded archive runs additionally pace every
  actual HTTP call through one shared transport.
- `index-message-events` is read from the `[flags]` section, not `[base]`. Every
  config in the fleet sets it under `[base]`, where it is silently ignored, which
  is why `message_event_attributes` is the largest table in the database.
- Upstream docs: `docs/quickstart.md`, `docs/reference`, `docs/usage`.

## Keep this current
When you add/rename a tax API route or change how it's deployed, update this file and
`platform/apps/web/app/tax/AGENTS.md` in the same change.
