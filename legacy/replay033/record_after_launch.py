#!/usr/bin/env python3
"""Write the launch record of a Hub 1 part that was started without its supervisor.

Nothing here claims a check made before launch. Every fact is measured now and
labelled so: the helper that ran is rebuilt from the commit it names and must
come out byte-identical, the worktree the Python ran from must still be that
clean signed commit, and while the part is still running the executing helper
image is hashed straight from the process. It reads the live part, never
writes into it and never signals a process.
"""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
sys.dont_write_bytecode = True
from consume_bounded import sha
from seal_closed import blob_sha, build_info, clean_at, code_list, git
from verify_movement_rehearsal import require

SCHEMA = 'native033-launch-record-after-launch-v1'
ENTRIES = ('consume_closed.py', 'consume_split.py')


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def observe(proof, launch, entry, helper):
    """The processes still writing this part, found by their exact receipt paths."""
    seen = {}
    for directory in Path('/proc').iterdir():
        if not directory.name.isdigit():
            continue
        try:
            argv = (directory / 'cmdline').read_bytes().split(b'\0')[:-1]
            argv = [a.decode() for a in argv]
            if '--receipt' not in argv or argv[0].endswith('sh'):
                continue
            target = argv[argv.index('--receipt') + 1]
            if target == str(proof / 'complete-component.private.json'):
                require(str(launch / 'legacy/replay033' / entry) in argv, 'running recorder was not started from the launch worktree')
                seen['recorder'] = dict(pid=int(directory.name), argv=argv, started_utc=started(directory))
            elif target == str(proof / 'native-proof.private.json'):
                image = hashlib.sha256()
                with (directory / 'exe').open('rb') as stream:
                    for chunk in iter(lambda: stream.read(1 << 20), b''):
                        image.update(chunk)
                require(argv[0] == str(helper), 'running helper path differs from the named helper')
                seen['helper'] = dict(pid=int(directory.name), argv=argv, executing_image_sha256=image.hexdigest(), started_utc=started(directory))
        except (FileNotFoundError, ProcessLookupError, PermissionError):
            continue
    return seen


def started(directory):
    ticks = int((directory / 'stat').read_text().rsplit(')', 1)[1].split()[19])
    boot = next(int(line.split()[1]) for line in Path('/proc/stat').read_text().splitlines() if line.startswith('btime'))
    return datetime.datetime.fromtimestamp(boot + ticks / os.sysconf('SC_CLK_TCK'), datetime.timezone.utc).isoformat()


def record(first, through, proof, launch, build, trimpath, entry, helper, built_from, launcher, out, opening=None, manifest=None, history=None, tests=None, earlier=None):
    proof, launch, build, helper, built_from, launcher, out = [p.resolve() for p in (proof, launch, build, helper, built_from, launcher, out)]
    # Tests may run in another clean worktree of the same commit, so nothing but the compiler reads the tree live recorders run from.
    tests = (tests or build).resolve()
    require(entry in ENTRIES and first <= through, 'unknown recorder entry')
    commit = built_from.read_text().strip()
    require(len(commit) == 40 and all(c in '0123456789abcdef' for c in commit), 'helper build commit file is not one literal commit')
    for repo in (launch, build, tests):
        clean_at(repo, commit, 'worktree is not the commit the helper names')
    subprocess.run(['git', '-C', str(launch), 'verify-commit', commit], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
    digest = sha(helper)
    info = build_info(helper)
    require(info['revision'] == commit and info['modified'] == 'false' and info['trimpath'] == ('true' if trimpath else 'false'), 'helper build stamp differs from the named clean commit or build mode')
    text = launcher.read_text()
    names = commit in text and digest in text and str(launch) in text and entry in text
    require(names, 'launcher does not name this commit, helper hash, worktree and recorder')
    out.mkdir(mode=0o700)
    live = observe(proof, launch, entry, helper)
    require('helper' not in live or live['helper']['executing_image_sha256'] == digest, 'executing helper image differs from the named helper')
    finished = (proof / 'exit.private.json').exists()
    require(finished or {'recorder', 'helper'} <= set(live) or (entry == 'consume_split.py' and 'recorder' in live), 'part is neither finished nor observably running')
    born = int(subprocess.check_output(['stat', '-c', '%W', str(proof)], text=True))
    require('recorder' in live or born > 0, 'launch time of a finished part cannot be read')
    launched = live['recorder']['started_utc'] if 'recorder' in live else datetime.datetime.fromtimestamp(born, datetime.timezone.utc).isoformat()
    basis = 'start time of the running recorder process' if 'recorder' in live else 'creation time of the proof directory'
    source = build / 'legacy/replay033'
    tmp = out / 'tmp'
    tmp.mkdir(mode=0o700)
    require(not str(out).startswith('/tmp/'), 'records are never written under /tmp')
    env = dict(os.environ, TMPDIR=str(tmp), GOTMPDIR=str(tmp), GOMAXPROCS='2', GOMEMLIMIT='512MiB', GOFLAGS='-mod=readonly')
    arguments = ['go', 'build'] + (['-trimpath'] if trimpath else []) + ['-o', str(out / 'consume-native.rebuilt'), './cmd/consume-native']
    if trimpath:
        env['CGO_ENABLED'] = info['cgo']
    names_of_tests = ['test_consume_closed', 'test_closed_lifecycle', 'test_consume_bounded.Scalars', 'test_consume_bounded.PhaseOwners'] + (['test_consume_split'] if (source / 'test_consume_split.py').exists() else [])
    tested = tests / 'legacy/replay033'
    with (out / 'build-test.private.log').open('xb') as log:
        log.write(('# Rebuilt and tested AFTER launch, at %s, from commit %s. Not a pre-launch check.\n' % (now(), commit)).encode())
        log.flush()
        subprocess.run(arguments, cwd=source, env=env, stdout=log, stderr=subprocess.STDOUT, check=True)
        subprocess.run(['go', 'test', './cmd/consume-native'], cwd=tested, env=env, stdout=log, stderr=subprocess.STDOUT, check=True)
        subprocess.run([sys.executable, '-B', '-m', 'unittest'] + names_of_tests, cwd=tested, env=env, stdout=log, stderr=subprocess.STDOUT, check=True)
    rebuilt = sha(out / 'consume-native.rebuilt')
    require(rebuilt == digest, 'rebuilding the named commit does not reproduce the helper that ran')
    compiled = {}
    listing = subprocess.check_output(['go', 'list', '-deps', '-f', '{{.Dir}} {{join .GoFiles " "}}', './cmd/consume-native'], cwd=source, env=env, text=True)
    for line in listing.splitlines():
        parts = line.split()
        for name in parts[1:]:
            path = Path(parts[0]) / name
            compiled[str(path)] = hashlib.sha256(path.read_bytes()).hexdigest()
    (out / 'compiled-go-source-pins.private.json').write_text(json.dumps(compiled, indent=2, sort_keys=True))
    for repo in (launch, build, tests):
        clean_at(repo, commit, 'rebuild or test changed a worktree')
    pins = {}
    for name in code_list(launch, commit):
        if name.endswith('.py'):
            pins[name] = blob_sha(launch, commit, name)
            require(sha(launch / name) == pins[name], 'Python in the launch worktree differs from the signed commit')
    copies = dict(launcher=out / 'launcher.sh', built_from=out / 'built-from-commit.txt', helper=out / 'consume-native')
    shutil.copyfile(helper, copies['helper'])
    os.chmod(copies['helper'], 0o700)
    require(sha(copies['helper']) == digest, 'retained helper copy differs')
    shutil.copyfile(launcher, copies['launcher'])
    shutil.copyfile(built_from, copies['built_from'])
    state = dict(schema=SCHEMA, checked_before_launch=False,
                 statement='Written after launch. The launcher asserted the commit, a clean tree and the helper hash before starting; this record re-measures them and rebuilds the helper, and proves nothing about the instant of launch beyond that.',
                 first=first, through=through, entry=entry, signed_source_commit=commit, python_source_pins=pins,
                 launch_worktree=str(launch), build_worktree=str(build), test_worktree=str(tests), build_arguments=arguments, helper_path=str(helper), helper_copy=str(copies['helper']), launched_utc_basis=basis,
                 helper_sha256=digest, rebuilt_helper_sha256=rebuilt, helper_build=info, launcher_names_commit_and_helper=names,
                 launcher=str(copies['launcher']), launcher_sha256=sha(launcher), built_from=str(copies['built_from']),
                 live_processes=live, part_finished_when_recorded=finished,
                 # Attempts at the same heights that were killed before finishing. They gave this part nothing: it recorded every height itself, in one run, from its opening.
                 earlier_attempts=earlier or [], recorded_in_one_run_from_its_opening=True, resumed_from_saved_state=False, launched_utc=launched, proof=str(proof))
    if history is not None:
        shutil.copyfile(history, out / 'validator-history.private.jsonl')
        state.update(validator_history=str(out / 'validator-history.private.jsonl'), validator_history_sha256=sha(out / 'validator-history.private.jsonl'))
        require(state['validator_history_sha256'] == sha(history) and state['validator_history_sha256'] in text, 'validator history differs from the one the launcher pinned')
    if opening is not None:
        from consume_split import opening_manifest
        shutil.copytree(opening, out / 'opening-checkpoint')
        shutil.copyfile(manifest, out / 'opening-checkpoint.manifest.sha256')
        state.update(opening_checkpoint=str(out / 'opening-checkpoint'), opening_manifest=str(out / 'opening-checkpoint.manifest.sha256'),
                     opening_checkpoint_manifest_sha256=opening_manifest(out / 'opening-checkpoint', out / 'opening-checkpoint.manifest.sha256'))
        require(state['opening_checkpoint_manifest_sha256'] == sha(manifest) and state['opening_checkpoint_manifest_sha256'] in text, 'opening checkpoint differs from the one the launcher pinned')
    state['produced_utc'] = now()
    with (out / 'process.private.json').open('x') as stream:
        json.dump(state, stream, indent=2, sort_keys=True)
    return state


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--first', type=int, required=True)
    parser.add_argument('--through', type=int, required=True)
    for name in ('proof', 'launch-worktree', 'build-worktree', 'native-helper', 'built-from', 'launcher', 'directory'):
        parser.add_argument('--' + name, type=Path, required=True)
    parser.add_argument('--entry', choices=ENTRIES, required=True)
    parser.add_argument('--trimpath', action='store_true')
    for name in ('opening-checkpoint', 'opening-manifest', 'validator-history', 'test-worktree'):
        parser.add_argument('--' + name, type=Path)
    a = parser.parse_args()
    os.umask(0o077)
    r = record(a.first, a.through, a.proof, a.launch_worktree, a.build_worktree, a.trimpath, a.entry, a.native_helper, a.built_from, a.launcher, a.directory, a.opening_checkpoint, a.opening_manifest, a.validator_history, a.test_worktree)
    print(json.dumps({k: r[k] for k in ('first', 'through', 'signed_source_commit', 'helper_sha256', 'rebuilt_helper_sha256', 'checked_before_launch', 'part_finished_when_recorded')}))
