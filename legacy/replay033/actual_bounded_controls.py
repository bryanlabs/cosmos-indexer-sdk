#!/usr/bin/env python3
"""Actual-data consumer mutations. No native block or payout keeper execution."""
import argparse
import copy
import gzip
import hashlib
import json
import os
from pathlib import Path
import subprocess
from consume_bounded import Consumer, selected_lines, sha
from verify_movement_rehearsal import require

class FixtureDone(Exception):
    pass

def build_fixture(root, output):
    binding = json.loads((root / 'capture.private.json').read_bytes())
    histories = list(selected_lines(root, binding, 'native-history'))
    history = {int(r['height']): r for r in histories if r['kind'] == 'height'}
    counts = dict(requests=0)
    with gzip.open(output, 'xb') as out:
        def send(value):
            out.write((json.dumps(value, separators=(',', ':')) + '\n').encode())
            counts['requests'] += 1
            if value['kind'] == 'phase' and any(r['kind'] == 'principal' for r in value['rows']):
                counts['through_actual_principal_phase'] = value['height']
                raise FixtureDone()
        send(dict(kind='anchor', anchor=histories[0], through=binding['through']))
        consumer = Consumer(send, history, binding['through'])
        try:
            for r in selected_lines(root, binding, 'movements'):
                consumer.accept(r)
        except FixtureDone:
            pass
    require('through_actual_principal_phase' in counts, 'actual native principal target absent')
    return counts

def control(fixture, root, directory, binary, name):
    reason = {'fee': 'all-account all-coin native bank effects unexplained',
              'share': 'native shares differ', 'principal': 'native principal differs',
              'request': 'native token request differs from original signed message',
              'root': 'complete raw store does not reproduce native IAVL commitment',
              'native_result': 'complete native ABCI result differs',
              'native_application_root': 'raw source journals do not reproduce native IAVL application commitment',
              'native_before': 'native before-image bytes differ'}[name]
    output = directory / (name + '.refused.private.log')
    changed = False
    with output.open('xb') as log:
        process = subprocess.Popen([str(binary), '--cache', str(root / 'cache'), '--receipt', str(directory / (name + '.must-not-exist.json'))], stdin=subprocess.PIPE, stdout=log, stderr=subprocess.STDOUT, env=dict(os.environ, GOMAXPROCS='2', GOMEMLIMIT='512MiB'))
        try:
            with gzip.open(fixture, 'rb') as stream:
                for raw in stream:
                    q = json.loads(raw)
                    if not changed:
                        if q['kind'] == 'phase' and name in ('fee', 'share', 'principal', 'request'):
                            target = {'fee': 'movement', 'share': 'staking_shares', 'principal': 'principal', 'request': 'principal_request'}[name]
                            for row in q['rows']:
                                if row['kind'] == target and (name != 'fee' or row['label'] == 'ante_fee'):
                                    row['amount'] = str(int(row['amount']) - 1) if name != 'share' else '999.000000000000000001'
                                    changed = True
                                    break
                        if q['kind'] == 'phase' and name == 'native_before':
                            for values in q['changes'].values():
                                for pair in values:
                                    if pair['before'] is not None:
                                        pair['before'] = 'AQIDBA=='
                                        changed = True
                                        break
                                if changed:
                                    break
                        if q['kind'] == 'commit':
                            if name == 'root':
                                q['history']['native_commit_info']['StoreInfos'][0]['Core']['CommitID']['Hash'] = 'AQIDBA=='
                                changed = True
                            if name == 'native_application_root':
                                q['history']['commit']['Hash'] = 'AQIDBA=='
                                changed = True
                            if name == 'native_result' and q['history']['deliver']:
                                q['history']['deliver'][0]['code'] = 999
                                changed = True
                    process.stdin.write((json.dumps(q, separators=(',', ':')) + '\n').encode())
                    if changed:
                        break
        except BrokenPipeError:
            pass
        finally:
            try:
                process.stdin.close()
            except BrokenPipeError:
                pass
        code = process.wait(timeout=120)
    text = output.read_text()
    require(changed and code != 0 and reason in text and not (directory / (name + '.must-not-exist.json')).exists(), 'actual native negative target not specifically refused: ' + name)
    return dict(control=name, exit=code, refused_for_expected_native_reason=True, log_sha256=sha(output))

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--capture', type=Path, required=True)
    parser.add_argument('--directory', type=Path, required=True)
    parser.add_argument('--native-helper', type=Path, required=True)
    args = parser.parse_args()
    os.umask(0o077)
    args.directory.mkdir(mode=0o700)
    fixture = args.directory / 'actual-consumer-requests-prefix.private.ndjson.gz'
    counts = build_fixture(args.capture, fixture)
    controls = []
    for name in ('fee', 'share', 'principal', 'request', 'root', 'native_result', 'native_application_root', 'native_before'):
        controls.append(control(fixture, args.capture, args.directory, args.native_helper, name))
        with (args.directory / ('completed-' + name + '.private.json')).open('x') as stream:
            json.dump(controls[-1], stream)
    result = dict(actual_input_fixture=counts, fixture_sha256=sha(fixture), actual_negative_controls=controls, source_capture_sha256=sha(args.capture / 'capture.private.json'), independent_native_helper_sha256=sha(args.native_helper), financial_admission=False)
    with (args.directory / 'actual-negative-controls.private.json').open('x') as stream:
        json.dump(result, stream, indent=2, sort_keys=True)
    print(json.dumps(dict(actual_native_controls_refused=len(controls), financial_admission=False)))

if __name__ == '__main__':
    main()
