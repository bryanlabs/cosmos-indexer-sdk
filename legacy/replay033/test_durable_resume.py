import copy
import gzip
import hashlib
import json
import os
from pathlib import Path
import tempfile
import time
import unittest

from consume_bounded import Consumer, NAMES, sha
from consume_closed import ClosedConsumer, ClosedHistory, closed_lines
from durable_resume import (SeekableRows, canonical, graph_dump, graph_load,
                            load_checkpoint, prepare_sources, save_checkpoint)


class Durable(unittest.TestCase):
    def test_typed_graph_preserves_aliases_and_journal_state(self):
        c = ClosedConsumer(lambda _: None, {}, 3)
        state = {b'key': 'dmFsdWU='}
        c.latest = {name: state for name in NAMES}
        c.versions = {(name, 'hash'): state for name in NAMES}
        payload = graph_load(graph_dump(dict(c=c.latest, versions=c.versions, counts=c.counts,
                                           keys={b'key'}, scope=(1, -1, -1, 'block'))))
        self.assertIs(payload['c']['acc'], payload['versions']['acc', 'hash'])
        self.assertEqual(payload['keys'], {b'key'})
        payload['counts']['block'] += 1
        self.assertEqual(payload['counts']['block'], 1)

    def fixture(self, root):
        rows = [dict(kind='height_completed', height=i, payload=hashlib.sha256(str(i).encode()).hexdigest()) for i in range(1, 12001)]
        rows.append(dict(kind='observation_completed', height=12000))
        binding = dict(first=1, through=12000, streams={})
        for name, last in [('movements', 'observation_completed'), ('native-history', 'history_completed')]:
            stream_rows = copy.deepcopy(rows)
            stream_rows[-1]['kind'] = last
            raw = b''.join(canonical(r) + b'\n' for r in stream_rows)
            data = gzip.compress(raw, mtime=0)
            (root / (name + '.source-closed.gz')).write_bytes(data)
            binding['streams'][name] = dict(sha256=hashlib.sha256(data).hexdigest(), compressed_bytes=len(data),
                                           records=len(stream_rows), raw_bytes=len(raw), raw_sha256=hashlib.sha256(raw).hexdigest(), completion_kind=last)
        (root / 'capture.private.json').write_bytes(canonical(binding))
        return binding

    def test_source_cursor_and_small_sample_seek_cost(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            binding = self.fixture(root)
            start = time.monotonic()
            manifest = prepare_sources(root, binding, root / 'seek', closed_lines, chunk_bytes=65536)
            preparation = time.monotonic() - start
            reader = SeekableRows(root / 'seek', manifest, 'movements')
            for _ in range(11000):
                next(reader)
            cursor = reader.cursor()
            expected = next(reader)
            start = time.monotonic()
            restored = SeekableRows(root / 'seek', manifest, 'movements', cursor)
            self.assertEqual(next(restored), expected)
            seek = time.monotonic() - start
            start = time.monotonic()
            source = closed_lines(root, binding, 'movements')
            for _ in range(11001):
                next(source)
            prefix = time.monotonic() - start
            print(json.dumps(dict(sample_records=12001, preparation_seconds=preparation,
                                  gzip_prefix_seconds=prefix, chunk_restore_seconds=seek,
                                  chunk_bytes=65536, chunks=len(manifest['streams']['movements']['chunks']))))
            chunk = root / 'seek' / manifest['streams']['movements']['chunks'][cursor[0]]['file']
            chunk.write_bytes(chunk.read_bytes() + b' ')
            with self.assertRaises(ValueError):
                next(SeekableRows(root / 'seek', manifest, 'movements', cursor))

    def test_integrated_seal_rejects_tamper_torn_and_identity(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            c = ClosedConsumer(lambda _: None, {}, 3)
            c.heights = c.journal.heights = 1
            c.latest = {name: {} for name in NAMES}
            history = ClosedHistory.__new__(ClosedHistory)
            history.anchor, history.cache, history.last = {}, {}, 1
            class Cursor:
                def cursor(self):
                    return [0, 10]
            history.rows = Cursor()
            def native(path, height):
                path.mkdir()
                (path / 'db').write_bytes(b'exact scratch db')
            identity = dict(test='identity')
            result = save_checkpoint(root / 'checkpoints', identity, c, history, Cursor(), native)
            gen, state, _ = load_checkpoint(root / 'checkpoints', identity)
            self.assertEqual(state['consumer']['heights'], 1)
            with self.assertRaises(ValueError):
                load_checkpoint(root / 'checkpoints', dict(test='changed'))
            (root / 'checkpoints' / 'ignored.partial').mkdir()
            load_checkpoint(root / 'checkpoints', identity)
            (gen / 'native' / 'db').write_bytes(b'tampered scratch db')
            with self.assertRaises(ValueError):
                load_checkpoint(root / 'checkpoints', identity)
            self.assertEqual(result['height'], 1)


if __name__ == '__main__':
    unittest.main()
