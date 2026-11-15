import base64
import copy
import gzip
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import adapter as a
import verify_projection as v


def b64(raw):
    return base64.b64encode(raw).decode()


def commit(h):
    return {'Version': h, 'Hash': b64(bytes([h % 256]) * 32)}


def root_info(h):
    return {'Version': h, 'StoreInfos': [
        {'Name': name, 'Core': {'CommitID': commit(h)}} for name in sorted(a.STORES)]}


def fixture(first=1, last=2):
    rows = [{'schema': a.SCHEMA, 'kind': 'source_anchor', 'release': a.RELEASE,
             'chain': a.CHAIN, 'height': first - 1, 'commit': commit(first - 1),
             'native_commit_info': root_info(first - 1), 'genesis_sha256': 'a' * 64}]
    for h in range(first, last + 1):
        rows.append({'schema': a.SCHEMA, 'kind': 'height', 'release': a.RELEASE,
                     'chain': a.CHAIN, 'height': h, 'header': {
                         'chain_id': a.CHAIN, 'height': h, 'time': '2019-03-14T00:00:00Z',
                         'num_txs': 1, 'proposer_address': 'AB' * 20,
                         'last_block_id': {'hash': bytes([(h - 1) % 256]).hex().upper() * 32},
                         'app_hash': bytes([(h - 1) % 256]).hex().upper() * 32},
                     'block_hash': b64(bytes([h % 256]) * 32), 'commit': commit(h),
                     'native_commit_info': root_info(h), 'transactions': [b64(b'signed synthetic bytes')],
                     'begin': {'unknown_field': ['exact', 7, None]},
                     'deliver': [{'code': 9, 'log': 'failed\u0000result', 'data': b64(b'\xff\x00')}],
                     'end': {}, 'unknown_rich_future_field': {'shares': '1.000000000000000001'}})
    rows.append({'schema': a.SCHEMA, 'kind': 'history_completed', 'first_height': first,
                 'height': last, 'commit': commit(last), 'unexecuted_next_header': {
                     'chain_id': a.CHAIN, 'height': last + 1,
                     'last_block_id': {'hash': bytes([last % 256]).hex().upper() * 32},
                     'app_hash': bytes([last % 256]).hex().upper() * 32},
                 'unexecuted_next_block_hash': b64(b'N' * 32)})
    return rows


class AdapterTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.source = self.root / 'source'
        self.source.mkdir()

    def tearDown(self):
        self.tmp.cleanup()

    def component(self, rows=None, name='history.gz'):
        if rows is None:
            rows = fixture()
        path = self.source / name
        with gzip.open(path, 'wb') as f:
            for row in rows:
                f.write(json.dumps(row, ensure_ascii=True).encode() + b'\n')
        artifact = self.source / 'original.bin'
        if not artifact.exists():
            artifact.write_bytes(bytes(range(256)) * 2000)
        return {'first': rows[0]['height'] + 1, 'last': rows[-1]['height'],
                'genesis_sha256': 'a' * 64,
                'history': {'path': str(path), 'sha256': a.digest(path)},
                'artifacts': [{'path': str(artifact), 'sha256': a.digest(artifact)}]}

    def project(self, components):
        return a.project({'version': 1, 'chain': a.CHAIN, 'components': components}, self.root / 'out')

    def test_lossless_raw_rows_and_deferred_inclusions(self):
        c = self.component()
        r = self.project([c])
        self.assertFalse(r['production_admitted'])
        self.assertFalse(r['full_chain_books_tax_acceptance'])
        self.assertFalse(r['sql_inclusion_projection_complete'])
        self.assertFalse(r['full_hub1_history_complete'])
        self.assertEqual(r['components'][0]['transactions_preserved'], 2)
        rows = [json.loads(line) for line in (self.root / 'out/block_events.ndjson').read_bytes().splitlines()]
        raw_history = b''.join(base64.b64decode(dict(row['attributes'])['raw'])
                               for row in rows if row['type'] == 'hub1_native033_raw_history')
        self.assertEqual(raw_history, gzip.decompress(Path(c['history']['path']).read_bytes()))
        original = b''.join(base64.b64decode(dict(row['attributes'])['raw'])
                            for row in rows if row['type'] == 'hub1_source_artifact_chunk')
        self.assertEqual(original, Path(c['artifacts'][0]['path']).read_bytes())
        complete = [dict(row['attributes']) for row in rows if row['type'] == 'hub1_source_artifact_complete']
        self.assertEqual(complete[0]['sha256'], hashlib.sha256(original).hexdigest())
        self.assertEqual(int(complete[0]['chunks']), 3)
        keys = [(row['height'], row['phase'], row['event_index']) for row in rows]
        self.assertEqual(keys, sorted(set(keys)))
        for table in r['tables']:
            self.assertEqual(a.digest(table['file']['path']), table['file']['sha256'])
            self.assertEqual(Path(table['file']['path']).stat().st_mode & 0o777, 0o600)
        self.assertFalse((self.root / 'out/tx_inclusions.ndjson').exists())
        self.assertFalse((self.root / 'out/taxable_events.ndjson').exists())
        check = v.verify({'version': 1, 'chain': a.CHAIN, 'components': [c]}, self.root / 'out')
        self.assertTrue(check['all_raw_bytes_roundtripped'])
        self.assertEqual(check['signed_transactions_retained'], 2)

    def test_duplicate_unknown_key_refused(self):
        with self.assertRaises(a.Refused):
            a.decode(b'{"unknown":1,"unknown":2}')

    def test_invalid_histories_fail_before_output(self):
        cases = []
        for index, field, value in [(1, 'release', a.RELEASE.replace('0.33', '0.34')),
                                    (1, 'chain', 'cosmoshub-2'), (1, 'height', 2),
                                    (-1, 'first_height', 2), (-1, 'commit', commit(99)),
                                    (-1, 'unexecuted_next_block_hash', '')]:
            rows = fixture()
            rows[index][field] = value
            cases.append(rows)
        rows = fixture(); rows[1]['deliver'] = []; cases.append(rows)
        rows = fixture(); rows[2]['header']['app_hash'] = '00' * 32; cases.append(rows)
        rows = fixture(); rows[2]['header']['last_block_id']['hash'] = '00' * 32; cases.append(rows)
        rows = fixture(); rows[1]['native_commit_info']['StoreInfos'].pop(); cases.append(rows)
        rows = fixture(); rows[-1]['unexecuted_next_header']['height'] = 99; cases.append(rows)
        rows = fixture(); rows[-1]['unexecuted_next_header']['app_hash'] = '00' * 32; cases.append(rows)
        rows = fixture(); rows.pop(); cases.append(rows)
        for index, rows in enumerate(cases):
            with self.subTest(index=index):
                c = self.component(rows, name=str(index) + '.gz')
                c['last'] = 2
                with self.assertRaises((a.Refused, KeyError)):
                    self.project([c])
                self.assertFalse((self.root / 'out').exists())

    def test_bad_pin_and_gzip_trailer_refused(self):
        c = self.component()
        c['history']['sha256'] = 'b' * 64
        with self.assertRaises(a.Refused):
            self.project([c])
        path = Path(c['history']['path'])
        path.write_bytes(path.read_bytes()[:-8])
        c['history']['sha256'] = a.digest(path)
        with self.assertRaises(EOFError):
            self.project([c])
        self.assertFalse((self.root / 'out').exists())

    def test_gap_and_bad_seam_refused(self):
        c = self.component()
        d = self.component(fixture(4, 5), 'second.gz')
        with self.assertRaises(a.Refused):
            self.project([c, d])
        rows = fixture(3, 4)
        rows[0]['commit']['Hash'] = b64(b'x' * 32)
        rows[1]['header']['app_hash'] = '78' * 32
        d = self.component(rows, 'second.gz')
        with self.assertRaises(a.Refused):
            self.project([c, d])
        self.assertFalse((self.root / 'out').exists())

    def test_refuses_source_directory_output_and_existing_output(self):
        c = self.component()
        manifest = {'version': 1, 'chain': a.CHAIN, 'components': [c]}
        with self.assertRaises(a.Refused):
            a.project(manifest, self.source / 'out')
        self.project([c])
        with self.assertRaises(FileExistsError):
            self.project([c])

    def test_mutation_after_validation_retains_failed_output_without_receipt(self):
        c = self.component()
        original_scan = a.scan
        def mutate(component, visit=None):
            if visit is not None:
                Path(c['artifacts'][0]['path']).write_bytes(b'mutated')
            return original_scan(component, visit)
        with patch.object(a, 'scan', mutate), self.assertRaises(a.Refused):
            self.project([c])
        self.assertTrue((self.root / 'out/block_events.ndjson').exists())
        self.assertFalse((self.root / 'out/projection.safe.json').exists())

    def test_two_components(self):
        r = self.project([self.component(), self.component(fixture(3, 4), 'second.gz')])
        self.assertEqual(r['tables'][0]['rows'], 4)

    def test_projection_tampering_fails_roundtrip(self):
        c = self.component()
        self.project([c])
        path = self.root / 'out/block_events.ndjson'
        raw = path.read_bytes()
        path.write_bytes(raw.replace(b'base64-exact-source-bytes', b'base64-incorrect-encoding', 1))
        with self.assertRaises(a.Refused):
            v.verify({'version': 1, 'chain': a.CHAIN, 'components': [c]}, self.root / 'out')

    def test_progress_is_safe_and_cannot_overwrite_source(self):
        c = self.component()
        manifest = {'version': 1, 'chain': a.CHAIN, 'components': [c]}
        with self.assertRaises(a.Refused):
            a.project(manifest, self.root / 'bad', c['history']['path'])
        path = self.root / 'progress.safe.json'
        a.project(manifest, self.root / 'out', path)
        progress = json.loads(path.read_bytes())
        self.assertTrue(progress['projection_finished'])
        self.assertEqual(progress['done_input_bytes'], progress['total_input_bytes'])
        self.assertFalse(progress['production_admitted'])

    def test_closed_terminal_preserves_original_seal(self):
        rows = fixture(500001, 500015)
        for row in rows[1:-1]:
            row['transactions'] = []
            row['deliver'] = None
            row['header']['num_txs'] = 0
        proof = {'schema': 'native033-closed-terminal-v1', 'kind': 'closed_terminal_history_completed',
                 'first_height': 500001, 'height': 500015, 'chain': a.CHAIN, 'release': a.RELEASE,
                 'proof_mode': 'closed_pinned_native_reference', 'next_header_proof': False,
                 'financial_admission': False,
                 'source_manifest_sha256': '417424c8075ea3dcfd166eddaad8f5012c2510797abeca7f4f0864d399f0fd86',
                 'closed_reference': {'commit': commit(500015), 'block_hash': rows[-2]['block_hash'],
                                      'original_status': {'state_hash_verified_through': 500014,
                                                          'last_height_committed': 500015},
                                      'original_tail': {'height': 500015}}}
        rows[-1] = proof
        c = self.component(rows)
        path = self.source / 'terminal-proof.json'
        path.write_text(json.dumps(proof))
        c['terminal_proof'] = {'path': str(path), 'sha256': a.digest(path)}
        r = self.project([c])
        self.assertEqual(r['components'][0]['terminal_mode'], 'closed_pinned_native_reference')
        check = v.verify({'version': 1, 'chain': a.CHAIN, 'components': [c]}, self.root / 'out')
        self.assertEqual(check['blocks'], 15)
        self.assertEqual(check['signed_transactions_retained'], 0)
        proof['next_header_proof'] = True
        path.write_text(json.dumps(proof))
        c['terminal_proof']['sha256'] = a.digest(path)
        with self.assertRaises(a.Refused):
            a.scan(c)

    def test_terminal_never_uses_nonexistent_next_header(self):
        c = self.component(fixture(500001, 500015))
        with self.assertRaises(KeyError):
            a.scan(c)


if __name__ == '__main__':
    unittest.main()
