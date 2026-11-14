import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("proof_audit", Path(__file__).with_name("commission-proof-audit.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class AuditTests(unittest.TestCase):
    def setUp(self):
        self.context = dict(version="proof", build="sha256:build", chain="mainnet", from_day="2024-03-21", as_of="2026-11-12", validators=["validator-one"])
        run = dict(Validator="validator-one", Chain="mainnet", FromDay="2024-03-21T00:00:00Z", AsOf="2026-11-12T00:00:00Z", Status="reconciled", Reason="", Wallets=1, OpeningUnknown=0, Unresolved=0, Commission='{"2025-01":"100"}', Overdraw='{"fifo":"0","pro_rata":"0"}', DriftBase="0")
        rows = [dict(Validator="validator-one", Chain="mainnet", Rule=rule, Month="2025-01", Class="held", Detail="", ViaUnbonding=False, AmountBase="100") for rule in ("fifo", "pro_rata")]
        raw = '{\n  "a": 1\n}'
        self.snapshot = dict(version="proof", build="sha256:build", chain="mainnet", validator="validator-one", run_json=run, rows_json=rows, evidence_json=dict(Archive=[dict(Response=raw, SHA256=hashlib.sha256(raw.encode()).hexdigest())], Wallets=[dict(Address="wallet-one", OpeningKnown=True, Since="2024-03-21T00:00:00Z", ClosingDay="2026-11-12T00:00:00Z", OpeningSource=dict(Kind="ledger_exact", Height=123), ClosingSource=dict(Kind="ledger_exact", Height=456), ActualLiquidBase="100", ActualStakedBase={}, ActualUnbondingBase={}, SimulatedBase="100", DriftBase="0", Overdraw=dict(fifo="0", pro_rata="0"))]))

    def report(self):
        return module.audit([self.snapshot], self.context)

    def kinds(self):
        return {finding["kind"] for finding in self.report()["findings"]}

    def test_never_accepts_financially(self):
        report = self.report()
        self.assertTrue(report["coverage_complete"])
        self.assertEqual([], report["findings"])
        self.assertFalse(report["financially_accepted"])

    def test_partial_coverage(self):
        self.context["validators"].append("validator-two")
        self.assertFalse(self.report()["coverage_complete"])

    def test_all_identity_fences(self):
        for field in ("version", "build", "chain"):
            with self.subTest(field=field):
                snapshot = copy.deepcopy(self.snapshot)
                snapshot[field] = "wrong"
                report = module.audit([snapshot], self.context)
                self.assertIn("wrong_" + field, {x["kind"] for x in report["findings"]})

    def test_duplicate_snapshot_and_rows(self):
        report = module.audit([self.snapshot, self.snapshot], self.context)
        self.assertIn("duplicate_snapshot", {x["kind"] for x in report["findings"]})
        self.snapshot["rows_json"].append(copy.deepcopy(self.snapshot["rows_json"][0]))
        self.assertIn("duplicate_flow_key", self.kinds())

    def test_decimal_residual_not_rounded_or_accepted(self):
        self.snapshot["rows_json"][1]["AmountBase"] = "99.99999999999999999999999999999999999999999"
        self.assertIn("conservation_residual_pro_rata", self.kinds())
        self.assertNotIn("conservation_at_least_one_base_unit", self.kinds())

    def test_large_residual_and_extra_month(self):
        self.snapshot["rows_json"][1]["AmountBase"] = "98"
        self.assertIn("conservation_at_least_one_base_unit", self.kinds())
        self.snapshot["rows_json"][1]["Month"] = "2025-02"
        self.assertIn("flow_without_commission_month", self.kinds())

    def test_mutated_raw_archive_bytes(self):
        self.snapshot["evidence_json"]["Archive"][0]["Response"] = '{"a":1}'
        self.assertIn("archive_response_hash_mismatch", self.kinds())

    def test_cash_flags_are_independent_of_reconciled_status(self):
        self.snapshot["run_json"]["Overdraw"] = '{"fifo":"1","pro_rata":"0"}'
        self.assertTrue(self.report()["results"][0]["cash_review_required"])

    def test_failures_missing_openings_destinations(self):
        self.snapshot["run_json"].update(Status="error", Reason="refused", OpeningUnknown=1, Unresolved=1)
        self.assertIn("execution_failure", self.kinds())
        self.assertIn("missing_holdings_or_destinations", self.kinds())

    def test_invalid_quantities_and_midnight(self):
        for value in ("NaN", "Infinity", "-1"):
            with self.subTest(value=value):
                self.snapshot["rows_json"][0]["AmountBase"] = value
                with self.assertRaises(ValueError):
                    self.report()
        self.snapshot["rows_json"][0]["AmountBase"] = "100"
        self.snapshot["run_json"]["AsOf"] = "2026-11-12T01:00:00Z"
        with self.assertRaises(ValueError):
            self.report()

    def test_wallet_evidence_coverage_and_accounting(self):
        self.snapshot["evidence_json"]["Wallets"].append(copy.deepcopy(self.snapshot["evidence_json"]["Wallets"][0]))
        self.assertIn("wallet_accounting_coverage_mismatch", self.kinds())
        self.snapshot["evidence_json"]["Wallets"] = self.snapshot["evidence_json"]["Wallets"][:1]
        self.snapshot["evidence_json"]["Wallets"][0]["DriftBase"] = "-1"
        self.assertIn("wallet_drift_sum_mismatch", self.kinds())
        self.snapshot["evidence_json"]["Wallets"][0]["Overdraw"]["fifo"] = "1"
        self.assertIn("wallet_overdraw_sum_mismatch", self.kinds())
        self.snapshot["evidence_json"].pop("Wallets")
        self.assertIn("wallet_accounting_evidence_absent", self.kinds())

    def test_wallet_source_and_drift_are_independently_checked(self):
        wallet = self.snapshot["evidence_json"]["Wallets"][0]
        wallet["OpeningSource"]["Kind"] = "ledger_approximate"
        self.assertIn("wallet_holding_source_not_exact", self.kinds())
        wallet["OpeningSource"] = dict(Kind="archive_exact", Height=123, ReceiptHashes=["unrelated"])
        self.assertIn("wallet_holding_source_not_exact", self.kinds())
        wallet["ActualLiquidBase"] = "99"
        self.assertIn("wallet_drift_comparison_mismatch", self.kinds())

    def test_sql_json_string_exports_supported(self):
        for key in ("run_json", "rows_json", "evidence_json"):
            self.snapshot[key] = json.dumps(self.snapshot[key])
        self.assertEqual([], self.report()["findings"])

    def test_archive_source_is_bound_to_wallet_height_and_boundary(self):
        receipts = {}
        source = dict(Kind="archive_exact", Height=123, ReceiptHashes=[])
        def add(method, height, body, path=""):
            raw = json.dumps(body)
            digest = hashlib.sha256(raw.encode()).hexdigest()
            if digest not in source["ReceiptHashes"]:
                source["ReceiptHashes"].append(digest)
            receipts.setdefault(digest, []).append(dict(Address="wallet-one", Height=height, Method=method, QueryPath=path, Response=raw))
        for h, at in ((123, "2026-11-12T23:59:59Z"), (124, "2026-11-13T00:00:00Z")):
            add("block", h, dict(result=dict(block=dict(header=dict(chain_id="cosmoshub-4", height=str(h), time=at)))))
        for path in ("/cosmos.bank.v1beta1.Query/AllBalances", "/cosmos.staking.v1beta1.Query/DelegatorDelegations", "/cosmos.staking.v1beta1.Query/DelegatorUnbondingDelegations"):
            add("abci_query", 123, dict(result=dict(response=dict(code=0, height="123", value="AA=="))), path)
        self.assertTrue(module.exact_source(source, "wallet-one", "2026-11-12", receipts))
        self.assertFalse(module.exact_source(source, "wallet-two", "2026-11-12", receipts))
        self.assertFalse(module.exact_source(source, "wallet-one", "2026-11-11", receipts))
        source["Height"] = 122
        self.assertFalse(module.exact_source(source, "wallet-one", "2026-11-12", receipts))
        source["Height"] = 123
        source["ReceiptHashes"].pop()
        self.assertFalse(module.exact_source(source, "wallet-one", "2026-11-12", receipts))

    def test_output_is_aggregate_and_redacted(self):
        report = json.dumps(self.report())
        self.assertNotIn("validator-one", report)
        self.assertNotIn("sha256:build", report)


if __name__ == "__main__":
    unittest.main()
