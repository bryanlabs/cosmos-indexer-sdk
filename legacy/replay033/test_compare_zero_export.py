import argparse
import contextlib
import copy
import hashlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import compare_zero_export as boundary


class CompleteBoundaryTests(unittest.TestCase):
    def test_duplicate_keys_and_nonfinite_tokens_refused(self):
        for raw in ['{"a":1,"a":2}', '{"a":{"z":1,"z":2}}', '[NaN]', '[Infinity]', '[-Infinity]']:
            with self.assertRaises(ValueError):
                boundary.strict_json(raw)

    def test_object_order_ignored_and_numbers_exact_without_byte_gate(self):
        left = boundary.strict_json('{"b":1.0,"a":123456789012345678901234567890123456789}')
        right = boundary.strict_json('{"a":123456789012345678901234567890123456789,"b":1e0}')
        self.assertEqual(boundary.differences(left, right), [])
        self.assertEqual(boundary.tree_sha(left), boundary.tree_sha(right))
        changed = boundary.strict_json('{"a":123456789012345678901234567890123456788,"b":1}')
        self.assertEqual(len(boundary.differences(left, changed)), 1)

    def test_array_positions_types_and_all_fields_are_compared(self):
        original = boundary.strict_json('{"array":[1,"2",true,null],"other":{"kept":"yes"}}')
        for raw in ['{"array":["2",1,true,null],"other":{"kept":"yes"}}',
                    '{"array":[1,2,true,null],"other":{"kept":"yes"}}',
                    '{"array":[1,"2",1,null],"other":{"kept":"yes"}}',
                    '{"array":[1,"2",true],"other":{"kept":"yes"}}',
                    '{"array":[1,"2",true,null],"other":{}}',
                    '{"array":[1,"2",true,null],"other":{"kept":"yes"},"extra":false}']:
            changed = boundary.strict_json(raw)
            self.assertNotEqual(boundary.tree_sha(original), boundary.tree_sha(changed))
            self.assertTrue(boundary.differences(original, changed))

    def migration(self):
        base = Path(__file__).resolve().parent / 'testdata' / 'hub1-migration'
        script = boundary.pinned(base / 'v0.33.x-to-v0.34.0.py', boundary.SCRIPT_SHA)
        library = boundary.pinned(base / 'lib.py', boundary.LIB_SHA)
        return script, library

    def fixture(self):
        source = {'chain_id': 'cosmoshub-1', 'genesis_time': 'source-time', 'app_hash': '',
                  'consensus_params': {'block_size': {'max_bytes': 'old', 'max_gas': 'old'}, 'evidence': {}},
                  'app_state': {'not_reused': True}}
        native = {'release': boundary.RELEASE, 'source_height': 500000, 'source_app_hash': 'a' * 64, 'for_zero_height': True,
                  'app_state': {'mint': {'params': {'blocks_per_year': 'old'}},
                                'bank': {'send_enabled': False}, 'distr': {'withdraw_addr_enabled': False},
                                'accounts': [{'fixture_only': 'preserved'}]}, 'validators': []}
        candidate = copy.deepcopy(source)
        candidate['app_state'] = copy.deepcopy(native['app_state'])
        candidate['validators'] = copy.deepcopy(native['validators'])
        target = boundary.migrate(candidate, {'chain_id': 'cosmoshub-2', 'genesis_time': 'next-time'}, *self.migration())
        return source, native, target

    def test_unmodified_pinned_migration_only_changes_documented_fields(self):
        source, _, target = self.fixture()
        self.assertEqual(target['app_state']['accounts'], [{'fixture_only': 'preserved'}])
        self.assertEqual(target['app_state']['crisis'], {'constant_fee': {'amount': '1333000000', 'denom': 'uatom'}})
        self.assertEqual(target['app_state']['mint']['params']['blocks_per_year'], '4855015')
        self.assertIs(target['app_state']['bank']['send_enabled'], True)
        self.assertIs(target['app_state']['distr']['withdraw_addr_enabled'], True)
        self.assertEqual(target['consensus_params']['block'], {'max_bytes': '200000', 'max_gas': '2000000', 'time_iota_ms': '1000'})
        self.assertNotIn('block_size', target['consensus_params'])
        self.assertEqual(source['chain_id'], 'cosmoshub-1')
        self.assertEqual((target['chain_id'], target['genesis_time']), ('cosmoshub-2', 'next-time'))

    def invocation(self, directory, native_override=None):
        source, native, target = self.fixture()
        if native_override:
            native_override(native)
        raw = [json.dumps(value).encode() for value in (source, native, target)]
        paths = [directory / name for name in ['source.json', 'native.json', 'next.json']]
        for path, body in zip(paths, raw):
            path.write_bytes(body)
        proof = {'kind': 'checkpoint_next_header_only', 'release': boundary.RELEASE, 'chain': 'cosmoshub-1',
                 'checkpoint_height': 500000, 'state_hash_verified_through': 500000, 'next_header_height': 500001,
                 'application_hash': 'a' * 64}
        proof_path = directory / 'proof.json'
        proof_path.write_text(json.dumps(proof))
        migration = Path(__file__).resolve().parent / 'testdata' / 'hub1-migration'
        args = argparse.Namespace(source_genesis=paths[0], native_export=paths[1], next_genesis=paths[2],
                                  receipt=directory / 'receipt.json', checkpoint_proof=proof_path, migration_script=migration / 'v0.33.x-to-v0.34.0.py',
                                  migration_lib=migration / 'lib.py')
        hashes = {'SOURCE_SHA': hashlib.sha256(raw[0]).hexdigest(), 'NEXT_SHA': hashlib.sha256(raw[2]).hexdigest()}
        return args, hashes, raw

    def test_complete_receipt_private_exclusive_and_not_tax_acceptance(self):
        with tempfile.TemporaryDirectory() as temporary:
            args, hashes, original = self.invocation(Path(temporary))
            with patch.multiple(boundary, **hashes), contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(boundary.compare(args), 0)
                with self.assertRaises(FileExistsError):
                    boundary.compare(args)
            receipt = json.loads(args.receipt.read_bytes())
            self.assertTrue(receipt['full_genesis_tree_equal'])
            self.assertFalse(receipt['accounting_or_tax_acceptance'])
            self.assertEqual(receipt['difference_count'], 0)
            self.assertEqual(args.receipt.stat().st_mode & 0o777, 0o600)
            self.assertEqual([path.read_bytes() for path in [args.source_genesis, args.native_export, args.next_genesis]], original)

    def test_changed_native_field_records_failure_without_acceptance(self):
        with tempfile.TemporaryDirectory() as temporary:
            args, hashes, _ = self.invocation(Path(temporary), lambda value: value['app_state']['accounts'][0].update(fixture_only='changed'))
            with patch.multiple(boundary, **hashes), contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(boundary.compare(args), 1)
            receipt = json.loads(args.receipt.read_bytes())
            self.assertFalse(receipt['full_genesis_tree_equal'])
            self.assertEqual(receipt['difference_count'], 1)
            self.assertFalse(receipt['accounting_or_tax_acceptance'])

    def test_export_must_bind_the_actual_boundary_proof_root(self):
        for key, wrong in [('kind', 'wrong'), ('release', 'wrong'), ('chain', 'cosmoshub-2'),
                           ('checkpoint_height', 499999), ('state_hash_verified_through', 499999),
                           ('next_header_height', 500002), ('application_hash', 'b' * 64)]:
            with tempfile.TemporaryDirectory() as temporary:
                args, hashes, _ = self.invocation(Path(temporary))
                proof = json.loads(args.checkpoint_proof.read_bytes())
                proof[key] = wrong
                args.checkpoint_proof.write_text(json.dumps(proof))
                with patch.multiple(boundary, **hashes), self.assertRaises(ValueError):
                    boundary.compare(args)
                self.assertFalse(args.receipt.exists())

    def test_source_pin_and_partial_native_or_wrong_height_fail_closed(self):
        for mutate in [lambda value: value.update(source_height=499999), lambda value: value.update(source_height='500000'),
                       lambda value: value.update(for_zero_height=False), lambda value: value.update(extra=True),
                       lambda value: value.update(source_app_hash='b' * 64), lambda value: value.update(source_app_hash='not-a-root'),
                       lambda value: value.pop('validators')]:
            with tempfile.TemporaryDirectory() as temporary:
                args, hashes, _ = self.invocation(Path(temporary), mutate)
                with patch.multiple(boundary, **hashes), self.assertRaises(ValueError):
                    boundary.compare(args)
                self.assertFalse(args.receipt.exists())
        with tempfile.TemporaryDirectory() as temporary:
            args, _, _ = self.invocation(Path(temporary))
            with self.assertRaises(ValueError):
                boundary.compare(args)
            self.assertFalse(args.receipt.exists())


if __name__ == '__main__':
    unittest.main()
