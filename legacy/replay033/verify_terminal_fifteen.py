#!/usr/bin/env python3
"""Fixed-scope native Hub1 tail proof. Never imports or admits financial rows."""
import argparse
import copy
import hashlib
import json
import os
from pathlib import Path
import verify_movement_rehearsal as movement
from compare_zero_export import RELEASE, strict_json, typed, tree_sha
from terminal_journal import Journal

FIRST, LAST = 500001, 500015
MANIFEST = '417424c8075ea3dcfd166eddaad8f5012c2510797abeca7f4f0864d399f0fd86'
GENESIS = '73a866b21723ecbc28b6d15951b2eb3aa2f2443650ff6df489bf55ac5edceefa'
BLOCK = 'a050aed356f1138980ac066b3abf67ab5ffc43c53875a775f3ef2ec31e1f394c'
RESULTS = '8e1fe3fe0221de321d961039c342fb8d21fc21425b52b9993292d7e9f62ff776'
NAMES = {'main', 'acc', 'staking', 'slashing', 'mint', 'distr', 'gov', 'fee', 'params'}
need, integer, lines = movement.require, movement.integer, movement.lines


def proof_check(proof, audit):
    for name, expected in [('schema', 'native033-closed-terminal-v1'), ('kind', 'closed_terminal_history_completed'),
                           ('release', RELEASE), ('chain', 'cosmoshub-1'), ('proof_mode', 'closed_pinned_native_reference'),
                           ('source_manifest_sha256', MANIFEST), ('genesis_sha256', GENESIS),
                           ('terminal_block_capture_sha256', BLOCK), ('terminal_results_capture_sha256', RESULTS)]:
        need(proof.get(name) == expected, 'closed terminal proof identity differs')
    need(integer(proof['first_height']) == FIRST and integer(proof['height']) == LAST and
         proof['next_header_proof'] is False and proof['financial_admission'] is False and
         'unexecuted_next_header' not in proof, 'false terminal coverage/consensus/admission claim')
    ref = proof['closed_reference']
    for name in ('commit', 'all_persistent_stores', 'begin', 'deliver', 'end', 'block_hash'):
        need(typed(ref[name]) == typed(audit[name]), 'complete closed native state/result/header differs')
    need(integer(ref['original_status']['state_hash_verified_through']) == LAST-1 and
         integer(ref['original_status']['last_height_committed']) == LAST and
         integer(ref['original_tail']['height']) == LAST, 'original reference status misrepresented')


def typed_export(native, observed, root):
    need(typed(native) == typed(observed), 'whole typed terminal export differs')
    need(native['release'] == RELEASE and integer(native['source_height']) == LAST and
         native['for_zero_height'] is True and native['terminal_proof_mode'] == 'closed_pinned_native_reference' and
         native['closed_source_manifest_sha256'] == MANIFEST and
         native['source_app_hash'] == movement.blob(root['Hash']).hex(), 'terminal export not bound to native ending state')
    return tree_sha(native)


def financial_context(contexts, raw_lines, audits):
    need(len(contexts) == len(raw_lines) == 16, 'complete financial context required')
    for offset, (context, raw) in enumerate(zip(contexts, raw_lines)):
        h = FIRST-1+offset
        need(integer(context['height']) == h and context['raw_native_sha256'] == hashlib.sha256(raw).hexdigest(),
             'typed financial context not bound to complete raw native state')
        if offset:
            need(context['header_hash'] == audits[offset-1]['block_hash'], 'financial context actual native header differs')
    for before, after in zip(contexts, contexts[1:]):
        # Actual cached tail has no transactions. Reject invented fee, principal,
        # shares, bank income, slash or maturity interpretation instead of silently
        # accepting a new category. Raw unknown-prefix writes remain fully proved.
        for name in ('accounts', 'bank', 'validators', 'delegations', 'unbondings', 'redelegations', 'fees', 'bond_denom'):
            need(typed(before[name]) == typed(after[name]), 'unexplained terminal native financial transition')
        need(typed(after['minter']) == typed(before['next_minter']), 'native exact terminal minter transition differs')
        provision = after['mint_provision']
        need(provision['denom'] == before['bond_denom'], 'native mint asset differs')
        expected = copy.deepcopy(before['pool'])
        expected['not_bonded_tokens'] = str(int(expected['not_bonded_tokens']) + int(provision['amount']))
        need(typed(expected) == typed(after['pool']), 'native terminal mint supply principal differs')
    return {'native_read_contexts': 16, 'native_exact_mint_transitions': 15,
            'all_coin_accounts_and_staking_principal_shares_unchanged': True,
            'ordinary_tail_fees_or_paid_income': False}


def record_check(record, exported):
    h = integer(record['height'])
    need(h == LAST if exported else FIRST <= h <= LAST, 'terminal record outside exact fifteen heights')
    if exported:
        need(record['phase'] == 'zero_height_export' and record['status'] == 'exported', 'terminal export phase/disposition differs')
    else:
        need(record['phase'] in ('begin_block','end_block','block','commit','segment','segment_closed_pinned_native_reference') and
             record['status'] == 'committed' and integer(record['tx']) == integer(record['message']) == -1,
             'terminal ordinary native phase/disposition differs')
        if record['kind'] == 'native_write':
            need(record['phase'] == 'begin_block', 'actual empty-tail source write phase differs')
        if record['kind'] == 'native_cache_write':
            need(record['phase'] == 'commit', 'actual empty-tail source flush phase differs')
        need(record['phase'] not in ('ante', 'message', 'transaction') and
             record['kind'] not in ('movement', 'principal', 'principal_request', 'staking_shares', 'share_request'),
             'invented fee/principal/share/income in actual zero-transaction tail')
    if record['kind'] == 'observation_completed':
        raise ValueError('terminal cannot use next-header completion marker')
    if record['kind'] == 'observation_closed_terminal_completed':
        need(not exported and record['phase'] == 'segment_closed_pinned_native_reference', 'terminal mode marker differs')
        record = dict(record, kind='observation_completed')
    return record


def stream_check(path, states, txs, baseline, exported=False):
    journal = Journal(exported)
    def records(unused):
        for original in lines(path):
            record = record_check(original, exported)
            journal.accept(record)
            if record['kind'] not in ('native_write', 'native_cache_write'):
                yield record
    previous = movement.lines
    movement.lines = records
    try:
        result = movement.verify_stream(path, states, txs, exported=exported, baseline=baseline, export_height=LAST if exported else None)
    finally:
        movement.lines = previous
    need(journal.heights == (0 if exported else 15), 'complete source-write journal coverage differs')
    result['native_source_write_journal'] = dict(journal.counts)
    return result


def canonical_boundary(directory):
    root = Path(directory)
    pins = {'complete-genesis-comparison-signed-final.json':'3ccdc373b1af7327dc9e68bf866e9df62082dc3ee2ef537d77f01746ea3ff57b',
            'checkpoint-proof.json':'321fafeefb17f3b0b650e4c066afb100ad33c3237993968222c77c5d10336bec',
            'native-export.json':'f191855b5291fad621eb1267049fcb29e845bf801fb3035a6c6f0e791e7918ab',
            'genesis.cosmoshub-1.json':GENESIS,
            'genesis.cosmoshub-2.json':'1e349fb39b85f7707ee78d39879f9d5d61f4d30f67980bb0bf07bd35b2f8bf30'}
    objects = {}
    for name, pin in pins.items():
        raw = (root/name).read_bytes()
        need(hashlib.sha256(raw).hexdigest() == pin, 'accepted canonical boundary original bytes differ')
        objects[name] = strict_json(raw)
    native, proof, receipt = (objects[name] for name in ('native-export.json','checkpoint-proof.json','complete-genesis-comparison-signed-final.json'))
    need(integer(native['source_height']) == 500000 and integer(proof['checkpoint_height']) == 500000 and
         integer(proof['next_header_height']) == 500001 and integer(proof['state_hash_verified_through']) == 500000 and
         native['source_app_hash'] == proof['application_hash'] == receipt['source_app_hash'] and
         receipt['full_genesis_tree_equal'] is True and receipt['all_fields_compared'] is True and
         integer(receipt['difference_count']) == 0 and receipt['native_export_sha256'] == pins['native-export.json'] and
         receipt['checkpoint_proof_sha256'] == pins['checkpoint-proof.json'] and
         receipt['next_genesis_sha256'] == pins['genesis.cosmoshub-2.json'], 'canonical migration source binding differs')
    return {'canonical_migration_height':500000, 'retained_tail_recorded_in_full':[FIRST,LAST],
            'terminal_export_is_next_chain_opening':False, 'existing_proof_reused_without_reexecution':True,
            'original_source_sha256':pins}


def verify(directory, boundary):
    root = Path(directory)
    source = strict_json((root/'native-audit.ndjson.source-checkpoint.json').read_bytes())
    observed_source = strict_json((root/'observed-audit.ndjson.source-checkpoint.json').read_bytes())
    movement.validate_audit(source, FIRST-1)
    need(typed(source) == typed(observed_source), 'complete starting native states differ')
    txs = movement.audit_compare(root/'native-audit.ndjson', root/'observed-audit.ndjson', FIRST, LAST)
    need(txs == {}, 'new terminal transaction requires explicit financial decomposition')
    audits = list(lines(root/'native-audit.ndjson'))
    for row in audits:
        need(row['transactions'] == [] and row['deliver'] is None, 'actual terminal empty transaction/result shape differs')
    raw_lines = (root/'native-all-store-values.ndjson').read_bytes().splitlines(keepends=True)
    states = {}
    for raw in raw_lines:
        need(raw.endswith(b'\n'), 'partial raw native state')
        row = strict_json(raw)
        h = integer(row['height'])
        names = [store['name'] for store in row['stores']]
        need(h not in states and len(names) == 9 and set(names) == NAMES, 'complete native state identity differs')
        states[h] = {store['name']: movement.state_values(store['values']) for store in row['stores']}
    need(list(states) == list(range(FIRST-1, LAST+1)), 'every source and terminal height required')
    for mode in ('native', 'observed'):
        proof = strict_json((root/(mode+'-terminal-proof.json')).read_bytes())
        proof_check(proof, audits[-1])
        history = list(lines(root/(mode+'-history.ndjson')))
        need(len(history) == 17 and history[0]['kind'] == 'source_anchor' and
             integer(history[0]['height']) == FIRST-1, 'complete native history required')
        need(typed(history[-1]) == typed(proof), 'terminal history seal not bound to final proof')
        for height, row, audit in zip(range(FIRST, LAST+1), history[1:-1], audits):
            need(row['kind'] == 'height' and integer(row['height']) == height, 'native history lost final height')
            for field in ('release', 'chain', 'height', 'block_hash', 'commit', 'transactions', 'begin', 'deliver', 'end'):
                need(typed(row[field]) == typed(audit[field]), 'complete native history result differs')
        status = strict_json((root/(mode+'-home')/'status.json').read_bytes())
        need(integer(status['last_height_committed']) == LAST and integer(status['state_hash_verified_through']) == LAST-1 and
             integer(status['closed_reference_verified_through']) == LAST and
             status['terminal_proof_mode'] == 'closed_pinned_native_reference', 'terminal status misrepresents consensus proof')
    native = strict_json((root/'native-export.json').read_bytes())
    observed = strict_json((root/'observed-export.json').read_bytes())
    result = dict(scope='Hub1 ALL500001..500015, independent closed native reference, no500016',
                  financial_admission=False, tax_acceptance=False,
                  replay=stream_check(root/'observed-movements.ndjson', {h:states[h] for h in range(FIRST, LAST+1)}, txs, states[FIRST-1]),
                  export=stream_check(root/'observed-export-movements.ndjson', None, None, states[LAST], True),
                  typed_terminal_export_sha256=typed_export(native, observed, audits[-1]['commit']),
                  financial_context=financial_context(list(lines(root/'native-financial-context.ndjson')), raw_lines, audits),
                  chain_boundary=canonical_boundary(boundary))
    need((root/'source-before.private.json').read_bytes() == (root/'source-after.private.json').read_bytes(), 'immutable sources changed')
    execution = json.loads((root/'execution-complete.private.json').read_text())
    need(execution['source_unchanged'] is True and len(execution['runs']) == 6 and
         all(row['exit'] == 0 for row in execution['runs']), 'new native executions/decoders not complete')
    result['inputs_sha256'] = {p.name:hashlib.sha256(p.read_bytes()).hexdigest() for p in root.iterdir()
                               if p.is_file() and p.suffix in ('.ndjson', '.json')}
    return result


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--directory', required=True)
    parser.add_argument('--receipt', required=True)
    parser.add_argument('--canonical-boundary', required=True)
    args = parser.parse_args()
    os.umask(0o077)
    result = verify(args.directory, args.canonical_boundary)
    with open(args.receipt, 'x') as stream:
        json.dump(result, stream, indent=2, sort_keys=True)
        stream.write('\n')
    print('complete fifteen-height native raw/financial/typed-terminal component verified; admission false')
