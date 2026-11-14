# Optional native Hub 1 accounting observer

This app derives from exactly Cosmos SDK v0.33.2 Gaia. It does not instantiate
v0.34.10 keepers. `app.go`, `genesis.go`, `invariants.go`, and `export.go` retain
that native wiring, Gaia's launch-era bank handler, invariant order and native
staking EndBlock sequence. In particular, the native discarded AppendTags
return is not repaired. The ordinary replay constructor remains upstream Gaia
unless `--coin-records` is explicitly supplied. The original scan and default
run/export behavior are unchanged.

## Native evidence, not final-balance inference

The bank decorator calls each actual native mutator exactly once using the
original context. Independent infinite-gas reads measure its actual account
before/after amounts. Full account snapshots reconcile each begin block, ante,
message, transaction, end block, block and zero-height export. Unexplained
bank changes and unknown causal classifications fail closed before commit.
Rewards and commission identify the actual validator, income owner and separate
withdrawal destination. EndBlock unbonding returns and governance refunds/burns
are scoped around the actual native keeper call, not inferred from residuals.
Explicit and hook-triggered withdrawals are distinct from staking principal.
No zero or fractional reward is fabricated into a payout.

Every scope also has all-nine-persistent-store before/after commitments and
EVERY changed raw key's before/after value. The whole segment/export is bracketed
by full raw native state checkpoints. These include account metadata, staking
pool/validators/delegations/unbonding/redelegation arrays and queues, distribution
reward/slash/commission state, fee collection, mint, governance, params and main
store, without a field whitelist, sampling or a record cap. Raw bytes encode as
base64, missing and present-empty remain different, and key ordering is native
iterator order. Store commitment SHA256 is over the complete JSON encoding of
the ordered key/value pairs plus a newline. Hashes do not replace the raw changed
values or baseline/final full state. Transient stores are not persistent financial
state. Failed native message caches retain attempted raw changes, explicitly
aborted; transaction/block scopes describe only retained state.

Full-state snapshots are not duplicated for every withdrawal. That would retain
thousands of whole states in the pending export buffer. Instead, the complete
baseline/end plus every raw changed key and per-scope all-store commitments
preserve the same native information. No financial field or unsupported message
is dropped to make the observer faster.

## Shares are not tokens

Native033 `MsgUndelegate` and `MsgBeginRedelegate` request `SharesAmount`, an SDK
decimal. `share_request` preserves that source share request. `staking_shares`
records exact actual source and destination delegation share deltas with unit
`delegation_shares` and NO token denomination. The destination uses its own
native exchange rate. `principal` is only the source validator's actual rounded
integer token debit, not a reinterpretation of requested shares. Native deletion
of an unbonded zero-share validator is explicit. Delegate/CreateValidator request
tokens but produce separately measured decimal shares. Native before/after
validator and delegation objects accompany these records. Principal and share
records are not wallet income or extra payouts.

Slashes, reward accruals, unbonding/redelegation objects, fees, validator changes
and governance state remain complete raw native evidence even where there is no
wallet payout. This does not claim a complete semantic financial ledger for
every changed store key. Unknown bank causes fail closed. The Hub2-only root
importer and strict record reader intentionally do not admit this new Hub1
schema; there is no production import, tax acceptance or ledger repair here.

## Commit, failures and private output

All block evidence remains buffered until native DB commit succeeds. Check
`RecorderError` before and after commit and fsync the caller's exclusive0600
file. A hard kill can leave a committed DB ahead of evidence. Therefore recording
is SINGLE ATTEMPT PER HOME: a durable exclusive `.native033-recording-attempt`
marker refuses any retry in that home. Preserve failures and use a new clone of
an earlier immutable checkpoint plus new output paths. Do not delete the marker,
resume past a gap, write completion before DB commit, or overwrite partial files.

Recorded run requires exact Hub1, all cached result comparisons and target next-
header proof. ANY native/cache code/data/gas/log/tag/validator difference refuses
the block before commit. Original unobserved result-difference diagnostics are
unchanged. Post-message-cache deferred block-gas failure also refuses commit,
including net-zero operations. Failed ante attempts and a successful ante fee
retained after failed messages are not conflated.

`SealRecorder` writes the full final state and `observation_completed` only after
the caller validates the target against its unexecuted next header. Export runs
on an independent read-only checkpoint app, never Commit. Actual export payout
calls and native return codes are recorded, including zero/error withdrawals;
the native zero-height state reset is preserved. Full export and reconciliation
must succeed before `export_completed` is emitted. Incomplete or failed output
is evidence of an attempt, never accepted financial data.

`native_execution` preserves all signed transaction bytes and full native ABCI
responses, including codes, gas, tags, data and logs. It is source evidence, not
permission to publish private financial records. The optional independent
`--native-audit` path works against either untouched Gaia or this app and captures
every complete response plus all nine committed store subspaces. Native033
rejects an empty prefix, so ALL256 first-byte prefixes are queried per store,
including 0xff and empty results. This exhausts all legal nonempty native keys.
It is only used immediately after the exact commit, not as a historical-height
subspace query. Compare the entire native/observed audit and entire typed export,
retaining every field/type/array position. Object member order is not a data gate.

## Verification

Run `go test ./...`, `go test -race ./...`, and `go vet ./...` from replay033.
Nonempty fixtures cover real native payout calls, withdrawal address overrides,
full response/gas/state equivalence, failed caches/ante, late gas errors,
unknown classifications, full store changes and lossless array/empty values,
non1:1 share rates and rounding, native validator deletion, ordered unbondings,
and failed redelegation partial writes. Synthetic fixtures do not establish
historical coverage. Bound actual checkpoint rehearsals separately and state
which classes were and were not exercised.
