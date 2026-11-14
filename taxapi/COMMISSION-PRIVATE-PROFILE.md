# Read-only private classification staging

The ordinary/public report gate still refuses retained parser errors. The opt-in
collector profile is a separate in-memory staging projection, not a production
repair, error waiver, reindex, or financial acceptance.

It admits exactly the previously reviewed MsgExec(MsgEditValidator) source. Each
repeatable-read READ ONLY snapshot rechecks the full original source seal,
including database system/database identity, transaction, message, every error
and message in that transaction, ordered raw events and attributes, fee and
signer rows, retained classifications, and row xmins. It reparses the exact raw
message under the one-line guarded parser correction and requires zero inner
economic effects. Unknown or economic alternatives require a proper staged-row
projection and are rejected here. The original error is never deleted or
rewritten. The unchanged wallet fee reader retains the outer charged fee. The
profile binds the accounting owner's independently verified actual native fee
and mixed-economic controls, not a replacement fee calculation.

Readiness reports original, reclassified, and remaining counts separately. Any
other parser/decode/opaque/unreviewed error still blocks. Receipts are scoped to
one validator and only retained after the complete source-read transaction
commits. Every saved result carries the full profile identity, even if that
validator never encounters the known error. Before saving, a fresh read-only
snapshot rechecks the exact bound source. This is not a claim that all wallets
or chain source tables were globally frozen throughout a long trace.

Both immutable generation and result tracking record the full profile SHA256.
No-profile runs cannot use a profile-named generation; mismatched profile,
build, window or result identity cannot be reused. Existing a5 results must not
be relabeled or mixed into this fresh generation. The profile file is bounded,
regular, SHA-pinned, and source/parser/economic-proof/window constrained.

The source-bound fixture and live PostgreSQL checks are opt-in tests, with
private files and no source writes. Native tests verify original1,
reclassified1, remaining0, default refusal before wallet loading, unchanged fee
reads, and rollback receipt isolation. Docker-backed API tests require their
own Docker environment; they are not reported as passing on the worker pod.

Public promotion, source error clearing, ledger repair, production imports and
API rollout remain separate, unauthorized actions. Full financial acceptance
still needs independent cash, native fee/refund, source and current-lot closure.
