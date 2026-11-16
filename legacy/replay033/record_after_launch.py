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


def launch_assertions(text, commit, digest, launch):
    """What the launcher itself checked before it started anything, read from its text."""
    if text.lstrip().startswith('{'):
        note = json.loads(text)
        ours = note.get('checked_before_start') is True and note.get('signed_source_commit') == commit and note.get('helper_sha256') == digest and note.get('launch_worktree') == str(launch)
        return dict(commit=ours, clean_tree=ours, helper_hash=ours)
    return dict(commit='rev-parse HEAD)" = ' + commit in text and str(launch) in text,
                clean_tree='status --porcelain)" ]' in text and '-z "$(git' in text and str(launch) in text,
                helper_hash='= ' + digest + ' ]' in text and 'sha256sum' in text)


def record(first, through, proof, launch, build, trimpath, entry, helper, built_from, launcher, out, opening=None, manifest=None, history=None, tests=None, earlier=None, resume=False, origin=None, saved=False):
    """launch: the worktree the recorder's Python ran from. build: where the helper was compiled (its path is inside a non-trimpath helper).
    tests: any clean checkout of the same commit, for the tests and the compiled-source list."""
    proof, launch, build, helper, launcher, out = [p.resolve() for p in (proof, launch, build, helper, launcher, out)]
    tests = (tests or build).resolve()
    require(entry in ENTRIES and first <= through and not str(out).startswith('/tmp/'), 'unknown recorder entry or record under /tmp')
    digest = sha(helper)
    info = build_info(helper)
    commit = info['revision']
    require(len(commit) == 40 and all(c in '0123456789abcdef' for c in commit) and info['modified'] == 'false' and info['trimpath'] == ('true' if trimpath else 'false'), 'helper is not stamped as a build of one unmodified commit in the expected build mode')
    require(built_from is None or built_from.read_text().strip() == commit, 'build note beside the helper names another commit')
    clean_at(tests, commit, 'test worktree is not a clean checkout of the helper commit')
    subprocess.run(['git', '-C', str(tests), 'verify-commit', commit], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
    text = launcher.read_text()
    asserted = launch_assertions(text, commit, digest, launch)
    out.mkdir(mode=0o700)
    live = observe(proof, launch, entry, helper)
    require('helper' not in live or live['helper']['executing_image_sha256'] == digest, 'executing helper image differs from the named helper')
    finished = (proof / 'exit.private.json').exists()
    require(finished or saved or 'recorder' in live, 'part is neither finished nor observably running')
    require('recorder' not in live or ('--resume-opening' in live['recorder']['argv']) == resume, 'running recorder and plan disagree on whether this part opens from an interrupted run')
    born = int(subprocess.check_output(['stat', '-c', '%W', str(proof)], text=True))
    require('recorder' in live or born > 0, 'launch time of a finished part cannot be read')
    launched = live['recorder']['started_utc'] if 'recorder' in live else datetime.datetime.fromtimestamp(born, datetime.timezone.utc).isoformat()
    basis = 'start time of the running recorder process' if 'recorder' in live else 'creation time of the proof directory'
    started_at = datetime.datetime.fromisoformat(launched).timestamp()

    # Python: the files in the worktree it ran from, against the signed commit.
    pins, changed = {}, {}
    for name in code_list(tests, commit):
        if name.endswith('.py'):
            pins[name] = blob_sha(tests, commit, name)
            if not (launch / name).is_file() or sha(launch / name) != pins[name]:
                changed[name] = datetime.datetime.fromtimestamp((launch / name).stat().st_mtime, datetime.timezone.utc).isoformat() if (launch / name).exists() else 'missing'
    if not changed and git(launch, 'rev-parse', 'HEAD').strip() == commit:
        python_proof = 'launch_worktree_equal_to_commit_when_recorded'
    else:
        # Someone edited the worktree after launch. That is acceptable only when the launcher itself checked the commit and a clean tree
        # before starting, and every file that now differs was last written after this recorder had started.
        require(asserted['commit'] and asserted['clean_tree'], 'launch worktree differs from the commit and the launcher did not check it before starting')
        require(all(when != 'missing' and datetime.datetime.fromisoformat(when).timestamp() > started_at + 1 for when in changed.values()), 'Python in the launch worktree was changed before or at launch')
        python_proof = 'launcher_checked_clean_commit_and_every_later_edit_postdates_start'

    tmp = out / 'tmp'
    tmp.mkdir(mode=0o700)
    env = dict(os.environ, TMPDIR=str(tmp), GOTMPDIR=str(tmp), GOMAXPROCS='2', GOMEMLIMIT='512MiB', GOFLAGS='-mod=readonly')
    arguments = ['go', 'build', '-p', '2'] + (['-trimpath'] if trimpath else []) + ['-o', str(out / 'consume-native.rebuilt'), './cmd/consume-native']
    if trimpath:
        env['CGO_ENABLED'] = info['cgo']
    tested = tests / 'legacy/replay033'
    names_of_tests = ['test_consume_closed', 'test_closed_lifecycle', 'test_consume_bounded.Scalars', 'test_consume_bounded.PhaseOwners'] + (['test_consume_split'] if (tested / 'test_consume_split.py').exists() else [])
    # A helper built without -trimpath carries its build path, so only a build at that same path can reproduce it.
    can_rebuild = git(build, 'rev-parse', 'HEAD').strip() == commit and git(build, 'status', '--porcelain') == ''
    rebuilt = ''
    with (out / 'build-test.private.log').open('xb') as log:
        log.write(('# Built and tested AFTER launch, at %s, from commit %s. Not a pre-launch check.\n' % (now(), commit)).encode())
        log.flush()
        if can_rebuild:
            subprocess.run(arguments, cwd=build / 'legacy/replay033', env=env, stdout=log, stderr=subprocess.STDOUT, check=True)
            rebuilt = sha(out / 'consume-native.rebuilt')
            require(rebuilt == digest, 'rebuilding the named commit does not reproduce the helper that ran')
            clean_at(build, commit, 'rebuild changed the build worktree')
        else:
            log.write(('# No byte-for-byte rebuild: %s is no longer a clean checkout of %s, and the helper embeds that path.\n' % (build, commit)).encode())
            log.flush()
        subprocess.run(['go', 'test', '-p', '2', './cmd/consume-native'], cwd=tested, env=env, stdout=log, stderr=subprocess.STDOUT, check=True)
        subprocess.run([sys.executable, '-B', '-m', 'unittest'] + names_of_tests, cwd=tested, env=env, stdout=log, stderr=subprocess.STDOUT, check=True)
    if rebuilt:
        helper_proof = 'rebuilt_byte_identical_from_commit'
    else:
        require(asserted['helper_hash'], 'helper cannot be rebuilt at its build path and the launcher did not pin its hash')
        helper_proof = 'toolchain_stamp_of_unmodified_commit_and_launcher_pinned_hash'
    compiled = {}
    listing = subprocess.check_output(['go', 'list', '-deps', '-f', '{{.Dir}} {{join .GoFiles " "}}', './cmd/consume-native'], cwd=tested, env=env, text=True)
    for line in listing.splitlines():
        parts = line.split()
        for name in parts[1:]:
            path = Path(parts[0]) / name
            compiled[str(path)] = hashlib.sha256(path.read_bytes()).hexdigest()
    (out / 'compiled-go-source-pins.private.json').write_text(json.dumps(compiled, indent=2, sort_keys=True))
    clean_at(tests, commit, 'tests changed the test worktree')
    copies = dict(launcher=out / 'launcher.sh', helper=out / 'consume-native')
    shutil.copyfile(helper, copies['helper'])
    os.chmod(copies['helper'], 0o700)
    require(sha(copies['helper']) == digest, 'retained helper copy differs')
    shutil.copyfile(launcher, copies['launcher'])
    kind = ('interrupted_run_state' if resume else 'saved_checkpoint') if opening is not None else 'source_range_start'
    state = dict(schema=SCHEMA, checked_before_launch=False,
                 statement='Written after launch. Nothing here was checked by this tool before the part started; each proof below says how it was obtained and when.',
                 first=first, through=through, entry=entry, signed_source_commit=commit, python_source_pins=pins,
                 python_source_proof=python_proof, python_files_edited_after_launch=changed, launcher_checked_before_start=asserted,
                 helper_proof=helper_proof, launch_worktree=str(launch), build_worktree=str(build), test_worktree=str(tests), build_arguments=arguments,
                 helper_path=str(helper), helper_copy=str(copies['helper']), launched_utc_basis=basis,
                 helper_sha256=digest, rebuilt_helper_sha256=rebuilt, helper_build=info,
                 launcher=str(copies['launcher']), launcher_sha256=sha(launcher),
                 live_processes=live, part_finished_when_recorded=finished,
                 # A part always checks every one of its own heights in one run. What differs is where its opening state came from.
                 opening_kind=kind, resumed_from_interrupted_run=resume, interrupted_run=origin or '',
                 # Attempts at the same heights that were killed before finishing. None of their recorded output is used.
                 earlier_attempts=earlier or [], launched_utc=launched, proof=str(proof))
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
        require(state['opening_checkpoint_manifest_sha256'] == sha(manifest), 'opening checkpoint manifest changed while it was copied')
    state['produced_utc'] = now()
    with (out / 'process.private.json').open('x') as stream:
        json.dump(state, stream, indent=2, sort_keys=True)
    return state


if __name__ == '__main__':
    sys.exit('use hub1_finish.py prepare, which reads the part from its plan')
