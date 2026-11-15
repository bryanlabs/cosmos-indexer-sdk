# Commission performance preparation, 2026-11-14

Owner: performance-prep only. Financial/private collector owner 8aa4b662-4145-415 remains independent. No production mutation, signal, API write, SQL write, Job launch, restart, suspension or deletion was performed. No tracked Go source or 8aa harness was edited. This is a separate detached worktree at signed 6754a0d.

## Source choice

ONLY 6754a0d7d7986e48e3e3467b2448aae84d2c1b92 / image3375126286bfa08649f4671328c61bb80fb595fe41d4d017e6bfe47b2e8eb42e is the private execution candidate; old01bb/8704 results are comparison/failure evidence only, not future acceptance inputs.

Verified git signature 15:07:34. Running native taxledger SHA256343de092d598b4a60db13435430a86df7154f782b771313bef7405f278cbb704 matches prior image binding. Live command remains workers2/private60s/full dates/version. Public remains48/324/incomplete. Accounting acceptance is false.

Code evidence:
- 01bb->8704 changes walletFees to effectiveWalletFees for private readers, preserving native charged-versus-declared fee distinction. Old01bb is not a correct-fee execution source.
- 8704->6754 changes private event selection to address materialization, distinct authoritative metadata IDs, ordered primary-key lookups. Old8704 has correct fee behavior but predecessor metadata-query performance.
- Current 8aa router15:02 confirms all8052 current6754 input windows complete, exact current source pinned, old seven-cohort capture already finished, current private proof uses retained source. No current dependency in that router requires either old full324 Job to keep consuming PG. Old data/logs/attempts must remain.

## Fresh baseline

Read-only receipts:
- /Users/danb/.cache/commission-acceleration/20261114T151021.222110Z/baseline.safe.json
- Same directory queue-state.safe.json and guard-live.safe.json.
- /Users/danb/.cache/commission-acceleration/resources-20261114T150242.374671Z/resource.safe.json
- /Users/danb/.cache/commission-acceleration/pause-identity-20261114T150857.355776Z/pause-identity.safe.json
- Source/serving ten-fingerprint receipt under20261114T145517.976610Z, equal the retained12:16 fingerprints. This is targeted preservation, not a full source checksum.

At15:10, original01bb47 execution successes/6 stored errors/8 native terminal failures;8704 48 successes/3 stored errors/4 native terminal failures;current6754 28 successes/1 stored error/2 native terminal failures. Each original Job still Running/one Pod/two native workers, restart0. Stored failure counts miss unsaved INSERT failures.

Current corrected failure1: native14:44:07 private-result INSERT SQLSTATE54000, total size of JSONB array elements exceeds268435455 bytes. Same validator hashbc008966... also has this failure in BOTH old native logs, so it is not a new metadata-query regression. saveCommissionFlowVersion writes run/rows/evidence JSONB in one INSERT; exact failed column/serialized payload is not captured. Large Archive evidence is inferred, not proven. A longer deadline or new version alone cannot fix this.

Current failure2: computed_at14:44:07 is attempt START, native terminal14:45:34; readiness19ms/causeother, prefix16. It is not a60s timeout. Old8704 has the same wallet/source-window readiness refusal26ms. Do not bypass classification/IBC-basis gates or repair financial data to make it pass.

At15:12, native ordered queue reconstruction validates all30 terminal attempts against the first324 pinned expected membership ordering. Active reserved ordinals23 and32 were assigned after14:40:55.689778 and14:52:48.683476; elapsed upper bounds31m16s/19m23s. First is old3797-wallet hard case, old runtimes3200/3358s. Second is909-wallet but slow case, old3952/3910s. These active ages are NOT successful runtime measurements.292 validators unassigned, plus2 active, plus2 unsuccessful =296 not yet successful.

PG actual16CPU/24Gi cgroup.15:02 sample9.307cores over5.616s;15:10 sample10.083cores over7.122s. PG anon+shmem about4.6Gi, memory PSI fullavg60 .17 then .40, OOMkills0. Total memory.current nearly24Gi mostly file cache, shmem is already included in file; do not call this24Gi anonymous memory. Stable-PID old backendCPU samples omit short-lived parallel workers and cannot allocate total PG CPU accurately. Unlabelled independent private readers remain untouched.

Native current/peak bytes at15:02:01bb5.627GB/7.053GB,8704 5.043GB/5.128GB,current1.358GB/3.436GB; all enforced4CPU8Gi andzeroOOM. SIGSTOP retains those heaps, does NOT free their memory.

Public exact endpoint https://bryanlabs.net/api/tax/v1/commission-flows?chain=cosmoshub. Five200 responses per measurement, baseline p50=55.346ms/max86.626ms;15:10 p95 nearest-rank72.505ms. Serving byte SHA7c34f19c22545d603b0cd48de6d077c0c4fd6d49f8e8017ab04f678c5bc491ab unchanged. Cached public behavior and fresh small serving-table fingerprints are separate evidence. Initial mainnet selector returned our400 error, corrected to source-documented cosmoshub, not a public outage.

## Preservation-safe pause and rollback, for Main review only

Kubernetes Job.spec.suspend deletes active Pods. It fails the no-deletes/preserve-original-Pods requirement. Do not use it without an explicit Dan exception. No cooperative pause/drain interface, signal handler or durable per-validator heap checkpoint exists in this CLI. SIGTERM exits and loses unfinished trace progress. Changing a Job command is an immutable-template change, not2->4 live scaling.

A viable existing-process pause is ancestor-host-namespace SIGSTOP, with exact SIGCONT rollback. All3 native processes are PID1 in their own namespace. Container-local kill1 is not the correct reliable signal path for a namespace init process. Existing hostPID worker Pod kube-system/cilium-node-init-89qjt was verified read-only and successfully mapped each container cgroup to one host PID and namespace PID1.15:08 identities:01bb2349006/birth174716531;8704 3296336/birth175328447;6754 3276707/birth178019154. These are STALE SAMPLES, never signal them without revalidation of original PodUID/containerID/image/PID/birthticks and native Name.

PostgreSQL runs independently from the stopped Go client. SIGSTOP does NOT guarantee an active query drains: large results can block ClientWrite, and an unfinished explicit read transaction waits for the client COMMIT. A stopped client can pin a snapshot and hold locks indefinitely. Therefore:
1. Main reviews the exact pause policy and standing authority. No blanket new approval for harmless research is invented. Additional production load/new collector Jobs still need the applicable yes; Pod deletion needs explicit exception.
2. Revalidate original source/template/UID and process identity, capture immutable output subset/logs. Poll tagged sessions for all client connections idle, xact_startNULL, no parallel workers/locks. No active write/DDL transaction is acceptable.
3. Only after Main review, signal exactly the revalidated host process from the ancestor hostPID namespace. Process state must becomeT and all Go threads stop. No process group or guessedPID.
4. Immediately re-read PG tagged sessions. Require no active query, no open transaction, no parallel worker and no lock wait over three reads spanning5s. The precheck has a race. If a SELECT started between precheck/STOP, restore the same exact PID with SIGCONT within5s if it cannot finish; do NOT cancel/terminate the PG backend. Retain unsuccessful pause attempts, then retry only at another safe idle boundary. Never leave an idle-in-transaction or ClientWrite backend frozen.
5. Re-read original Job/Pod/container identities and restart counts, source/serving fingerprints, original output subset. No originals removed or replaced. Heaps remain in their processes.
6. Rollback any later guard violation: preservation-safe STOP of only new workload processes after reviewed no-write/quiescence checks, then SIGCONT of exact originals after identity verification. Keep new Job/Pod/log/output evidence too. No delete, restart, Job suspend, UPDATE, DELETE or cleanup. Do not resume the corrected original while overlapping same-version side queues are still active.

A root-only pause is an alternative only after implementing a reviewed cooperative drain interface in a successor source, proving accounting/reader equivalence and binding a new signed image/version. The current binary does not have it. A log observer alone cannot reliably stop between roots without a dequeue race.

## Same-version resume and disjoint4->6 path

Writer source commissionflows_versions.go is transactional INSERT-only. Existing successes (ok/reconciled/drift) are reusable after build/window/identity checks. Drift is execution success, NOT accounting acceptance. Failed immutable rows require a new version. BuildCommissionFlows only owns an in-process unbuffered ordered channel, no cross-process lease. Two unrestricted Jobs on the same version can duplicate work and race their INSERT, even though the PK prevents overwriting a row. Do NOT launch an unrestricted4worker replacement beside the original2.

Prepared exact native337 alternative preserves unfinished original heaps:
1. Preservation-safe pause the two obsolete clients after Main review, then remeasure PG/public baseline. Do not touch8aa/private proof readers.
2. Separately preservation-safe hold the current corrected2 client, retaining its two active roots. Main must explicitly include that hold in the reviewed operation; it is a technical necessity of this static same-version sharding path, not a Kubernetes suspension.
3. Regenerate preparation after the frozen queue is stable. Check pinned324 membership/native ordered terminal log versus stored rows, all source/date/build guards, and original subset. Terminal unsaved errors are separate from active roots, otherwise the wrong roots would be reserved. At14:59:29 stored (28good+1error),1unsavedterminal failure,2reserved current roots,196four-worker queue,96later-two queue. The union is exactly324 and the new queues are disjoint. The snapshot is not deployable unchanged while the original process still advances.
4. Unapplied files stage-four.job.private.json and stage-later-two.job.private.json reuse exact337 nativeCLI and Secret refs/date/assets/deadline/network labels. Stage1 adds only the4worker Job. Stage2 adds the disjoint2worker Job only after bounded4-concurrent-full-reader equivalence proof and actual hard-root success under guards. Six active workers then occupy two independent cgroups, each4CPU8Gi/GOMAX4/GOMEM6Gi, aggregate maximum8CPU16Gi for new workloads, NOT an already-proven six-worker8Gi profile. The old heaps remain additional node memory.
5. Before every ship use a fresh bare-metal worktree/root and full scripts/cluster-apply diff/read/ship. Prepared JSON is valid manifest input, not a stale-root apply. Preserve all old resources. Prepare additive per-shard completion/native-error observers and alerts before a long deployment; existing global watcher alone does not prove the side Jobs' membership/completion and already misses unsaved failures. Do not mutate the existing104-test checker or schedule unreviewed code.
6. Once both new disjoint queues terminate and exact stored successful memberships are verified, resume the original corrected process. It finishes its two retained active roots, then skips newly stored successes. Its existing failed counter remains truthful and its immutable error remains retained. It can never become324good/0error for this version without violating immutability. Separate exact retry versions/source-bound acceptance manifests are needed for failures. A fresh full324 version instead is simpler but redoes completed work and does not fix JSONB-size failures; do not pretend it is progress-preserving same-version resume.

The prepared path is technically concrete but not yet execution-ready: original process has NOT been frozen; post-freeze sharding proof, a bounded concurrent-reader proof, full native4worker outcome/resource observation, additive runtime monitoring and Main's fresh whole-root review are outstanding. The two known failures protect real missing proof/storage, not artificial approval loops. Resolve their causes without dropping coverage or evidence. This performance owner does not change8aa's accounting source/harness.

## Proposed guards

Review-proposed, not Dan/provider limits: PG anon+shmem<12Gi, zeroOOMkills, memoryPSIfullavg60<2, PG CPU<14/16 cores, unchanged original subset/UIDs/Pods, blocked queries0, same public48/324/body and p95<=max(250ms,2*baselinep95). Current resource gates passed an actionless live7.122s check15:10. Missing/invalid/stale metrics fail closed. Native terminal log failures are checked separately from DB stored errors. For the new stage, any new native error, OOM/restart, private60s failure or strict comparison mismatch stops advancement and invokes the reviewed reversible rollback. These resource checks alone authorize nothing and prove neither cold cache nor4/6concurrent reads. Do not benchmark against the production DB while the old clients are still consuming it without Main's bounded target/resource review.

## Honest execution clock

New native successful whole-trace slow cases >=30min are3984s/3931wallets,2034s/422wallets,1992s/39wallets, mean2670s (44m30s). Different wallet counts can be slow. The25 quick cases and both failures are excluded from this slow-case sensitivity model. Only the3931case is also>=1000wallets; its old same-root times5983s/5666s show a real measured improvement, not a4worker benchmark.

For an illustrative15:20UTC ramp origin and296 not-yet-successful validators, assuming all cost2670s, both failure causes are fixed with no additional delay, and constant parallelism with no ramp/tail imbalance:2workers109h46m =>Nov19 05:06UTC;4workers54h53m =>Nov16 22:13UTC;6workers36h35m20s =>Nov16 03:55:20UTC. These are CONDITIONAL EXECUTION SCENARIOS, not promised ETAs, not measured4/6-worker throughput, and not financial-acceptance clocks. Actual static-shard ramp adds review/benchmark time and a retained-two-root tail. Slow-case sample is small and selected, not a statistical estimate of all324. OldNov18 10:42 was an old four-case projection, never a94h completed successful run. No defensible all324 financial-finish ETA exists while two failure causes and independent cash/route acceptance remain unresolved.

## Verification and limits

14 local Python tests pass. Native Go/accounting source untouched. No production SQL mutation, schema/index change, public promotion, collector launch, other-owner edits, scheduler change, alert suppression or evidence cleanup. Active checker SHA remains043b1c8ccbbf02656f5df9682d8847356b560fb2c6768d0b94452e5aa174fc4d at15:07.

Priority verbatim: accuracy first, then speed. Do not sacrifice accounting correctness, existing coverage or evidence. Equally, do not turn that priority into endless preparation, artificial approval loops or monitoring without implementation.
