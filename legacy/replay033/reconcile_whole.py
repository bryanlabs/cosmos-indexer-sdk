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
# The six Hub 1 blocks where BeginBlock took stake from delegators. The join
# refuses unless the parts carry a penalty fact for exactly these heights.
PENALTY_HEIGHTS = (134125, 219358, 221354, 231614, 359194, 486010)
OLDER, NEWER = '199da294f4be111d89fc17501032dc27c42e2b28', '0f35e0d7d1ac6a39ed414c7a5374711b1c1e0e78'
# Every file that differs between the two launch commits, as (older, newer)
# SHA-256, None where the older commit has no such file. Nothing else may differ.
#
# Only the 400001..500000 whole range ran the older build. Why that is the same
# recording the newer build would have made:
# - consume_split.py and its test are new and were not run by that range, which
#   ran consume_closed.py, identical in both commits.
# - reconcile_whole.py and seal_closed.py are not imported by either recorder.
# - main.go differs in three places. The opening height allow-list moved into
#   anchor_allow.go and gained 100000 and 300000; 400000 is allowed by both.
#   --progress-phase-base is added to progress lines and is 0 without the flag.
#   The newer build also refuses when the opening block hash differs from the
#   raw block; a refusal check cannot change what a passing run records, and the
#   join requires a run of the newer build on the same 400001 opening that
#   passed it (cross_build_witness).
BUILD_DIFFERENCES = {
    'legacy/replay033/cmd/consume-native/main.go': ('4a6dada6d77df218cc0de9a38ec6a8d0d59ae7cf74b4d2ec9c9b1d4c2cc89627', '69c6ec724b032edfb14fddd9a2c8209b919cc14bac494fa2073aa7b140b9668b'),
    'legacy/replay033/cmd/consume-native/anchor_allow.go': (None, '431717214dd7dda57a3f7abd9c22914416907ddba82a81baa531d334ac1827bc'),
    'legacy/replay033/cmd/consume-native/anchor_allow_splitproof.go': (None, '103c1f1194610e68bb3cc99a5cb49d9dd1e13056653e6b4022dbdd3aaf55591f'),
    'legacy/replay033/cmd/consume-native/anchor_allow_test.go': (None, '34d4097dbf9fc2373a3ab37dcd673a59962e4708afb6bfe2b79d03df96a62563'),
    'legacy/replay033/consume_split.py': (None, 'd8eac9f928079c153d536bdc366886c185b993792832de78fa6f6b4c03478a03'),
    'legacy/replay033/test_consume_split.py': (None, '7109cf07deb4c065fba8fb395b9e069210899de5f651df0db1de8e2dc09ec8e1'),
    'legacy/replay033/reconcile_whole.py': ('11a942fc4b04cf59d8793f6b471730f6f7f224e8985500d98db1f190bfdfd034', '138274a6b9d7922775ce0cd5c6a3f49cccd74d3700f9a004b764b087d5e76300'),
    'legacy/replay033/seal_closed.py': ('bdd5a92ff5894047690691f69096657cb83dcca8e7873d07d0ea7ace6c316d84', 'dc13c7d7fe7e1cebe5b84c40336d9fd90eed1ad775119aed5215721b472ad647'),
}


def pinned(path, digest):
    require(path.is_file() and not path.is_symlink() and sha(path) == digest, 'changed, missing or nonregular sealed artifact')
    return json.loads(path.read_bytes())


def signed_profile(path, expected_sha, repo, progress=None, with_seal=False):
    seal = pinned(path, expected_sha)
    require(seal['schema'] == 'native033-complete-profile-seal-v1' and seal['financial_admission'] is False, 'complete independently sealed profile required')
    commit = seal['signed_source_commit']
    require(len(commit) == 40 and all(c in '0123456789abcdef' for c in commit), 'literal signed source commit required')
    subprocess.run(['git', '-C', str(repo), 'verify-commit', commit], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
    from seal_closed import bound_facts, code_list
    require(set(seal['signed_code_pins']) == set(code_list(repo, commit)), 'complete signed consumer code coverage missing')
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
    require(exit_state['exit'] == 0 and result['schema'] in ('native033-complete-profile-financial-v1', 'native033-split-range-financial-v1') and (result['schema'] != 'native033-complete-profile-financial-v1' or result['source_whole_complete'] is True) and result['financial_admission'] is False and result['global_books_tax_acceptance'] is False, 'failed or bounded source cannot become complete profile')
    require(result['native_proof_sha256'] == seal['native_proof_sha256'] and result['native_helper_sha256'] == seal['native_helper_sha256'], 'complete component native proof/helper binding differs')
    require(native['schema'] == 'native033-independent-bounded-financial-v1' and native['normal_opening_height'] == result['first'] - 1, 'independent native opening schema differs')
    require(result['first'] == native['first'] and result['through'] == native['through'] and result['transactions'] == native['transactions'] and result['heights'] == native['native_iavl_heights'] and sum(result['phases'].values()) == native['typed_financial_phases'] and native['financial_admission'] is False and native['global_books_tax_acceptance'] is False, 'independent native component coverage differs')
    require(seal['source_preserved_after_proof'] is True and seal['actual_negative_controls_passed'] is True and seal['historical_blocks_executed'] == 0, 'complete source preservation or independent controls absent')
    # Penalty facts are re-read from the sealed bytes, not trusted from the seal.
    facts = bound_facts((path.parent / seal['result_path']).parent, result, native)
    require(facts == seal['stake_asset_fact_blocks'] and seal['stake_asset_tax_classification'] == 'unclassified', 'sealed penalty facts differ from the retained fact file')
    if facts:
        require(seal['artifact_pins'].get(seal['stake_asset_facts_path']) == seal['stake_asset_facts_sha256'] == result['native_stake_asset_facts_sha256'] and (path.parent / seal['stake_asset_facts_path']).resolve() == ((path.parent / seal['result_path']).parent / 'native-stake-reductions.private.jsonl').resolve(), 'penalty fact file is not in the sealed artifact inventory')
    else:
        require(not seal['stake_asset_facts_path'] and not seal['stake_asset_facts_sha256'], 'seal names penalty facts the checker did not count')
    if with_seal:
        return result, seal, path
    return result


def check_builds(sealed, witness):
    """All parts from one commit, or the one reviewed pair of commits with every difference accounted for."""
    commits = {seal['signed_source_commit'] for _, seal, _ in sealed}
    if len(commits) == 1:
        require(witness is None, 'cross-build witness given for a single-build join')
        return sorted(commits)
    require(commits == {OLDER, NEWER}, 'parts come from builds whose differences were never reviewed')
    older = [(r, s) for r, s, _ in sealed if s['signed_source_commit'] == OLDER]
    newer = [(r, s) for r, s, _ in sealed if s['signed_source_commit'] == NEWER]
    require(all(r['schema'] == 'native033-complete-profile-financial-v1' and r['first'] == 400001 and r['through'] == 500000 for r, _ in older) and len(older) == 1, 'only the whole 400001 range may come from the older build')
    a, b = older[0][1]['signed_code_pins'], newer[0][1]['signed_code_pins']
    require(all(s['signed_code_pins'] == b for _, s in newer) and len({s['native_helper_sha256'] for _, s in newer}) == 1, 'parts of the newer build differ among themselves')
    for name in set(a) | set(b):
        if a.get(name) != b.get(name):
            require(BUILD_DIFFERENCES.get(name) == (a.get(name), b.get(name)), 'unreviewed source difference between the two builds: ' + name)
    require(witness is not None, 'cross-build witness required')
    directory = Path(witness['path'])
    run = pinned(directory / 'complete-component.private.json', witness['sha256'])
    native = pinned(directory / 'native-proof.private.json', run['native_proof_sha256'])
    exited = json.loads((directory / 'exit.private.json').read_bytes())
    require(exited['exit'] == 0 and run['schema'] == 'native033-split-range-financial-v1' and run['first'] == 400001 == native['first'] and run['through'] == native['through'] >= 400100 and run['fast_forward']['heights'] == 0 and run['capture_sha256'] == older[0][0]['capture_sha256'] and run['native_helper_sha256'] == newer[0][1]['native_helper_sha256'] and exact(run['opening_commit']) == exact(older[0][0]['opening_commit']), 'newer build did not pass the same 400001 opening the older build recorded from')
    return [OLDER, NEWER]


def join_facts(sealed):
    """Every penalty fact line of every part, in height order, as exact original bytes."""
    entries, lines = [], []
    for result, seal, path in sealed:
        if not seal['stake_asset_fact_blocks']:
            continue
        raw = (path.parent / seal['stake_asset_facts_path']).read_bytes().split(b'\n')[:-1]
        for entry, line in zip(seal['stake_asset_fact_blocks'], raw):
            require(hashlib.sha256(line + b'\n').hexdigest() == entry['line_sha256'] and result['first'] <= entry['height'] <= result['through'], 'penalty fact line differs from its seal')
            entries.append(dict(entry, part=[result['first'], result['through']]))
            lines.append(line + b'\n')
    require(tuple(e['height'] for e in entries) == PENALTY_HEIGHTS, 'recorded penalty blocks differ from the six known Hub 1 penalty heights')
    return entries, b''.join(lines)


def named_roots(roots, height):
    names={'main','acc','staking','slashing','mint','distr','gov','fee','params'}
    require(roots['Version']==height and len(roots['StoreInfos'])==9 and {s['Name'] for s in roots['StoreInfos']}==names,'complete all-nine normal store roots required')
    result={}
    for s in roots['StoreInfos']:
        require(s['Core']['CommitID']['Version']==height,'normal native store root version differs')
        result[s['Name']]=exact(s['Core']['CommitID'])
    return result


SPLIT_SEAMS = {100000, 300000}


def check_profiles(profiles):
    ends = {PROFILES[first][0] for first in PROFILES}
    require(3 <= len(profiles) <= 3 + len(SPLIT_SEAMS), 'three whole source profiles, or their saved-checkpoint parts, required')
    require(profiles[0]['first'] == 1 and profiles[-1]['through'] == 500000, 'gap, duplicate, bounded component or missing complete profile')
    seams = []
    for p in profiles:
        first, through = p['first'], p['through']
        split = p.get('schema') == 'native033-split-range-financial-v1'
        source_first = max(f for f in PROFILES if f <= first) if first >= 1 else 0
        require(source_first in PROFILES and through <= PROFILES[source_first][0], 'gap, duplicate, bounded component or missing complete profile')
        target = PROFILES[source_first][0]
        if split:
            require(p['source_range'] == [source_first, target] and (first == source_first or first - 1 in SPLIT_SEAMS) and (through == target or through in SPLIT_SEAMS) and first <= through, 'part does not start and end at a saved native checkpoint of its own source range')
            require(p['source_closed_at_through'] is (through == target) and p['next_header_is_source_closure'] is (through == target) and p['fast_forward']['heights'] == first - source_first and p['progress_phase_base'] == p['fast_forward']['typed_phases'], 'part source closure or rebuilt opening coverage differs')
            require(bool(p['opening_checkpoint_manifest_sha256']) == (first > source_first), 'part opening checkpoint binding differs')
        else:
            require(first == source_first and through == target and p['source_whole_complete'] is True, 'gap, duplicate, bounded component or missing complete profile')
        require(p['heights'] == through - first + 1, 'gap, duplicate, bounded component or missing complete profile')
        require(set(p['phases']) <= {'begin_block','ante','message','transaction','end_block','block'} and all(p['phases'].get(k) == p['heights'] for k in ('begin_block','end_block','block')) and p['phases'].get('transaction',0) == p['transactions'], 'complete native phase/transaction coverage differs')
        require(p['opening_commit']['Version'] == first - 1 and p['ending_commit']['Version'] == through, 'complete profile native opening/ending height differs')
        named_roots(p['ending_store_roots'],through)
    for left, right in zip(profiles, profiles[1:]):
        require(left['through'] + 1 == right['first'] and exact(left['ending_commit']) == exact(right['opening_commit']) and named_roots(left['ending_store_roots'],left['through']) == named_roots(right['opening_store_roots'],left['through']), 'normal checkpoint financial/native all-nine-store seam differs')
        seams.append(left['through'])
        if right.get('schema') == 'native033-split-range-financial-v1' and right['fast_forward']['heights']:
            require(left.get('schema') == right['schema'] and left['source_range'] == right['source_range'] and right['progress_phase_base'] == left['progress_phase_base'] + sum(left['phases'].values()), 'rebuilt opening phase count differs from the preceding recorded part')
    require(set(seams) >= ends - {500000} and set(seams) <= (ends | SPLIT_SEAMS) - {500000} and len(set(seams)) == len(seams), 'gap, duplicate, bounded component or missing complete profile')
    return dict(distinct_prefix_heights=sum(p['heights'] for p in profiles), transactions=sum(p['transactions'] for p in profiles), all_nine_normal_checkpoint_seams=seams)


def verify(config, repo, progress=None, facts_path=None):
    require({'profiles', 'terminal_seal', 'canonical_boundary'} <= set(config) <= {'profiles', 'terminal_seal', 'canonical_boundary', 'cross_build_witness'}, 'unknown whole assembly input')
    sealed = [signed_profile(Path(p['path']), p['sha256'], repo, progress, with_seal=True) for p in config['profiles']]
    profiles = [result for result, _, _ in sealed]
    builds = check_builds(sealed, config.get('cross_build_witness'))
    summary = check_profiles(profiles)
    fact_blocks, fact_bytes = join_facts(sealed)
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
    if facts_path is not None:
        with facts_path.open('xb') as out:
            out.write(fact_bytes)
    return dict(schema='native033-whole-source-financial-reconciliation-v1', first=1, through=500015,
                distinct_historical_heights=summary['distinct_prefix_heights'] + 15,
                historical_transactions=summary['transactions'], normal_seams=summary['all_nine_normal_checkpoint_seams'],
                canonical_migration_height=500000, retained_separate_tail=[500001,500015],
                tail_proof_mode='closed_pinned_native_reference', next500016_execution_or_header_claim=False,
                zero_height_validation_payouts_are_historical_income=False,
                existing_terminal_and_canonical_proofs_reused_without_reexecution=True,
                all_historical_sources_and_coinflows_reconciled=True, financial_admission=False, books_tax_acceptance=False,
                production_import_or_public_promotion_authorized=False, input_config_sha256=hashlib.sha256(json.dumps(config,sort_keys=True).encode()).hexdigest(),
                profile_seals=config['profiles'], accepted_terminal_seal_sha256=TERMINAL_SHA,
                signed_source_commits=builds, cross_build_witness=config.get('cross_build_witness'),
                stake_asset_reduction_blocks=fact_blocks, stake_asset_reduction_fact_rows=sum(e['fact_rows'] for e in fact_blocks),
                stake_asset_reductions_joined_sha256=hashlib.sha256(fact_bytes).hexdigest(), stake_asset_tax_classification='unclassified',
                launch_records=sorted({seal['launch_record'] for _, seal, _ in sealed}))

if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--config', type=Path, required=True)
    parser.add_argument('--repo', type=Path, required=True)
    parser.add_argument('--receipt', type=Path, required=True)
    a = parser.parse_args()
    os.umask(0o077)
    result = verify(json.loads(a.config.read_bytes()), a.repo, facts_path=a.receipt.parent / (a.receipt.name.split('.')[0] + '.native-stake-reductions.joined.private.jsonl'))
    with a.receipt.open('x') as out:
        json.dump(result, out, indent=2, sort_keys=True)
    print('all 500015 historical source/coinflow components reconciled; production admission remains false')
