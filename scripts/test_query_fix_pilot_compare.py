import copy
import gzip
import hashlib
import json
import pathlib
import tempfile
import unittest

import query_fix_pilot_compare as proof


class ComparisonTests(unittest.TestCase):
    def fixture(self):
        root = 'fixture-root'
        sha = lambda x: hashlib.sha256(x.encode()).hexdigest()
        old_manifest = dict(Chain='mainnet', Validator=root, Version='baseline-version', Build='baseline-build')
        new_manifest = dict(Chain='mainnet', Validator=root, Version='candidate-version', Build='candidate-build')
        expected = dict(chain='mainnet', root_sha256=sha(root), old_version='baseline-version', new_version='candidate-version', old_build='baseline-build', new_build='candidate-build', from_day='2024-03-21T00:00:00Z', as_of_inclusive='2026-11-12T00:00:00Z', profile_sha256=sha('profile'), corrections_sha256=sha('corrections'))
        output = dict(run=dict(Chain='mainnet', Validator=root, FromDay=expected['from_day'], AsOf=expected['as_of_inclusive'], ComputedAt='2026-11-15T00:00:00Z', Status='reconciled', Wallets=2), rows=[dict(Rule='fifo', AmountBase='fixture-native-value')], evidence=dict(PrivateProfile=dict(ProfileSHA256=sha('profile')), SourceCorrectionProfile=dict(ProfileSHA256=sha('corrections')), Wallets=[dict(Address='fixture-wallet', ActualLiquidBase='fixture-nonzero')], SourceCorrectionReads=[dict(Snapshot='snapshot-a', Status='verified', WalletSHA256=sha('wallet'))]))
        new = copy.deepcopy(output)
        new['run']['ComputedAt'] = '2026-11-15T01:00:00Z'
        new['evidence']['SourceCorrectionReads'][0]['Snapshot'] = 'snapshot-b'
        return old_manifest, output, new_manifest, new, expected

    def test_complete_match_distinct_metadata_not_same_input_claim(self):
        result = proof.compare(*self.fixture())
        self.assertEqual(result['result'], 'match_with_distinct_capture_metadata')
        self.assertTrue(all(result['matches'].values()))
        self.assertFalse(result['same_input_equivalence_proven'])
        self.assertFalse(result['rollout_authorized'])
        self.assertNotEqual(result['snapshot_metadata'][0], result['snapshot_metadata'][1])

    def test_changed_usable_holding_is_not_ignored(self):
        a, b, c, d, e = self.fixture()
        d['evidence']['Wallets'][0]['ActualLiquidBase'] = 'changed-fixture-value'
        result = proof.compare(a, b, c, d, e)
        self.assertEqual(result['result'], 'economic_match_evidence_changed')
        self.assertFalse(result['matches']['evidence'])

    def test_economic_difference_report_has_no_values(self):
        a, b, c, d, e = self.fixture()
        d['rows'][0]['AmountBase'] = 'sensitive-fixture-value'
        result = proof.compare(a, b, c, d, e)
        self.assertEqual(result['result'], 'economic_mismatch')
        self.assertNotIn('sensitive-fixture-value', json.dumps(result))

    def test_missing_field_refuses(self):
        a, b, c, d, e = self.fixture()
        del d['evidence']
        with self.assertRaises(AssertionError):
            proof.compare(a, b, c, d, e)

    def test_wrong_profile_or_build_refuses(self):
        for field in ['profile', 'build']:
            a, b, c, d, e = self.fixture()
            if field == 'profile':
                d['evidence']['PrivateProfile']['ProfileSHA256'] = 'wrong'
            else:
                c['Build'] = 'wrong'
            with self.assertRaises(AssertionError):
                proof.compare(a, b, c, d, e)

    def test_failure_not_promoted_as_match(self):
        a, b, c, d, e = self.fixture()
        d['run']['Status'] = 'error'
        result = proof.compare(a, b, c, d, e)
        self.assertEqual(result['result'], 'pilot_failed')
        self.assertFalse(result['complete_native_result'])

    def test_changed_receipt_scope_is_not_metadata(self):
        a, b, c, d, e = self.fixture()
        d['evidence']['SourceCorrectionReads'][0]['WalletSHA256'] = 'different-wallet'
        result = proof.compare(a, b, c, d, e)
        self.assertFalse(result['matches']['evidence'])

    def test_identity_keys_redacted(self):
        key = 'cosmos1' + 'fixture' * 10
        result = proof.difference_paths({key: 1}, {key: 2})
        self.assertNotIn(key, json.dumps(result))
        self.assertIn('identity-sha256', result[0])

    def test_raw_and_encoded_digest_checked(self):
        root = pathlib.Path(tempfile.mkdtemp(prefix='query-proof-comparison-retained-'))
        fields = []
        for name, value in dict(run={}, rows=[], evidence={}).items():
            raw = json.dumps(value).encode()
            encoded = gzip.compress(raw, mtime=0)
            (root / (name + '.json.gz')).write_bytes(encoded)
            fields.append(dict(Field=name, Codec='gzip-json-bytes-v1', RawBytes=len(raw), RawSHA256=proof.digest(raw), EncodedBytes=len(encoded), EncodedSHA256=proof.digest(encoded)))
        manifest = dict(Fields=fields)
        data = json.dumps(manifest).encode()
        (root / 'manifest.json').write_bytes(data)
        _, decoded, _ = proof.load_capture(root, proof.digest(data))
        self.assertEqual(set(decoded), proof.FIELDS)
        (root / 'rows.json.gz').write_bytes(b'corrupt-retained-attempt')
        with self.assertRaises(AssertionError):
            proof.load_capture(root, proof.digest(data))


if __name__ == '__main__':
    unittest.main()
