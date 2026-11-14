#!/usr/bin/env python3
"""Synthetic decoder controls, no private evidence required."""
import unittest

import freeze_exact_gas_profile as freeze


class ExactIntegerTests(unittest.TestCase):
    def test_strict_number_lexemes(self):
        for text in ("0", "1", "-1", "9223372036854775807", "-9223372036854775808"):
            self.assertEqual(str(freeze.number(text)), text)
        for text in ("-0", "+1", "01", "1e0", "1.0", "9223372036854775808", "-9223372036854775809"):
            with self.assertRaises(ValueError):
                freeze.number(text)

    def test_unicode_case_aliases(self):
        for pair in (("s", "ſ"), ("k", "K")):
            with self.assertRaises(ValueError):
                freeze.obj([(pair[0], 1), (pair[1], 2)])

    def test_no_number_information_lost(self):
        for raw in (b'{"n":-0}', b'{"n":1e0}', b'{"n":1.0}', b'{"n":NaN}',
                    b'{"n":1,"n":2}', b'{"n":1,"N":2}', b'{"n":9223372036854775808}'):
            with self.assertRaises(ValueError):
                freeze.load(raw)
        self.assertEqual(freeze.load(b'{"n":1}'), {"n": 1})


if __name__ == "__main__":
    unittest.main()
