import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('window', Path(__file__).with_name('hub2_sealed_window.py'))
w = importlib.util.module_from_spec(spec)
spec.loader.exec_module(w)


def records(first=1, last=4):
    raw = bytearray()
    for h in range(first, last+1):
        for kind, phase in [('accounting', 'begin_block'), ('movement', 'message'),
                            ('principal', 'message'), ('accounting', 'end_block'), ('height_completed', 'block')]:
            r = dict(kind=kind, phase=phase, height=h, tx=-1, message=-1, code=0, status='committed',
                     label='', address='', denom='', amount='')
            if phase == 'message':
                r.update(tx=0, message=0, address='fixture', denom='asset', amount='-1' if kind == 'movement' else '0',
                         status='aborted', delegator='fixture', validator='fixture')
            raw.extend((json.dumps(r, separators=(',', ':'))+'\n').encode())
    return bytes(raw)


class WindowTest(unittest.TestCase):
    def test_movement_lossless_prefix_and_aborts(self):
        with tempfile.TemporaryDirectory() as d:
            d = Path(d)
            raw = records()+b'{"kind":"partial'
            w.save(d/'source', raw)
            n = w.select_prefix(d/'source', 'movements', d/'prefix', d/'selected', 2, 3)
            self.assertEqual((d/'source').read_bytes(), raw)
            self.assertEqual((d/'prefix').read_bytes(), records(1, 3))
            self.assertEqual((d/'selected').read_bytes(), records(2, 3))
            self.assertEqual(n, len(records(1, 3)))
            self.assertEqual((d/'selected').stat().st_mode & 0o777, 0o600)

    def test_movement_refusals(self):
        for name, raw in {
            'missing-context': records(2, 4),
            'missing-height': records(1, 1)+records(3, 4),
            'duplicate-height': records(1, 2)+records(2, 4),
            'reorder': records(2, 2)+records(1, 1),
            'no-completion': records(1, 2)+records(3, 3).rsplit(b'\n', 2)[0]+b'\n',
            'partial': records(1, 3)[:-1],
            'oversized': b'x'*(1048577),
            'bad-completion': records(1, 3).replace(b'"phase":"block"', b'"phase":"message"'),
        }.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory() as d:
                d = Path(d)
                w.save(d/'source', raw)
                with self.assertRaises(w.Refused):
                    w.select_prefix(d/'source', 'movements', d/'prefix', first=2, last=3)
                self.assertEqual((d/'source').read_bytes(), raw)

    def test_sparse_prefixes_not_normalized(self):
        for kind, raw, expected in [
                ('validators', b'{"h":0,"u":[{}]}\n{ "h":3, "u":[{}]}\n{"h":4,"u":[{}]}\npartial',
                 b'{"h":0,"u":[{}]}\n{ "h":3, "u":[{}]}\n'),
                ('differences', b'1 tx:0:tags\n3 tx:1:gas\n5 tx:0:tags\npartial',
                 b'1 tx:0:tags\n3 tx:1:gas\n')]:
            with tempfile.TemporaryDirectory() as d:
                d = Path(d)
                w.save(d/'source', raw)
                w.select_prefix(d/'source', kind, d/'prefix', first=2, last=3)
                self.assertEqual((d/'prefix').read_bytes(), expected)
                self.assertEqual((d/'source').read_bytes(), raw)

    def test_sparse_refusals(self):
        for kind, raw in [('validators', b'{"h":1,"u":[{}]}\n'), ('validators', b'{"h":0,"u":null}\n'),
                          ('validators', b'{"h":0,"u":[{}],"unknown":1}\n'),
                          ('validators', b'{"h":0,"u":[{}]}\n{"h":0,"u":[{}]}\n'),
                          ('differences', b'2 tag\n1 tag\n'), ('differences', b'02 tag\n'),
                          ('differences', b'1 tag\n3 partial')]:
            with self.subTest(kind=kind, raw=raw), tempfile.TemporaryDirectory() as d:
                d = Path(d)
                w.save(d/'source', raw)
                with self.assertRaises(w.Refused):
                    w.select_prefix(d/'source', kind, d/'prefix', first=2, last=3)

    def test_exact_readiness_without_completion_claim(self):
        good = dict(release=w.RELEASE, chain='cosmoshub-2', first_height_this_run=1, target=2902000,
                    last_height_committed=482091, state_hash_verified_through=482090)
        self.assertTrue(w.ready_status(good))
        for key, value in [('chain', 'cosmoshub-1'), ('release', 'latest'), ('first_height_this_run', 400001),
                           ('target', 482090), ('last_height_committed', 482090),
                           ('state_hash_verified_through', 482089), ('stop_reason', 'error'),
                           ('last_height_committed', '482091'), ('state_hash_verified_through', True)]:
            s = dict(good)
            s[key] = value
            self.assertFalse(w.ready_status(s))

    def test_capture_binding_append_only(self):
        class FakeRemote:
            def __init__(self, mutation=None):
                self.raw = records()
                self.mutation = mutation
                self.calls = 0

            def stat(self, _):
                self.calls += 1
                if self.calls == 1:
                    return len(self.raw), 123
                if self.mutation == 'shrink':
                    return len(self.raw)-1, 123
                if self.mutation == 'replace':
                    return len(self.raw), 124
                return len(self.raw)+1000, 123

            def movement_offset(self, _, last):
                return len(records(1, last))

            def capture(self, _, size, dest):
                w.save(dest, self.raw[:size])

            def digest(self, _, size):
                return '0'*64 if self.mutation == 'mutate' else w.sha(self.raw[:size])

        old = w.LAST
        try:
            w.LAST = 3
            with tempfile.TemporaryDirectory() as d:
                r = w.seal(FakeRemote(), 'movements', Path(d))
                self.assertEqual(r['bytes'], len(records(1, 3)))
                self.assertEqual(r['sha256'], w.sha(records(1, 3)))
            for name in ('shrink', 'replace', 'mutate'):
                with self.subTest(name=name), tempfile.TemporaryDirectory() as d:
                    with self.assertRaises(w.Refused):
                        w.seal(FakeRemote(name), 'movements', Path(d))
                    self.assertTrue((Path(d)/'movements.captured-prefix.raw').exists())
        finally:
            w.LAST = old

    def test_validation_cli_is_reproduced_not_caller_chosen(self):
        class Result:
            returncode = 0
            stdout = b''
        for state in ('good', 'wrong-binary', 'dirty-source', 'bad-signature'):
            with self.subTest(state=state), tempfile.TemporaryDirectory() as d:
                d = Path(d)
                supplied = d/'supplied-cli'
                w.save(supplied, b'wrong' if state == 'wrong-binary' else b'fixture-cli')
                def run(command, **kwargs):
                    r = Result()
                    if command[0] == 'git':
                        if '--porcelain' in command:
                            r.stdout = b'M file' if state == 'dirty-source' else b''
                        elif '--format=%G?' in command:
                            r.stdout = b'B' if state == 'bad-signature' else b'G'
                        else:
                            r.stdout = b'0'*40
                    else:
                        Path(command[command.index('-o')+1]).write_bytes(b'fixture-cli')
                    return r
                with patch.object(w.subprocess, 'run', side_effect=run):
                    if state == 'good':
                        w.bind_cli(supplied, d)
                        self.assertTrue((d/'cli.binding.private.json').exists())
                    else:
                        with self.assertRaises(w.Refused):
                            w.bind_cli(supplied, d)
                        self.assertFalse((d/'cli.binding.private.json').exists())

    def test_exclusive_private_files(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d)/'evidence'
            w.save(p, b'original')
            self.assertEqual(p.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(FileExistsError):
                w.save(p, b'replacement')
            self.assertEqual(p.read_bytes(), b'original')

    def test_duplicate_unknown_numbers(self):
        for raw in [b'{"height":1,"height":2}', b'{"height":1,"Height":2}', b'{"x":NaN}']:
            with self.assertRaises(w.Refused):
                w.decode(raw)
        self.assertEqual(w.decode(b'{"amount":"123456789012345678901234567890"}')['amount'],
                         '123456789012345678901234567890')

if __name__ == '__main__':
    unittest.main(verbosity=2)
