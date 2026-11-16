# Hub1 scratch handoff, 2026-11-15

Not ready for activation. Dan's 22:20 handoff order cancels the 22:30 launch.
No original Job, Pod, process, frozen code, root, source or failed artifact was
changed. Existing ranges 1 and 3 remain reference checkers. Range 2 failed at
20:31 UTC because the old BeginBlock checker requires bonded tokens unchanged.
BrokenPipe is a consequence, not the original failure.

## Branches and ownership

- `fix/hub1-native-slash-handoff-20261115`: latest slash checker and diagnostic.
- `fix/hub1-resume-split-handoff-20261115`: separate unfinished durable-resume
  worktree snapshot. It includes an earlier slash candidate, not the final
  binding fixes in this branch. Do not blindly cherry-pick its main.go.
- Claude owns final review, integration, authority checks and any future launch.
  This handoff authorizes no launch, promotion, reboot, repair or guard change.

## Checker versus recorded output

The old closed consumer checks immutable movement/history sources and emits
verification receipts. It does not replace the historical financial rows.
Original source records remain unchanged. The correction changes the unsupported
slash check and adds a separate, unclassified native stake-asset fact artifact.
It changes receipt metadata, so receipts are not byte-identical. Ordinary
nonslash phases produce no new asset fact file. There is no claim that the old
unfinished checker has a resumable or accepted component receipt.

Known downtime candidates are range 1: 134125; range 2: 219358, 221354, 231614,
359194; range 3: 486010. Ranges 1 and 3 had not reached their first candidate
at handoff. Transaction-driven removals 99677, 168912, 347236 and 347349 are not
BeginBlock classifications. All three completed original source-status receipts
report zero evidence items. General preanchor evidence still refuses unless an
independently verified historical header is available; a self-supplied pin alone
is not treated as consensus authentication.

## Strict slash proof

The witness reconstructs the exact before-state, compares all nine stores with
the independently committed predecessor, validates the cached block and parent,
then executes SDK v0.33.2 mint, distribution, slashing and the original hooks.
Every after-leaf must match. Pool total plus exact provision minus validator and
unbonding token burns must equal the native after-pool total. Redelegation claims
are not double-counted as additional supply.

Ownership facts cover all delegations and unbonding positions, including new or
removed states. They retain native shares, native SDK decimal before/after,
exact rational ownership, and explicit rational rounding remainders. They are
asset-state changes, not income or deductible-loss classifications. Partial
facts remain private partial artifacts. Final fact publication is exclusive and
occurs only after the complete verified height sequence; the receipt binds its
hash. The final publication/resume combination still requires integration review.
The resume branch is a divergent earlier slash snapshot. Its three-height kill/
restart fixtures pass at checkpoints1,2,3 with matching final receipts, but emit
no slash facts. Nonempty facts are covered only by native checkpoint unit tests.
The120-second default is a design interval, not a proven production loss bound.

At 219358, a retained dead-attempt copy has the exact 219357 application root.
Genuine Gaia BeginBlock leaves form the independent after-state reference. The
manual full-height and genuine Gaia full-height roots both match the retained
219359 header, with zero differences in all nine stores. One validator is newly
jailed; bonded fall equals validator burn; NotBonded rises by exact provision;
delegations, unbondings and redelegations are unchanged. Private financial values
stay outside Git. Private proof SHA256:
`a6113af092946fa8426e6349d3254c11dd969be22efbb0eb97c857428219f21f`.

## Tests and review

Latest historical diagnostic and native package tests passed in the isolated
scratch worktree. Tests include wrong before-state and unknown after-leaf
refusals, exact ownership/rounding, changing shares, new/deleted owners,
unbonding-entry burns, missing share coverage, H+2 validator updates and source
pin mismatch. Eight closed-consumer Python tests passed, including native JSON number
normalization and fact-hash validation. A parser-number compatibility defect was
found during resume integration and fixed in this branch. SDK staking slash and
slashing BeginBlock tests passed. All Go builds/tests used GOMAXPROCS=2 and -p1;
new tests are serialized through the shared scratch test lock.

Two read-only Opus reviews found six issues in total. The first two were fixed
by full unbonding ownership coverage and a corrected negative control. The next
four required predecessor binding, early block validation/deferred publication,
independent genuine BeginBlock leaves plus mandatory root/leaf assertions, and
historical evidence-header authentication. Fixes are implemented, but the final
post-fix integration has not received a fresh independent review. A double-sign
redelegation reward-payout path may still be refused by the existing bank and
liability invariants. No such evidence exists in these three Hub1 sources;
this is a general-path limitation, not permission to waive an invariant.

## Split preparation, not a launch plan

The original read carrier's complete markers at 100000, 200000, 300000, 400000,
500000 and 500015 were verified read-only. Directory inode size is not DB size;
aggregate checkpoint DB sizes remain unmeasured. Genesis is anchor 0, not a
saved disk checkpoint. Ten financial windows of 50000 heights plus a separate
15-height tail need five derived anchors (50000, 150000, 250000, 350000, 450000).
Each requires at most 50000 new native warmup heights from an existing anchor.
No 50k derived anchor or split launch was created.

A bounded 100-height actual SDK sample, 219358..219457, reproduced every
application root and complete ABCI result. It took 7.10805238 seconds, 14.06855
height/s, with 90644 KiB peak test-process RSS. Its dead-attempt checkpoint copy
took 0.02667859 seconds. Extrapolating 50000 warmup heights gives 59m14s, not a
measured 50k duration. At old slowest financial rate .67 height/s, 50k takes
20h43m47s. A fully simultaneous ten-window scenario is therefore about21h43m
plus source preparation, checkpoints, controls and acceptance. It is not a
validated concurrency result or a promised finish.

The node was at45.717/64 CPU cores (71%) and207380 MiB memory (80%). Current old
checker pairs together used about741 MiB. Ten additional old-size pairs would
need roughly3.7 GiB plus the existing pairs, beyond the old3 GiB aggregate guard.
Do not silently change that guard. Physical pool is87% written with1.698 TB free;
volume promises exceed physical space. Agent-home available space was126.6 GB.
Ramp only after a reviewed resumable single-window equivalence test and measured
CPU/RSS/H2/Tia headroom. No resource setting was changed.

A metadata-only size check found218161458173 raw bytes across the three original
movement/history sources, versus35453398876 compressed bytes. The current resume
prototype writes uncompressed seekable chunks. Preparing all derivatives would
exceed the126.6 GB available on agent-home. A losslessly compressed chunk format
or separately reviewed storage plan is needed; neither is implemented or
approved here. Do not run full preparation under the halt order.

## Clock basis

All earlier clocks named range3 Nov16 15:35, range1 Nov19 01:08, and H1 load
Nov18 16:00 are invalid for the old binary. Blanket83h full-restart planning is
not a transition decision and must not discard valid old prefix evidence.

Actual Hub3 importer Job wall time was6691 seconds (Nov13 13:07:03..14:58:34),
container6687 seconds. This is end-to-end importer time, not isolated SQL time.
Hub3 liquid closes took20 and25 container seconds, but those do not establish
full financial acceptance, which omitted delegation/unbonding comparisons.
OPS-814 comment07060247-267b-432e-9dfe-a04c561654d3 supplies a conservative
post-record acceptance proxy:111 minutes importer time, then147 minutes from
import completion14:58 to adjusted close17:25, including waiting and Proposal29
work. This is not measured acceptance CPU. Hub3 loaded before closure; Hub1's
strict gate requires acceptance before load. The full Hub1 durations remain
unmeasured;147 minutes is a chronology-based planning proxy, not a bound.

Counterfactual only: if Claude started a reviewed ten-way50k plan at Nov16 01:00,
old .67 financial rate plus the measured100-height warmup extrapolation gives
compute Nov16 22:43, acceptance Nov17 01:10 using147 minutes, and load Nov17 03:01
using111 minutes. Add unmeasured source preparation, checkpoint/seal overhead,
concurrency contention and Hub1-size scaling. Ten-way resource approval and
resumability are not established. The separate old full-restart example of
record Nov19 09:25, acceptance11:52, load13:43 is likewise counterfactual.
Neither example is an authorized start or current ETA. No22:30 launch.

Detailed live identities, exact test logs, hashes, helper inventory and safe
receipts remain at `/Users/danb/.cache/cosmos-tax-reboot-20261115/`.
