import base64
import copy
import gzip
import hashlib
from pathlib import Path
import tempfile
import unittest
from consume_bounded import CommitJournal
from consume_split import SplitHistory, fast_forward, json
from reconcile_whole import check_profiles
from verify_movement_rehearsal import commitment

LABELS = ['store:acc:account', 'store:main:main', 'store:staking:staking', 'store:slashing:slashing', 'store:mint:mint',
          'store:distr:distribution', 'store:gov:governance', 'store:fee:fee_collection', 'store:params:params']
NAMES = [label.split(':')[1] for label in LABELS]


def b64(text):
    return base64.b64encode(text.encode()).decode()


def stream(heights):
    """A tiny recorded range: one direct write, one nested cache write that is flushed, one that is dropped."""
    state = {label: {b64('k0'): b64('v0'), b64('empty'): ''} for label in LABELS}
    rows = [dict(kind='native_state_checkpoint', label='segment_baseline/' + label, height=1, tx=-1, message=-1, phase='block', status='committed', code=0,
                 before=[{'key': k, 'value': v} if v else {'key': k} for k, v in sorted(state[label].items(), key=lambda i: base64.b64decode(i[0]))]) for label in LABELS]
    for h in range(1, heights + 1):
        store = LABELS[h % 9]
        common = dict(height=h, tx=-1, message=-1, status='committed')
        key, value = b64('h%d' % h), b64('value%d' % h)
        rows.append(dict(common, kind='native_write', label=store + '/key:' + key, phase='begin_block', native_view=2, native_parent=1, write_operation='set', before=None, after=value))
        rows.append(dict(common, kind='native_write', label=store + '/key:' + b64('k0'), phase='begin_block', native_view=3, native_parent=1, write_operation='delete', before=state[store].get(b64('k0')), after=None))
        rows.append(dict(common, kind='native_cache_write', label=store, phase='begin_block', native_view=2, native_parent=1))
        rows.append(dict(common, kind='native_cache_write', label=store, phase='commit', native_view=1, native_parent=0))
        state[store][key] = value
        for phase in ('begin_block', 'end_block', 'block'):
            for label in LABELS:
                raw = {base64.b64decode(k): v for k, v in state[label].items()}
                rows.append(dict(common, kind='native_state_hash', label=label, phase=phase, before=commitment(raw), after=commitment(raw)))
        rows.append(dict(common, kind='height_completed', label='height', phase='block'))
    return [json.dumps(r, separators=(',', ':')).encode() for r in rows], state


class FastForward(unittest.TestCase):
    def test_rebuilt_state_equals_the_every_height_journal(self):
        lines, state = stream(40)
        whole = CommitJournal()
        for raw in lines:
            record = json.loads(raw)
            whole.accept(record)
            if record['kind'] == 'height_completed' and record['height'] == 25:
                break
        baseline, phases = fast_forward(iter(lines), 1, 25)
        self.assertEqual(phases, 75)
        self.assertEqual({r['label'].split('/', 1)[1]: {p['key']: p.get('value', '') for p in r['before']} for r in baseline}, whole.canonical)
        self.assertTrue(all(r['height'] == 26 and r['label'].startswith('segment_baseline/') for r in baseline))
        for r in baseline:
            keys = [base64.b64decode(p['key']) for p in r['before']]
            self.assertEqual(keys, sorted(keys))

    def test_changed_write_or_missing_height_is_refused(self):
        lines, _ = stream(12)
        for change in ('after', 'drop_height', 'short'):
            rows = [json.loads(raw) for raw in lines]
            if change == 'after':
                next(r for r in rows if r['kind'] == 'native_write' and r['height'] == 5)['after'] = b64('changed')
            elif change == 'drop_height':
                rows = [r for r in rows if not (r['kind'] == 'height_completed' and r['height'] == 4)]
            with self.subTest(change=change):
                with self.assertRaises((ValueError, KeyError)):
                    fast_forward(iter(json.dumps(r, separators=(',', ':')).encode() for r in rows), 1, 30 if change == 'short' else 12)


RELEASE = 'cosmos-sdk v0.33.2 (gaia), tendermint v0.31.0-dev0-fix0, iavl v0.12.1'
GENESIS = '73a866b21723ecbc28b6d15951b2eb3aa2f2443650ff6df489bf55ac5edceefa'


class SourceEndCompletion(unittest.TestCase):
    """A part that opens mid-source and runs to the source end reads the source's own completion record."""
    def source(self, directory, last, first_height=1):
        def commit(height): return dict(Version=height, Hash=str(height))
        common = dict(schema='native033-lossless-history-v1', release=RELEASE, chain='cosmoshub-1')
        rows = [dict(common, kind='source_anchor', height=0, commit=commit(0), native_commit_info={}, genesis_sha256=GENESIS)]
        rows += [dict(common, kind='height', height=h, commit=commit(h), native_commit_info={}, header=dict(height=h), block_hash=str(h),
                      transactions=[], begin={}, deliver=[], end={}) for h in range(1, last + 1)]
        rows.append(dict(schema='native033-lossless-history-v1', kind='history_completed', first_height=first_height, height=last, commit=commit(last),
                         unexecuted_next_header=dict(height=last + 1, chain_id='cosmoshub-1'), unexecuted_next_block_hash=str(last + 1)))
        raw = b''.join(json.dumps(r, separators=(',', ':')).encode() + b'\n' for r in rows)
        path = directory / 'native-history.source-closed.gz'
        path.write_bytes(gzip.compress(raw, mtime=0))
        spec = dict(sha256=hashlib.sha256(path.read_bytes()).hexdigest(), compressed_bytes=path.stat().st_size, records=len(rows),
                    raw_bytes=len(raw), raw_sha256=hashlib.sha256(raw).hexdigest(), completion_kind='history_completed')
        return dict(first=1, through=last, streams={'native-history': spec})

    def walk(self, history, first, last):
        for h in range(first, last + 1):
            self.assertEqual(history[h]['height'], h)
        return history[last + 1]['header']

    def test_mid_source_part_closes_at_the_source_end(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            b = self.source(root, 12)
            for first in (1, 2, 7, 12):
                with self.subTest(first=first):
                    history = SplitHistory(root, b, first)
                    self.assertEqual(history.anchor['height'], first - 1)
                    self.assertEqual(self.walk(history, first, 12), dict(height=13, chain_id='cosmoshub-1'))
                    self.assertEqual(history.completion['first_height'], 1)

    def test_wrong_completion_first_height_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            for wrong in (0, 2, 7, 8):
                b = self.source(root, 12, first_height=wrong)
                for first in (1, 8):
                    with self.subTest(wrong=wrong, first=first):
                        history = SplitHistory(root, b, first)
                        with self.assertRaisesRegex(ValueError, 'completion identity differs'):
                            self.walk(history, first, 12)


class FiveSeams(unittest.TestCase):
    def parts(self):
        def commit(height): return dict(Version=height, Hash=str(height))
        def roots(height): return dict(Version=height, StoreInfos=[dict(Name=name, Core=dict(CommitID=commit(height))) for name in NAMES])
        result = []
        for first, last, source in ((1, 100000, (1, 200000)), (100001, 200000, (1, 200000)), (200001, 300000, (200001, 400000)), (300001, 400000, (200001, 400000))):
            n, opened = last - first + 1, first > source[0]
            result.append(dict(schema='native033-split-range-financial-v1', first=first, through=last, heights=n, transactions=0, source_range=list(source),
                               phases=dict(begin_block=n, end_block=n, block=n), source_closed_at_through=last == source[1], next_header_is_source_closure=last == source[1],
                               fast_forward=dict(heights=first - source[0], typed_phases=300000 if opened else 0, seconds=0.0), progress_phase_base=300000 if opened else 0,
                               opening_checkpoint_manifest_sha256='a' * 64 if opened else '',
                               opening_commit=commit(first - 1), ending_commit=commit(last), opening_store_roots=roots(first - 1), ending_store_roots=roots(last)))
        result.append(dict(first=400001, through=500000, heights=100000, transactions=0, phases=dict(begin_block=100000, end_block=100000, block=100000), source_whole_complete=True,
                           opening_commit=commit(400000), ending_commit=commit(500000), opening_store_roots=roots(400000), ending_store_roots=roots(500000)))
        return result

    def test_five_parts_join_at_four_saved_checkpoints(self):
        summary = check_profiles(self.parts())
        self.assertEqual(summary['distinct_prefix_heights'], 500000)
        self.assertEqual(summary['all_nine_normal_checkpoint_seams'], [100000, 200000, 300000, 400000])

    def test_gap_overlap_unsaved_seam_or_unrebuilt_opening_is_refused(self):
        p = self.parts()
        changes = [(0, 'through', 99999), (1, 'first', 100002), (1, 'opening_checkpoint_manifest_sha256', ''), (1, 'progress_phase_base', 6), (3, 'fast_forward', dict(heights=100000, typed_phases=299999, seconds=0.0)),
                   (1, 'source_closed_at_through', False), (0, 'source_closed_at_through', True), (2, 'source_range', [1, 200000]), (3, 'heights', 99999)]
        for index, field, value in changes:
            q = copy.deepcopy(p)
            q[index][field] = value
            with self.subTest(index=index, field=field):
                with self.assertRaises(ValueError): check_profiles(q)
        q = copy.deepcopy(p)
        q[1]['opening_commit']['Hash'] = 'changed'
        with self.assertRaises(ValueError): check_profiles(q)
        q = copy.deepcopy(p)
        q[0]['through'], q[0]['heights'], q[1]['first'], q[1]['heights'] = 50000, 50000, 50001, 150000
        for item in q[:2]: item['phases'] = dict(begin_block=item['heights'], end_block=item['heights'], block=item['heights'])
        with self.assertRaises(ValueError): check_profiles(q)
        with self.assertRaises(ValueError): check_profiles(p[:1] + p[2:])
        with self.assertRaises(ValueError): check_profiles(p + p[-1:])


if __name__ == '__main__':
    unittest.main()
