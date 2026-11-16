#!/usr/bin/env python3
"""Seal each Hub 1 part when it finishes, then join them. One step at a time.

    setsid nohup python3 -B hub1_seal_watch.py --catalog CATALOG >> LOG 2>&1 &

Every minute it looks at the roots the catalog names. A part whose run is
still alive gets its after-launch record written while the process can be
observed. A part with exit 0, or the saved heights of a killed run that
another run has reopened, is sealed. When nothing waits to be sealed it runs
one missing set of refusal controls. When every part of an unbroken chain from
1 to 500000 is sealed it runs the join once.

It reads the recording folders and never signals, starts or changes a
recording. It writes only under the record root and the seal prefix. A step
that fails is tried three times, an hour apart, and stays reported as failed.

One JSON line of kind hub1_seal_watch goes to the agent container's stdout
every minute, which Loki collects, and the same object is kept in
seal-watch.status.json under the record root.
"""
import argparse
import datetime
import fcntl
import json
import os
from pathlib import Path
import sys
import threading
import time
import traceback

from hub1_finish import TOOL, background, join, make_plan, prepare, running, seal_part, seal_path
from seal_closed import clean_at, git

KIND = 'hub1_seal_watch'
ATTEMPTS, RETRY_SECONDS = 3, 3600


def now():
    return datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')


class Status:
    def __init__(self, root, quiet):
        self.root, self.quiet, self.lock = root, quiet, threading.Lock()
        self.row = dict(kind=KIND, generation=root.name, parts=0, sealed=0, controls_made=0, seal_failed=0, join_failed=0, joined=0, plan_error=0,
                        chain_complete=0, action='starting', action_since=now(), failures={}, started_utc=now(), pid=os.getpid())

    def set(self, **changes):
        with self.lock:
            if 'action' in changes and changes['action'] != self.row['action']:
                changes['action_since'] = now()
            self.row.update(changes)

    def emit(self):
        with self.lock:
            row = dict(self.row, at=now())
        text = json.dumps(row, sort_keys=True)
        partial = self.root / 'seal-watch.status.json.partial'
        partial.write_text(text + '\n')
        os.replace(partial, self.root / 'seal-watch.status.json')
        # Only the numbers go to Loki. Failure text can quote private paths and stays in the status file.
        line = json.dumps({k: v for k, v in row.items() if k != 'failures'}, sort_keys=True) + '\n'
        if self.quiet:
            sys.stdout.write(line)
            sys.stdout.flush()
        else:
            with open('/proc/1/fd/1', 'a') as out:
                out.write(line)

    def beat(self):
        while True:
            try:
                self.emit()
            except Exception:
                traceback.print_exc()
            time.sleep(60)


def controls_made(plan, part):
    if part.get('controls'):
        return Path(part['controls']).exists()
    out = Path(plan['record_root']) / ('part-%d' % part['first'])
    return any((f / 'actual-negative-controls.private.json').exists() for f in [out / 'controls'] + [out / ('controls-%d' % n) for n in range(2, 20)])


def attempt(status, tried, key, action, step):
    """Run one step. Returns True when it was run, whatever the outcome."""
    count, last = tried.get(key, (0, 0))
    if count >= ATTEMPTS or time.time() - last < RETRY_SECONDS:
        return False
    status.set(action=action)
    print(json.dumps(dict(at=now(), step=action, attempt=count + 1)), flush=True)
    try:
        step()
        tried.pop(key, None)
        with status.lock:
            status.row['failures'].pop(key, None)
    except BaseException as error:
        if isinstance(error, KeyboardInterrupt):
            raise
        traceback.print_exc()
        sys.stderr.flush()
        tried[key] = (count + 1, time.time())
        with status.lock:
            status.row['failures'][key] = dict(attempts=count + 1, error=type(error).__name__, reason=str(error)[:300], at=now())
    return True


def watch(catalog_path, once=False, quiet=False):
    catalog = json.loads(catalog_path.read_bytes())
    root = Path(catalog['record_root'])
    root.mkdir(mode=0o700, exist_ok=True)
    background(root / 'tmp')
    held = open(root / 'seal-watch.lock', 'a')
    try:
        fcntl.flock(held, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        sys.exit('another seal watcher holds ' + str(root / 'seal-watch.lock'))
    clean_at(TOOL, git(TOOL, 'rev-parse', 'HEAD').strip(), 'sealing tool tree is not clean')
    status, tried = Status(root, quiet), {}
    if not once:
        threading.Thread(target=status.beat, daemon=True).start()
    while True:
        did = False
        try:
            plan = make_plan(json.loads(catalog_path.read_bytes()))
            partial = root / 'seal-watch.plan.json.partial'
            partial.write_text(json.dumps(plan, indent=1))
            os.replace(partial, root / 'seal-watch.plan.json')
            parts = plan['parts']
            status.set(plan_error=0, parts=len(parts), chain_complete=int(plan['complete']), covered_through=plan['covered_through'])
            live = running()
            for part in parts:
                recorded = (root / ('part-%d' % part['first']) / 'process.private.json').exists()
                if not did and part['state'] == 'recording' and not recorded and str(Path(part['proof']) / 'complete-component.private.json') in live:
                    did = attempt(status, tried, 'record-%d' % part['first'], 'writing the launch record of part %d' % part['first'], lambda: prepare(plan, part, False))
            for part in parts:
                if not did and part['state'] in ('finished', 'savepoint') and not seal_path(plan, part).exists():
                    did = attempt(status, tried, 'seal-%d' % part['first'], 'sealing part %d to %d' % (part['first'], part['through']), lambda: seal_part(plan, part))
            sealed = [p for p in parts if seal_path(plan, p).exists()]
            joined = (root / 'whole-join.private.json').exists()
            if not did and not joined and plan['complete'] and len(sealed) == len(parts):
                did = attempt(status, tried, 'join', 'joining %d sealed parts' % len(parts), lambda: join(plan))
                joined = (root / 'whole-join.private.json').exists()
            for part in parts:
                if not did and not joined and not controls_made(plan, part) and (root / ('part-%d' % part['first']) / 'process.private.json').exists():
                    did = attempt(status, tried, 'controls-%d' % part['first'], 'running the refusal controls of part %d' % part['first'], lambda: prepare(plan, part, True))
            with status.lock:
                failures = status.row['failures']
                seal_failed = sum(1 for k in failures if not k.startswith('join'))
                join_failed = int('join' in failures)
            status.set(sealed=len([p for p in parts if seal_path(plan, p).exists()]), controls_made=sum(1 for p in parts if controls_made(plan, p)),
                       seal_failed=seal_failed, join_failed=join_failed, joined=int(joined),
                       action='joined: nothing left to do' if joined else 'waiting for a part to finish')
        except BaseException as error:
            if isinstance(error, (KeyboardInterrupt, SystemExit)):
                raise
            traceback.print_exc()
            sys.stderr.flush()
            status.set(plan_error=1, action='cannot read the roots: ' + type(error).__name__)
        if once:
            status.emit()
            return 0
        if not did:
            time.sleep(60)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--catalog', type=Path, required=True)
    parser.add_argument('--once', action='store_true', help='one pass, at most one step, status line to this terminal')
    a = parser.parse_args()
    sys.exit(watch(a.catalog.resolve(), a.once, a.once))
