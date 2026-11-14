# Hub2 SDK v0.34.10 withdraw-address label correction

The accepted source-bound financial recorder uses Cosmos SDK v0.34.10 and emits
`msg.Route() + "/" + msg.Type()` for ordinary accounting markers. Its typed
`distribution.MsgSetWithdrawAddress` has route `distr` and type
`set_withdraw_address`. The consumer's `distribution/set_withdraw_address`
literal was wrong. Only that literal is corrected, not normalized or aliased.
No supported recorder evidence establishes the old spelling as valid.

The first actual e8d1036 component retained 82,090 complete heights and refused
20. All 20 first refused this exact label comparison. The private actual-byte
regression consumes those unchanged 20 heights, including every other message,
fee, aborted record, principal and asset in each height. Before the correction
it must report 20 held; after it must report zero. This does not substitute for
full-window validation or promise that no later financial issue exists.

`legacy/replay034/cmd/sdk-message-label-proof` tests the real typed SDK methods
inside the v0.34.10 module. Root core tests reject the incorrect old spelling,
unknown routes, case changes and a correct withdraw label on a bank message.
The existing strict financial, missing-movement, zero-fee, principal, failed
transaction and gas/default-admission tests remain in force.

`scripts/hub2_retained_window.py` creates a new exclusive private output and
references the original immutable failed capture. It authenticates all five
prefixes, all 164,184 raw cache member hashes against their retained source
hashes, exact profile/marker bytes and complete ordered record heights. The
original failed result and original live-source receipt stay unchanged. A fresh
retained-input descriptor binds original input hashes, new signed consumer and
reproduced CLI. This is retained-source provenance, not a new live attestation.
The existing validation-only CLI is invoked once with the original paths, then
input bytes are verified again. Only actual exit zero, 82,090 validated heights
and zero held heights qualify the new bounded component.

No cached records, messages, gas or failed artifacts are rewritten. No native
replay, Kubernetes request, database import, serving write or container build is
part of this correction. Even a successful bounded component does not establish
full-chain, books or tax acceptance.
