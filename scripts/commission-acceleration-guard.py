#!/usr/bin/env python3
"""Actionless conservative acceleration gates. Reads receipts, never changes jobs.

Thresholds are proposed safety gates from review, not Dan/provider rate limits.
Any collector, source, or fingerprint failure is unknown/unsafe, never zero.
"""
import argparse
import datetime as dt
import json
import math
from pathlib import Path
import statistics

GIB = 1024 ** 3


def number(value):
    if isinstance(value, bool) or not isinstance(value, (int,float)) or not math.isfinite(value):
        raise ValueError('missing/nonfinite metric')
    return value


def stats(text):
    return {k:int(v) for k,v in (line.split() for line in text.splitlines())}


def public_limit(baseline):
    samples=baseline['public']['samples']
    if len(samples)<5 or any(s.get('http_status')!=200 for s in samples):
        raise ValueError('public baseline incomplete')
    values=sorted(number(s['elapsed_ms']) for s in samples)
    # Nearest-rank p95 is deliberately conservative for this small baseline.
    return max(250.0, 2 * values[math.ceil(.95*len(values))-1])


def assess(baseline, before, after, interval_seconds, now=None):
    failures=[]
    measured={}
    now=now or dt.datetime.now(dt.timezone.utc)
    def check(label,condition):
        if not condition:
            failures.append(label)
    try:
        interval=number(interval_seconds)
        check('CPU interval must be 5..30s',5 <= interval <= 30)
        for report in (baseline,before,after):
            check('collection incomplete',not report.get('errors'))
        stamp=dt.datetime.fromisoformat(after['at'])
        age=(now-stamp).total_seconds()
        check('stale or future receipt',-5 <= age <= 120)
        pg=after['pg_cgroup']
        memory=number(pg['anon_plus_shmem_bytes'])
        psi=number(pg['memory_full_avg60'])
        oom=number(pg['oom_kill'])
        check('PG anon+shmem >=12Gi',memory<12*GIB)
        check('PG memory PSI full avg60 >=2',psi<2)
        check('PG OOM kill nonzero',oom==0)
        check('wrong PG memory cgroup',int(pg['memory.max'])==24*GIB)
        check('wrong PG CPU cgroup',pg['cpu.max'].split()==['1600000','100000'])
        cpu1=stats(before['pg_cgroup']['cpu.stat'])['usage_usec']
        cpu2=stats(pg['cpu.stat'])['usage_usec']
        check('CPU counter reset',cpu2>=cpu1)
        cores=(cpu2-cpu1)/1e6/interval
        check('PG CPU >=14/16 cores',cores<14)
        measured.update(pg_cpu_cores=cores,pg_anon_plus_shmem_bytes=memory,pg_memory_full_avg60=psi,pg_oom_kill=oom)
        limit=public_limit(baseline)
        samples=after['public']['samples']
        check('public samples missing',len(samples)>=5)
        check('public HTTP error',all(s.get('http_status')==200 for s in samples))
        values=sorted(number(s['elapsed_ms']) for s in samples)
        p95=values[math.ceil(.95*len(values))-1]
        check('public latency regression',p95<=limit)
        check('public coverage changed',all(s.get('coverage')=={'traced':48,'expected':324,'complete':False} for s in samples))
        public_hashes={s['body_sha256'] for s in baseline['public']['samples']}
        check('public serving bytes changed',all(s.get('body_sha256') in public_hashes for s in samples))
        measured.update(public_p95_ms=p95,public_limit_ms=limit)
        check('blocked database queries',all(s['blocked']==0 for s in after['sessions']))
        original={(r['version'],r['key_hash']):r for r in baseline['output_row_identity']}
        current={(r['version'],r['key_hash']):r for r in after['output_row_identity']}
        check('duplicate result identities',len(current)==len(after['output_row_identity']))
        check('original immutable output changed/missing',all(current.get(k)==v for k,v in original.items()))
        for label in ('01bb','8704','6754'):
            old=baseline['job_'+label]
            new=after['job_'+label]
            check('original job UID/spec changed',old['uid']==new['uid'] and old['spec_sha256']==new['spec_sha256'])
            oldpods={(p['name'],p['uid']) for p in old['pods']}
            newpods={(p['name'],p['uid']) for p in new['pods']}
            check('original Pod removed/replaced',oldpods<=newpods)
            check('original native runtime absent',bool(new['pods']))
            for pod in new['pods']:
                check('original native runtime absent',pod.get('phase') in ('Running','Succeeded') and bool(pod.get('states')))
                check('original native container restart',all(s.get('restartCount')==0 for s in pod.get('states',[])))
        for label in ('01bb','8704','6754'):
            old_failure=baseline['job_'+label]['native'].get('terminal_failures')
            new_failure=after['job_'+label]['native'].get('terminal_failures')
            check('native failure capture missing',old_failure is not None and new_failure is not None)
            check('new original-run failure',old_failure==new_failure)
        for stage in after.get('expected_stage_jobs',[]):
            native=after['job_'+stage]
            check('stage runtime absent/not running',bool(native['pods']) and all(p.get('phase') in ('Running','Succeeded') for p in native['pods']))
            check('stage job source changed',all(p['states'] and all(s.get('imageID','').endswith('sha256:3375126286bfa08649f4671328c61bb80fb595fe41d4d017e6bfe47b2e8eb42e') and s.get('restartCount')==0 for s in p['states']) for p in native['pods']))
            check('stage native failed',not native['native']['terminal_failures'] and not native['status'].get('failed'))
        # Whole financial source checksums are intentionally not claimed here.
    except (KeyError,ValueError,TypeError,ZeroDivisionError,IndexError):
        failures.append('missing/invalid evidence')
    return {'safe_for_next_reviewed_stage':not failures,'resource_guard_only':True,'execution_authorized':False,'actions_performed':[],
            'proposed_review_gates_not_provider_limits':True,'measured':measured,'failures':sorted(set(failures)),
            'does_not_prove':['cold cache','four/six concurrent hard reads','accounting acceptance','whole source-table preservation']}


def main():
    p=argparse.ArgumentParser()
    p.add_argument('--baseline',required=True)
    p.add_argument('--before',required=True)
    p.add_argument('--after',required=True)
    p.add_argument('--interval-seconds',required=True,type=float)
    a=p.parse_args()
    result=assess(*(json.loads(Path(x).read_text()) for x in (a.baseline,a.before,a.after)),a.interval_seconds)
    print(json.dumps(result,indent=2))
    raise SystemExit(0 if result['safe_for_next_reviewed_stage'] else 2)


if __name__=='__main__':
    main()
