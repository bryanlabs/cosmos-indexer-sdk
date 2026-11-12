# Legacy Tendermint result fixtures

Derived read-only from existing cosmoshub-3 archive-cache captures on 2026-11-10.
No provider requests were made to obtain these fixtures.

| Height | Observed transactions | Purpose |
| --- | --- | --- |
| 1 | none | First block, nested BeginBlock and EndBlock shape |
| 100000 | codes 0, 0 | Successful historical results, camelCase gas fields and base64 attributes |
| 3410168 | codes 10, 0 | Failed and successful transactions in one block; failure must stay failed |

Sanitization removes transaction result data, info, free-text logs, validator
updates and account-bearing event values. Amount/module/action attributes remain;
other attribute values become `[redacted]` with the original legacy base64
encoding retained. Successful structured logs keep their shape with corresponding
redactions. These fixtures contain no raw transaction payloads, wallet addresses,
private provider URLs or credentials. They are not byte-exact original captures.

Optional tests with `LEGACY_PRIVATE_FIXTURE_DIR` also check the local original
captures without copying their data into this repository or logging it. Those
checks passed for all three heights. Original Amino transaction bytes at heights
100000 and 3410168 are unchanged and correctly rejected by the current protobuf
transaction path; these tests do not claim Amino support. In particular, successful
result normalization does not establish that the block's transactions can be
indexed or that a failed transaction paid its declared fee.

No real terminal block capture was available in the inspected cache. Enqueue
terminal tests exercise boundary/control-flow contracts with mocked Status, not
end-to-end historical terminal-block acceptance. The explicit terminal archive
entry point must only be selected after independent halt-height verification;
existing default callers still leave the live head alone.
