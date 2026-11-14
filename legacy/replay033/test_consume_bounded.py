import copy
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import zlib
from consume_bounded import Consumer, HistoryWindow, native_numbers, selected_lines, sha, verify
from compare_zero_export import strict_json

ROOT = os.environ.get('HUB1_BOUNDED_CAPTURE')

class Scalars(unittest.TestCase):
    def test_native_numeric_types_not_strings(self):
        value = native_numbers(strict_json('{"number":1,"text":"1"}'))
        self.assertIs(type(value['number']), int)
        self.assertIs(type(value['text']), str)
    def test_duplicate_fields_and_fraction_refused(self):
        for text in ('{"a":1,"a":2}', '{"a":1.0}', '{"a":NaN}'):
            with self.assertRaises(ValueError):
                native_numbers(strict_json(text))

@unittest.skipUnless(ROOT, 'explicit actual read-only bounded capture required')
class Actual(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.root = Path(ROOT)
        cls.binding = json.loads((cls.root / 'capture.private.json').read_bytes())
        cls.history = {int(r['height']): r for r in selected_lines(cls.root, cls.binding, 'native-history') if r['kind'] == 'height'}
    def control(self, name):
        changed = False
        consumer = Consumer(lambda value: None, self.history, self.binding['through'])
        def records():
            nonlocal changed
            for original in selected_lines(self.root, self.binding, 'movements'):
                r = original
                target = False
                if not changed:
                    if name == 'before' and r['kind'] == 'native_write':
                        r = dict(r, before='AQIDBA=='); target = True
                    if name == 'raw_key' and r['kind'] == 'native_state':
                        r = dict(r, label=r['label'].split('/key:')[0] + '/key:/wD/'); target = True
                    if name == 'unknown_prefix' and r['kind'] == 'native_write':
                        r = dict(r, label=r['label'].split('/key:')[0] + '/key:/wD/'); target = True
                    if name == 'phase' and r['kind'] == 'native_state_hash':
                        r = dict(r, phase='ante'); target = True
                    if name == 'missing_anchor_key' and r['kind'] == 'native_state_checkpoint' and r['before']:
                        r = dict(r, before=r['before'][1:]); target = True
                    if name == 'unknown_record_field':
                        r = dict(r, unproved_sourceComplete=True); target = True
                    if name == 'fee_classification' and r['kind'] == 'movement':
                        r = dict(r, classification='paid_income'); target = True
                    if name == 'native_result' and r['kind'] == 'native_execution' and r['phase'] == 'transaction':
                        r = copy.deepcopy(r); r['after']['code'] = 999; target = True
                    if name == 'gap' and r['kind'] == 'height_completed':
                        r = dict(r, height=r['height'] + 1); target = True
                    if name == 'fake_completion':
                        r = dict(r, kind='observation_completed'); target = True
                    if name == 'unknown_kind':
                        r = dict(r, kind='invented_financial_effect'); target = True
                if name == 'overlap' and not changed and r['kind'] == 'height_completed':
                    changed = True
                    yield r
                changed = changed or target
                yield r
        with self.assertRaises((ValueError, KeyError)):
            for r in records():
                consumer.accept(r)
        self.assertTrue(changed, 'actual-data negative target absent')
    def test_actual_controls(self):
        for name in ('before', 'raw_key', 'unknown_prefix', 'phase', 'missing_anchor_key', 'unknown_record_field', 'fee_classification', 'native_result', 'gap', 'overlap', 'fake_completion', 'unknown_kind'):
            with self.subTest(control=name):
                self.control(name)
    def test_history_cursor_keeps_current_and_next_only(self):
        history = HistoryWindow(self.root, self.binding)
        for height in range(1, self.binding['through'] + 2):
            self.assertEqual(history[height]['height'], height)
            self.assertLessEqual(len(history.cache), 2)
        with self.assertRaises(ValueError):
            history[1]
    def test_changed_source_bytes_refused(self):
        binding = copy.deepcopy(self.binding)
        binding['streams']['movements']['fragment_sha256'] = '0' * 64
        with self.assertRaises(ValueError):
            next(selected_lines(self.root, binding, 'movements'))
    def test_truncated_gzip_no_boundary_refused_even_rehashed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with (self.root / 'movements.source-fragment.gz').open('rb') as source:
                raw = source.read(1024)
            path = root / 'movements.source-fragment.gz'
            path.write_bytes(raw)
            binding = copy.deepcopy(self.binding)
            binding['streams']['movements']['compressed_fragment_bytes'] = len(raw)
            binding['streams']['movements']['fragment_sha256'] = hashlib.sha256(raw).hexdigest()
            with self.assertRaises((ValueError, zlib.error)):
                list(selected_lines(root, binding, 'movements'))
    def test_false_source_completion_never_adapted(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binding = copy.deepcopy(self.binding)
            binding['source_whole_complete'] = True
            (root / 'capture.private.json').write_text(json.dumps(binding))
            with self.assertRaises(ValueError):
                verify(root, Path('/no-helper'), root / 'receipt.json')

@unittest.skipUnless(os.environ.get('HUB1_CANONICAL_BOUNDARY'), 'explicit accepted read-only canonical fixture required')
class CanonicalTail(unittest.TestCase):
    def test_actual_terminal_tail_cannot_replace_canonical_opening(self):
        import verify_terminal_fifteen as terminal
        directory = os.environ['HUB1_CANONICAL_BOUNDARY']
        result = terminal.canonical_boundary(directory)
        self.assertEqual(result['canonical_migration_height'], 500000)
        self.assertFalse(result['terminal_export_is_next_chain_opening'])
        original = terminal.strict_json
        changed = False
        def actual_tail_substitution(raw):
            nonlocal changed
            value = original(raw)
            if type(value) is dict and 'source_height' in value:
                from compare_zero_export import Number
                value['source_height'] = Number('500015')
                changed = True
            return value
        with patch.object(terminal, 'strict_json', actual_tail_substitution):
            with self.assertRaises(ValueError):
                terminal.canonical_boundary(directory)
        self.assertTrue(changed, 'actual accepted export fixture target absent')

if __name__ == '__main__':
    unittest.main()
