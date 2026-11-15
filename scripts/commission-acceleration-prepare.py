#!/usr/bin/env python3
"""PREPARATION ONLY. Read metadata and emit unapplied disjoint worker manifests.

No apply, patch, signal, delete, SQL write, collector execution or automatic retry.
The parent must obtain explicit approval, freeze the existing process without
losing its heap, repeat metadata capture, review the complete root diff, then ship.
"""
import argparse
import copy
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess

SOURCE = '6754a0d7d7986e48e3e3467b2448aae84d2c1b92'
DIGEST = '3375126286bfa08649f4671328c61bb80fb595fe41d4d017e6bfe47b2e8eb42e'
VERSION = 'ops818-batched-metadata-full324-20261114'
JOB = 'cosmos-tax-sdk-commission-full324-batched-20261114'
CM = 'cosmos-tax-commission-full324-batched-watch-8dg49cmgbk'
BASE = ['kubectl', '--context', 'bryanlabs', '-n', 'apps']
PG = 'cosmos-tax-sdk-postgres-85c85559c-864tr'


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':')).encode()


def sha(value):
    return hashlib.sha256(canonical(value)).hexdigest()


def read_json(args, stdin=None):
    p = subprocess.run(args, input=stdin, text=True, capture_output=True, timeout=15)
    if p.returncode:
        raise RuntimeError('read-only metadata query failed; raw error withheld')
    return json.loads(p.stdout)


def partition(expected, rows, workers=2, terminal_unsaved=()):
    """Reserve a conservative superset of all in-flight roots of the old process.

    Proof assumptions are explicit: exactly two ordered, unbuffered workers;
    initially empty generation; every completed attempt has either an immutable
    row or a retained terminal log failure; no outside writer or changed
    membership. While frozen, the first two not-yet-attempted ordinals contain
    every unsaved in-flight root. Unassigned extras
    are harmlessly reserved for the existing process too. This is NOT a durable
    wallet queue checkpoint or permission to freeze that process.
    """
    if workers != 2 or len(expected) != 324 or len(set(expected)) != 324 or expected != sorted(expected):
        raise ValueError('wrong membership or original worker count')
    by_validator = {}
    for row in rows:
        v = row['validator']
        if v not in expected or v in by_validator:
            raise ValueError('foreign or duplicate result')
        if row['build'] != 'sha256:' + DIGEST or row['chain'] != 'mainnet':
            raise ValueError('wrong source')
        if row['from_day'] != '2024-03-21' or row['as_of'] != '2026-11-12':
            raise ValueError('wrong date window')
        if row['status'] not in ('ok', 'reconciled', 'drift', 'error'):
            raise ValueError('unknown immutable status')
        by_validator[v] = row
    if len(set(terminal_unsaved)) != len(terminal_unsaved) or any(v not in expected or v in by_validator for v in terminal_unsaved):
        raise ValueError('invalid unsaved terminal failure identity')
    missing = [v for v in expected if v not in by_validator and v not in terminal_unsaved]
    reserve = missing[:workers]
    available = missing[workers:]
    # The same fixed six-lane assignment supports a first four-worker Job and
    # a later two-worker Job. Their queues never overlap or change after review.
    first_four = [v for i,v in enumerate(available) if i % 6 < 4]
    later_two = [v for i,v in enumerate(available) if i % 6 >= 4]
    groups = [set(by_validator), set(terminal_unsaved), set(reserve), set(first_four), set(later_two)]
    if set.union(*groups) != set(expected):
        raise ValueError('incomplete union')
    for i, group in enumerate(groups):
        if any(group & other for other in groups[i+1:]):
            raise ValueError('overlapping queues')
    return {'stored':[v for v in expected if v in by_validator],
            'immutable_failed':[v for v in expected if v in by_validator and by_validator[v]['status']=='error'],
            'unsaved_terminal_failures':list(terminal_unsaved),
            'reserve_existing_two':reserve,'stage_four':first_four,'stage_later_two':later_two}


def make_job(original, validators, workers, suffix):
    if not validators or len(set(validators)) != len(validators):
        raise ValueError('empty or duplicate shard')
    spec = copy.deepcopy(original['spec'])
    for k in ('selector','manualSelector','suspend'):
        spec.pop(k, None)
    spec['backoffLimit'] = 0
    spec.pop('ttlSecondsAfterFinished', None)
    template = spec['template']
    labels = {k:v for k,v in template.get('metadata',{}).get('labels',{}).items()
              if k not in ('job-name','controller-uid') and not k.startswith('batch.kubernetes.io/')}
    labels['commission-stage'] = suffix
    template['metadata'] = {'labels':labels, 'annotations':{'bryanlabs.net/source':SOURCE}}
    container = template['spec']['containers'][0]
    if not container['image'].endswith('@sha256:' + DIGEST):
        raise ValueError('wrong immutable image')
    old_command = container['command']
    required = {'/usr/local/bin/taxledger','--commission-flows','--chain=mainnet',
                '--commission-flows-from=2024-03-21','--commission-as-of=2026-11-13',
                '--commission-workers=2','--commission-read-snapshot-timeout=60s',
                '--commission-output-version='+VERSION,'--commission-build=sha256:'+DIGEST,
                '--commission-archive-config=/archive/probe.toml'}
    if set(old_command) != required or len(old_command) != len(required):
        raise ValueError('wrong original command')
    if any(x.startswith('--commission-validators=') for x in old_command):
        raise ValueError('unexpected existing filter')
    container['command'] = [('--commission-workers=' + str(workers)) if x=='--commission-workers=2' else x for x in old_command]
    container['command'].append('--commission-validators=' + ','.join(validators))
    name = 'cosmos-tax-commission-accel-' + suffix + '-20261114'
    for env in container.get('env',[]):
        if env['name']=='PGAPPNAME':
            env['value'] = name
    # Preserve reviewed per-container cgroup and runtime settings verbatim.
    if container['resources'].get('limits') != {'cpu':'4','memory':'8Gi'}:
        raise ValueError('unexpected cgroup profile')
    spec['parallelism'] = 1
    spec['completions'] = 1
    return {'apiVersion':'batch/v1','kind':'Job','metadata':{'name':name,'namespace':'apps',
            'annotations':{'bryanlabs.net/preparation-only':'requires explicit approval and fresh root review',
                           'bryanlabs.net/source':SOURCE}},'spec':spec}


def private(path, value):
    fd = os.open(path, os.O_WRONLY|os.O_CREAT|os.O_EXCL, 0o600)
    with os.fdopen(fd,'w') as f:
        json.dump(value,f,indent=2)
        f.write('\n')


def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('--output',required=True)
    args=parser.parse_args()
    os.umask(0o077)
    out=Path(args.output)
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    original=read_json(BASE+['get','job',JOB,'-o','json'])
    cm=read_json(BASE+['get','configmap',CM,'-o','json'])
    expected=re.findall(r"\('(cosmosvaloper1[a-z0-9]+)'\)",cm['data']['counts.sql'])
    sql="""\\set ON_ERROR_STOP on
BEGIN READ ONLY;
SET LOCAL statement_timeout='5s'; SET LOCAL lock_timeout='500ms'; SET LOCAL work_mem='1MB'; SET LOCAL TIME ZONE 'UTC';
SELECT coalesce(json_agg(s),'[]') FROM (
SELECT version,chain,validator,build,computed_at,xmin::text,
(run_json->>'FromDay')::timestamptz::date::text AS from_day,
(run_json->>'AsOf')::timestamptz::date::text AS as_of,
run_json->>'Status' AS status FROM tax_commission_flow_versions
WHERE version='ops818-batched-metadata-full324-20261114' ORDER BY validator) s;
COMMIT;
"""
    rows=read_json(BASE+['exec','-i',PG,'--','psql','-X','-qAt','-U','postgres','-d','cosmostax'],sql)
    logs=subprocess.run(BASE+['logs','job/'+JOB,'-c','flows','--limit-bytes=8388608'],capture_output=True,text=True,timeout=20,check=True).stdout
    succeeded=set(re.findall(r'commission flows: (cosmosvaloper1[a-z0-9]+) (?:ok|reconciled|drift):',logs))
    failed=set(re.findall(r'commission flows: (cosmosvaloper1[a-z0-9]+):',logs))
    stored={r['validator'] for r in rows}
    if not succeeded <= stored or stored != ((succeeded | failed) & stored):
        raise RuntimeError('log/row mismatch or snapshot race; refresh after approved freeze')
    unsaved=sorted(failed-stored)
    parts=partition(expected,rows,terminal_unsaved=unsaved)
    private(out/'original-job.private.json',original)
    fd=os.open(out/'original-logs.private',os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    with os.fdopen(fd,'w') as f:
        f.write(logs)
    private(out/'membership-and-partition.private.json',{'expected':expected,'rows':rows,'partition':parts})
    private(out/'stage-four.job.private.json',make_job(original,parts['stage_four'],4,'four'))
    private(out/'stage-later-two.job.private.json',make_job(original,parts['stage_later_two'],2,'later-two'))
    safe={'at':dt.datetime.now(dt.timezone.utc).isoformat(),'preparation_only':True,
          'not_frozen':True,'not_execution_ready':True,'source':SOURCE,'image_digest':DIGEST,'version':VERSION,
          'original_job_uid':original['metadata']['uid'],'original_spec_sha256':sha(original['spec']),
          'membership_sha256':sha(expected),'membership':len(expected),
          'counts':{k:len(v) for k,v in parts.items()},
          'partition_sha256':sha(parts),'rows_sha256':sha(rows),
          'required_before_execution':['Main review of preservation-safe holds under standing authority; applicable explicit yes for additional production DB load',
              'freeze current corrected process without removing Pod or losing heap',
              'verify zero write transactions and safe read transaction release',
              'repeat this planner after freeze, source/template/UID and log-row agreement',
              'preserve original output subset xmin and entire original logs',
              'fresh complete root diff review and normal cluster-apply ship',
              'guard monitoring live and tested rollback on exact new process PIDs'],
          'failed_row_policy':'Retain stored error and unsaved native failure. A stored error cannot become success in this version. A new output version does NOT fix the JSONB-size limit. Resolve readiness and lossless evidence storage before retry; no source-row repair is authorized.',
          'coverage':'stored + unsaved terminal failures + reserved original roots + disjoint four-worker and later-two queues = 324; execution only, never accounting acceptance'}
    private(out/'preparation.safe.json',safe)
    print(json.dumps(safe,indent=2))


if __name__=='__main__':
    main()
