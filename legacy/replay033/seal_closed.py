#!/usr/bin/env python3
"""Seal an ACTUAL complete-profile proof after controls and source readback."""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import subprocess
from consume_bounded import sha
from consume_closed import validate_source
from verify_movement_rehearsal import require

CODE = ['legacy/replay033/' + name for name in ('capture_bounded.py','capture_closed.py','consume_bounded.py','consume_closed.py','reconcile_whole.py','seal_closed.py','run_closed_profile.py','run_whole.py','source_reader.py','verify_source_preservation.py','verify_terminal_fifteen.py','actual_full_controls.py','actual_bounded_controls.py','actual_checkpoint_controls.py','terminal_journal.py','compare_zero_export.py','verify_movement_rehearsal.py','test_consume_bounded.py','test_consume_closed.py','test_closed_lifecycle.py','go.mod','go.sum','cmd/consume-native/main.go','cmd/consume-native/main_test.go')]

def validate_start(repo, start_path):
    started = json.loads(start_path.read_bytes())
    commit = started['signed_source_commit']
    require(subprocess.check_output(['git','-C',str(repo),'rev-parse','HEAD'],text=True).strip() == commit, 'consumer HEAD differs from actual run-start signed source')
    expected_python = {name for name in CODE if name.endswith('.py')}
    require(set(started['python_source_pins']) == expected_python, 'run-start Python source coverage missing')
    for name, digest in started['python_source_pins'].items():
        require(sha(repo/name) == digest and hashlib.sha256(subprocess.check_output(['git','-C',str(repo),'show',commit+':'+name])).hexdigest() == digest, 'executed Python source differs from actual signed run start')
    return commit


def seal(root, proof, helper, controls_path, preservation_path, repo, output, start_path):
    root,proof,helper,controls_path,preservation_path,repo,output,start_path=[p.resolve() for p in (root,proof,helper,controls_path,preservation_path,repo,output,start_path)]
    b = validate_source(root)
    result_path = proof / 'complete-component.private.json'
    native_path = proof / 'native-proof.private.json'
    exit_path = proof / 'exit.private.json'
    result, native, exited, controls, preserved = [json.loads(p.read_bytes()) for p in (result_path,native_path,exit_path,controls_path,preservation_path)]
    require(exited['exit'] == 0 and result['schema'] == 'native033-complete-profile-financial-v1' and result['source_whole_complete'] is True and result['financial_admission'] is False and result['global_books_tax_acceptance'] is False and result['capture_sha256'] == sha(root/'capture.private.json'), 'actual complete source financial proof missing')
    require(result['first'] == native['first'] == b['first'] and result['through'] == native['through'] == b['through'] and result['heights'] == native['native_iavl_heights'] == b['through']-b['first']+1 and result['transactions'] == native['transactions'] and sum(result['phases'].values()) == native['typed_financial_phases'] and result['native_helper_sha256'] == sha(helper) and result['native_proof_sha256'] == sha(native_path), 'complete independent native proof/helper coverage differs')
    require(preserved['source_capture_sha256'] == sha(root/'capture.private.json') and preserved['source_preserved_after_proof'] is True and preserved['checked_utc'] >= exited['finished_utc'], 'post-proof actual source preservation missing')
    require(controls['native_helper_sha256'] == sha(helper) and controls['first'] == b['first'] and controls['source_capture_sha256'] == sha(root/'capture.private.json') and controls['historical_blocks_executed'] == 0, 'actual controls not bound to this complete source/helper')
    expected_controls = {'fee','share','principal','request','root','native_result','native_application_root','native_before'} if b['first'] == 1 else {'opening_root','opening_store','opening_baseline','before','final_root','final_result','next_header'}
    require(len(controls['controls']) == len(expected_controls) and {c['control'] for c in controls['controls']} == expected_controls and all(c['exit'] != 0 and c['expected_refusal'] is True for c in controls['controls']), 'specific actual native controls missing')
    for c in controls['controls']:
        require(sha(Path(c['retained_log_path'])) == c['log_sha256'], 'actual negative control log changed')
    require(subprocess.check_output(['git','-C',str(repo),'status','--porcelain'],text=True) == '', 'signed clean consumer tree required')
    commit = validate_start(repo, start_path)
    subprocess.run(['git','-C',str(repo),'verify-commit',commit],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,check=True)
    code = {}
    for name in CODE:
        original = subprocess.check_output(['git','-C',str(repo),'show',commit+':'+name])
        require(hashlib.sha256(original).hexdigest() == sha(repo/name), 'actual consumer source differs from signed code')
        code[name] = sha(repo/name)
    compiled_path = proof.parent / 'compiled-go-source-pins.private.json'
    compiled = json.loads(compiled_path.read_bytes())
    require(len(compiled) > 100 and compiled[str(repo/'legacy/replay033/cmd/consume-native/main.go')] == sha(repo/'legacy/replay033/cmd/consume-native/main.go'), 'actual compiled native source pins missing')
    for filename, digest in compiled.items():
        require(sha(Path(filename)) == digest, 'actual compiled native source changed')
    paths = [result_path,native_path,exit_path,helper,controls_path,preservation_path,compiled_path,proof.parent/'build-test.private.log',root/'capture.private.json',start_path]
    pins = {}
    for path in paths:
        rel = os.path.relpath(path,output.parent)
        require('..' not in Path(rel).parts, 'seal must be placed above retained component artifacts')
        pins[rel] = sha(path)
    source_pins = dict(b['source_metadata_sha256'])
    source_pins.update({'cache/'+name:pin for name,pin in b['cache_members_sha256'].items()})
    source_pins.update({'opening-checkpoint/'+name:pin for name,pin in (b.get('opening_checkpoint_members_sha256') or {}).items()})
    for name,spec in b['streams'].items():
        path=root/(name+'.source-closed.gz')
        require(path.is_file() and not path.is_symlink() and sha(path)==spec['sha256'],'full original compressed source changed after native proof')
        source_pins[path.name]=spec['sha256']
    for name in ('cache-source.private.tar','opening-checkpoint.private.tar'):
        if (root/name).exists():source_pins[name]=sha(root/name)
    for name,pin in source_pins.items():
        relname=os.path.relpath(root/name,output.parent)
        require('..' not in Path(relname).parts,'source artifact escapes retained seal root')
        pins[relname]=pin
    def rel(path):return os.path.relpath(path,output.parent)
    result = dict(schema='native033-complete-profile-seal-v1',signed_source_commit=commit,signed_code_pins=code,artifact_pins=pins,
                  result_path=rel(result_path),result_sha256=sha(result_path),native_proof_path=rel(native_path),native_proof_sha256=sha(native_path),
                  exit_path=rel(exit_path),exit_sha256=sha(exit_path),native_helper_sha256=sha(helper),
                  source_preserved_after_proof=True,actual_negative_controls_passed=True,historical_blocks_executed=0,
                  financial_admission=False,books_tax_acceptance=False,sealed_utc=datetime.datetime.now(datetime.timezone.utc).isoformat())
    with output.open('x') as stream:
        json.dump(result,stream,indent=2,sort_keys=True)
    return result

if __name__ == '__main__':
    parser=argparse.ArgumentParser()
    for name in ('capture','proof','native-helper','controls','preservation','repo','receipt','start-record'):
        parser.add_argument('--'+name,type=Path,required=True)
    a=parser.parse_args()
    os.umask(0o077)
    seal(a.capture,a.proof,a.native_helper,a.controls,a.preservation,a.repo,a.receipt,a.start_record)
    print('actual complete-profile evidence sealed, production admission false')
