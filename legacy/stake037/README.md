# Hub 3 staking handover comparison

This standalone module pins Cosmos SDK v0.37.4 for its staking record codec and
IAVL/multistore proof verification. It compares the complete terminal staking
state with the preserved Hub 4 genesis. It reads no financial database and
writes no chain state, schema or ledger rows. Standard output contains counts
and digests only. All raw evidence and decoded snapshots are private.

Hub 4 has `initial_height=5200791`. This handover is not a zero-height export.
Delegation shares, validator tokens/shares/status/jailed, and every unbonding
entry's creation height, completion instant, initial/current balance and order
must match exactly. Delegation token valuation uses the pinned SDK's own
TokensFromShares followed by TruncateInt. No payout or balance is inferred.

## Historical subspace trap

SDK v0.37.4 `store/iavl/store.go` handles `/subspace` by iterating the latest
tree even when the response echoes an older requested height. Neither echoed
height nor two matching subspace replies proves the requested state.

The tool therefore requires each archive to be fixed at terminal height
5200791 before and after enumeration. It verifies membership of staking's
LastTotalPower key at 5200790 through both IAVL and multistore proof operators,
anchoring the application root in the cached 5200791 block header. It separately
verifies that the **entire staking root** at 5200791 equals the export-height
root, before and after enumeration. Only that equality permits use of latest
subspace contents for this handover. A root difference refuses the run, never
patches the state or skips a record. Two archives must agree field-for-field.

Validators are enumerated as a complete prefix. Delegations and unbondings use
all 256 disjoint first-address-byte prefix partitions, with key/value identity
and duplicate checks. Empty partitions remain evidence. This is not sampling.
Large unpartitioned responses and per-validator delegation queries can exceed
archive response/latency limits; no failed attempt is accepted.

The input validator universe is extracted read-only from the complete Hub 3
rehearsal ledger plus opening genesis validators/unbondings. Its coverage must
be 5200790 of 5200790 heights, with unique valid validator addresses. Terminal
validators absent from that independent history cause refusal. The full prefix
census also includes unbondings of validators removed before the handover.

## Inputs and retained artifacts

- `--genesis`: preserved Hub 4 genesis, plain or gzip. The full decompressed
  SHA-256 is pinned in the source, not a prefix. The SDK JSON codec accepts
  protobuf JSON's quoted int64 heights without changing their values.
- `--terminal-header`: retained raw block RPC response at height 5200791.
- `--validator-universe`: private JSON emitted by the accompanying read-only SQL.
- `--out`: a fresh mode-0700 evidence directory. Existing files are never reused
  or truncated. Endpoints come only from LEGACY_ARCHIVE_1/LEGACY_ARCHIVE_2 and
  are never printed or written into receipts.

A successful acquisition preserves raw RPC replies, decoded snapshots, the
source header/universe, a count-only receipt, and SHA256SUMS. Failure retains
all files acquired so far. An absent or failed receipt is not acceptance.

`--recheck-evidence` re-runs every proof, partition decode, identity check,
archive agreement and genesis comparison from preserved files without a
network call or source-file write. It still requires a fresh `--out` directory
for its new receipt and digest manifest. Missing evidence fails closed.

## Proof limits

This verifies terminal delegation/unbonding state and its genesis handover, not
the per-transaction staking ledger, tax acceptance or the website. The cached
terminal header is the retained chain-history anchor; this tool does not
independently re-verify the Hub 3 validator signature history. No production
write follows automatically from a successful comparison.
