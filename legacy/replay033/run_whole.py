#!/usr/bin/env python3
"""Sole local supervisor for exactly three approved private original profiles."""
import argparse
import datetime
import fcntl
import hashlib
import json
import os
import re
from pathlib import Path
import subprocess
import sys
sys.dont_write_bytecode = True
import time
from capture_bounded import PROFILES
from source_reader import Reader
from consume_bounded import sha
from reconcile_whole import verify as reconcile
from verify_movement_rehearsal import require


def now():return datetime.datetime.now(datetime.timezone.utc).isoformat()

def source_ready(reader, first):
    target=PROFILES[first][0]
    root=f'/recordings/range-{first}-{target}'
    status=json.loads(reader.command(['dd','if='+root+'/home/status.json','bs=65536','count=1']))
    require(status['chain']=='cosmoshub-1' and status['first_height_this_run']==first and status['target']==target and status['heights_where_results_differ_from_cache']==0,'original financial recorder identity/results changed')
    reader.current_source_status=status
    require(status.get('stop_reason') in (None,'','reached target; verified by unexecuted next header'),'original recorder stopped unsuccessfully, not healthy source waiting')
    return status.get('stop_reason')=='reached target; verified by unexecuted next header' and status['last_height_committed']==status['state_hash_verified_through']==target


def probe_source(reader, first):
    root=f'/recordings/range-{first}-{PROFILES[first][0]}'
    try:
        require(not reader.exists(root+'/failed.private'),'original recorder shell failed, not healthy source waiting')
        ready=source_ready(reader,first)
        s=reader.current_source_status
        result=dict(ready=False,original_source_last_committed=s['last_height_committed'],original_source_native_verified_through=s['state_hash_verified_through'],stage='waiting_for_real_original_source_closure')
        if ready:
            result['stage']='waiting_for_original_shell_integrity_seal'
            if reader.exists(root+'/recording-complete.private'):
                stamp=reader.command(['dd','if='+root+'/recording-complete.private','bs=65536','count=1'])
                if not stamp.strip():return result
                sizes=[int(reader.command(['stat','-c','%s',root+'/'+name+'.ndjson.gz'])) for name in ('movements','native-history')]
                require(all(size>0 for size in sizes),'empty completed original stream')
                result.update(ready=True,storage_reservation_bytes=2*sum(sizes)+(75<<30))
        return result
    except (subprocess.CalledProcessError,subprocess.TimeoutExpired,OSError) as error:
        return dict(ready=None,stage='source_read_unavailable_not_healthy',read_error_class=type(error).__name__)


def alive_profile(pid, first, attempt, repo):
    if type(pid) is not int or pid <= 0:return False
    try:
        args=Path('/proc/'+str(pid)+'/cmdline').read_bytes().split(b'\0')
    except FileNotFoundError:
        return False
    expected=[str(repo/'legacy/replay033/run_closed_profile.py'),'--first',str(first),'--directory',str(attempt),'--repo',str(repo)]
    words=[a.decode() for a in args if a]
    return len(words)>=len(expected) and words[-len(expected):]==expected


def write_state(path,state):
    temp=path.with_suffix('.tmp')
    with temp.open('w') as out:json.dump(state,out,indent=2,sort_keys=True)
    os.replace(temp,path)


def record_exit(directory,code,**extra):
    result=dict(exit=code,supervisor_pid=os.getpid(),finished_utc=now(),**extra)
    with (directory/f'controller-exit-{os.getpid()}-{time.time_ns()}.private.json').open('x') as out:json.dump(result,out,indent=2,sort_keys=True)
    write_state(directory/'exit.private.json',result)


def config_digest(config):return hashlib.sha256(json.dumps(config,sort_keys=True).encode()).hexdigest()

def save_config(directory,config):
    path=directory/('whole-config-'+config_digest(config)+'.private.json')
    if path.exists():require(json.loads(path.read_bytes())==config,'existing immutable whole config differs')
    else:
        with path.open('x') as out:json.dump(config,out,indent=2,sort_keys=True)
    return path


def adopt_completed(directory,repo,boundary,terminal):
    path=directory/'whole-reconciliation.private.json'
    if not path.exists():return False
    result=json.loads(path.read_bytes())
    configs=[]
    for candidate in directory.glob('whole-config*.private.json'):
        config=json.loads(candidate.read_bytes())
        if config_digest(config)==result['input_config_sha256']:configs.append(config)
    require(len(configs)==1,'actual completed whole result must bind one immutable config')
    config=configs[0]
    require(config['canonical_boundary']==str(boundary) and config['terminal_seal']==str(terminal),'restart arguments differ from actual completed whole source')
    require(reconcile(config,repo)==result,'actual completed whole result changed or fails independent readback')
    return True


def remaining_reservation(item,attempt):
    if item['stage']=='complete':return 0
    allocated=0
    for name in ('movements.source-closed.gz','native-history.source-closed.gz','cache-source.private.tar','opening-checkpoint.private.tar'):
        path=attempt/'source'/name
        if path.is_file() and not path.is_symlink():allocated+=path.stat().st_blocks*512
    return max(0,item.get('storage_reservation_bytes',0)-allocated)


def proof_progress(attempt):
    log=attempt/'proof/native-consumer.private.log'
    if not log.is_file() or log.is_symlink():return {}
    with log.open('rb') as stream:
        stream.seek(max(0,log.stat().st_size-4096))
        text=stream.read(4096).decode(errors='strict')
    rows=re.findall(r'^bounded_native_verified_height=(\d+) typed_phases=(\d+)$',text,re.M)
    if not rows:return {}
    return dict(native_verified_through=int(rows[-1][0]),typed_phases_checked=int(rows[-1][1]))


def run(directory, repo, boundary, terminal):
    directory,repo,boundary,terminal=[p.resolve() for p in (directory,repo,boundary,terminal)]
    owner=Path('/Users/danb/.cache/cosmos-tax-hub1-whole-consumer-owner.lock').open('a')
    fcntl.flock(owner,fcntl.LOCK_EX|fcntl.LOCK_NB)
    directory.mkdir(mode=0o700,exist_ok=True)
    lock=(directory/'controller.lock').open('a')
    fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
    require(subprocess.check_output(['git','-C',str(repo),'status','--porcelain'],text=True)=='','frozen signed whole-consumer source must remain clean')
    commit=subprocess.check_output(['git','-C',str(repo),'rev-parse','HEAD'],text=True).strip()
    subprocess.run(['git','-C',str(repo),'verify-commit',commit],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,check=True)
    if adopt_completed(directory,repo,boundary,terminal):return 0
    state=dict(schema='native033-whole-consumer-supervisor-v1',supervisor_pid=os.getpid(),signed_source_commit=commit,started_utc=now(),updated_utc=now(),profiles={},financial_admission=False,all500015_financial_books_tax_acceptance=False)
    owner.write(json.dumps(dict(supervisor_pid=os.getpid(),directory=str(directory),source_commit=commit,started_utc=state['started_utc']))+'\n');owner.flush()
    children={}
    reader=Reader()
    previous_path=directory/'state.private.json'
    previous=json.loads(previous_path.read_bytes()) if previous_path.exists() else None
    if previous:
        require(previous['signed_source_commit']==commit and set(previous['profiles'])=={str(first) for first in PROFILES},'existing controller source/profile ownership differs')
    for first in PROFILES:
        attempt=directory/('profile-'+str(first))
        item=dict(first=first,target=PROFILES[first][0],stage='waiting_for_real_original_source_closure')
        if attempt.exists():
            require(previous is not None,'unknown existing private attempt, refusing duplicate execution')
            item=dict(previous['profiles'][str(first)])
            require(item['first']==first and item['target']==PROFILES[first][0],'existing profile scope differs')
            exit_path=attempt/'exit.private.json'
            if exit_path.exists():
                exited=json.loads(exit_path.read_bytes())
                require(exited['exit']==0 and (attempt/'profile-seal.private.json').is_file(),'existing actual failed profile retained, diagnose instead of blind rerun')
                item.update(stage='complete',exit=0,seal_path=str(attempt/'profile-seal.private.json'),seal_sha256=sha(attempt/'profile-seal.private.json'),finished_utc=exited['finished_utc'])
            else:
                start=json.loads((attempt/'process.private.json').read_bytes())
                pid=start['supervisor_pid']
                require(start['storage_reservation_bytes'] >= (75<<30),'live adopted profile lacks actual run-start workspace reservation')
                item['storage_reservation_bytes']=start['storage_reservation_bytes']
                require(start['signed_source_commit']==commit and alive_profile(pid,first,attempt,repo),'existing attempt has no actual exit and no live owned supervisor, diagnose retained child/process receipts')
                children[first]=dict(process=None,log=None,pid=pid)
                item.update(stage='adopted_live_private_full_profile_proof',pid=pid)
        state['profiles'][str(first)]=item
    try:
        while True:
            require(subprocess.check_output(['git','-C',str(repo),'rev-parse','HEAD'],text=True).strip()==commit,'frozen whole consumer source changed')
            state['source_observation_complete']=True
            for first in PROFILES:
                item=state['profiles'][str(first)]
                if item['stage']=='complete':continue
                attempt=directory/('profile-'+str(first))
                if first in children:
                    owned=children[first]
                    code=owned['process'].poll() if owned['process'] is not None else None
                    if code is None and (owned['process'] is not None or not (attempt/'exit.private.json').exists()):
                        if owned['process'] is None:require(alive_profile(owned['pid'],first,attempt,repo),'owned profile disappeared without actual exit, diagnose retained receipts')
                        item['stage']='running_private_full_profile_proof'
                        item.update(proof_progress(attempt))
                        continue
                    if owned['log'] is not None:owned['log'].close()
                    exit_state=json.loads((attempt/'exit.private.json').read_bytes())
                    require((code is None or code==0) and exit_state['exit']==0,'actual complete profile failed, attempt retained; do not blindly rerun')
                    seal=attempt/'profile-seal.private.json'
                    require(seal.is_file(),'successful private profile lacks actual seal')
                    item.update(stage='complete',exit=0,seal_path=str(seal),seal_sha256=sha(seal),finished_utc=exit_state['finished_utc'])
                    continue
                probe=probe_source(reader,first)
                item.update(probe)
                if probe['ready'] is None:
                    item['consecutive_read_errors']=item.get('consecutive_read_errors',0)+1
                    state['source_observation_complete']=False
                    continue
                item['consecutive_read_errors']=0
                if probe['ready']:
                    other=sum(remaining_reservation(state['profiles'][str(k)],directory/('profile-'+str(k))) for k in children if k!=first)
                    fs=os.statvfs(directory);free=fs.f_bavail*fs.f_frsize
                    item.update(other_future_reservations_bytes=other,observed_workspace_free_bytes=free)
                    if free<other+item['storage_reservation_bytes']:
                        item['stage']='waiting_for_reserved_private_workspace_headroom'
                        continue
                    log_path=directory/f'profile-{first}-supervisor-{time.time_ns()}.private.log'
                    log=log_path.open('xb')
                    child=subprocess.Popen([sys.executable,'-B',str(repo/'legacy/replay033/run_closed_profile.py'),'--first',str(first),'--directory',str(attempt),'--repo',str(repo)],stdout=log,stderr=subprocess.STDOUT,env=dict(os.environ,GOMAXPROCS='2',GOMEMLIMIT='512MiB',HUB1_OTHER_RESERVED_BYTES=str(other),HUB1_STORAGE_RESERVATION_BYTES=str(item['storage_reservation_bytes'])))
                    children[first]=dict(process=child,log=log,pid=child.pid)
                    item.update(stage='starting_private_full_profile_proof',pid=child.pid,supervisor_log_path=str(log_path),started_utc=now())
                    write_state(directory/'state.private.json',state)
            state['updated_utc']=now()
            write_state(directory/'state.private.json',state)
            if all(p['stage']=='complete' for p in state['profiles'].values()):break
            time.sleep(30)
        config=dict(profiles=[dict(path=state['profiles'][str(first)]['seal_path'],sha256=state['profiles'][str(first)]['seal_sha256']) for first in sorted(PROFILES)],terminal_seal=str(terminal),canonical_boundary=str(boundary))
        state['assembly_config_path']=str(save_config(directory,config))
        write_state(directory/'state.private.json',state)
        def assembly_progress(path,count,total):
            state.update(stage='verifying_all_full_source_artifact_pins_and_seams',updated_utc=now(),assembly_component_seal=path,assembly_refs_checked=count,assembly_component_refs_total=total)
            write_state(directory/'state.private.json',state)
        result=reconcile(config,repo,progress=assembly_progress)
        with (directory/'whole-reconciliation.private.json').open('x') as out:json.dump(result,out,indent=2,sort_keys=True)
        state.update(stage='all_historical_sources_and_coinflows_reconciled',updated_utc=now(),whole_result_sha256=sha(directory/'whole-reconciliation.private.json'))
        write_state(directory/'state.private.json',state)
        record_exit(directory,0)
        return 0
    except BaseException as error:
        state.update(stage='failed_attempts_and_live_children_retained',updated_utc=now(),error_class=type(error).__name__,live_child_pids=[owned['pid'] for first,owned in children.items() if alive_profile(owned['pid'],first,directory/('profile-'+str(first)),repo)])
        write_state(directory/'state.private.json',state)
        record_exit(directory,1,live_child_pids=state['live_child_pids'])
        raise

if __name__=='__main__':
    p=argparse.ArgumentParser()
    for name in ('directory','repo','canonical-boundary','terminal-seal'):
        p.add_argument('--'+name,type=Path,required=True)
    a=p.parse_args();os.umask(0o077)
    sys.exit(run(a.directory,a.repo,a.canonical_boundary,a.terminal_seal))
