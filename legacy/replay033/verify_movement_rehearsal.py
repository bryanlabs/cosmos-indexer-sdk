#!/usr/bin/env python3
"""Complete bounded native033 evidence verification. No tax-row admission."""
import argparse
import base64
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
from compare_zero_export import RELEASE, Number, strict_json, typed, tree_sha

CLASSES = {
    'ante_fee':'signed_ante_fee',
    'delegator_reward/bank.add':'delegator_reward_payout',
    'distribution/delegation_reward/bank.add':'delegator_reward_payout',
    'validator_commission/bank.add':'validator_commission_payout',
    'distribution/validator_commission/bank.add':'validator_commission_payout',
    'distribution/removed_validator_commission/bank.add':'validator_commission_payout',
    'staking/delegate/bank.delegate':'staking_principal_debit',
    'staking/create_validator/bank.delegate':'staking_principal_debit',
    'staking/unbonding_complete/bank.undelegate':'unbonding_principal_return',
    'staking/undelegate/bank.undelegate':'immediate_unbond_principal_return',
    'bank/send/bank.send':'bank_transfer',
    'bank/multisend/bank.input_output':'bank_transfer',
    'gov/gov/submit_proposal/bank.send':'governance_deposit',
    'gov/gov/deposit/bank.send':'governance_deposit',
    'gov/deposit_refund/bank.send':'governance_deposit_refund',
    'gov/deposit_burn/bank.send':'governance_deposit_burn',
}


def require(ok, reason):
    if not ok:
        raise ValueError(reason)


def integer(value):
    require(type(value) is Number and str(int(value)) == value, 'invalid native JSON integer')
    return int(value)


def native_query_height(value):
    # Pinned ResponseQuery.MarshalJSON uses jsonpb int64 STRINGS. The whole
    # typed audit comparison retains this exact native scalar type unchanged.
    require(type(value) is str and str(int(value)) == value and 0 < int(value) < 2**63,
            'invalid pinned native jsonpb height string')
    return int(value)


def lines(path):
    with Path(path).open('rb') as stream:
        for raw in stream:
            require(raw.endswith(b'\n'), 'partial evidence line')
            yield strict_json(raw)


def blob(value):
    require(type(value) is str, 'native raw bytes must be base64 strings')
    result = base64.b64decode(value, validate=True)
    require(base64.b64encode(result).decode() == value, 'noncanonical native bytes')
    return result


def state_values(values):
    require(type(values) is list, 'complete native state must be array')
    state, previous = {}, None
    for pair in values:
        require(type(pair) is dict and set(pair) <= {'key','value'} and 'key' in pair,
                'unknown raw native pair field')
        key = blob(pair['key'])
        require(key and (previous is None or previous < key), 'native keys not ordered unique complete keys')
        # Native amino KVPair omits empty bytes in JSON, NOT the present key.
        value = pair.get('value','')
        blob(value)
        state[key], previous = value, key
    return state


def commitment(state):
    pairs = [{'key':base64.b64encode(key).decode(),'value':state[key]} for key in sorted(state)]
    raw = (json.dumps(pairs,separators=(',',':'))+'\n').encode()
    return {'sha256':hashlib.sha256(raw).hexdigest(),'keys':len(state)}


def check_commitment(value,state):
    require(type(value) is dict and set(value) == {'sha256','keys'}, 'incomplete store commitment')
    expected = commitment(state)
    require(value['sha256'] == expected['sha256'] and integer(value['keys']) == expected['keys'],
            'complete native state commitment differs')


def validate_audit(record,height):
    require(record['release'] == RELEASE and record['chain'] == 'cosmoshub-1' and
            integer(record['height']) == height, 'native audit provenance differs')
    stores = record['all_persistent_stores']
    require(type(stores) is list and len(stores) == 9 and len({s['name'] for s in stores}) == 9,
            'all nine native stores required')
    for store in stores:
        parts = store['partitions']
        require(type(parts) is list and len(parts) == 256, 'all native prefixes required')
        for prefix,response in enumerate(parts):
            require(integer(response.get('code',Number('0'))) == 0 and
                    native_query_height(response['height']) == height and blob(response['key']) == bytes([prefix]),
                    'native prefix query identity differs')


def audit_compare(native_path,observed_path,first,target):
    native,observed = list(lines(native_path)),list(lines(observed_path))
    require(len(native) == len(observed) == target-first+1, 'whole audit coverage differs')
    txs = {}
    for offset,(left,right) in enumerate(zip(native,observed)):
        require(typed(left) == typed(right), 'whole native response/state audit differs')
        height = first+offset
        validate_audit(left,height)
        responses = left['deliver']
        if responses is None:
            require(left['transactions'] == [], 'null native responses with transactions')
            responses = []
        require(type(left['transactions']) is list and type(responses) is list and
                len(left['transactions']) == len(responses), 'native response array coverage differs')
        for index,(raw,response) in enumerate(zip(left['transactions'],responses)):
            txs[(height,index)] = (raw,response)
    return txs


def verify_stream(path,expected_states=None,expected_txs=None,exported=False,baseline=None,export_height=None):
    require(type(baseline) is dict and len(baseline) == 9, 'native-anchored complete baseline required')
    current,latest = baseline.copy(),baseline.copy()
    block_start,tx_start,ante_end,message_tip,message_index = {},{},{},{},{}
    baseline_seen,final_seen,export_root = set(),set(),set()
    scopes,versions = {},set()
    pending,final = None,False
    counters,classes,attempted_classes = Counter(),Counter(),Counter()
    tx_evidence,completed = set(),[]

    def finish():
        nonlocal pending
        if pending is None:
            return
        store,r,changes = pending
        h,tx,msg = integer(r['height']),integer(r['tx']),integer(r['message'])
        phase,identity = r['phase'],(h,tx,store)
        require(store in baseline_seen, 'store scope precedes anchored baseline')
        scope = (phase,h,tx,msg,r.get('validator',''),r.get('delegator',''))
        require(store not in scopes.setdefault(scope,set()), 'duplicate native scope/store')
        scopes[scope].add(store)
        root_export = exported and not r.get('validator') and not r.get('delegator')
        if exported:
            require(phase == 'zero_height_export' and store not in export_root, 'invalid export native scope')
            before = baseline[store] if root_export else current[store]
        elif phase == 'begin_block':
            require((h,store) not in block_start, 'duplicate native begin scope')
            before = current[store]
            block_start[(h,store)] = before
        elif phase == 'ante':
            require(tx >= 0 and msg == -1 and identity not in tx_start, 'invalid ante scope order')
            before = current[store]
            tx_start[identity] = before
        elif phase == 'message':
            require(tx >= 0 and identity in ante_end and msg == message_index.get(identity,-1)+1,
                    'message has no native ante predecessor or ordered index')
            before = message_tip.get(identity,ante_end[identity])
            message_index[identity] = msg
        elif phase == 'transaction':
            require(tx >= 0 and msg == -1 and (h,tx) in expected_txs, 'invalid native transaction scope')
            before = tx_start.get(identity,current[store])
        elif phase == 'end_block':
            before = current[store]
        elif phase == 'block':
            require((h,store) in block_start, 'native block lacks begin predecessor')
            before = block_start[(h,store)]
        else:
            raise ValueError('unsupported native scope, decompose before acceptance')
        check_commitment(r['before'],before)
        state,seen = before.copy(),set()
        for change in changes:
            key = blob(change['label'].split('/key:',1)[1])
            require(key not in seen, 'duplicate native key change')
            seen.add(key)
            for field in ('height','tx','message','phase','status','validator','delegator'):
                require(change.get(field) == r.get(field), 'native raw effect scope differs')
            require((state.get(key) if key in state else None) == change['before'], 'raw before-value differs')
            if change['after'] is None:
                require(key in state, 'deletion of unknown key')
                del state[key]
            else:
                blob(change['after'])
                state[key] = change['after']
        check_commitment(r['after'],state)
        versions.add((store,r['after']['sha256']))
        if exported:
            current[store] = state
            if root_export:
                export_root.add(store)
                latest[store] = state
        elif phase == 'ante':
            ante_end[identity] = state if r['status'] == 'committed' else before
        elif phase == 'message':
            message_tip[identity] = state
        elif phase == 'transaction':
            code = integer(expected_txs[(h,tx)][1].get('code',Number('0')))
            retained = message_tip.get(identity,ante_end.get(identity,before)) if code == 0 else ante_end.get(identity,before)
            require(state == retained, 'native transaction rollback/retention state differs')
            current[store] = state
        elif phase == 'block':
            require(state == current[store], 'block aggregate differs from actual phase predecessor chain')
            latest[store] = state
        else:
            current[store] = state
        pending = None

    for r in lines(path):
        require(not final, 'data follows native completion')
        kind,h = r['kind'],integer(r['height'])
        require(h == export_height if exported else h in expected_states, 'record outside native source scope')
        require(r['status'] in ('exported',) if exported else r['status'] in ('committed','aborted'), 'invalid disposition')
        counters[kind] += 1
        if kind != 'native_state':
            finish()
        if kind == 'native_state_checkpoint':
            label,store = r['label'].split('/store:',1)
            store = store.split(':',1)[0]
            allowed = ('export_baseline','export_final') if exported else ('segment_baseline','segment_final')
            require(label in allowed and store in baseline, 'unsupported native checkpoint label/store')
            state = state_values(r['before'])
            if label == allowed[0]:
                require(store not in baseline_seen and state == baseline[store], 'baseline differs from untouched native state')
                baseline_seen.add(store)
            else:
                require(store not in final_seen and store in baseline_seen and state == latest[store], 'final checkpoint differs from reconstructed native state')
                final_seen.add(store)
        elif kind == 'native_state_hash':
            require(r['unit'] == 'native_store_sha256', 'unknown native commitment units')
            pending = (r['label'].split(':',2)[1],r,[])
        elif kind == 'native_state':
            require(pending is not None and r['label'].split('/key:',1)[0] == pending[1]['label'] and
                    r['unit'] == 'native_store_bytes', 'raw effect lacks enclosing native commitment')
            pending[2].append(r)
        elif kind == 'movement':
            label = r['label']
            require(label in CLASSES and r.get('classification') == CLASSES[label] and
                    r.get('unit') == 'native_bank_tokens', 'cause/classification/units differ')
            amount = r['amount']
            require(type(amount) is str and str(int(amount)) == amount, 'noncanonical token integer')
            if label == 'ante_fee':
                require(r['phase'] == 'ante' and integer(r['tx']) >= 0 and integer(r['message']) == -1 and int(amount) <= 0,
                        'signed fee cannot become payout income')
            else:
                require(type(r['before']) is str and type(r['after']) is str and
                        str(int(r['before'])) == r['before'] and str(int(r['after'])) == r['after'] and
                        min(int(r['before']),int(r['after'])) >= 0 and
                        str(int(r['after'])-int(r['before'])) == amount, 'bank before/after payout differs')
            (attempted_classes if r['status'] == 'aborted' else classes)[r['classification']] += 1
        elif kind == 'staking_shares':
            require(r['unit'] == 'delegation_shares' and r['denom'] == '', 'shares are not tokens')
        elif kind == 'native_execution' and r['phase'] == 'transaction':
            identity = (h,integer(r['tx']))
            require(identity in expected_txs and identity not in tx_evidence, 'native transaction provenance differs')
            raw,response = expected_txs[identity]
            require(r['before'] == raw and typed(r['after']) == typed(response), 'signed bytes/full typed native response differ')
            tx_evidence.add(identity)
        elif kind == 'height_completed':
            require(h not in completed and latest == expected_states[h], 'whole committed state differs from untouched native Gaia')
            completed.append(h)
        elif kind in ('observation_completed','export_completed'):
            require(kind == ('export_completed' if exported else 'observation_completed'), 'wrong native completion kind')
            final = True
        elif kind not in ('accounting','principal','principal_request','share_request','native_execution'):
            raise ValueError('unsupported record kind, decompose before acceptance')
    finish()
    require(final and baseline_seen == final_seen == set(baseline) and
            all(stores == set(baseline) for stores in scopes.values()), 'incomplete native baseline/final/scope coverage')
    if exported:
        require(export_root == set(baseline), 'missing whole export state scope')
    else:
        require(completed == sorted(expected_states) and tx_evidence == set(expected_txs), 'whole height/transaction coverage differs')
    return {'records':dict(counters),'classifications':dict(classes),
            'aborted_attempt_classifications':dict(attempted_classes),'native_state_versions':len(versions)}


def verify(directory,receipt):
    root = Path(directory)
    source = strict_json((root/'native-audit.ndjson.source-checkpoint.json').read_bytes())
    other = strict_json((root/'observed-audit.ndjson.source-checkpoint.json').read_bytes())
    require(typed(source) == typed(other), 'whole native source checkpoint audit differs')
    validate_audit(source,400000)
    txs = audit_compare(root/'native-audit.ndjson',root/'observed-audit.ndjson',400001,400026)
    states = {}
    for r in lines(root/'native-all-store-values.ndjson'):
        h = integer(r['height'])
        require(h not in states and len(r['stores']) == 9, 'native decoded-state coverage differs')
        states[h] = {s['name']:state_values(s['values']) for s in r['stores']}
    require(sorted(states) == list(range(400000,400027)), 'entire native initial and committed state segment required')
    result = {'scope':'Hub1 complete400001..400026, immutable400000 source, unexecuted400027',
              'financial_admission':False,'tax_acceptance':False,
              'observed_stream':verify_stream(root/'observed-movements.ndjson',
                  {h:states[h] for h in range(400001,400027)},txs,baseline=states[400000]),
              'observed_export':verify_stream(root/'observed-export-movements.ndjson',exported=True,
                  baseline=states[400026],export_height=400026)}
    native = strict_json((root/'native-export.json').read_bytes())
    observed = strict_json((root/'observed-export.json').read_bytes())
    require(typed(native) == typed(observed), 'entire typed zero-height export differs')
    result['complete_native_export_tree_sha256'] = tree_sha(native)
    names = ('native-audit.ndjson','observed-audit.ndjson','native-export.json','observed-export.json',
             'observed-movements.ndjson','observed-export-movements.ndjson','native-all-store-values.ndjson',
             'native-audit.ndjson.source-checkpoint.json','observed-audit.ndjson.source-checkpoint.json')
    result['inputs_sha256'] = {name:hashlib.sha256((root/name).read_bytes()).hexdigest() for name in names}
    with open(receipt,'x',opener=lambda p,f:os.open(p,f,0o600)) as stream:
        json.dump(result,stream,sort_keys=True,indent=2)
        stream.write('\n')
    return result


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--directory',required=True)
    parser.add_argument('--receipt',required=True)
    args = parser.parse_args()
    try:
        verify(args.directory,args.receipt)
    except (ValueError,KeyError,TypeError,IndexError,OSError):
        raise SystemExit('bounded native movement verification refused; preserve private inputs')
    print('complete bounded native state/response/movement proof verified; financial admission false')
