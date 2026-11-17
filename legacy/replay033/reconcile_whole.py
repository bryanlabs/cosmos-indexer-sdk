#!/usr/bin/env python3
"""Assemble ONLY fully sealed 1..500000 profiles plus the accepted separate tail."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import subprocess
from consume_bounded import exact, sha
from capture_bounded import PROFILES
from verify_movement_rehearsal import require
from verify_terminal_fifteen import canonical_boundary

TERMINAL_SHA = 'c32582a7eb3f42544b60501541cba915c6f4cd26acda5f15b99bb438bc9f5e9e'
# Six Hub 1 blocks were found where a validator stopped signing and left the
# set. Where stake was taken, the recorder writes a penalty fact, and the join
# refuses unless the parts carry one for exactly these heights.
PENALTY_HEIGHTS = (134125, 219358, 221354, 359194, 486010)
# A validator left the set at 231614 with no stake taken: its operator
# undelegated its own stake below the minimum self-delegation in a message and
# was jailed for it. No penalty fact exists there, so the join instead needs a
# pinned proof of that, made from recorded native state with two real penalty
# blocks as controls (hub1_jailing_proof.py).
JAILED_WITHOUT_STAKE_MOVED = (231614,)
OLDER, NEWER = '199da294f4be111d89fc17501032dc27c42e2b28', '0f35e0d7d1ac6a39ed414c7a5374711b1c1e0e78'
# Every file that differs between the two launch commits, as (older, newer)
# SHA-256, None where the older commit has no such file. Nothing else may differ.
#
# Only the 400001..500000 whole range ran the older build. Why that is the same
# recording the newer build would have made:
# - consume_split.py and its test are new and were not run by that range, which
#   ran consume_closed.py, identical in both commits.
# - reconcile_whole.py and seal_closed.py are not imported by either recorder.
# - main.go differs in three places. The opening height allow-list moved into
#   anchor_allow.go and gained 100000 and 300000; 400000 is allowed by both.
#   --progress-phase-base is added to progress lines and is 0 without the flag.
#   The newer build also refuses when the opening block hash differs from the
#   raw block; a refusal check cannot change what a passing run records, and the
#   join requires a run of the newer build on the same 400001 opening that
#   passed it (cross_build_witness).
BUILD_DIFFERENCES = {
    'legacy/replay033/cmd/consume-native/main.go': ('4a6dada6d77df218cc0de9a38ec6a8d0d59ae7cf74b4d2ec9c9b1d4c2cc89627', '69c6ec724b032edfb14fddd9a2c8209b919cc14bac494fa2073aa7b140b9668b'),
    'legacy/replay033/cmd/consume-native/anchor_allow.go': (None, '431717214dd7dda57a3f7abd9c22914416907ddba82a81baa531d334ac1827bc'),
    'legacy/replay033/cmd/consume-native/anchor_allow_splitproof.go': (None, '103c1f1194610e68bb3cc99a5cb49d9dd1e13056653e6b4022dbdd3aaf55591f'),
    'legacy/replay033/cmd/consume-native/anchor_allow_test.go': (None, '34d4097dbf9fc2373a3ab37dcd673a59962e4708afb6bfe2b79d03df96a62563'),
    'legacy/replay033/consume_split.py': (None, 'd8eac9f928079c153d536bdc366886c185b993792832de78fa6f6b4c03478a03'),
    'legacy/replay033/test_consume_split.py': (None, '7109cf07deb4c065fba8fb395b9e069210899de5f651df0db1de8e2dc09ec8e1'),
    'legacy/replay033/reconcile_whole.py': ('11a942fc4b04cf59d8793f6b471730f6f7f224e8985500d98db1f190bfdfd034', '138274a6b9d7922775ce0cd5c6a3f49cccd74d3700f9a004b764b087d5e76300'),
    'legacy/replay033/seal_closed.py': ('bdd5a92ff5894047690691f69096657cb83dcca8e7873d07d0ea7ace6c316d84', 'dc13c7d7fe7e1cebe5b84c40336d9fd90eed1ad775119aed5215721b472ad647'),
}


# The second reviewed pair. Some relaunched parts run a build that can also save
# its state every so many heights; the others run the build just before it.
# The two commits differ in exactly these two files, and only by this:
# - main.go gains a "savepoint" message. On it the helper copies its committed
#   state to a new folder and writes a side receipt there. The message handler
#   reads the working state and writes nothing to it. The final receipt is
#   built by the same expression as before, moved into a function.
# - consume_split.py gains --savepoint-every, which sends that message, and
#   builds its receipt with the same fields, moved into a function.
# Reading the change is not treated as enough. The join also requires one
# stretch of heights recorded by both builds from the same opening, the newer
# one taking at least one savepoint on the way, with identical receipts.
BEFORE_SAVE, WITH_SAVE = '2630a0d16aeaa02124bf1e70274f1d34d59a6136', '26a4e0c3f6e15eaabb8e7dcbd9514ea0e3724ccd'
SAVE_DIFFERENCES = {
    'legacy/replay033/cmd/consume-native/main.go': ('32dba21cad4c6bba5e918d2ff0b0910a96d4125db0b20ece48cc9259783c7511', 'dfb6dc33ea42423253d1168ffe8d62e3495e470d70c351d088da1cd22444c5d0'),
    'legacy/replay033/consume_split.py': ('d8915f29c0085b28bb7b7a4e81e634998a37ba10c103ce3b6997b9a1f3299d33', 'a50d443dce28772a0719b5ba9faee79b1af79f0bba2ce850a07f1c39d7c2a92c'),
}


# The saving build plus one fix to the check at the end of a part. A part that
# opens inside a source range recorded every height, then refused at the
# source's closing record, which names the first height of the whole source:
# the check compared it with the part's own opening instead. The fix keeps the
# source's opening for that one comparison. Nothing that records a height
# changed, and a part that opens at its source start compares the same height
# as before. Only the last heights of the three source ranges that a
# saving-build run had recorded to the end, then refused on exactly that
# check, may come from this build. Each must open from that run's sealed
# savepoint, and the two runs' progress lines over every height both verified
# must be the same.
COMPLETION_FIX = '6267bb77ceae4e694ec9f7158141d7e3c543f5b4'
COMPLETION_FIX_DIFFERENCES = {
    'legacy/replay033/consume_closed.py': ('2de9b06d58c41a1fe9d6078616a4555816c2db135e44781c1710756d5057e2ae', 'e8145cbb43e9457169f6554d6c48ec4f6bc962090c14cd5b3fa02ee8c7c077ef'),
    'legacy/replay033/consume_split.py': ('a50d443dce28772a0719b5ba9faee79b1af79f0bba2ce850a07f1c39d7c2a92c', 'ba41026a3e401e1d5809a468a17d1de355fc0a7e11b537cac9595ae8c4e74ff0'),
}
COMPLETION_FIX_PARTS = ((197501, 200000), (397501, 400000), (497501, 500000))
COMPLETION_REFUSAL = b'ValueError: closed native history completion identity differs'


def pinned(path, digest):
    require(path.is_file() and not path.is_symlink() and sha(path) == digest, 'changed, missing or nonregular sealed artifact')
    return json.loads(path.read_bytes())


def signed_profile(path, expected_sha, repo, progress=None, with_seal=False):
    seal = pinned(path, expected_sha)
    require(seal['schema'] == 'native033-complete-profile-seal-v1' and seal['financial_admission'] is False, 'complete independently sealed profile required')
    commit = seal['signed_source_commit']
    require(len(commit) == 40 and all(c in '0123456789abcdef' for c in commit), 'literal signed source commit required')
    subprocess.run(['git', '-C', str(repo), 'verify-commit', commit], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
    from seal_closed import bound_facts, code_list
    require(set(seal['signed_code_pins']) == set(code_list(repo, commit)), 'complete signed consumer code coverage missing')
    for filename, digest in seal['signed_code_pins'].items():
        rel = Path(filename)
        require(not rel.is_absolute() and '..' not in rel.parts, 'unsafe signed code path')
        original = subprocess.check_output(['git', '-C', str(repo), 'show', commit + ':' + filename])
        require(hashlib.sha256(original).hexdigest() == digest, 'complete component code not bound to signed source')
    for index,(filename, digest) in enumerate(seal['artifact_pins'].items(),1):
        rel = Path(filename)
        require(not rel.is_absolute() and '..' not in rel.parts, 'unsafe complete component artifact path')
        artifact = path.parent / rel
        require(artifact.is_file() and not artifact.is_symlink() and sha(artifact) == digest, 'sealed component bytes changed')
        if progress is not None and (index % 1000 == 0 or index == len(seal['artifact_pins'])):
            progress(str(path),index,len(seal['artifact_pins']))
    saved = seal['savepoint_of_killed_run']
    for field, digest_field in (('result_path','result_sha256'),('native_proof_path','native_proof_sha256')) + ((('exit_path','exit_sha256'),) if seal['exit_path'] else ()):
        require(seal['artifact_pins'].get(seal[field]) == seal[digest_field], 'complete result not in sealed artifact inventory')
    require(seal['native_helper_sha256'] in seal['artifact_pins'].values(), 'complete native helper binary pin missing')
    result = pinned(path.parent / seal['result_path'], seal['result_sha256'])
    native = pinned(path.parent / seal['native_proof_path'], seal['native_proof_sha256'])
    exit_state = pinned(path.parent / seal['exit_path'], seal['exit_sha256']) if seal['exit_path'] else None
    if saved:
        # Heights a killed run had proved and saved before it died. Its exit, if it left one, must not be a success, and nothing past the savepoint is taken from it.
        require(result.get('savepoint') is True and Path(seal['result_path']).name == 'part.private.json' and Path(seal['result_path']).parent.name == str(result['through']) and seal['killed_run_gave_heights'] == [result['first'], result['through']] and (exit_state is None or exit_state['exit'] != 0), 'savepoint seal misstates the killed run')
    else:
        require('savepoint' not in result and exit_state is not None and exit_state['exit'] == 0, 'failed or bounded source cannot be assembled as complete')
    require(result['schema'] in ('native033-complete-profile-financial-v1', 'native033-split-range-financial-v1') and (result['schema'] != 'native033-complete-profile-financial-v1' or result['source_whole_complete'] is True) and result['financial_admission'] is False and result['global_books_tax_acceptance'] is False, 'failed or bounded source cannot become complete profile')
    require(result['native_proof_sha256'] == seal['native_proof_sha256'] and result['native_helper_sha256'] == seal['native_helper_sha256'], 'complete component native proof/helper binding differs')
    require(native['schema'] == 'native033-independent-bounded-financial-v1' and native['normal_opening_height'] == result['first'] - 1, 'independent native opening schema differs')
    require(result['first'] == native['first'] and result['through'] == native['through'] and result['transactions'] == native['transactions'] and result['heights'] == native['native_iavl_heights'] and sum(result['phases'].values()) == native['typed_financial_phases'] and native['financial_admission'] is False and native['global_books_tax_acceptance'] is False, 'independent native component coverage differs')
    require(seal['source_preserved_after_proof'] is True and seal['actual_negative_controls_passed'] is True and seal['historical_blocks_executed'] == 0, 'complete source preservation or independent controls absent')
    kind = seal['opening_kind']
    require(kind == result.get('opening_kind', kind) and seal['resumed_from_interrupted_run'] is (kind == 'interrupted_run_state') and (kind == 'source_range_start') is (not result.get('opening_checkpoint_manifest_sha256')), 'seal misstates where the opening state of this part came from')
    # Penalty facts are re-read from the sealed bytes, not trusted from the seal.
    facts = bound_facts((path.parent / seal['result_path']).parent, result, native)
    require(facts == seal['stake_asset_fact_blocks'] and seal['stake_asset_tax_classification'] == 'unclassified', 'sealed penalty facts differ from the retained fact file')
    if facts:
        require(seal['artifact_pins'].get(seal['stake_asset_facts_path']) == seal['stake_asset_facts_sha256'] == result['native_stake_asset_facts_sha256'] and (path.parent / seal['stake_asset_facts_path']).resolve() == ((path.parent / seal['result_path']).parent / 'native-stake-reductions.private.jsonl').resolve(), 'penalty fact file is not in the sealed artifact inventory')
    else:
        require(not seal['stake_asset_facts_path'] and not seal['stake_asset_facts_sha256'], 'seal names penalty facts the checker did not count')
    if with_seal:
        return result, seal, path
    return result


# Files that never run while a part records. Two launch commits may differ in
# these freely; a difference anywhere else must be a reviewed one.
def records_nothing(name):
    base = name.rsplit('/', 1)[-1]
    return base.startswith('test_') or base.endswith('_test.go') or base in ('reconcile_whole.py', 'seal_closed.py', 'hub1_finish.py', 'record_after_launch.py',
        'run_closed_profile.py', 'run_whole.py', 'actual_full_controls.py', 'actual_bounded_controls.py', 'actual_checkpoint_controls.py', 'verify_source_preservation.py')


def same_recording(pair, helpers):
    """One stretch of heights recorded by both builds, with nothing different but the helper that made it."""
    require(pair is not None and len(pair) == 2, 'a stretch recorded by both builds is required')
    runs = []
    for witness in pair:
        directory = Path(witness['path'])
        run = pinned(directory / 'complete-component.private.json', witness['sha256'])
        native = pinned(directory / 'native-proof.private.json', run['native_proof_sha256'])
        require(json.loads((directory / 'exit.private.json').read_bytes())['exit'] == 0 and run['schema'] == 'native033-split-range-financial-v1' and run['heights'] >= 100, 'cross-build stretch did not finish or is too short')
        saved = sorted(d.name for d in (directory / 'savepoints').iterdir() if (d / 'part.private.json').is_file()) if (directory / 'savepoints').is_dir() else []
        comparable = dict(run, native_helper_sha256='', fast_forward=dict(run['fast_forward'], seconds=0))
        runs.append((run['native_helper_sha256'], comparable, native, saved))
    require({r[0] for r in runs} == set(helpers.values()) and len(set(helpers.values())) == 2, 'cross-build stretch was not recorded by the two helpers being joined')
    require(runs[0][1] == runs[1][1] and runs[0][2] == runs[1][2], 'the two builds recorded the same heights differently')
    require(all(bool(r[3]) is (r[0] == helpers[WITH_SAVE]) for r in runs), 'the saving build took no savepoint on the cross-build stretch')
    return dict(first=runs[0][1]['first'], through=runs[0][1]['through'], savepoints=sorted(set(runs[0][3] + runs[1][3]), key=int))


STAMP_ONLY, REBUILT = 'toolchain_stamp_of_unmodified_commit_and_launcher_pinned_hash', 'rebuilt_byte_identical_from_commit'
STRETCH = 200
OPENING_FIELDS = ('opening_commit', 'opening_store_roots', 'opening_checkpoint_manifest_sha256', 'opening_kind', 'progress_phase_base', 'capture_sha256', 'source_range', 'first')


def progress_lines(path, first, through):
    """The helper's own per-hundred-heights lines for a stretch: verified height and running count of financial phases."""
    lines = []
    for line in path.read_bytes().split(b'\n'):
        if line.startswith(b'bounded_native_verified_height='):
            if first <= int(line.split()[0].split(b'=')[1]) <= through:
                lines.append(line)
    return lines


def check_stretches(sealed, stretches):
    """A part whose helper could not be rebuilt byte for byte is backed by a second recording of its first heights.

    For each such part its first 200 heights were recorded again from the
    part's own opening, once by the same helper and once by the helper that
    does rebuild byte for byte. The two receipts must be equal in everything
    but the helper's name, must open exactly where the sealed part opened, and
    the progress lines of all three runs over the stretch must be the same."""
    stretches = stretches or {}
    needed = {str(r['first']) for r, s, _ in sealed if s['helper_proof'] == STAMP_ONLY}
    require(set(stretches) == needed, 'second recordings are required for exactly the parts whose helper was not rebuilt byte for byte: ' + ', '.join(sorted(needed ^ set(stretches), key=int)))
    rebuilt = {s['native_helper_sha256'] for _, s, _ in sealed if s['helper_proof'] == REBUILT}
    require(not needed or rebuilt, 'no part was recorded by a helper rebuilt byte for byte, so there is nothing to compare with')
    checked = []
    for result, seal, path in sealed:
        if str(result['first']) not in needed:
            continue
        pair = stretches[str(result['first'])]
        require(len(pair) == 2, 'two second recordings per part are required')
        through = min(result['through'], result['first'] + STRETCH - 1)
        runs = []
        for witness in pair:
            directory = Path(witness['path'])
            run = pinned(directory / 'complete-component.private.json', witness['sha256'])
            native = pinned(directory / 'native-proof.private.json', run['native_proof_sha256'])
            require(json.loads((directory / 'exit.private.json').read_bytes())['exit'] == 0 and run['schema'] == 'native033-split-range-financial-v1' and 'savepoint' not in run and run['through'] == through == native['through'], 'second recording did not finish or covers other heights: ' + str(directory))
            require(all(run.get(k) == result.get(k) for k in OPENING_FIELDS) and dict(run['fast_forward'], seconds=0) == dict(result['fast_forward'], seconds=0), 'second recording did not open where the sealed part opened: ' + str(directory))
            lines = progress_lines(directory / 'native-consumer.private.log', result['first'], through)
            runs.append((run['native_helper_sha256'], dict(run, native_helper_sha256='', fast_forward=dict(run['fast_forward'], seconds=0)), native, lines))
        helpers = {r[0] for r in runs}
        require(len(helpers) == 2 and seal['native_helper_sha256'] in helpers and helpers & rebuilt, 'second recordings must come from the part\'s own helper and from the helper rebuilt byte for byte')
        require(runs[0][1] == runs[1][1] and runs[0][2] == runs[1][2], 'the two helpers recorded the first heights of part %d differently' % result['first'])
        folder = (path.parent / seal['result_path']).parent
        live = progress_lines((folder.parent.parent if seal['savepoint_of_killed_run'] else folder) / 'native-consumer.private.log', result['first'], through)
        require(live and live == runs[0][3] == runs[1][3], 'progress lines of part %d over its first heights differ from the second recordings' % result['first'])
        checked.append(dict(first=result['first'], through=through, helpers=sorted(helpers), receipts_equal_except_helper_name=True, opening_equal_to_sealed_part=True,
                            second_recordings=pair, progress_lines_equal=len(live), progress_lines_sha256=hashlib.sha256(b'\n'.join(live)).hexdigest(),
                            ending_commit=runs[0][1]['ending_commit'], phases=sum(runs[0][1]['phases'].values()), transactions=runs[0][1]['transactions']))
    return checked


def pinned_log(path, seal, name):
    """A log file the seal pinned, checked against its pin."""
    log = path.parent / name
    require(log.is_file() and not log.is_symlink() and seal['artifact_pins'].get(name) == sha(log), 'log changed or missing from the sealed artifact inventory: ' + name)
    return log


def check_completion_fix(sealed):
    """Each part of the completion-fix build, against the saving-build run that recorded the same heights and refused only at the closing check."""
    saved = {(path.parent / s['result_path']).parent.resolve(): (s, path) for _, s, path in sealed if s['savepoint_of_killed_run'] and s['signed_source_commit'] == WITH_SAVE}
    checked = []
    for result, seal, path in sealed:
        if seal['signed_source_commit'] != COMPLETION_FIX:
            continue
        first, through = result['first'], result['through']
        require((first, through) in COMPLETION_FIX_PARTS and seal['helper_proof'] == REBUILT and seal['opening_kind'] == 'interrupted_run_state' and not seal['savepoint_of_killed_run'],
                'the completion fix build may record only the reviewed closing parts: %d..%d' % (first, through))
        killed, killed_path = saved.get(Path(seal['interrupted_run']).resolve(), (None, None))
        require(killed is not None and killed['killed_run_gave_heights'][1] == first - 1 and Path(killed['result_path']).parent.name == str(first - 1), 'completion fix part %d does not open from a sealed savepoint of the saving build' % first)
        run = Path(killed['killed_run_proof'])
        require((killed_path.parent / run).resolve() == (killed_path.parent / killed['result_path']).parent.parent.parent.resolve(), 'savepoint seal names another killed run')
        refusal = pinned_log(killed_path, killed, str(run / 'consumer.private.log')).read_bytes().strip().split(b'\n')[-1]
        require(refusal == COMPLETION_REFUSAL, 'the saving-build run of part %d did not stop at the closing check' % first)
        before = progress_lines(pinned_log(killed_path, killed, str(run / 'native-consumer.private.log')), first, through)
        after = progress_lines(pinned_log(path, seal, str(Path(seal['result_path']).parent / 'native-consumer.private.log')), first, through)
        hundreds = [b'bounded_native_verified_height=%d' % h for h in range((first + 99) // 100 * 100, through + 1, 100)]
        require([line.split()[0] for line in after] == hundreds and before and before == after[:-1], 'the two builds verified the heights of part %d differently' % first)
        checked.append(dict(first=first, through=through, saving_build_run=str(run), saving_build_refusal=COMPLETION_REFUSAL.decode(), progress_lines_equal=len(before),
                            equal_through=int(before[-1].split()[0].split(b'=')[1]), progress_lines_sha256=hashlib.sha256(b'\n'.join(before)).hexdigest()))
    return checked


def check_builds(sealed, witness, pair=None):
    """All parts from one recorder, or from recorders whose every difference is accounted for."""
    by_commit = {}
    for result, seal, _ in sealed:
        by_commit.setdefault(seal['signed_source_commit'], []).append((result, seal))
    for parts in by_commit.values():
        require(all(s['signed_code_pins'] == parts[0][1]['signed_code_pins'] and s['native_helper_sha256'] == parts[0][1]['native_helper_sha256'] for _, s in parts), 'parts of one commit differ in code or helper')
    reference = NEWER if NEWER in by_commit else sorted(by_commit)[0]
    b = by_commit[reference][0][1]['signed_code_pins']
    reviewed = saving = False
    for commit, parts in sorted(by_commit.items()):
        a = parts[0][1]['signed_code_pins']
        differing = sorted(name for name in set(a) | set(b) if a.get(name) != b.get(name) and not records_nothing(name))
        if not differing:
            # Same recorder and same helper sources. Only a build setting may differ, and a different binary of identical source is refused.
            require(parts[0][1]['native_helper_sha256'] == by_commit[reference][0][1]['native_helper_sha256'], 'same recording source built into different helpers: ' + commit)
            continue
        if {commit, reference} == {BEFORE_SAVE, WITH_SAVE}:
            old, new = (a, b) if commit == BEFORE_SAVE else (b, a)
            for name in set(a) | set(b):
                if a.get(name) != b.get(name):
                    require(SAVE_DIFFERENCES.get(name) == (old.get(name), new.get(name)), 'unreviewed source difference between the two builds: ' + name)
            same_recording(pair, {c: by_commit[c][0][1]['native_helper_sha256'] for c in (BEFORE_SAVE, WITH_SAVE)})
            saving = True
            continue
        if commit == COMPLETION_FIX:
            require(WITH_SAVE in by_commit, 'the completion fix build joins only beside the saving build it fixes')
            w = by_commit[WITH_SAVE][0][1]['signed_code_pins']
            changed = sorted(name for name in set(a) | set(w) if a.get(name) != w.get(name) and not records_nothing(name))
            require(changed == sorted(COMPLETION_FIX_DIFFERENCES) and all(COMPLETION_FIX_DIFFERENCES[name] == (w.get(name), a.get(name)) for name in changed), 'unreviewed source difference from the saving build: ' + ', '.join(changed))
            require(len(check_completion_fix(sealed)) == len(parts), 'completion fix parts not all checked')
            continue
        require(commit == OLDER and reference == NEWER, 'unreviewed difference in recording code between parts, commit %s: %s' % (commit, ', '.join(differing)))
        for name in set(a) | set(b):
            if a.get(name) != b.get(name):
                require(BUILD_DIFFERENCES.get(name) == (a.get(name), b.get(name)), 'unreviewed source difference between the two builds: ' + name)
        require(len(parts) == 1 and parts[0][0]['schema'] == 'native033-complete-profile-financial-v1' and parts[0][0]['first'] == 400001 and parts[0][0]['through'] == 500000, 'only the whole 400001 range may come from the older build')
        reviewed = True
    require(saving or pair is None, 'stretch recorded by both builds given where no such pair is joined')
    if not reviewed:
        require(witness is None, 'cross-build witness given where no reviewed build pair is joined')
        return sorted(by_commit)
    older, newer = by_commit[OLDER], by_commit[NEWER]
    require(witness is not None, 'cross-build witness required')
    directory = Path(witness['path'])
    run = pinned(directory / 'complete-component.private.json', witness['sha256'])
    native = pinned(directory / 'native-proof.private.json', run['native_proof_sha256'])
    exited = json.loads((directory / 'exit.private.json').read_bytes())
    require(exited['exit'] == 0 and run['schema'] == 'native033-split-range-financial-v1' and run['first'] == 400001 == native['first'] and run['through'] == native['through'] >= 400100 and run['fast_forward']['heights'] == 0 and run['capture_sha256'] == older[0][0]['capture_sha256'] and run['native_helper_sha256'] == newer[0][1]['native_helper_sha256'] and exact(run['opening_commit']) == exact(older[0][0]['opening_commit']), 'newer build did not pass the same 400001 opening the older build recorded from')
    return sorted(by_commit)


def join_facts(sealed):
    """Every penalty fact line of every part, in height order, as exact original bytes."""
    entries, lines = [], []
    for result, seal, path in sealed:
        if not seal['stake_asset_fact_blocks']:
            continue
        raw = (path.parent / seal['stake_asset_facts_path']).read_bytes().split(b'\n')[:-1]
        for entry, line in zip(seal['stake_asset_fact_blocks'], raw):
            require(hashlib.sha256(line + b'\n').hexdigest() == entry['line_sha256'] and result['first'] <= entry['height'] <= result['through'], 'penalty fact line differs from its seal')
            entries.append(dict(entry, part=[result['first'], result['through']]))
            lines.append(line + b'\n')
    require(tuple(e['height'] for e in entries) == PENALTY_HEIGHTS, 'recorded penalty blocks differ from the known Hub 1 blocks where stake was taken')
    return entries, b''.join(lines)


def check_jailings(entries):
    """Each block that jailed a validator without taking stake, with its pinned proof."""
    from hub1_jailing_proof import CHECKS, FACT_SCHEMA, SCHEMA, VERDICT
    require(isinstance(entries, list) and tuple(e.get('height') for e in entries) == JAILED_WITHOUT_STAKE_MOVED, 'jailings without stake moved differ from the known Hub 1 list')
    out = []
    for e in entries:
        require(set(e) == {'height', 'path', 'sha256'}, 'unknown jailing proof input')
        proof = pinned(Path(e['path']), e['sha256'])
        require(proof['schema'] == SCHEMA and proof['height'] == e['height'] and proof['verdict'] == VERDICT and proof['stake_moved'] is False, 'jailing proof differs')
        require(set(proof['checks']) == set(CHECKS) and all(v is True for v in proof['checks'].values()), 'jailing proof has a failed or missing check')
        compared = proof['comparisons']
        for c in compared.values():
            require(c['public']['schema'] == FACT_SCHEMA and sha(Path(c['public_path'])) == c['public_sha256'] and sha(Path(c['private_path'])) == c['private_sha256'] == c['public']['private_sha256'], 'jailing proof comparison changed')
        block = compared['block']['public']
        require(block['after_height'] == e['height'] and block['validators']['newly_jailed'] >= 1 and block['validator_tokens_plus_unbonding_balances_unchanged'] is True
                and block['distribution_slash_events']['added'] == 0 and block['pool_total_went_down'] is False, 'jailing proof block shows stake moved')
        controls = [c['public'] for n, c in compared.items() if n.startswith('control')]
        require(len(controls) >= 2 and all(c['validator_tokens_plus_unbonding_balances_went_down'] is True and c['distribution_slash_events']['added'] >= 1 and c['after_height'] in PENALTY_HEIGHTS for c in controls),
                'jailing proof controls do not show a real penalty')
        require(not proof['block_run']['penalty_fact_heights'] and not proof['next_block_run']['penalty_fact_heights'], 'jailing proof recordings carry a penalty fact')
        out.append(dict(e, verdict=proof['verdict'], cause=proof['cause'], controls=sorted(c['after_height'] for c in controls)))
    return out


def named_roots(roots, height):
    names={'main','acc','staking','slashing','mint','distr','gov','fee','params'}
    require(roots['Version']==height and len(roots['StoreInfos'])==9 and {s['Name'] for s in roots['StoreInfos']}==names,'complete all-nine normal store roots required')
    result={}
    for s in roots['StoreInfos']:
        require(s['Core']['CommitID']['Version']==height,'normal native store root version differs')
        result[s['Name']]=exact(s['Core']['CommitID'])
    return result




def check_profiles(profiles):
    ends = {PROFILES[first][0] for first in PROFILES}
    # Any number of parts is accepted. What makes a seam safe is not a list of
    # heights kept here: a part that opens inside a source range must have
    # rebuilt its opening from the range start and had the helper compare it,
    # key for key, with a saved native checkpoint, and its opening root and all
    # nine store roots must equal the ending of the part before it.
    require(len(profiles) >= 3, 'three whole source profiles, or contiguous parts of them, required')
    require(profiles[0]['first'] == 1 and profiles[-1]['through'] == 500000, 'gap, duplicate, bounded component or missing complete profile')
    seams = []
    for p in profiles:
        first, through = p['first'], p['through']
        split = p.get('schema') == 'native033-split-range-financial-v1'
        source_first = max(f for f in PROFILES if f <= first) if first >= 1 else 0
        require(source_first in PROFILES and through <= PROFILES[source_first][0], 'gap, duplicate, bounded component or missing complete profile')
        target = PROFILES[source_first][0]
        if split:
            require(p['source_range'] == [source_first, target] and first <= through, 'part lies outside its own source range')
            require(p['source_closed_at_through'] is (through == target) and p['next_header_is_source_closure'] is (through == target) and p['fast_forward']['heights'] == first - source_first and p['progress_phase_base'] == p['fast_forward']['typed_phases'], 'part source closure or rebuilt opening coverage differs')
            require(bool(p['opening_checkpoint_manifest_sha256']) == (first > source_first), 'part opening checkpoint binding differs')
        else:
            require(first == source_first and through == target and p['source_whole_complete'] is True, 'gap, duplicate, bounded component or missing complete profile')
        require(p['heights'] == through - first + 1, 'gap, duplicate, bounded component or missing complete profile')
        require(set(p['phases']) <= {'begin_block','ante','message','transaction','end_block','block'} and all(p['phases'].get(k) == p['heights'] for k in ('begin_block','end_block','block')) and p['phases'].get('transaction',0) == p['transactions'], 'complete native phase/transaction coverage differs')
        require(p['opening_commit']['Version'] == first - 1 and p['ending_commit']['Version'] == through, 'complete profile native opening/ending height differs')
        named_roots(p['ending_store_roots'],through)
    for left, right in zip(profiles, profiles[1:]):
        require(left['through'] + 1 == right['first'] and exact(left['ending_commit']) == exact(right['opening_commit']) and named_roots(left['ending_store_roots'],left['through']) == named_roots(right['opening_store_roots'],left['through']), 'normal checkpoint financial/native all-nine-store seam differs')
        seams.append(left['through'])
        if right.get('schema') == 'native033-split-range-financial-v1' and right['fast_forward']['heights']:
            require(left.get('schema') == right['schema'] and left['source_range'] == right['source_range'] and right['progress_phase_base'] == left['progress_phase_base'] + sum(left['phases'].values()), 'rebuilt opening phase count differs from the preceding recorded part')
    require(set(seams) >= ends - {500000} and seams == sorted(set(seams)), 'gap, duplicate, bounded component or missing complete profile')
    return dict(distinct_prefix_heights=sum(p['heights'] for p in profiles), transactions=sum(p['transactions'] for p in profiles), all_nine_normal_checkpoint_seams=seams)


def verify(config, repo, progress=None, facts_path=None):
    require({'profiles', 'terminal_seal', 'canonical_boundary'} <= set(config) <= {'profiles', 'terminal_seal', 'canonical_boundary', 'cross_build_witness', 'both_builds_stretch', 'weaker_proof_stretches', 'jailed_without_stake_moved'}, 'unknown whole assembly input')
    sealed = [signed_profile(Path(p['path']), p['sha256'], repo, progress, with_seal=True) for p in config['profiles']]
    profiles = [result for result, _, _ in sealed]
    builds = check_builds(sealed, config.get('cross_build_witness'), config.get('both_builds_stretch'))
    stretches = check_stretches(sealed, config.get('weaker_proof_stretches'))
    completion_fix = check_completion_fix(sealed)
    summary = check_profiles(profiles)
    fact_blocks, fact_bytes = join_facts(sealed)
    jailings = check_jailings(config.get('jailed_without_stake_moved'))
    boundary = canonical_boundary(config['canonical_boundary'])
    require(boundary['canonical_migration_height'] == 500000 and boundary['terminal_export_is_next_chain_opening'] is False, 'canonical boundary cannot move to retained tail')
    proof = json.loads((Path(config['canonical_boundary']) / 'checkpoint-proof.json').read_bytes())
    end = profiles[-1]
    require(base64.b64decode(end['ending_commit']['Hash'], validate=True).hex() == proof['application_hash'], 'complete prefix ending root differs from accepted canonical migration')
    terminal_path = Path(config['terminal_seal'])
    terminal = pinned(terminal_path, TERMINAL_SHA)
    require(terminal['complete_scope'] == [500001,500015] and terminal['canonical_migration_height'] == 500000 and terminal['terminal_next_header_proof'] is False and terminal['financial_admission'] is False and terminal['prior500000_export_or_payout_reexecuted'] is False and terminal['whole_required_scope_acceptance'] is False, 'accepted separate tail scope or proof mode differs')
    pins = terminal['all_retained_files_sha256']
    anchor = pinned(terminal_path.parent / 'final-image/native-audit.ndjson.source-checkpoint.json', pins['final-image/native-audit.ndjson.source-checkpoint.json'])
    whole = pinned(terminal_path.parent / 'final-image/whole-terminal-component.private.json', pins['final-image/whole-terminal-component.private.json'])
    require(anchor['height'] == 500000 and exact(anchor['commit']) == exact(end['ending_commit']), 'complete prefix does not join accepted native tail opening')
    require(whole['financial_admission'] is False and whole['tax_acceptance'] is False and whole['financial_context']['native_read_contexts'] == 16 and whole['financial_context']['native_exact_mint_transitions'] == 15 and whole['financial_context']['ordinary_tail_fees_or_paid_income'] is False and whole['chain_boundary']['canonical_migration_height'] == 500000, 'actual accepted separate tail financial/canonical proof absent')
    if facts_path is not None:
        with facts_path.open('xb') as out:
            out.write(fact_bytes)
    return dict(schema='native033-whole-source-financial-reconciliation-v1', first=1, through=500015,
                distinct_historical_heights=summary['distinct_prefix_heights'] + 15,
                historical_transactions=summary['transactions'], normal_seams=summary['all_nine_normal_checkpoint_seams'],
                canonical_migration_height=500000, retained_separate_tail=[500001,500015],
                tail_proof_mode='closed_pinned_native_reference', next500016_execution_or_header_claim=False,
                zero_height_validation_payouts_are_historical_income=False,
                existing_terminal_and_canonical_proofs_reused_without_reexecution=True,
                all_historical_sources_and_coinflows_reconciled=True, financial_admission=False, books_tax_acceptance=False,
                production_import_or_public_promotion_authorized=False, input_config_sha256=hashlib.sha256(json.dumps(config,sort_keys=True).encode()).hexdigest(),
                profile_seals=config['profiles'], accepted_terminal_seal_sha256=TERMINAL_SHA,
                signed_source_commits=builds, cross_build_witness=config.get('cross_build_witness'), both_builds_stretch=config.get('both_builds_stretch'), first_heights_recorded_again_by_reproducible_helper=stretches,
                closing_check_fix_parts=completion_fix,
                stake_asset_reduction_blocks=fact_blocks, stake_asset_reduction_fact_rows=sum(e['fact_rows'] for e in fact_blocks),
                stake_asset_reductions_joined_sha256=hashlib.sha256(fact_bytes).hexdigest(),
                penalty_blocks_with_stake_moved=list(PENALTY_HEIGHTS), jailings_with_no_stake_moved=jailings, stake_asset_tax_classification='unclassified',
                launch_records=sorted({seal['launch_record'] for _, seal, _ in sealed}),
                parts=[dict(first=r['first'], through=r['through'], signed_source_commit=s['signed_source_commit'], launch_record=s['launch_record'], opening_kind=s['opening_kind'], resumed_from_interrupted_run=s['resumed_from_interrupted_run'], interrupted_run=s['interrupted_run'], savepoint_of_killed_run=s['savepoint_of_killed_run'], killed_run_exit=s['killed_run_exit'], python_source_proof=s['python_source_proof'], helper_proof=s['helper_proof'], earlier_attempts=s['earlier_attempts']) for r, s, _ in sealed])

if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--config', type=Path, required=True)
    parser.add_argument('--repo', type=Path, required=True)
    parser.add_argument('--receipt', type=Path, required=True)
    a = parser.parse_args()
    os.umask(0o077)
    result = verify(json.loads(a.config.read_bytes()), a.repo, facts_path=a.receipt.parent / (a.receipt.name.split('.')[0] + '.native-stake-reductions.joined.private.jsonl'))
    with a.receipt.open('x') as out:
        json.dump(result, out, indent=2, sort_keys=True)
    print('all 500015 historical source/coinflow components reconciled; production admission remains false')
