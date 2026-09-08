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
  (`/asset-review` readiness/audit and explicit per-record decisions; see
  `taxapi/ASSET-REVIEW.md`), `balances.go` (`/balance` = live
  node read via `NODE_REST_API`, persisted to `balance_snapshots`), `form8949.go`,
  `form990t.go` (reports/forms).
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

Covered today: Gaia `x/liquid` (LSM, tokenized-share reward income), IBC channel
v2 (inbound bridge transfers), and tokenfactory (minted balances, STARS on the
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
3. **Watch for the `ibccallbackerror-` prefix.** A failed destination callback
   makes ibc-go re-emit that execution's events with that prefix on the type *and*
   every attribute key. The transfer still happened.

An undecodable message is skipped individually (`core/tx.go`); it does not fail
the block. Before that fix one unknown type destroyed every unrelated
transaction in its block.

Deliberately left undecoded, having checked on chain that their transactions
move only fees and tips: `ibc.core.client` v1/v2 updates, `interchain_security`
provider messages, `interchain_accounts` controller messages, and
`cosmos.gov.v1.MsgSubmitProposal` (refundable deposit; gov v1beta1 is not
classified either).

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
running anything against an archive.

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
  identities or proof of jurisdiction. The event's operator address is canonical.
- The top-level `README.md` is the **upstream** (generic) doc; the BryanLabs-specific
  code is the `tax*` dirs + `Dockerfile.tax`, on the `tax-layer` branch.
- Build the deployed image from `Dockerfile.tax`, not the plain `Dockerfile`.
- Before trusting query results, confirm the indexer is caught up
  (`cosmos-tax-sdk-indexer` logs) *and* that the range you care about is actually
  covered: `cosmos-tax-coverage` reports how many days of the tax year are not
  fully indexed, and the `cosmos-tax` Grafana dashboard shows it.
- `throttling` is a float used as `time.Duration(f)`, so anything below 1
  truncates to zero. Configs that look throttled are not; `rpc-workers` is the
  only real rate control.
- `index-message-events` is read from the `[flags]` section, not `[base]`. Every
  config in the fleet sets it under `[base]`, where it is silently ignored, which
  is why `message_event_attributes` is the largest table in the database.
- Upstream docs: `docs/quickstart.md`, `docs/reference`, `docs/usage`.

## Keep this current
When you add/rename a tax API route or change how it's deployed, update this file and
`platform/apps/web/app/tax/AGENTS.md` in the same change.
