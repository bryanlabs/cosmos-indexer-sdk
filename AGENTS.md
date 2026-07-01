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
- `taxapi/` — the HTTP API: `server.go` (routes), `balances.go` (`/balance` = live
  node read via `NODE_REST_API`, persisted to `balance_snapshots`), `form8949.go`,
  `form990t.go` (reports/forms).
- `taxapid/main.go` — the API daemon entrypoint.
- `taxindexer/` — the indexing entrypoint; `tax/` — tax computation logic.
- Upstream machinery (rarely touched): `cmd/`, `core/`, `cosmos/`, `db/`, `indexer/`,
  `parsers/`, `probe/`, `rpc/`, `filter/`.

## Who consumes it
The platform mono-app `/tax` section. The web app calls this API via `TAX_SDK_API_URL`
(mainnet events/forms, `cosmos-tax-sdk-api:8082`), `TAX_API_URL` (balances) and
`TAX_API_URL_TESTNET` (`cosmos-tax-sdk-testnet-api:8082`), surfaced to the browser as
`/api/tax/v1/{events,balances}` + `/api/tax/{report,forms,export}`. See
`platform/apps/web/app/tax/AGENTS.md` and `platform/ARCHITECTURE.md` ("The Cosmos SDK
indexer (tax)").

## Gotchas
- The top-level `README.md` is the **upstream** (generic) doc; the BryanLabs-specific
  code is the `tax*` dirs + `Dockerfile.tax`, on the `tax-layer` branch.
- Build the deployed image from `Dockerfile.tax`, not the plain `Dockerfile`.
- Before trusting query results, confirm the indexer is caught up
  (`cosmos-tax-sdk-indexer` logs).
- Upstream docs: `docs/quickstart.md`, `docs/reference`, `docs/usage`.

## Keep this current
When you add/rename a tax API route or change how it's deployed, update this file and
`platform/apps/web/app/tax/AGENTS.md` in the same change.
