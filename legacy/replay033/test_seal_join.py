import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from reconcile_whole import REBUILT, STAMP_ONLY, check_stretches
from reconcile_whole import BEFORE_SAVE, BUILD_DIFFERENCES, NEWER, OLDER, PENALTY_HEIGHTS, SAVE_DIFFERENCES, WITH_SAVE, check_builds, join_facts
from seal_closed import FACTS, bound_facts, stake_facts

WHOLE, SPLIT = 'native033-complete-profile-financial-v1', 'native033-split-range-financial-v1'


def line(height, facts=None):
    facts = [dict(native_dec_before='10.5', native_dec_after='10.25', native_dec_reduction='0.25')] if facts is None else facts
    return (json.dumps(dict(schema='native033-stake-asset-reduction-v1', height=height, facts=facts, tax_classification='unclassified', native_all_store_witness=True)) + '\n').encode()


def receipts(first, through, data, blocks, rows):
    digest = hashlib.sha256(data).hexdigest() if blocks else ''
    counts = dict(native_stake_asset_blocks=blocks, native_stake_asset_fact_rows=rows, native_stake_asset_facts_sha256=digest)
    return dict(counts, first=first, through=through, stake_asset_tax_classification='unclassified'), dict(counts)


class PenaltyFacts(unittest.TestCase):
    def test_fact_file_must_be_exactly_what_the_checker_counted(self):
        with tempfile.TemporaryDirectory() as d:
            proof = Path(d)
            data = line(219358) + line(221354)
            result, native = receipts(200001, 300000, data, 2, 2)
            with self.assertRaises(ValueError):
                bound_facts(proof, result, native)
            (proof / FACTS).write_bytes(data)
            self.assertEqual([e['height'] for e in bound_facts(proof, result, native)], [219358, 221354])
            self.assertEqual(set(bound_facts(proof, result, native)[0]), {'height', 'fact_rows', 'line_sha256'})
            for wrong in (receipts(200001, 300000, data, 1, 2), receipts(200001, 300000, data, 2, 3), receipts(200001, 220000, data, 2, 2), receipts(220000, 300000, data, 2, 2)):
                with self.assertRaises(ValueError):
                    bound_facts(proof, *wrong)
            native['native_stake_asset_fact_rows'] = 1
            with self.assertRaises(ValueError):
                bound_facts(proof, result, native)

    def test_uncounted_fact_file_and_bad_records_refused(self):
        with tempfile.TemporaryDirectory() as d:
            proof = Path(d)
            result, native = receipts(1, 100000, b'', 0, 0)
            self.assertEqual(bound_facts(proof, result, native), [])
            (proof / FACTS).write_bytes(line(5))
            with self.assertRaises(ValueError):
                bound_facts(proof, result, native)
            for data in (line(5)[:-1], line(7) + line(5), line(5) + line(5), line(5, [dict(native_dec_before='3', native_dec_after='1', native_dec_reduction='1')]),
                         line(5).replace(b'unclassified', b'income'), line(5).replace(b'"native_all_store_witness": true', b'"native_all_store_witness": false')):
                (proof / FACTS).write_bytes(data)
                with self.assertRaises(ValueError):
                    stake_facts(proof / FACTS, 1, 100000)


def sealed_part(root, first, through, heights):
    directory = root / str(first)
    directory.mkdir()
    data = b''.join(line(h) for h in heights)
    entries = []
    if heights:
        (directory / FACTS).write_bytes(data)
        entries = stake_facts(directory / FACTS, first, through)
    seal = dict(stake_asset_fact_blocks=entries, stake_asset_facts_path=str(Path(str(first)) / FACTS) if heights else '')
    return dict(first=first, through=through), seal, root / ('seal-%d.json' % first)


class JoinedFacts(unittest.TestCase):
    def test_all_six_known_penalty_blocks_are_carried_or_the_join_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            parts = [sealed_part(root, 1, 100000, []), sealed_part(root, 100001, 200000, [134125]), sealed_part(root, 200001, 300000, [219358, 221354, 231614]),
                     sealed_part(root, 300001, 400000, [359194]), sealed_part(root, 400001, 500000, [486010])]
            entries, data = join_facts(parts)
            self.assertEqual(tuple(e['height'] for e in entries), PENALTY_HEIGHTS)
            self.assertEqual(data, b''.join(line(h) for h in PENALTY_HEIGHTS))
            self.assertEqual(entries[1]['part'], [200001, 300000])
            with self.assertRaises(ValueError):
                join_facts(parts[:4] + [sealed_part(root / '..' / Path(d).name, 400002, 500000, [])])
            (root / '300001' / FACTS).write_bytes(line(359194, []))
            with self.assertRaises(ValueError):
                join_facts(parts)


def build(commit, schema, first, through, pins, helper='h-new'):
    return dict(schema=schema, first=first, through=through, capture_sha256='c%d' % first, opening_commit=dict(Hash='AQ==', Version=first - 1)), dict(signed_source_commit=commit, signed_code_pins=pins, native_helper_sha256=helper), None


class TwoBuilds(unittest.TestCase):
    def setUp(self):
        same = {'legacy/replay033/consume_closed.py': 'aa', 'legacy/replay033/cmd/consume-native/slash_native.go': 'bb'}
        self.old = dict(same, **{k: v[0] for k, v in BUILD_DIFFERENCES.items() if v[0]})
        self.new = dict(same, **{k: v[1] for k, v in BUILD_DIFFERENCES.items()})

    def parts(self):
        return [build(NEWER, SPLIT, f, f + 99999, self.new) for f in (1, 100001, 200001, 300001)] + [build(OLDER, WHOLE, 400001, 500000, self.old, 'h-old')]

    def witness(self, d, **change):
        directory = Path(d)
        native = dict(first=400001, through=400100)
        (directory / 'native-proof.private.json').write_text(json.dumps(native))
        run = dict(dict(schema=SPLIT, first=400001, through=400100, fast_forward=dict(heights=0), capture_sha256='c400001', native_helper_sha256='h-new',
                        opening_commit=dict(Hash='AQ==', Version=400000), native_proof_sha256=hashlib.sha256((directory / 'native-proof.private.json').read_bytes()).hexdigest()), **change)
        (directory / 'complete-component.private.json').write_text(json.dumps(run))
        (directory / 'exit.private.json').write_text(json.dumps(dict(exit=change.get('exit', 0))))
        return dict(path=d, sha256=hashlib.sha256((directory / 'complete-component.private.json').read_bytes()).hexdigest())

    def test_one_build_needs_no_witness(self):
        parts = [build(NEWER, SPLIT, f, f + 99999, self.new) for f in (1, 100001)]
        self.assertEqual(check_builds(parts, None), [NEWER])
        with self.assertRaises(ValueError):
            check_builds(parts, dict(path='x', sha256='y'))

    def test_reviewed_pair_accepted_only_with_passing_witness_on_same_opening(self):
        with tempfile.TemporaryDirectory() as d:
            self.assertEqual(check_builds(self.parts(), self.witness(d)), sorted([OLDER, NEWER]))
            with self.assertRaises(ValueError):
                check_builds(self.parts(), None)
            for change in (dict(exit=1), dict(capture_sha256='other'), dict(native_helper_sha256='h-old'), dict(through=400050), dict(first=400002),
                           dict(opening_commit=dict(Hash='Ag==', Version=400000)), dict(fast_forward=dict(heights=5))):
                with self.assertRaises(ValueError, msg=str(change)):
                    check_builds(self.parts(), self.witness(d, **change))

    def test_any_unreviewed_difference_or_other_part_from_old_build_refused(self):
        with tempfile.TemporaryDirectory() as d:
            witness = self.witness(d)
            self.old['legacy/replay033/cmd/consume-native/slash_native.go'] = 'changed'
            with self.assertRaises(ValueError):
                check_builds(self.parts(), witness)
            self.setUp()
            self.old['legacy/replay033/cmd/consume-native/main.go'] = 'another-main'
            with self.assertRaises(ValueError):
                check_builds(self.parts(), witness)
            self.setUp()
            parts = self.parts()
            parts[2] = build(OLDER, WHOLE, 200001, 400000, self.old, 'h-old')
            with self.assertRaises(ValueError):
                check_builds(parts, witness)
            parts = self.parts()
            parts[0] = build('1' * 40, SPLIT, 1, 100000, dict(self.new, **{'legacy/replay033/consume_closed.py': 'other'}))
            with self.assertRaises(ValueError):
                check_builds(parts, witness)
            parts[0] = build('1' * 40, SPLIT, 1, 100000, self.new)
            self.assertEqual(len(check_builds(parts, witness)), 3)


class FirstHeightsRecordedAgain(unittest.TestCase):
    """A part whose helper was not rebuilt byte for byte needs its first 200 heights recorded again by both helpers, identically."""

    def run_of(self, root, name, helper, first, through, lines, **change):
        directory = root / name
        directory.mkdir(parents=True)
        (directory / 'native-proof.private.json').write_text(json.dumps(dict(first=first, through=through)))
        run = dict(dict(schema=SPLIT, first=first, through=through, transactions=3, phases=dict(a=7), fast_forward=dict(heights=4, seconds=len(name)), ending_commit='e', opening_commit='o',
                        opening_store_roots=['r'], opening_checkpoint_manifest_sha256='m', opening_kind='interrupted_run_state', progress_phase_base=9, capture_sha256='c', source_range=[1, 100000],
                        native_helper_sha256=helper, native_proof_sha256=hashlib.sha256((directory / 'native-proof.private.json').read_bytes()).hexdigest()), **change)
        (directory / 'complete-component.private.json').write_text(json.dumps(run))
        (directory / 'exit.private.json').write_text(json.dumps(dict(exit=0)))
        (directory / 'native-consumer.private.log').write_text(''.join('bounded_native_verified_height=%d typed_phases=%d\n' % x for x in lines))
        return run, dict(path=str(directory), sha256=hashlib.sha256((directory / 'complete-component.private.json').read_bytes()).hexdigest())

    def joined(self, root, lines=((600, 20), (700, 31)), **change):
        live = ((600, 20), (700, 31), (800, 40))
        weak, _ = self.run_of(root, 'live/proof', 'h-plain', 501, 5000, live, ending_commit='later')
        strong, _ = self.run_of(root, 'other/proof', 'h-save', 5001, 9000, ())
        seal = lambda helper, proof, name: dict(native_helper_sha256=helper, helper_proof=proof, result_path=name + '/proof/complete-component.private.json', savepoint_of_killed_run=False)
        sealed = [(weak, seal('h-plain', STAMP_ONLY, 'live'), root / 'seal-501.json'), (strong, seal('h-save', REBUILT, 'other'), root / 'seal-5001.json')]
        pair = [self.run_of(root, 'again-a', 'h-plain', 501, 700, ((600, 20), (700, 31)))[1], self.run_of(root, 'again-b', 'h-save', 501, 700, lines, **change)[1]]
        return sealed, pair

    def test_equal_second_recordings_are_bound_and_any_difference_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            sealed, pair = self.joined(Path(d))
            checked = check_stretches(sealed, {'501': pair})
            self.assertEqual([(c['first'], c['through'], c['progress_lines_equal']) for c in checked], [(501, 700, 2)])
            for wrong in (None, {}, {'501': pair[:1] * 2}, {'501': pair, '5001': pair}):
                with self.assertRaises(ValueError):
                    check_stretches(sealed, wrong)
            self.assertEqual(check_stretches(sealed[1:], None), [])
            with self.assertRaises(ValueError):
                check_stretches(sealed[:1], {'501': pair})
        for change in (dict(ending_commit='f'), dict(transactions=4), dict(phases=dict(a=8)), dict(opening_commit='p'), dict(opening_store_roots=['s']), dict(opening_checkpoint_manifest_sha256='n'),
                       dict(progress_phase_base=8), dict(fast_forward=dict(heights=5, seconds=1)), dict(native_helper_sha256='h-plain'), dict(native_helper_sha256='h-third'), dict(savepoint=True), dict(schema='other')):
            with tempfile.TemporaryDirectory() as d:
                sealed, pair = self.joined(Path(d), **change)
                with self.assertRaises(ValueError):
                    check_stretches(sealed, {'501': pair})
        with tempfile.TemporaryDirectory() as d:
            sealed, pair = self.joined(Path(d), lines=((600, 20), (700, 32)))
            with self.assertRaises(ValueError):
                check_stretches(sealed, {'501': pair})


class SavingBuild(unittest.TestCase):
    def setUp(self):
        same = {'legacy/replay033/consume_closed.py': 'aa', 'legacy/replay033/cmd/consume-native/slash_native.go': 'bb'}
        self.old = dict(same, **{k: v[0] for k, v in SAVE_DIFFERENCES.items()})
        self.new = dict(same, **{k: v[1] for k, v in SAVE_DIFFERENCES.items()})

    def parts(self):
        return [build(BEFORE_SAVE, SPLIT, 1, 19792, self.old, 'h-plain'), build(WITH_SAVE, SPLIT, 19793, 100000, self.new, 'h-save')]

    def stretch(self, d, helper, saved, **change):
        directory = Path(d) / helper
        directory.mkdir()
        (directory / 'native-proof.private.json').write_text(json.dumps(dict(first=1, through=100)))
        run = dict(dict(schema=SPLIT, first=1, through=100, heights=100, transactions=3, fast_forward=dict(heights=0, seconds=len(helper)), ending_commit='e', native_helper_sha256=helper,
                        native_proof_sha256=hashlib.sha256((directory / 'native-proof.private.json').read_bytes()).hexdigest()), **change)
        (directory / 'complete-component.private.json').write_text(json.dumps(run))
        (directory / 'exit.private.json').write_text(json.dumps(dict(exit=0)))
        if saved:
            (directory / 'savepoints' / '50').mkdir(parents=True)
            (directory / 'savepoints' / '50' / 'part.private.json').write_text('{}')
        return dict(path=str(directory), sha256=hashlib.sha256((directory / 'complete-component.private.json').read_bytes()).hexdigest())

    def test_pair_joins_only_with_one_stretch_recorded_identically_by_both(self):
        with tempfile.TemporaryDirectory() as d:
            pair = [self.stretch(d, 'h-plain', False), self.stretch(d, 'h-save', True)]
            self.assertEqual(check_builds(self.parts(), None, pair), sorted([BEFORE_SAVE, WITH_SAVE]))
            with self.assertRaises(ValueError):
                check_builds(self.parts(), None, None)
            with self.assertRaises(ValueError):
                check_builds(self.parts(), None, pair[:1] * 2)
            with self.assertRaises(ValueError):
                check_builds(self.parts()[:1], None, pair)
        for change in (dict(ending_commit='other'), dict(transactions=4), dict(heights=50)):
            with tempfile.TemporaryDirectory() as d:
                with self.assertRaises(ValueError, msg=str(change)):
                    check_builds(self.parts(), None, [self.stretch(d, 'h-plain', False), self.stretch(d, 'h-save', True, **change)])
        with tempfile.TemporaryDirectory() as d:
            with self.assertRaises(ValueError):
                check_builds(self.parts(), None, [self.stretch(d, 'h-plain', False), self.stretch(d, 'h-save', False)])

    def test_any_other_difference_between_the_two_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            pair = [self.stretch(d, 'h-plain', False), self.stretch(d, 'h-save', True)]
            self.new['legacy/replay033/cmd/consume-native/slash_native.go'] = 'changed'
            with self.assertRaises(ValueError):
                check_builds(self.parts(), None, pair)
            self.setUp()
            self.new['legacy/replay033/cmd/consume-native/main.go'] = 'another-main'
            with self.assertRaises(ValueError):
                check_builds(self.parts(), None, pair)


class ManyParts(unittest.TestCase):
    def test_later_commit_that_changes_only_sealing_tools_joins_and_a_recorder_change_is_named(self):
        new = {'legacy/replay033/consume_split.py': 'aa', 'legacy/replay033/cmd/consume-native/main.go': 'bb', 'legacy/replay033/seal_closed.py': 'cc'}
        tools = dict(new, **{'legacy/replay033/seal_closed.py': 'dd', 'legacy/replay033/hub1_finish.py': 'ee', 'legacy/replay033/test_seal_join.py': 'ff', 'legacy/replay033/cmd/consume-native/x_test.go': 'gg'})
        parts = [build(NEWER, SPLIT, 1, 50000, new), build('2' * 40, SPLIT, 50001, 100000, tools)]
        self.assertEqual(check_builds(parts, None), sorted([NEWER, '2' * 40]))
        with self.assertRaises(ValueError):
            check_builds(parts[:1] + [build('2' * 40, SPLIT, 50001, 100000, tools, 'another-helper')], None)
        with self.assertRaisesRegex(ValueError, 'consume_split.py'):
            check_builds(parts[:1] + [build('2' * 40, SPLIT, 50001, 100000, dict(tools, **{'legacy/replay033/consume_split.py': 'zz'}))], None)
        with self.assertRaisesRegex(ValueError, 'main.go'):
            check_builds(parts[:1] + [build('2' * 40, SPLIT, 50001, 100000, dict(tools, **{'legacy/replay033/cmd/consume-native/main.go': 'zz'}))], None)


if __name__ == '__main__':
    unittest.main()
