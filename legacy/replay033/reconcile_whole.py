#!/usr/bin/env python3
"""Assemble ONLY fully sealed 1..500000 profiles plus the accepted separate tail."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import subprocess
from consume_bounded import exact, sha
from capture_bounded import PROFILES
from verify_movement_rehearsal import require
from verify_terminal_fifteen import canonical_boundary

TERMINAL_SHA = 'c32582a7eb3f42544b60501541cba915c6f4cd26acda5f15b99bb438bc9f5e9e'


def pinned(path, digest):
    require(path.is_file() and not path.is_symlink() and sha(path) == digest, 'changed, missing or nonregular sealed artifact')
    return json.loads(path.read_bytes())


def signed_profile(path, expected_sha, repo, progress=None):
    seal = pinned(path, expected_sha)
    require(seal['schema'] == 'native033-complete-profile-seal-v1' and seal['financial_admission'] is False, 'complete independently sealed profile required')
    commit = seal['signed_source_commit']
    require(len(commit) == 40 and all(c in '0123456789abcdef' for c in commit), 'literal signed source commit required')
    subprocess.run(['git', '-C', str(repo), 'verify-commit', commit], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
    from seal_closed import CODE
    require(set(seal['signed_code_pins']) == set(CODE), 'complete signed consumer code coverage missing')
    for filename, digest in seal['signed_code_pins'].items():
        rel = Path(filename)
        require(not rel.is_absolute() and '..' not in rel.parts, 'unsafe signed code path')
        original = subprocess.check_output(['git', '-C', str(repo), 'show', commit + ':' + filename])
        require(hashlib.sha256(original).hexdigest() == digest, 'complete component code not bound to signed source')
    for index,(filename, digest) in enumerate(seal['artifact_pins'].items(),1):
        rel = Path(filename)
        require(not rel.is_absolute() and '..' not in rel.parts, 'unsafe complete component artifact path')
        artifact = path.parent / rel
        require(artifact.is_file() and not artifact.is_symlink() and sha(artifact) == digest, 'sealed component bytes changed')
        if progress is not None and (index % 1000 == 0 or index == len(seal['artifact_pins'])):
            progress(str(path),index,len(seal['artifact_pins']))
    for field, digest_field in (('result_path','result_sha256'),('native_proof_path','native_proof_sha256'),('exit_path','exit_sha256')):
        require(seal['artifact_pins'].get(seal[field]) == seal[digest_field], 'complete result not in sealed artifact inventory')
    require(seal['native_helper_sha256'] in seal['artifact_pins'].values(), 'complete native helper binary pin missing')
    result = pinned(path.parent / seal['result_path'], seal['result_sha256'])
    native = pinned(path.parent / seal['native_proof_path'], seal['native_proof_sha256'])
    exit_state = pinned(path.parent / seal['exit_path'], seal['exit_sha256'])
    require(exit_state['exit'] == 0 and result['schema'] == 'native033-complete-profile-financial-v1' and result['source_whole_complete'] is True and result['financial_admission'] is False and result['global_books_tax_acceptance'] is False, 'failed or bounded source cannot become complete profile')
    require(result['native_proof_sha256'] == seal['native_proof_sha256'] and result['native_helper_sha256'] == seal['native_helper_sha256'], 'complete component native proof/helper binding differs')
    require(native['schema'] == 'native033-independent-bounded-financial-v1' and native['normal_opening_height'] == result['first'] - 1, 'independent native opening schema differs')
    require(result['first'] == native['first'] and result['through'] == native['through'] and result['transactions'] == native['transactions'] and result['heights'] == native['native_iavl_heights'] and sum(result['phases'].values()) == native['typed_financial_phases'] and native['financial_admission'] is False and native['global_books_tax_acceptance'] is False, 'independent native component coverage differs')
    require(seal['source_preserved_after_proof'] is True and seal['actual_negative_controls_passed'] is True and seal['historical_blocks_executed'] == 0, 'complete source preservation or independent controls absent')
    return result


def named_roots(roots, height):
    names={'main','acc','staking','slashing','mint','distr','gov','fee','params'}
    require(roots['Version']==height and len(roots['StoreInfos'])==9 and {s['Name'] for s in roots['StoreInfos']}==names,'complete all-nine normal store roots required')
    result={}
    for s in roots['StoreInfos']:
        require(s['Core']['CommitID']['Version']==height,'normal native store root version differs')
        result[s['Name']]=exact(s['Core']['CommitID'])
    return result


def check_profiles(profiles):
    require(len(profiles) == 3, 'exactly three whole source profiles required')
    for p, first in zip(profiles, sorted(PROFILES)):
        target = PROFILES[first][0]
        require(p['first'] == first and p['through'] == target and p['heights'] == target - first + 1 and p['source_whole_complete'] is True, 'gap, duplicate, bounded component or missing complete profile')
        require(set(p['phases']) <= {'begin_block','ante','message','transaction','end_block','block'} and all(p['phases'].get(k) == p['heights'] for k in ('begin_block','end_block','block')) and p['phases'].get('transaction',0) == p['transactions'], 'complete native phase/transaction coverage differs')
        require(p['opening_commit']['Version'] == first - 1 and p['ending_commit']['Version'] == target, 'complete profile native opening/ending height differs')
        named_roots(p['ending_store_roots'],target)
    for left, right in zip(profiles, profiles[1:]):
        require(left['through'] + 1 == right['first'] and exact(left['ending_commit']) == exact(right['opening_commit']) and named_roots(left['ending_store_roots'],left['through']) == named_roots(right['opening_store_roots'],left['through']), 'normal checkpoint financial/native all-nine-store seam differs')
    return dict(distinct_prefix_heights=sum(p['heights'] for p in profiles), transactions=sum(p['transactions'] for p in profiles), all_nine_normal_checkpoint_seams=[200000,400000])


def verify(config, repo, progress=None):
    require(set(config) == {'profiles', 'terminal_seal', 'canonical_boundary'}, 'unknown whole assembly input')
    profiles = [signed_profile(Path(p['path']), p['sha256'], repo, progress) for p in config['profiles']]
    summary = check_profiles(profiles)
    boundary = canonical_boundary(config['canonical_boundary'])
    require(boundary['canonical_migration_height'] == 500000 and boundary['terminal_export_is_next_chain_opening'] is False, 'canonical boundary cannot move to retained tail')
    proof = json.loads((Path(config['canonical_boundary']) / 'checkpoint-proof.json').read_bytes())
    end = profiles[-1]
    require(base64.b64decode(end['ending_commit']['Hash'], validate=True).hex() == proof['application_hash'], 'complete prefix ending root differs from accepted canonical migration')
    terminal_path = Path(config['terminal_seal'])
    terminal = pinned(terminal_path, TERMINAL_SHA)
    require(terminal['complete_scope'] == [500001,500015] and terminal['canonical_migration_height'] == 500000 and terminal['terminal_next_header_proof'] is False and terminal['financial_admission'] is False and terminal['prior500000_export_or_payout_reexecuted'] is False and terminal['whole_required_scope_acceptance'] is False, 'accepted separate tail scope or proof mode differs')
    pins = terminal['all_retained_files_sha256']
    anchor = pinned(terminal_path.parent / 'final-image/native-audit.ndjson.source-checkpoint.json', pins['final-image/native-audit.ndjson.source-checkpoint.json'])
    whole = pinned(terminal_path.parent / 'final-image/whole-terminal-component.private.json', pins['final-image/whole-terminal-component.private.json'])
    require(anchor['height'] == 500000 and exact(anchor['commit']) == exact(end['ending_commit']), 'complete prefix does not join accepted native tail opening')
    require(whole['financial_admission'] is False and whole['tax_acceptance'] is False and whole['financial_context']['native_read_contexts'] == 16 and whole['financial_context']['native_exact_mint_transitions'] == 15 and whole['financial_context']['ordinary_tail_fees_or_paid_income'] is False and whole['chain_boundary']['canonical_migration_height'] == 500000, 'actual accepted separate tail financial/canonical proof absent')
    return dict(schema='native033-whole-source-financial-reconciliation-v1', first=1, through=500015,
                distinct_historical_heights=summary['distinct_prefix_heights'] + 15,
                historical_transactions=summary['transactions'], normal_seams=summary['all_nine_normal_checkpoint_seams'],
                canonical_migration_height=500000, retained_separate_tail=[500001,500015],
                tail_proof_mode='closed_pinned_native_reference', next500016_execution_or_header_claim=False,
                zero_height_validation_payouts_are_historical_income=False,
                existing_terminal_and_canonical_proofs_reused_without_reexecution=True,
                all_historical_sources_and_coinflows_reconciled=True, financial_admission=False, books_tax_acceptance=False,
                production_import_or_public_promotion_authorized=False, input_config_sha256=hashlib.sha256(json.dumps(config,sort_keys=True).encode()).hexdigest(),
                profile_seals=config['profiles'], accepted_terminal_seal_sha256=TERMINAL_SHA)

if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--config', type=Path, required=True)
    parser.add_argument('--repo', type=Path, required=True)
    parser.add_argument('--receipt', type=Path, required=True)
    a = parser.parse_args()
    os.umask(0o077)
    result = verify(json.loads(a.config.read_bytes()), a.repo)
    with a.receipt.open('x') as out:
        json.dump(result, out, indent=2, sort_keys=True)
    print('all 500015 historical source/coinflow components reconciled; production admission remains false')
