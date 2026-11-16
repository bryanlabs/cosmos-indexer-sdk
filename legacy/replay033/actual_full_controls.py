#!/usr/bin/env python3
"""Bind native negative controls to one genuinely complete original profile."""
import argparse
import json
import os
from pathlib import Path
from consume_bounded import sha
from consume_closed import validate_source
from verify_movement_rehearsal import require
from actual_bounded_controls import build_fixture, control
from actual_checkpoint_controls import fixture as checkpoint_fixture, control as checkpoint_control


def run(root, directory, binary, first=None, opening=None, manifest=None):
    b = validate_source(root)
    first = b['first'] if first is None else first
    prefix = first != b['first']
    require(prefix == (opening is not None) == (manifest is not None), 'a later part needs its saved opening checkpoint and manifest, a range start must not have one')
    directory.mkdir(mode=0o700)
    source = directory / 'actual-native-requests-prefix.gz'
    results = []
    if first == 1:
        build_fixture(root, source)
        names = ('fee','share','principal','request','root','native_result','native_application_root','native_before')
        for name in names:
            r = control(source, root, directory, binary, name)
            r['expected_refusal'] = r.pop('refused_for_expected_native_reason')
            r['retained_log_path'] = str(directory / (name + '.refused.private.log'))
            results.append(r)
    else:
        checkpoint_fixture(root, source, first if prefix else None, opening, manifest)
        names = ('opening_root','opening_store','opening_baseline','before','final_root','final_result','next_header')
        for name in names:
            r = checkpoint_control(root, source, directory, binary, name, opening)
            r['retained_log_path'] = str(directory / (name + '.refused.private.log'))
            results.append(r)
    result = dict(schema='native033-actual-complete-profile-controls-v1', first=first, opening_checkpoint_manifest_sha256=sha(manifest) if prefix else '', source_capture_sha256=sha(root/'capture.private.json'), fixture_sha256=sha(source), native_helper_sha256=sha(binary), controls=results, historical_blocks_executed=0, financial_admission=False)
    with (directory/'actual-negative-controls.private.json').open('x') as out:
        json.dump(result,out,indent=2,sort_keys=True)
    return result

if __name__ == '__main__':
    parser=argparse.ArgumentParser()
    for name in ('capture','directory','native-helper'):
        parser.add_argument('--'+name,type=Path,required=True)
    parser.add_argument('--first',type=int)
    parser.add_argument('--opening-checkpoint',type=Path)
    parser.add_argument('--opening-manifest',type=Path)
    a=parser.parse_args()
    os.umask(0o077)
    r=run(a.capture,a.directory,a.native_helper,a.first,a.opening_checkpoint,a.opening_manifest)
    print(json.dumps(dict(first=r['first'],actual_native_controls_refused=len(r['controls']))))
