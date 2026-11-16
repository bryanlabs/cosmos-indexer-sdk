#!/usr/bin/env python3
"""One supervised private complete-profile capture/proof/control/seal, no polling."""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
sys.dont_write_bytecode = True
import time
from capture_closed import capture
from actual_full_controls import run as controls
from verify_source_preservation import verify as preserve
from seal_closed import CODE, seal
from verify_movement_rehearsal import require


def run(first, directory, repo):
    directory,repo=directory.resolve(),repo.resolve()
    # capture() refuses an unfinished original before making its destination.
    # This command never launches, changes, pauses, or retries original Jobs.
    require(subprocess.check_output(['git','-C',str(repo),'status','--porcelain'],text=True)=='','signed clean consumer source required')
    commit=subprocess.check_output(['git','-C',str(repo),'rev-parse','HEAD'],text=True).strip()
    subprocess.run(['git','-C',str(repo),'verify-commit',commit],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,check=True)
    directory.mkdir(mode=0o700)
    start=time.monotonic()
    state=dict(supervisor_pid=os.getpid(),first=first,storage_reservation_bytes=int(os.environ.get('HUB1_STORAGE_RESERVATION_BYTES','0')),signed_source_commit=commit,python_source_pins={name:hashlib.sha256((repo/name).read_bytes()).hexdigest() for name in CODE if name.endswith('.py')},started_utc=datetime.datetime.now(datetime.timezone.utc).isoformat())
    (directory/'process.private.json').write_text(json.dumps(state))
    code=1
    try:
        capture(directory/'source',first,int(os.environ.get('HUB1_OTHER_RESERVED_BYTES','0')))
        source=repo/'legacy/replay033'
        tmp=directory/'tmp';tmp.mkdir(mode=0o700)
        env=dict(os.environ,TMPDIR=str(tmp),GOTMPDIR=str(tmp),GOMAXPROCS='2',GOMEMLIMIT='512MiB')
        helper=directory/'consume-native'
        with (directory/'build-test.private.log').open('xb') as log:
            subprocess.run(['go','test','./cmd/consume-native'],cwd=source,env=env,stdout=log,stderr=subprocess.STDOUT,check=True)
            subprocess.run([sys.executable,'-B','-m','unittest','test_consume_closed','test_closed_lifecycle','test_consume_bounded.Scalars','test_consume_bounded.PhaseOwners'],cwd=source,env=env,stdout=log,stderr=subprocess.STDOUT,check=True)
            subprocess.run(['go','build','-o',str(helper),'./cmd/consume-native'],cwd=source,env=env,stdout=log,stderr=subprocess.STDOUT,check=True)
        compiled={}
        listing=subprocess.check_output(['go','list','-deps','-f','{{.Dir}} {{join .GoFiles " "}}','./cmd/consume-native'],cwd=source,env=env,text=True)
        for line in listing.splitlines():
            parts=line.split()
            for name in parts[1:]:
                path=Path(parts[0])/name
                compiled[str(path)]=hashlib.sha256(path.read_bytes()).hexdigest()
        (directory/'compiled-go-source-pins.private.json').write_text(json.dumps(compiled,indent=2,sort_keys=True))
        proof=directory/'proof';proof.mkdir(mode=0o700)
        proof_start=time.monotonic()
        with (proof/'consumer.private.log').open('xb') as log:
            p=subprocess.Popen([sys.executable,'-B',str(source/'consume_closed.py'),'--directory',str(directory/'source'),'--native-helper',str(helper),'--receipt',str(proof/'complete-component.private.json')],env=env,stdout=log,stderr=subprocess.STDOUT)
            (proof/'process.private.json').write_text(json.dumps(dict(supervisor_pid=os.getpid(),consumer_pid=p.pid,started_utc=datetime.datetime.now(datetime.timezone.utc).isoformat())))
            exit_code=p.wait()
        (proof/'exit.private.json').write_text(json.dumps(dict(exit=exit_code,elapsed_seconds=time.monotonic()-proof_start,finished_utc=datetime.datetime.now(datetime.timezone.utc).isoformat())))
        require(exit_code==0,'complete independent profile financial proof failed, original and attempt retained')
        controls(directory/'source',directory/'controls',helper)
        preserve(directory/'source',directory/'source-preservation.private.json')
        # All source/helper/closure receipts and compiled source pins remain in
        # the same exclusive attempt. No cleanup follows success or failure.
        seal(directory/'source',proof,helper,directory/'controls/actual-negative-controls.private.json',directory/'source-preservation.private.json',repo,directory/'profile-seal.private.json',directory/'process.private.json')
        code=0
    finally:
        (directory/'exit.private.json').write_text(json.dumps(dict(exit=code,elapsed_seconds=time.monotonic()-start,finished_utc=datetime.datetime.now(datetime.timezone.utc).isoformat())))
    return code

if __name__=='__main__':
    p=argparse.ArgumentParser();p.add_argument('--first',type=int,choices=(1,200001,400001),required=True);p.add_argument('--directory',type=Path,required=True);p.add_argument('--repo',type=Path,required=True);a=p.parse_args()
    os.umask(0o077)
    sys.exit(run(a.first,a.directory,a.repo))
