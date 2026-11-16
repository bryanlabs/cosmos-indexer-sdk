#!/usr/bin/env python3
"""Seal an ACTUAL complete-profile proof after controls and source readback."""
import argparse
import datetime
import decimal
import hashlib
import json
import os
from pathlib import Path
import subprocess
from consume_bounded import sha
from consume_closed import validate_source
from verify_movement_rehearsal import require

PREFIX = 'legacy/replay033/'
HELPER = PREFIX + 'cmd/consume-native/'
# The least any accepted launch commit must hold. The pinned set itself is every
# file code_list() finds at the launch commit, so a new helper file can never be
# executed without being pinned.
CODE = [PREFIX + name for name in ('capture_bounded.py','capture_closed.py','consume_bounded.py','consume_closed.py','reconcile_whole.py','seal_closed.py','run_closed_profile.py','run_whole.py','source_reader.py','verify_source_preservation.py','verify_terminal_fifteen.py','actual_full_controls.py','actual_bounded_controls.py','actual_checkpoint_controls.py','terminal_journal.py','compare_zero_export.py','verify_movement_rehearsal.py','test_consume_bounded.py','test_consume_closed.py','test_closed_lifecycle.py','go.mod','go.sum','cmd/consume-native/main.go','cmd/consume-native/main_test.go','cmd/consume-native/abci_equal.go','cmd/consume-native/slash_accounting.go','cmd/consume-native/slash_history.go','cmd/consume-native/slash_native.go','cmd/consume-native/slash_ownership.go','cmd/consume-native/slash_source.go')]
FACTS = 'native-stake-reductions.private.jsonl'
FACT_FIELDS = {'schema', 'height', 'facts', 'tax_classification', 'native_all_store_witness'}


def git(repo, *args, text=True):
    return subprocess.check_output(['git', '-C', str(repo)] + list(args), text=text)


def blob_sha(repo, commit, name):
    return hashlib.sha256(git(repo, 'show', commit + ':' + name, text=False)).hexdigest()


def code_list(repo, commit):
    """Every consumer file of one commit: all Python beside this file, the module files and the whole helper package."""
    names = git(repo, 'ls-tree', '-r', '--name-only', commit, '--', PREFIX.rstrip('/')).splitlines()
    chosen = sorted(n for n in names if n in (PREFIX + 'go.mod', PREFIX + 'go.sum') or n.startswith(HELPER) or (n.endswith('.py') and '/' not in n[len(PREFIX):]))
    require(set(CODE) <= set(chosen), 'launch commit lacks required consumer or penalty source')
    return chosen


def clean_at(repo, commit, reason):
    require(git(repo, 'rev-parse', 'HEAD').strip() == commit, reason)
    require(git(repo, 'status', '--porcelain') == '', 'signed clean consumer tree required')


def utc(text):
    value = datetime.datetime.fromisoformat(text.replace('Z', '+00:00'))
    require(value.tzinfo is not None, 'timestamp without zone')
    return value


def build_info(helper):
    """What the Go toolchain stamped into the helper when it was built."""
    text = subprocess.check_output(['go', 'version', '-m', str(helper)], text=True)
    settings = dict(line.split('\t')[2].split('=', 1) for line in text.splitlines() if line.startswith('\tbuild\t') and '=' in line)
    return dict(go=text.splitlines()[0].split(': ', 1)[1], revision=settings.get('vcs.revision', ''), modified=settings.get('vcs.modified', ''), trimpath=settings.get('-trimpath', 'false'), cgo=settings.get('CGO_ENABLED', ''))


def stake_facts(path, first, through):
    """Read one part's penalty facts. Returns one entry per penalty block, never the private rows."""
    raw = path.read_bytes()
    require(raw.endswith(b'\n'), 'penalty fact file ends mid-record')
    entries, last = [], first - 1
    for line in raw.split(b'\n')[:-1]:
        row = json.loads(line)
        require(set(row) == FACT_FIELDS and row['schema'] == 'native033-stake-asset-reduction-v1' and row['tax_classification'] == 'unclassified' and row['native_all_store_witness'] is True, 'unknown penalty fact record')
        height, facts = row['height'], row['facts'] or []
        require(type(height) is int and last < height <= through and type(facts) is list and all(type(f) is dict for f in facts), 'penalty fact outside its part or out of order')
        for fact in facts:
            before, after, reduction = (fact.get(k) for k in ('native_dec_before', 'native_dec_after', 'native_dec_reduction'))
            if before and after and reduction:
                with decimal.localcontext() as context:
                    context.prec = 200
                    require(decimal.Decimal(before) - decimal.Decimal(after) == decimal.Decimal(reduction), 'penalty fact amounts do not add up')
        entries.append(dict(height=height, fact_rows=len(facts), line_sha256=hashlib.sha256(line + b'\n').hexdigest()))
        last = height
    return entries


def bound_facts(proof, result, native):
    """The fact file must be exactly what the checker counted, or absent when it counted none."""
    path = proof / FACTS
    blocks, rows, digest = result['native_stake_asset_blocks'], result['native_stake_asset_fact_rows'], result['native_stake_asset_facts_sha256']
    require((blocks, rows, digest) == (native.get('native_stake_asset_blocks', 0), native.get('native_stake_asset_fact_rows', 0), native.get('native_stake_asset_facts_sha256', '')) and result['stake_asset_tax_classification'] == 'unclassified', 'penalty fact counts differ between receipts')
    if not blocks:
        require(not path.exists() and not rows and not digest, 'unbound penalty facts')
        return []
    require(path.is_file() and not path.is_symlink() and sha(path) == digest, 'penalty facts missing or changed')
    entries = stake_facts(path, result['first'], result['through'])
    require(len(entries) == blocks and sum(e['fact_rows'] for e in entries) == rows, 'penalty fact rows differ from the checker count')
    return entries


def validate_start(repo, start_path):
    started = json.loads(start_path.read_bytes())
    commit = started['signed_source_commit']
    require(len(commit) == 40 and all(c in '0123456789abcdef' for c in commit), 'literal signed source commit required')
    # The Python that ran was read from this worktree. It must still be the
    # launch commit, clean, and byte-equal to the signed commit.
    launch = Path(started.get('launch_worktree') or repo)
    require(git(launch, 'rev-parse', 'HEAD').strip() == commit, 'consumer HEAD differs from actual run-start signed source')
    expected_python = {name for name in code_list(launch, commit) if name.endswith('.py')}
    require(set(started['python_source_pins']) == expected_python, 'run-start Python source coverage missing')
    for name, digest in started['python_source_pins'].items():
        require(sha(launch/name) == digest and blob_sha(launch, commit, name) == digest, 'executed Python source differs from actual signed run start')
    return commit


def after_launch(started, commit, helper, result):
    """A record written after launch proves the helper by rebuilding it, never by claiming a pre-launch check."""
    require(started['schema'] == 'native033-launch-record-after-launch-v1' and started['checked_before_launch'] is False and started['first'] == result['first'] and started['through'] == result['through'], 'after-launch record does not describe this part')
    info = build_info(helper)
    require(started['helper_sha256'] == started['rebuilt_helper_sha256'] == sha(helper) and info['revision'] == commit == started['helper_build']['revision'] and info['modified'] == 'false' == started['helper_build']['modified'], 'helper that ran is not a clean rebuildable build of the launch commit')
    require(started['launcher_names_commit_and_helper'] is True and utc(started['produced_utc']) > utc(started['launched_utc']), 'after-launch record misstates when it was produced')
    build = Path(started['build_worktree'])
    clean_at(build, commit, 'rebuild worktree differs from the launch commit')
    return build


def seal(root, proof, helper, controls_path, preservation_path, repo, output, start_path):
    root,proof,helper,controls_path,preservation_path,repo,output,start_path=[p.resolve() for p in (root,proof,helper,controls_path,preservation_path,repo,output,start_path)]
    b = validate_source(root)
    record = start_path.parent
    result_path = proof / 'complete-component.private.json'
    native_path = proof / 'native-proof.private.json'
    exit_path = proof / 'exit.private.json'
    result, native, exited, controls, preserved, started = [json.loads(p.read_bytes()) for p in (result_path,native_path,exit_path,controls_path,preservation_path,start_path)]
    split = result['schema'] == 'native033-split-range-financial-v1'
    prefix = split and result['first'] > b['first']
    if split:
        require(result['source_range'] == [b['first'], b['through']] and b['first'] <= result['first'] <= result['through'] <= b['through'] and result['source_closed_at_through'] is (result['through'] == b['through']), 'part lies outside its complete source range or misstates its closure')
        if prefix:
            require(native['progress_phase_base'] == result['progress_phase_base'] == result['fast_forward']['typed_phases'] and result['fast_forward']['heights'] == result['first'] - b['first'] and len(result['opening_checkpoint_manifest_sha256']) == 64, 'part opening was not rebuilt from the range start and checked against a saved native checkpoint')
    else:
        require(result['schema'] == 'native033-complete-profile-financial-v1' and result['source_whole_complete'] is True and result['first'] == b['first'] and result['through'] == b['through'], 'actual complete source financial proof missing')
    require(exited['exit'] == 0 and result['financial_admission'] is False and result['global_books_tax_acceptance'] is False and result['capture_sha256'] == sha(root/'capture.private.json'), 'actual complete source financial proof missing')
    require(result['first'] == native['first'] and result['through'] == native['through'] and result['heights'] == native['native_iavl_heights'] == result['through']-result['first']+1 and result['transactions'] == native['transactions'] and sum(result['phases'].values()) == native['typed_financial_phases'] and result['native_helper_sha256'] == sha(helper) and result['native_proof_sha256'] == sha(native_path), 'complete independent native proof/helper coverage differs')
    facts = bound_facts(proof, result, native)
    require(preserved['source_capture_sha256'] == sha(root/'capture.private.json') and preserved['source_preserved_after_proof'] is True and utc(preserved['checked_utc']) >= utc(exited['finished_utc']), 'post-proof actual source preservation missing')
    require(controls['native_helper_sha256'] == sha(helper) and controls['first'] == result['first'] and controls['source_capture_sha256'] == sha(root/'capture.private.json') and controls['historical_blocks_executed'] == 0, 'actual controls not bound to this complete source/helper')
    expected_controls = {'fee','share','principal','request','root','native_result','native_application_root','native_before'} if result['first'] == 1 else {'opening_root','opening_store','opening_baseline','before','final_root','final_result','next_header'}
    require(len(controls['controls']) == len(expected_controls) and {c['control'] for c in controls['controls']} == expected_controls and all(c['exit'] != 0 and c['expected_refusal'] is True for c in controls['controls']), 'specific actual native controls missing')
    paths = [result_path,native_path,exit_path,helper,controls_path,preservation_path,record/'compiled-go-source-pins.private.json',record/'build-test.private.log',root/'capture.private.json',start_path]
    for name in ('native-consumer.private.log', 'consumer.private.log'):
        require((proof/name).is_file(), 'checker log missing')
        paths.append(proof/name)
    for c in controls['controls']:
        require(sha(Path(c['retained_log_path'])) == c['log_sha256'], 'actual negative control log changed')
        paths.append(Path(c['retained_log_path']))
    if facts:
        paths.append(proof/FACTS)
    commit = validate_start(repo, start_path)
    launch = Path(started.get('launch_worktree') or repo)
    clean_at(launch, commit, 'consumer HEAD differs from actual run-start signed source')
    subprocess.run(['git','-C',str(launch),'verify-commit',commit],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,check=True)
    reconstructed = started.get('schema') is not None
    build = after_launch(started, commit, helper, result) if reconstructed else launch
    code = {}
    for name in code_list(launch, commit):
        require(blob_sha(launch, commit, name) == sha(launch/name), 'actual consumer source differs from signed code')
        code[name] = sha(launch/name)
    compiled_path = record / 'compiled-go-source-pins.private.json'
    compiled = json.loads(compiled_path.read_bytes())
    require(len(compiled) > 100 and all(str(build/name) in compiled for name in code if name.startswith(HELPER) and name.endswith('.go') and not name.endswith('_test.go') and name != HELPER + 'anchor_allow_splitproof.go'), 'actual compiled native source pins missing')
    for filename, digest in compiled.items():
        require(sha(Path(filename)) == digest, 'actual compiled native source changed')
        if Path(filename).is_relative_to(build):
            require(blob_sha(build, commit, str(Path(filename).relative_to(build))) == digest, 'compiled native source differs from signed code')
    supplemental = native.get('supplemental_validator_history_sha256', '')
    if supplemental:
        history = Path(started.get('validator_history') or '')
        require(reconstructed and history.is_file() and sha(history) == supplemental, 'supplemental validator history the checker read is not retained')
        paths.append(history)
    require(not native.get('supplemental_evidence_capture_sha256'), 'supplemental evidence capture is not retained by this seal')
    if reconstructed:
        paths += [Path(started[k]) for k in ('launcher', 'built_from')]
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
    if prefix:
        # A later part opened from a saved checkpoint that is not part of its
        # source capture. Retain those exact bytes with the part.
        from consume_split import opening_manifest
        opening, manifest = Path(started.get('opening_checkpoint') or ''), Path(started.get('opening_manifest') or '')
        require(reconstructed and opening.is_dir() and opening_manifest(opening, manifest) == result['opening_checkpoint_manifest_sha256'] == controls.get('opening_checkpoint_manifest_sha256'), 'saved opening checkpoint of this part is not retained')
        for path in [manifest] + sorted(p for p in opening.rglob('*') if not p.is_dir()):
            relname = os.path.relpath(path, output.parent)
            require('..' not in Path(relname).parts, 'opening checkpoint escapes retained seal root')
            pins[relname] = sha(path)
    else:
        require('opening_checkpoint_manifest_sha256' not in controls or not controls['opening_checkpoint_manifest_sha256'], 'controls name an opening this part did not use')
    def rel(path):return os.path.relpath(path,output.parent)
    tool = Path(__file__).resolve().parents[2]
    tool_commit = git(tool, 'rev-parse', 'HEAD').strip()
    clean_at(tool, tool_commit, 'sealing tool tree is not clean')
    subprocess.run(['git','-C',str(tool),'verify-commit',tool_commit],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,check=True)
    result = dict(schema='native033-complete-profile-seal-v1',signed_source_commit=commit,signed_code_pins=code,artifact_pins=pins,
                  result_path=rel(result_path),result_sha256=sha(result_path),native_proof_path=rel(native_path),native_proof_sha256=sha(native_path),
                  exit_path=rel(exit_path),exit_sha256=sha(exit_path),native_helper_sha256=sha(helper),
                  launch_record='written_after_launch_from_rebuild' if reconstructed else 'written_by_supervisor_before_proof',
                  launch_record_path=rel(start_path),sealing_tool_commit=tool_commit,
                  stake_asset_facts_path=rel(proof/FACTS) if facts else '',stake_asset_facts_sha256=sha(proof/FACTS) if facts else '',stake_asset_fact_blocks=facts,
                  stake_asset_tax_classification='unclassified',
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
