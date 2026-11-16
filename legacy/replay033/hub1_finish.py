#!/usr/bin/env python3
"""Seal finished Hub 1 parts and join them. One plan file names every part.

  hub1_finish.py --plan PLAN plan --catalog C    find the parts in the roots the catalog names and write PLAN (new file)
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
import time
sys.dont_write_bytecode = True
from consume_bounded import sha
from seal_closed import build_info, clean_at, seal
from verify_movement_rehearsal import require

HERE = Path(__file__).resolve().parent
TOOL = HERE.parents[1]


def background(scratch):
    os.nice(19)
    subprocess.run(['ionice', '-c', '3', '-p', str(os.getpid())], check=True)
    os.umask(0o077)
    # Nothing may land on the pod's small /tmp: every temporary file goes beside the records.
    require(not str(scratch.resolve()).startswith('/tmp'), 'plan keeps its records under /tmp')
    scratch.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.environ.update(TMPDIR=str(scratch), GOTMPDIR=str(scratch), GOCACHE=str(Path.home() / '.cache/go-build'))


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


def prepare(plan, part, controls_too=True):
    """The after-launch record, then the refusal controls. Both depend only on the source and the helper, not on the part having finished."""
    out = record_dir(plan, part)
    Path(plan['record_root']).mkdir(mode=0o700, exist_ok=True)
    if not (out / 'process.private.json').exists():
        from record_after_launch import record
        record(part['first'], part['through'], Path(part.get('attempt_proof') or part['proof']), Path(part['launch_worktree']), Path(part['build_worktree']), part['trimpath'], part['entry'],
               Path(part['helper']), optional(part, 'built_from'), Path(part['launcher']), out, optional(part, 'opening_checkpoint'), optional(part, 'opening_manifest'),
               optional(part, 'validator_history'), optional(part, 'test_worktree'), part.get('earlier_attempts'), bool(part.get('resume_opening')), part.get('interrupted_run'), part['state'] == 'savepoint')
        print(json.dumps(dict(part=part['first'], launch_record=str(out / 'process.private.json'))), flush=True)
    started = json.loads((out / 'process.private.json').read_bytes())
    # Controls depend only on the source, the helper and the opening, so a plan may name ones already made for the same three.
    # A controls run that was stopped part way is left as it is and a new folder is used; nothing is deleted.
    folders = [out / 'controls'] + [out / ('controls-%d' % n) for n in range(2, 20)]
    done = [f for f in folders if (f / 'actual-negative-controls.private.json').exists()]
    fresh = next(f for f in folders if not f.exists()) if not done else done[0]
    controls = Path(part['controls']) if part.get('controls') else fresh / 'actual-negative-controls.private.json'
    if controls_too and not controls.exists():
        require(not part.get('controls'), 'named controls do not exist yet')
        from actual_full_controls import run
        prefix = 'opening_checkpoint' in started
        r = run(Path(part['source']), fresh, Path(started['helper_copy']), part['first'],
                Path(started['opening_checkpoint']) if prefix else None, Path(started['opening_manifest']) if prefix else None, started['resumed_from_interrupted_run'])
        print(json.dumps(dict(part=part['first'], controls_refused=len(r['controls']))), flush=True)
    return started, controls


def seal_part(plan, part):
    proof = Path(part['proof'])
    if part['state'] == 'savepoint':
        require(str(Path(part['attempt_proof']) / 'complete-component.private.json') not in running(), 'the run that wrote this savepoint is still recording')
    else:
        exited = json.loads((proof / 'exit.private.json').read_bytes()) if (proof / 'exit.private.json').exists() else None
        require(exited is not None, 'part %d has not finished: no exit record yet' % part['first'])
        require(exited['exit'] == 0, 'part %d ended with exit %s: it cannot be sealed, its files are left untouched' % (part['first'], exited['exit']))
    started, controls = prepare(plan, part)
    out = record_dir(plan, part)
    preservation = out / 'source-preservation.private.json'
    if not preservation.exists():
        from verify_source_preservation import verify
        # The readback goes through a long kubectl exec stream, which drops under load. A dropped stream proves nothing either way, so read again.
        for attempt in range(5):
            try:
                verify(Path(part['source']), preservation)
                break
            except (subprocess.CalledProcessError, subprocess.TimeoutExpired):
                require(attempt < 4 and not preservation.exists(), 'preserved source could not be read back in five attempts')
                time.sleep(60)
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
    if plan.get('both_builds_stretch'):
        config['both_builds_stretch'] = [dict(path=str(d), sha256=sha(Path(d) / 'complete-component.private.json')) for d in plan['both_builds_stretch']]
    if plan.get('cross_build_witness'):
        witness = Path(plan['cross_build_witness'])
        config['cross_build_witness'] = dict(path=str(witness), sha256=sha(witness / 'complete-component.private.json'))
    root = Path(plan['record_root'])
    # A refused join leaves its inputs on record and a later attempt writes its own.
    with (root / ('join-config-%s.private.json' % datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ'))).open('x') as out:
        json.dump(config, out, indent=2, sort_keys=True)
    def progress(name, index, total):
        print(json.dumps(dict(rehashed=index, of=total, seal=name)), flush=True)
    result = verify(config, TOOL, progress, root / 'whole-join.native-stake-reductions.private.jsonl')
    with (root / 'whole-join.private.json').open('x') as out:
        json.dump(result, out, indent=2, sort_keys=True)
    print(json.dumps(dict(joined=[result['first'], result['through']], heights=result['distinct_historical_heights'], parts=len(seals),
                          penalty_blocks=[e['height'] for e in result['stake_asset_reduction_blocks']], penalty_fact_rows=result['stake_asset_reduction_fact_rows'],
                          receipt=str(root / 'whole-join.private.json'), receipt_sha256=sha(root / 'whole-join.private.json'), financial_admission=False)), flush=True)


def last_height(proof):
    log = proof / 'native-consumer.private.log'
    if not log.is_file():
        return 0
    with log.open('rb') as stream:
        stream.seek(max(0, log.stat().st_size - 4096))
        found = [int(word.split(b'=')[1]) for word in stream.read().split() if word.startswith(b'bounded_native_verified_height=')]
    return found[-1] if found else 0


def running():
    """Recorders alive now, by the receipt path each will write."""
    found = {}
    for directory in Path('/proc').iterdir():
        try:
            argv = [a.decode() for a in (directory / 'cmdline').read_bytes().split(b'\0')[:-1]] if directory.name.isdigit() else []
        except (FileNotFoundError, ProcessLookupError, PermissionError):
            continue
        scripts = [a for a in argv if a.endswith(('/consume_split.py', '/consume_closed.py'))]
        if scripts and '--receipt' in argv and 'python' in argv[0]:
            found[argv[argv.index('--receipt') + 1]] = (scripts[0], dict({argv[i][2:].replace('-', '_'): argv[i + 1] for i in range(len(argv) - 1) if argv[i].startswith('--')}, _flags=argv))
    return found


def make_plan(catalog):
    """Find every attempt in the named roots and choose one unbroken chain of parts from 1 to 500000.

    A finished part with exit 0 wins over a running one; attempts that were
    killed or failed are kept, untouched, and named on the part that replaced
    them. Anything the catalog cannot identify is refused by name."""
    live, attempts = running(), []
    openings = dict(catalog.get('openings', {}))
    for root in catalog['roots']:
        for manifest in Path(root['path']).glob('openings/*.sha256'):
            openings[sha(manifest)] = dict(opening_checkpoint=str(manifest.with_suffix('.home')), opening_manifest=str(manifest))
        # A run reopened from a savepoint keeps the list of that savepoint's files in its own folder.
        for manifest in Path(root['path']).glob('profile-*/opening.sha256'):
            openings[sha(manifest)] = dict(opening_checkpoint=str(Path((manifest.parent / 'opening.from').read_text().strip()) / 'native-home'), opening_manifest=str(manifest))
    for root in catalog['roots']:
        for proof in sorted(Path(root['path']).glob('profile-*/proof')):
            receipt, exit_path = proof / 'complete-component.private.json', proof / 'exit.private.json'
            exited = json.loads(exit_path.read_bytes())['exit'] if exit_path.exists() else None
            own = proof.parent / 'launcher.private.txt'
            item = dict(proof=str(proof), launcher=str(own) if own.is_file() else root.get('launcher') or str(proof.parent / root['launcher_in_part']), last_verified_height=last_height(proof), exit=exited)
            if str(receipt) in live:
                script, args = live[str(receipt)]
                source = json.loads((Path(args['directory']) / 'capture.private.json').read_bytes())
                item.update(state='recording', entry=Path(script).name, launch_worktree=str(Path(script).parents[2]), source=args['directory'], helper_file=args['native_helper'],
                            first=int(args.get('first', source['first'])), through=int(args.get('through', source['through'])), validator_history=args.get('validator_history'),
                            opening_checkpoint=args.get('opening_checkpoint'), opening_manifest=args.get('opening_manifest'), resume_opening='--resume-opening' in args['_flags'])
                item['helper_sha256'] = sha(Path(item['helper_file']))
            elif exited == 0 and receipt.exists():
                result = json.loads(receipt.read_bytes())
                split = result['schema'] == 'native033-split-range-financial-v1'
                opening = openings.get(result.get('opening_checkpoint_manifest_sha256') or '', {})
                require(split or result['schema'] == 'native033-complete-profile-financial-v1', 'unknown receipt kind in ' + str(proof))
                require(not result.get('opening_checkpoint_manifest_sha256') or opening, 'catalog does not name the opening checkpoint of ' + str(proof))
                item.update(state='finished', entry='consume_split.py' if split else 'consume_closed.py', first=result['first'], through=result['through'], helper_sha256=result['native_helper_sha256'],
                            source=catalog['sources'][str(result['source_range'][0] if split else result['first'])], validator_history=catalog.get('validator_history'),
                            opening_checkpoint=opening.get('opening_checkpoint'), opening_manifest=opening.get('opening_manifest'), resume_opening=result.get('opening_kind') == 'interrupted_run_state')
            else:
                item.update(state='failed' if exited else 'killed', first=int(proof.parent.name.split('-')[1]))
                folder = proof / 'savepoints'
                item['savepoints'] = sorted(int(d.name) for d in folder.iterdir() if d.name.isdigit() and (d / 'part.private.json').is_file()) if folder.is_dir() else []
            if item['state'] != 'recording' and own.is_file() and own.read_text().lstrip().startswith('{'):
                item['launch_worktree'] = json.loads(own.read_text())['launch_worktree']
            attempts.append(item)
    usable = [a for a in attempts if a['state'] in ('finished', 'recording')]
    # A killed run counts for the heights up to its latest savepoint, but only once another run has opened from that savepoint.
    for a in list(attempts):
        for height in reversed(a.get('savepoints', [])):
            if any(u['first'] == height + 1 for u in usable):
                folder = Path(a['proof']) / 'savepoints' / str(height)
                result = json.loads((folder / 'part.private.json').read_bytes())
                opening = openings.get(result.get('opening_checkpoint_manifest_sha256') or '', {})
                require(result['first'] == a['first'] and result['through'] == height and (not result.get('opening_checkpoint_manifest_sha256') or opening), 'savepoint does not belong to its run, or its opening is unknown: ' + str(folder))
                usable.append(dict(a, state='savepoint', proof=str(folder), attempt_proof=a['proof'], through=height, entry='consume_split.py', helper_sha256=result['native_helper_sha256'],
                                   source=catalog['sources'][str(result['source_range'][0])], validator_history=catalog.get('validator_history'),
                                   opening_checkpoint=opening.get('opening_checkpoint'), opening_manifest=opening.get('opening_manifest'), resume_opening=result.get('opening_kind') == 'interrupted_run_state'))
                a['saved_through'] = height
                break
    parts, at = [], 1
    while at <= 500000:
        here = sorted((a for a in usable if a['first'] == at), key=lambda a: (a['state'] != 'finished', -a['through'], -a['last_verified_height']))
        if not here:
            break
        chosen = here[0]
        helper = catalog['helpers'].get(chosen['helper_sha256'])
        require(helper is not None, 'catalog does not know the helper %s used by %s' % (chosen['helper_sha256'], chosen['proof']))
        require(chosen.get('launch_worktree', helper['launch_worktree']) in (helper['launch_worktree'], helper.get('test_worktree')), 'recorder runs from a worktree the catalog does not expect: ' + chosen['proof'])
        part = dict(helper, **{k: chosen[k] for k in ('first', 'through', 'proof', 'source', 'entry', 'launcher', 'state', 'attempt_proof', 'launch_worktree') if k in chosen})
        for name in ('validator_history', 'opening_checkpoint', 'opening_manifest', 'resume_opening'):
            if chosen.get(name):
                part[name] = chosen[name]
        origin = Path(chosen['opening_checkpoint']).with_suffix('.from') if chosen.get('resume_opening') else None
        if origin is not None and origin.is_file():
            part['interrupted_run'] = origin.read_text().strip()
        for known in catalog.get('known_controls', []):
            made = json.loads(Path(known).read_bytes())
            if made['first'] == part['first'] and made['native_helper_sha256'] == chosen['helper_sha256'] and (made.get('opening_checkpoint_manifest_sha256') or '') == (sha(Path(part['opening_manifest'])) if part.get('opening_manifest') else ''):
                part['controls'] = known
        part['earlier_attempts'] = [dict(proof=a['proof'], state=a['state'], exit=a['exit'], first=a['first'], last_verified_height=a['last_verified_height'],
                                         contributed_recorded_output='heights %d to %d only, through its savepoint, sealed as their own part' % (a['first'], a['saved_through']) if a.get('saved_through') else False)
                                    for a in attempts if a is not chosen and a['proof'] != chosen.get('attempt_proof') and a['state'] != 'finished' and (a['first'] == chosen['first'] or (a['first'] <= chosen['through'] and a.get('through', a['last_verified_height']) >= chosen['first']))]
        parts.append(part)
        at = chosen['through'] + 1
    plan = {k: catalog[k] for k in ('record_root', 'seal_prefix', 'terminal_seal', 'canonical_boundary', 'cross_build_witness', 'both_builds_stretch') if k in catalog}
    plan.update(parts=parts, covered_through=at - 1, complete=at == 500001, made_utc=datetime.datetime.now(datetime.timezone.utc).isoformat())
    return plan


def reopen(catalog, first, through=None):
    """Start a new run from the latest savepoint of a killed run. Leaves the killed run exactly as it is.

    The new run gets its own folder, profile-<savepoint height + 1>, beside the
    killed one. The helper accepts the saved state only after rebuilding the
    same height from recorded history and finding it equal key for key."""
    live = running()
    found = []
    for root in catalog['roots']:
        proof = Path(root['path']) / ('profile-%d' % first) / 'proof'
        saves = sorted(int(d.name) for d in (proof / 'savepoints').iterdir() if d.name.isdigit() and (d / 'part.private.json').is_file()) if (proof / 'savepoints').is_dir() else []
        if saves:
            found.append((proof, saves[-1]))
    require(len(found) == 1, 'exactly one run opening at %d with a savepoint is needed, found %d' % (first, len(found)))
    proof, height = found[0]
    require(str(proof / 'complete-component.private.json') not in live and not (proof / 'complete-component.private.json').exists(), 'that run is still recording or has finished: nothing to reopen')
    saved = proof / 'savepoints' / str(height)
    result = json.loads((saved / 'part.private.json').read_bytes())
    known = Path(catalog['record_root']) / ('part-%d' % first) / 'process.private.json'
    through = through or (json.loads(known.read_bytes())['through'] if known.is_file() else None)
    require(through is not None and height < through, 'last height of the part is unknown: pass --through')
    directory = proof.parent.parent / ('profile-%d' % (height + 1))
    directory.mkdir(mode=0o700)
    lines = ''.join('%s  ./%s\n' % (sha(f), f.relative_to(saved / 'native-home')) for f in sorted(p for p in (saved / 'native-home').rglob('*') if not p.is_dir()))
    (directory / 'opening.sha256').write_text(lines)
    (directory / 'opening.from').write_text(str(saved) + '\n')
    helper = catalog['helpers'][result['native_helper_sha256']]
    part = dict(helper, launch_worktree=helper.get('test_worktree') or helper['launch_worktree'], entry='consume_split.py', source=catalog['sources'][str(result['source_range'][0])],
                opening_checkpoint=str(saved / 'native-home'), opening_manifest=str(directory / 'opening.sha256'), resume_opening=True, savepoint_every=catalog.get('savepoint_every', 2500), recorder_prefix=catalog.get('recorder_prefix', []))
    if catalog.get('validator_history'):
        part['validator_history'] = catalog['validator_history']
    print(json.dumps(dict(reopened_from_savepoint=height, of_run=str(proof), new_run=str(directory), first=height + 1, through=through)), flush=True)
    return mini(None, part, height + 1, through, directory)


def mini(plan, part, first, through, directory):
    """A short real slice recorded by the launch helper from the launch commit, checked before it starts."""
    launch, helper = Path(part['launch_worktree']), Path(part['helper'])
    commit = build_info(helper)['revision']
    clean_at(launch, commit, 'launch worktree is not the helper commit')
    require(part['entry'] == 'consume_split.py', 'a short slice needs the split recorder')
    proof = directory / 'proof'
    proof.mkdir(mode=0o700, parents=True)
    command = list(part.get('recorder_prefix', [])) + ['/usr/bin/python3', '-B', str(launch / 'legacy/replay033/consume_split.py'), '--directory', part['source'], '--first', str(first), '--through', str(through),
               '--native-helper', str(helper), '--receipt', str(proof / 'complete-component.private.json')]
    if part.get('opening_checkpoint'):
        command += ['--opening-checkpoint', part['opening_checkpoint'], '--opening-manifest', part['opening_manifest']] + (['--resume-opening'] if part.get('resume_opening') else [])
    if part.get('savepoint_every'):
        command += ['--savepoint-every', str(part['savepoint_every'])]
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
    parser.add_argument('--plan', type=Path, help='a plan written earlier; without it the plan is made afresh from --catalog')
    parser.add_argument('step', choices=('plan', 'prepare', 'seal', 'join', 'mini', 'reopen', 'watch'))
    parser.add_argument('--catalog', type=Path, help='plan only: the roots to search and the known helpers, sources and openings')
    parser.add_argument('--first', type=int)
    parser.add_argument('--through', type=int)
    parser.add_argument('--part', type=int, help='mini only: the plan part whose source, helper and launch worktree to use')
    parser.add_argument('--directory', type=Path)
    parser.add_argument('--record-only', action='store_true', help='prepare only: write the launch record now and leave the controls for later')
    a = parser.parse_args()
    if a.step == 'plan':
        catalog = json.loads(a.catalog.read_bytes())
        background(Path(catalog['record_root']) / 'tmp')
        plan = make_plan(catalog)
        with a.plan.open('x') as out:
            json.dump(plan, out, indent=1)
        print(json.dumps(dict(plan=str(a.plan), complete=plan['complete'], covered_through=plan['covered_through'],
                              parts=[[p['first'], p['through'], p['state'], len(p['earlier_attempts'])] for p in plan['parts']])), flush=True)
        sys.exit(0 if plan['complete'] else 3)
    if a.step == 'reopen':
        catalog = json.loads(a.catalog.read_bytes())
        background(Path(catalog['record_root']) / 'tmp')
        sys.exit(reopen(catalog, a.first, a.through))
    plan = json.loads(a.plan.read_bytes()) if a.plan else make_plan(json.loads(a.catalog.read_bytes()))
    background(Path(plan['record_root']) / 'tmp')
    if a.step == 'watch':
        sys.exit('the watcher is started as hub1_seal_watch.py')
    clean_at(TOOL, subprocess.check_output(['git', '-C', str(TOOL), 'rev-parse', 'HEAD'], text=True).strip(), 'sealing tool tree is not clean')
    if a.step == 'prepare':
        prepare(plan, part_of(plan, a.first), not a.record_only)
    elif a.step == 'seal':
        seal_part(plan, part_of(plan, a.first))
    elif a.step == 'join':
        join(plan)
    else:
        sys.exit(mini(plan, part_of(plan, a.part), a.first, a.through, a.directory.resolve()))
