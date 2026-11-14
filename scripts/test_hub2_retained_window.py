import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import hub2_retained_window as retained


class RetainedWindowTests(unittest.TestCase):
    def actual(self):
        source = os.environ.get('HUB2_ACTUAL_RETAINED_WINDOW')
        profile = os.environ.get('EXACT_GAS_PRIVATE_PROFILE')
        if not source or not profile:
            self.skipTest('private retained inputs not supplied')
        return Path(source), Path(profile)

    def test_actual_bound_unchanged_input(self):
        source, profile = self.actual()
        result = retained.verify_inputs(source, profile)
        self.assertEqual(result['complete_input_heights'], 82090)
        self.assertEqual(result['input_record_count'], 375524)
        self.assertEqual(result['input_record_bytes'], 66454626)
        self.assertEqual(result['raw_cache_members'], 164184)
        self.assertEqual(result['marker_count'], 3387)

    def test_duplicate_fields_on_actual_record_refused(self):
        source, _ = self.actual()
        with (source/'window.movements.ndjson').open('rb') as stream:
            raw = stream.readline()
        retained.original.decode(raw)
        for key in (b'height', b'Height'):
            bad = b'{"'+key+b'":400001,'+raw[1:]
            with self.assertRaises(retained.original.Refused):
                retained.original.decode(bad)

    def test_changed_original_failed_result_refused(self):
        source, profile = self.actual()
        actual = retained.original.file_sha
        touched = []
        def digest(path):
            if path.name == 'component-result.json':
                touched.append(True)
                return '0'*64
            return actual(path)
        with patch.object(retained.original, 'file_sha', side_effect=digest):
            with self.assertRaises(retained.original.Refused):
                retained.verify_inputs(source, profile)
        self.assertTrue(touched)

    def test_source_symlink_refused(self):
        source, profile = self.actual()
        with tempfile.TemporaryDirectory() as directory:
            link = Path(directory)/'source'
            link.symlink_to(source, target_is_directory=True)
            with self.assertRaises(retained.original.Refused):
                retained.verify_inputs(link, profile)


if __name__ == '__main__':
    unittest.main()
