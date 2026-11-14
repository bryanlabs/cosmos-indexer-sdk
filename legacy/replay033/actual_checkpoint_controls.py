#!/usr/bin/env python3
"""New actual normal-checkpoint controls, never execute historical blocks."""
import argparse
import gzip
import json
import os
from pathlib import Path
import subprocess
from consume_bounded import Consumer, HistoryWindow, selected_lines, sha
from verify_movement_rehearsal import require

class FixtureDone(Exception):
    pass

def fixture(root, output):
    binding = json.loads((root / 'capture.private.json').read_bytes())
    require(binding['first'] in (200001, 400001), 'actual checkpoint profile required')
    if binding['schema'] == 'native033-closed-source-capture-v1':
        from consume_closed import ClosedHistory, closed_lines
        history = ClosedHistory(root, binding)
        records = closed_lines(root, binding, 'movements')
    else:
        history = HistoryWindow(root, binding)
        records = selected_lines(root, binding, 'movements')
    with gzip.open(output, 'xb') as stream:
        def send(q):
            stream.write((json.dumps(q, separators=(',', ':')) + '\n').encode())
            if q['kind'] == 'commit':
                raise FixtureDone()
        send(dict(kind='anchor', anchor=history.anchor, through=binding['through']))
        consumer = Consumer(send, history, binding['through'], binding['first'])
        try:
            for r in records:
                consumer.accept(r)
        except FixtureDone:
            return binding['first']
    raise ValueError('complete actual checkpoint first height absent')

def control(root, source, directory, binary, name):
    cases = dict(opening_root='normal native application commitment differs',
                 opening_store='complete raw store does not reproduce native IAVL commitment',
                 opening_baseline='normal opening complete raw baseline differs from native checkpoint',
                 before='native before-image bytes differ',
                 final_root='complete raw store does not reproduce native IAVL commitment',
                 final_result='complete native ABCI result differs',
                 next_header='independent next-header root/result binding differs')
    changed = False
    logfile = directory / (name + '.refused.private.log')
    receipt = directory / (name + '.must-not-exist.json')
    command = [str(binary), '--cache', str(root / 'cache'), '--checkpoint-home', str(root / 'opening-checkpoint'), '--working-home', str(directory / (name + '-home')), '--receipt', str(receipt)]
    with logfile.open('xb') as log:
        p = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=log, stderr=subprocess.STDOUT, env=dict(os.environ, GOMAXPROCS='2', GOMEMLIMIT='512MiB'))
        try:
            with gzip.open(source, 'rb') as stream:
                for raw in stream:
                    q = json.loads(raw)
                    if not changed:
                        if q['kind'] == 'anchor' and name == 'opening_root':
                            q['anchor']['commit']['Hash'] = 'AQIDBA=='
                            changed = True
                        elif q['kind'] == 'anchor' and name == 'opening_store':
                            q['anchor']['native_commit_info']['StoreInfos'][0]['Core']['CommitID']['Hash'] = 'AQIDBA=='
                            changed = True
                        elif q['kind'] == 'phase' and name in ('opening_baseline', 'before'):
                            for pairs in q['restore'].values():
                                if pairs:
                                    pairs[0]['value' if name == 'opening_baseline' else 'before'] = 'AQIDBA=='
                                    changed = True
                                    break
                        elif q['kind'] == 'commit':
                            if name == 'final_root':
                                q['history']['native_commit_info']['StoreInfos'][0]['Core']['CommitID']['Hash'] = 'AQIDBA=='
                                changed = True
                            elif name == 'final_result':
                                q['history']['begin'] = dict(q['history']['begin'], tags=[dict(key='AQ==', value='Ag==')])
                                changed = True
                            elif name == 'next_header':
                                q['next_header']['app_hash'] = '01020304'
                                changed = True
                    p.stdin.write((json.dumps(q, separators=(',', ':')) + '\n').encode())
                    if changed:
                        break
        except BrokenPipeError:
            pass
        finally:
            try:
                p.stdin.close()
            except BrokenPipeError:
                pass
        code = p.wait(timeout=180)
    require(changed and code != 0 and cases[name] in logfile.read_text() and not receipt.exists(), 'specific actual checkpoint refusal missing: ' + name)
    return dict(control=name, exit=code, expected_refusal=True, log_sha256=sha(logfile))

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--capture', type=Path, required=True)
    parser.add_argument('--directory', type=Path, required=True)
    parser.add_argument('--native-helper', type=Path, required=True)
    a = parser.parse_args()
    os.umask(0o077)
    a.directory.mkdir(mode=0o700)
    source = a.directory / 'actual-first-complete-height.requests.gz'
    first = fixture(a.capture, source)
    results = []
    for name in ('opening_root', 'opening_store', 'opening_baseline', 'before', 'final_root', 'final_result', 'next_header'):
        result = control(a.capture, source, a.directory, a.native_helper, name)
        result['retained_log_path'] = str(a.directory / (name + '.refused.private.log'))
        results.append(result)
        (a.directory / (name + '.completed.private.json')).write_text(json.dumps(result))
    result = dict(schema='native033-actual-checkpoint-controls-v1', first=first, source_capture_sha256=sha(a.capture / 'capture.private.json'), fixture_sha256=sha(source), native_helper_sha256=sha(a.native_helper), controls=results, historical_blocks_executed=0, financial_admission=False)
    with (a.directory / 'actual-negative-controls.private.json').open('x') as stream:
        json.dump(result, stream, indent=2, sort_keys=True)
    print(json.dumps(dict(first=first, actual_checkpoint_controls_refused=len(results))), flush=True)

if __name__ == '__main__':
    main()
