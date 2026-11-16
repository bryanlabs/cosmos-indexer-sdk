#!/usr/bin/env python3
"""Seal finished Hub 1 parts and join them. One plan file names every part.

  hub1_finish.py --plan PLAN prepare --first N   launch record and controls, safe while the part still records
  hub1_finish.py --plan PLAN seal --first N      seal one finished part (runs prepare first when needed)
  hub1_finish.py --plan PLAN join                join every sealed part with the accepted tail
  hub1_finish.py --plan PLAN mini --first N --through M --directory D
                                                 record a short real slice with the launch helper into a new directory

It only reads a recording part, signals nothing and deletes nothing. Every
output is created exclusively, so a second run of a finished step is refused
rather than overwritten. Nothing here admits anything to the books.
"""
import argparse
import datetime
import json
import os
from pathlib import Path
import subprocess
import sys
sys.dont_write_bytecode = True
from consume_bounded import sha
from seal_closed import build_info, clean_at, seal
from verify_movement_rehearsal import require

HERE = Path(__file__).resolve().parent
TOOL = HERE.parents[1]


def background():
    os.nice(19)
    subprocess.run(['ionice', '-c', '3', '-p', str(os.getpid())], check=True)
    os.umask(0o077)


def part_of(plan, first):
    found = [p for p in plan['parts'] if p['first'] == first]
    require(len(found) == 1, 'plan has no such part')
    return found[0]


def record_dir(plan, part):
    return Path(plan['record_root']) / ('part-%d' % part['first'])


def seal_path(plan, part):
    return Path(plan['seal_prefix'] + '-part-%d.private.json' % part['first'])


def optional(part, name):
    return Path(part[name]) if part.get(name) else None


def prepare(plan, part):
    """The after-launch record, then the refusal controls. Both depend only on the source and the helper, not on the part having finished."""
    out = record_dir(plan, part)
    Path(plan['record_root']).mkdir(mode=0o700, exist_ok=True)
    if not (out / 'process.private.json').exists():
        from record_after_launch import record
        record(part['first'], part['through'], Path(part['proof']), Path(part['launch_worktree']), Path(part['build_worktree']), part['trimpath'], part['entry'],
               Path(part['helper']), Path(part['built_from']), Path(part['launcher']), out, optional(part, 'opening_checkpoint'), optional(part, 'opening_manifest'),
               optional(part, 'validator_history'), optional(part, 'test_worktree'))
        print(json.dumps(dict(part=part['first'], launch_record=str(out / 'process.private.json'))), flush=True)
    started = json.loads((out / 'process.private.json').read_bytes())
    controls = out / 'controls' / 'actual-negative-controls.private.json'
    if not controls.exists():
        from actual_full_controls import run
        prefix = 'opening_checkpoint' in started
        r = run(Path(part['source']), out / 'controls', Path(started['helper_copy']), part['first'],
                Path(started['opening_checkpoint']) if prefix else None, Path(started['opening_manifest']) if prefix else None)
        print(json.dumps(dict(part=part['first'], controls_refused=len(r['controls']))), flush=True)
    return started, controls


def seal_part(plan, part):
    proof = Path(part['proof'])
    exited = json.loads((proof / 'exit.private.json').read_bytes()) if (proof / 'exit.private.json').exists() else None
    require(exited is not None, 'part %d has not finished: no exit record yet' % part['first'])
    require(exited['exit'] == 0, 'part %d ended with exit %s: it cannot be sealed, its files are left untouched' % (part['first'], exited['exit']))
    started, controls = prepare(plan, part)
    out = record_dir(plan, part)
    preservation = out / 'source-preservation.private.json'
    if not preservation.exists():
        from verify_source_preservation import verify
        verify(Path(part['source']), preservation)
    result = seal(Path(part['source']), proof, Path(started['helper_copy']), controls, preservation, TOOL, seal_path(plan, part), out / 'process.private.json')
    print(json.dumps(dict(sealed_part=[part['first'], part['through']], seal=str(seal_path(plan, part)), seal_sha256=sha(seal_path(plan, part)),
                          pinned_files=len(result['artifact_pins']), penalty_blocks=[e['height'] for e in result['stake_asset_fact_blocks']],
                          launch_record=result['launch_record'], financial_admission=False)), flush=True)


def join(plan):
    from reconcile_whole import verify
    seals = [seal_path(plan, part) for part in sorted(plan['parts'], key=lambda p: p['first'])]
    missing = [str(p) for p in seals if not p.exists()]
    require(not missing, 'parts not sealed yet: ' + ', '.join(missing))
    config = dict(profiles=[dict(path=str(p), sha256=sha(p)) for p in seals], terminal_seal=plan['terminal_seal'], canonical_boundary=plan['canonical_boundary'])
    if len({json.loads(p.read_bytes())['signed_source_commit'] for p in seals}) > 1:
        witness = Path(plan['cross_build_witness'])
        config['cross_build_witness'] = dict(path=str(witness), sha256=sha(witness / 'complete-component.private.json'))
    root = Path(plan['record_root'])
    with (root / 'join-config.private.json').open('x') as out:
        json.dump(config, out, indent=2, sort_keys=True)
    def progress(name, index, total):
        print(json.dumps(dict(rehashed=index, of=total, seal=name)), flush=True)
    result = verify(config, TOOL, progress, root / 'whole-join.native-stake-reductions.private.jsonl')
    with (root / 'whole-join.private.json').open('x') as out:
        json.dump(result, out, indent=2, sort_keys=True)
    print(json.dumps(dict(joined=[result['first'], result['through']], heights=result['distinct_historical_heights'], parts=len(seals),
                          penalty_blocks=[e['height'] for e in result['stake_asset_reduction_blocks']], penalty_fact_rows=result['stake_asset_reduction_fact_rows'],
                          receipt=str(root / 'whole-join.private.json'), receipt_sha256=sha(root / 'whole-join.private.json'), financial_admission=False)), flush=True)


def mini(plan, part, first, through, directory):
    """A short real slice recorded by the launch helper from the launch commit, checked before it starts."""
    launch, helper = Path(part['launch_worktree']), Path(part['helper'])
    commit = Path(part['built_from']).read_text().strip()
    clean_at(launch, commit, 'launch worktree is not the helper commit')
    require(build_info(helper)['revision'] == commit and part['entry'] == 'consume_split.py', 'a short slice needs the split recorder and its own helper')
    proof = directory / 'proof'
    proof.mkdir(mode=0o700, parents=True)
    command = [sys.executable, '-B', str(launch / 'legacy/replay033/consume_split.py'), '--directory', part['source'], '--first', str(first), '--through', str(through),
               '--native-helper', str(helper), '--receipt', str(proof / 'complete-component.private.json')]
    if part.get('opening_checkpoint'):
        command += ['--opening-checkpoint', part['opening_checkpoint'], '--opening-manifest', part['opening_manifest']]
    if part.get('validator_history'):
        command += ['--validator-history', part['validator_history'], '--validator-history-sha256', sha(Path(part['validator_history']))]
    (directory / 'launcher.private.txt').write_text(json.dumps(dict(checked_before_start=True, signed_source_commit=commit, launch_worktree=str(launch), helper_sha256=sha(helper),
        opening_manifest_sha256=sha(Path(part['opening_manifest'])) if part.get('opening_manifest') else '', argv=command, checked_utc=datetime.datetime.now(datetime.timezone.utc).isoformat()), indent=2))
    with (proof / 'consumer.private.log').open('xb') as log:
        code = subprocess.run(command, env=dict(os.environ, GOMAXPROCS='2', GOMEMLIMIT='512MiB', GOFLAGS='-mod=readonly'), stdout=log, stderr=subprocess.STDOUT).returncode
    (proof / 'exit.private.json').write_text(json.dumps(dict(exit=code, financial_admission=False, finished_utc=datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'))) + '\n')
    print(json.dumps(dict(slice=[first, through], exit=code, proof=str(proof))), flush=True)
    return code


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--plan', type=Path, required=True)
    parser.add_argument('step', choices=('prepare', 'seal', 'join', 'mini'))
    parser.add_argument('--first', type=int)
    parser.add_argument('--through', type=int)
    parser.add_argument('--part', type=int, help='mini only: the plan part whose source, helper and launch worktree to use')
    parser.add_argument('--directory', type=Path)
    a = parser.parse_args()
    background()
    plan = json.loads(a.plan.read_bytes())
    clean_at(TOOL, subprocess.check_output(['git', '-C', str(TOOL), 'rev-parse', 'HEAD'], text=True).strip(), 'sealing tool tree is not clean')
    if a.step == 'prepare':
        prepare(plan, part_of(plan, a.first))
    elif a.step == 'seal':
        seal_part(plan, part_of(plan, a.first))
    elif a.step == 'join':
        join(plan)
    else:
        sys.exit(mini(plan, part_of(plan, a.part), a.first, a.through, a.directory.resolve()))
