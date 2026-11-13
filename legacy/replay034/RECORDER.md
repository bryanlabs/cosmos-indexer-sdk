# Hub2 observe-only replay and zero-height export

This directory alone owns the Cosmos SDK v0.34.10 replay instrumentation. The
ordinary `run` path still instantiates the upstream Gaia application unless
`-coin-records` is supplied. Neither command accesses a tax database.

## Replay

`run` accepts its existing flags plus `-coin-records <new-private-file>`.
The recorder is opt-in, and its output contains private chain evidence. Do not
paste records into logs, tickets, or terminal output. The file is created with
mode 0600 and exclusive creation, so an existing record file is never truncated.
Use a separate new output file for every resumed segment. Genesis initialization
is the starting account-state baseline, not a payout phase. Its source evidence
remains the genesis document; observation begins with BeginBlock.

The existing cached-parent, validator, results-hash and per-height application
hash checks remain in force. The final committed height still requires the next
cached header to prove its state hash. Add `-verify-target-next-header` to read
and validate that following cached block after the target commit, without
executing it. This checks the target state/result roots, parent link, validator
sets and proposer, then records the target itself as verified. It requires a
newly executed target, never rewrites the status of an already finished run,
and fails closed if the following capture is absent or differs. Use this for
recorded export-source runs so the export and fully verified stream have the
same height. Without this flag, the existing honest target-minus-one bound
remains unchanged. Observation is not a replacement for that proof.
A recorder failure stops the replay, and a write or sync failure after
application commit can leave the application ahead of its evidence file. Do not
claim that segment complete; retain it and rerun from an independent earlier
checkpoint into a new working directory and evidence file. No automatic rollback
or checkpoint deletion is attempted.

## Zero-height chain export

`export-zero -home <offline-checkpoint> -out <new-private-export> -coin-records <new-private-file>`

This opens the application LevelDB read-only, under its normal lock. Do not point
it at a running replay job. Use an offline, independently copied checkpoint for
rehearsals. It loads the committed height and invokes Gaia's real zero-height
export preparation, including commission and delegator reward withdrawal.
Export mutations occur in the in-memory check-state cache only. No Commit,
validator-history update, source-checkpoint write, file removal, or production
write is performed. SDK zero-height preparation internally clears historical
reward/slash keys in that disposable cache, as required by the chain's exporter.

The export artifact has `release`, `source_height`, `for_zero_height`,
`app_state`, and `validators`. It is an export bundle, not a complete new chain
GenesisDoc: a successor must explicitly supply its chain ID, genesis time, and
consensus parameters. `app_state` and `validators` are returned by the pinned
Gaia export, not inferred balances. Reward/commission payouts are observed keeper
operations, never an account residual or a conversion of fractional rewards.

Both export outputs must be outside the source checkpoint directory (including
symlink aliases) and are exclusive private files. Errors retain partial files
rather than deleting them. An empty/partial export or a command failure is not
accepted evidence. Only a successful command, complete export artifact, and
complete reconciled movement evidence constitute a local export result. Local
synthetic tests do not prove historical Hub2 state hashes or the upgrade payout;
those require the parent's isolated cache replay and export comparison.
