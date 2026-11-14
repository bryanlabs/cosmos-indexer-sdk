#!/usr/bin/env python3
"""Audit exported PRIVATE commission snapshots offline, without database writes.

Input is SELECT row_to_json(v) from tax_commission_flow_versions, collected as
an array. Context pins version/build/chain/from_day/as_of and validators. as_of
is the inclusive result day. The native Job flag is the following exclusive day.
This checks execution integrity, not cash correctness or financial acceptance.
No output includes wallet identities, coin quantities or private archive URLs.
"""
import argparse
import collections
import datetime
import decimal
import hashlib
import json
import os
from pathlib import Path


def decode(value):
    return json.loads(value) if isinstance(value, str) else value


def number(value):
    n = decimal.Decimal(value)
    if not n.is_finite() or n < 0:
        raise ValueError("invalid nonnegative quantity")
    return n


def day(value):
    at = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if at.tzinfo is None:
        raise ValueError("missing date timezone")
    at = at.astimezone(datetime.timezone.utc)
    if at.time() != datetime.time():
        raise ValueError("non-midnight date")
    return at.date().isoformat()


def exact_source(source, address, holding_day, archive):
    height = source.get("Height", 0)
    if height <= 0:
        return False
    if source.get("Kind") == "ledger_exact":
        return not source.get("ReceiptHashes")
    if source.get("Kind") != "archive_exact" or not source.get("ReceiptHashes"):
        return False
    headers, queries = {}, set()
    for digest in source["ReceiptHashes"]:
        # Empty protobuf responses from different query methods can have
        # identical raw JSON hashes. Keep all matching receipt envelopes.
        candidates = [r for r in archive.get(digest, []) if r["Address"] == address and r["Height"] in (height, height + 1)]
        if not candidates:
            return False
        for receipt in candidates:
            if hashlib.sha256(receipt["Response"].encode()).hexdigest() != digest:
                return False
            body = json.loads(receipt["Response"])
            if body.get("error"):
                return False
            if receipt["Method"] == "block":
                header = body["result"]["block"]["header"]
                h = int(header["height"])
                if header["chain_id"] != "cosmoshub-4" or h != receipt["Height"] or h not in (height, height + 1):
                    return False
                headers[h] = datetime.datetime.fromisoformat(header["time"].replace("Z", "+00:00"))
            elif receipt["Method"] == "abci_query":
                response = body["result"]["response"]
                if receipt["Height"] != height or int(response["height"]) != height or response["code"] != 0 or not response.get("value"):
                    return False
                queries.add(receipt["QueryPath"].strip('"'))
            else:
                return False
    cutoff = datetime.datetime.fromisoformat(holding_day + "T00:00:00+00:00") + datetime.timedelta(days=1)
    paths = {"/cosmos.bank.v1beta1.Query/AllBalances", "/cosmos.staking.v1beta1.Query/DelegatorDelegations", "/cosmos.staking.v1beta1.Query/DelegatorUnbondingDelegations"}
    return height in headers and height + 1 in headers and headers[height] < cutoff <= headers[height + 1] and paths <= queries


def audit(snapshots, context):
    expected = context["validators"]
    if len(expected) != len(set(expected)) or not expected:
        raise ValueError("expected validator identities must be unique and nonempty")
    seen = set()
    findings = []
    statuses = collections.Counter()
    summaries = []
    # Exact decimal sums must not be rounded by Python's default precision.
    # Derive sufficient precision from the input, including wide exponents.
    with decimal.localcontext() as ctx:
        ctx.prec = max(200, len(json.dumps(snapshots)))
        for ordinal, snapshot in enumerate(snapshots, 1):
            issues = []
            run, rows, evidence = (decode(snapshot[k]) for k in ("run_json", "rows_json", "evidence_json"))
            validator = snapshot["validator"]
            if validator in seen:
                issues.append("duplicate_snapshot")
            seen.add(validator)
            if validator not in expected:
                issues.append("unexpected_validator")
            for key in ("version", "build", "chain"):
                if snapshot[key] != context[key]:
                    issues.append("wrong_" + key)
            if run["Validator"] != validator or run["Chain"] != context["chain"]:
                issues.append("run_identity_mismatch")
            if day(run["FromDay"]) != context["from_day"] or day(run["AsOf"]) != context["as_of"]:
                issues.append("run_window_mismatch")
            statuses[run["Status"]] += 1
            if run["Status"] not in ("ok", "reconciled", "drift") or run["Reason"]:
                issues.append("execution_failure")
            if run["OpeningUnknown"] or run["Unresolved"]:
                issues.append("missing_holdings_or_destinations")
            totals = collections.defaultdict(decimal.Decimal)
            keys = set()
            for row in rows:
                if row["Validator"] != validator or row["Chain"] != context["chain"]:
                    issues.append("flow_identity_mismatch")
                if row["Rule"] not in ("fifo", "pro_rata"):
                    issues.append("unknown_rule")
                key = tuple(row[k] for k in ("Rule", "Month", "Class", "Detail", "ViaUnbonding"))
                if key in keys:
                    issues.append("duplicate_flow_key")
                keys.add(key)
                totals[row["Rule"], row["Month"]] += number(row["AmountBase"])
            commission = {m: number(n) for m, n in decode(run["Commission"]).items()}
            if any(month not in commission for _, month in totals):
                issues.append("flow_without_commission_month")
            residuals = collections.Counter()
            for rule in ("fifo", "pro_rata"):
                for month in commission:
                    delta = totals[rule, month] - commission[month]
                    if delta:
                        residuals[rule] += 1
                        # Classify even tiny residuals, never call them exact.
                        issues.append("conservation_residual_" + rule)
                        if abs(delta) >= 1:
                            issues.append("conservation_at_least_one_base_unit")
            archive_by_hash = collections.defaultdict(list)
            for receipt in evidence.get("Archive") or []:
                archive_by_hash[receipt["SHA256"]].append(receipt)
            wallets = evidence.get("Wallets") or []
            if not wallets:
                issues.append("wallet_accounting_evidence_absent")
            else:
                if len(wallets) != run["Wallets"] or len({w["Address"] for w in wallets}) != len(wallets):
                    issues.append("wallet_accounting_coverage_mismatch")
                if any(not w["OpeningKnown"] or day(w["ClosingDay"]) != context["as_of"] for w in wallets):
                    issues.append("wallet_accounting_window_or_opening_mismatch")
                for wallet in wallets:
                    opening_day = (datetime.date.fromisoformat(day(wallet["Since"])) - datetime.timedelta(days=1)).isoformat()
                    if not exact_source(wallet["OpeningSource"], wallet["Address"], opening_day, archive_by_hash) or not exact_source(wallet["ClosingSource"], wallet["Address"], context["as_of"], archive_by_hash):
                        issues.append("wallet_holding_source_not_exact")
                    actual = number(wallet["ActualLiquidBase"]) + sum((number(n) for n in wallet["ActualStakedBase"].values()), decimal.Decimal(0)) + sum((number(n) for n in wallet["ActualUnbondingBase"].values()), decimal.Decimal(0))
                    if decimal.Decimal(wallet["SimulatedBase"]) - actual != decimal.Decimal(wallet["DriftBase"]):
                        issues.append("wallet_drift_comparison_mismatch")
                if sum((abs(decimal.Decimal(w["DriftBase"])) for w in wallets), decimal.Decimal(0)) != number(run["DriftBase"]):
                    issues.append("wallet_drift_sum_mismatch")
                for rule in ("fifo", "pro_rata"):
                    if sum((number(w["Overdraw"][rule]) for w in wallets), decimal.Decimal(0)) != number(decode(run["Overdraw"])[rule]):
                        issues.append("wallet_overdraw_sum_mismatch")
            archive = evidence.get("Archive") or []
            for receipt in archive:
                if not isinstance(receipt["Response"], str) or hashlib.sha256(receipt["Response"].encode()).hexdigest() != receipt["SHA256"]:
                    issues.append("archive_response_hash_mismatch")
            summaries.append({"ordinal": ordinal, "status": run["Status"], "wallets": run["Wallets"],
                              "flow_rows": len(rows), "archive_receipts": len(archive),
                              "conservation_residual_cells": dict(residuals),
                              "cash_review_required": number(run["DriftBase"]) != 0 or any(number(n) != 0 for n in decode(run["Overdraw"]).values())})
            findings.extend({"ordinal": ordinal, "kind": kind} for kind in sorted(set(issues)))
    return {"snapshots": len(snapshots), "expected": len(expected),
            "coverage_complete": len(seen) == len(expected) and seen == set(expected),
            "statuses": dict(statuses), "findings": findings, "results": summaries,
            "financially_accepted": False,
            "acceptance_note": "Offline integrity checks cannot accept accounting. Native-cash/source residual review and preservation proofs are separate requirements."}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("snapshots", type=Path)
    parser.add_argument("context", type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    try:
        report = audit(json.loads(args.snapshots.read_text()), json.loads(args.context.read_text()))
    except (KeyError, TypeError, ValueError, decimal.DecimalException):
        raise SystemExit("invalid commission evidence input, no acceptance report written")
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as handle:
        json.dump(report, handle, indent=2)
        handle.write("\n")
    print(json.dumps(report))


if __name__ == "__main__":
    main()
