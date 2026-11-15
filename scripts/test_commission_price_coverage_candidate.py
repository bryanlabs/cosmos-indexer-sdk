import copy
import hashlib
import unittest
from commission_price_coverage_candidate import coverage, eligible_rows, price_guard


def fixture():
    members = [hashlib.sha256(str(i).encode()).hexdigest() for i in range(324)]
    row = dict(version='private-candidate', chain='fixture', root_sha256=members[0],
               build='source-fixture', private_profile_sha256='a'*64,
               row_sha256='b'*64, status='reconciled')
    decision = dict(row, native_price_independence_proved=True,
                    valuation_status='unverified_no_valuations_persisted',
                    price_completeness_guard_passed=False, financial_coverage_quarantined=True,
                    capture_manifest_sha256='f'*64)
    manifest = dict(schema='commission-private-price-quarantine/v1',
                    canonical_membership=dict(root_sha256=members), cases=[decision],
                    accepted_valuation_versions=[])
    return row, decision, manifest


def allow(row, decision, manifest):
    manifest['accepted_valuation_versions'] = [row['version']]
    decision.update(valuation_status='verified_price_complete',
                    price_completeness_guard_passed=True, valuation_proof_sha256='c'*64,
                    financial_coverage_quarantined=False,
                    price_completeness=dict(required_nonzero=2, priced_nonzero=2,
                        missing=0, fallback=0, lookup_errors=0, unknown=0,
                        zero_or_non_economic_proved=3, source_provenance_complete=True,
                        cache_context_verified=True, requirements_inventory_complete=True,
                        input_row_sha256=row['row_sha256'],
                        source_receipt_sha256='d'*64, derivation_config_sha256='e'*64,
                        native_artifact_sha256='f'*64))


class CoverageTests(unittest.TestCase):
    def test_native_capture_not_financial_coverage(self):
        row,d,m=fixture(); result=coverage([row],m)
        self.assertEqual(result,dict(physical_saved=1,reusable_native=1,
            usable_price_clean=0,financial_quarantined=1,unaudited_or_changed=0))
        self.assertEqual(eligible_rows([row],m),[])

    def test_zero_origin_zero_usd_and_no_error_not_evidence(self):
        row,d,m=fixture()
        d.update(commission_origin_zero=True,USD='0',stored_errors=0)
        self.assertEqual(coverage([row],m)['usable_price_clean'],0)

    def test_complete_identity_bound_proof(self):
        row,d,m=fixture();allow(row,d,m)
        self.assertEqual(coverage([row],m)['usable_price_clean'],1)
        self.assertEqual(eligible_rows([row],m),[row])

    def test_no_version_review_no_acceptance(self):
        row,d,m=fixture();allow(row,d,m);m['accepted_valuation_versions']=[]
        self.assertEqual(coverage([row],m)['usable_price_clean'],0)

    def test_missing_fallback_error_unknown_all_fail(self):
        for field in ['missing','fallback','lookup_errors','unknown']:
            row,d,m=fixture();allow(row,d,m);d['price_completeness'][field]=1
            with self.subTest(field=field):
                self.assertFalse(price_guard(d))
                self.assertEqual(coverage([row],m)['usable_price_clean'],0)

    def test_unpriced_requirement_fails(self):
        row,d,m=fixture();allow(row,d,m);d['price_completeness']['priced_nonzero']=1
        self.assertEqual(coverage([row],m)['usable_price_clean'],0)

    def test_cache_and_source_unknown_fail(self):
        for field in ['source_provenance_complete','cache_context_verified']:
            row,d,m=fixture();allow(row,d,m);d['price_completeness'][field]=False
            self.assertEqual(coverage([row],m)['usable_price_clean'],0)

    def test_new_save_defaults_quarantine(self):
        row,d,m=fixture();new=dict(row,root_sha256=m['canonical_membership']['root_sha256'][1])
        result=coverage([row,new],m)
        self.assertEqual(result['physical_saved'],2)
        self.assertEqual(result['financial_quarantined'],2)
        self.assertEqual(result['unaudited_or_changed'],1)
        self.assertEqual(eligible_rows([new],m),[])

    def test_identity_mutation_defaults_quarantine(self):
        for field in ['build','private_profile_sha256','row_sha256']:
            row,d,m=fixture();allow(row,d,m);row[field]='changed'
            self.assertEqual(coverage([row],m)['usable_price_clean'],0)
            self.assertEqual(coverage([row],m)['unaudited_or_changed'],1)

    def test_drift_and_error_never_accepted(self):
        for status in ['drift','error']:
            row,d,m=fixture();allow(row,d,m);row['status']=status
            self.assertEqual(coverage([row],m)['usable_price_clean'],0)

    def test_membership_and_duplicates_fail_closed(self):
        row,d,m=fixture()
        with self.assertRaises(ValueError):coverage([row,row],m)
        with self.assertRaises(ValueError):coverage([dict(row,root_sha256='not-member')],m)
        m['canonical_membership']['root_sha256'].pop()
        with self.assertRaises(ValueError):coverage([row],m)

    def test_duplicate_priced_root_across_versions_rejected(self):
        row,d,m=fixture();allow(row,d,m)
        new=dict(row,version='other');nd=copy.deepcopy(d);nd['version']='other'
        m['cases'].append(nd);m['accepted_valuation_versions'].append('other')
        with self.assertRaises(ValueError):coverage([row,new],m)

    def test_bad_proof_and_boolean_counts_rejected(self):
        row,d,m=fixture();allow(row,d,m);d['price_completeness']['priced_nonzero']=True
        self.assertEqual(coverage([row],m)['usable_price_clean'],0)
        d['price_completeness']['priced_nonzero']=2;d['valuation_proof_sha256']='x'*64
        self.assertEqual(coverage([row],m)['usable_price_clean'],0)


    def test_explicit_quarantine_overrides_complete_proof(self):
        row,d,m=fixture();allow(row,d,m);d['financial_coverage_quarantined']=True
        self.assertEqual(coverage([row],m)['usable_price_clean'],0)

    def test_zero_required_needs_explicit_no_price_reason(self):
        row,d,m=fixture();allow(row,d,m)
        d['price_completeness'].update(required_nonzero=0,priced_nonzero=0)
        self.assertEqual(coverage([row],m)['usable_price_clean'],0)
        d['price_completeness']['no_pricing_required_reason']='zero_asset_quantity_proved'
        self.assertEqual(coverage([row],m)['usable_price_clean'],1)

    def test_wrong_native_artifact_and_incomplete_inventory_fail(self):
        row,d,m=fixture();allow(row,d,m)
        d['price_completeness']['native_artifact_sha256']='0'*64
        self.assertEqual(coverage([row],m)['usable_price_clean'],0)
        d['price_completeness']['native_artifact_sha256']='f'*64
        d['price_completeness']['requirements_inventory_complete']=False
        self.assertEqual(coverage([row],m)['usable_price_clean'],0)


if __name__=='__main__':unittest.main()
