import json
from pathlib import Path
import tempfile
import unittest

import adapter as a
import canonicalize as c


class CanonicalTests(unittest.TestCase):
    def row(self):
        return {'chain_id': a.CHAIN, 'height': 1, 'phase': 1, 'event_index': 0,
                'type': 'raw', 'attributes': [['raw', 'AA=='], ['escapes', '\\"\n\t<>/']]}

    def test_postgresql_order_and_exact_values(self):
        row = self.row()
        raw = c.canonical_event(row)
        self.assertEqual(json.loads(raw), row)
        self.assertEqual(list(json.loads(raw)), ['type', 'phase', 'height', 'chain_id', 'attributes', 'event_index'])
        self.assertIn(b'"type": "raw", "phase": 1', raw)
        self.assertNotIn(b'\\u003c', raw)

    def test_never_approximate_outside_domain(self):
        for key, value in [('height', True), ('height', 1.5), ('event_index', 2147483648),
                           ('type', 'non-ascii-\u2603')]:
            row = self.row()
            row[key] = value
            with self.assertRaises(a.Refused):
                c.canonical_event(row)
        row = self.row(); row['unknown'] = 1
        with self.assertRaises(a.Refused):
            c.canonical_event(row)
        row = self.row(); row['attributes'][0][1] = '\x00'
        with self.assertRaises(a.Refused):
            c.canonical_event(row)

    def test_full_input_transform_hash_and_no_reuse(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d)
            source, output, samples = (p / x for x in ('input', 'output', 'samples'))
            source.write_text(json.dumps(self.row()) + '\n')
            r = c.canonicalize(source, output, samples)
            self.assertEqual(r['rows'], 1)
            self.assertEqual(a.digest(source), r['input_sha256'])
            self.assertEqual(a.digest(output), r['output_sha256'])
            self.assertEqual(json.loads(source.read_bytes()), json.loads(output.read_bytes()))
            with self.assertRaises(FileExistsError):
                c.canonicalize(source, output, samples)


if __name__ == '__main__':
    unittest.main()
