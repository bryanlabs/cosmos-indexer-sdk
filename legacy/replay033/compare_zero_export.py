#!/usr/bin/env python3
"""Compare the entire native Hub 1 export against the pinned Hub 2 genesis.

Only the unmodified, SHA256-pinned public SDK migration is applied. Object
key ordering is ignored; array positions, scalar types and numeric values
are exact, without floating-point rounding or a raw-byte serialization gate. This component does not admit accounting records or tax results.
"""
import argparse
import copy
from decimal import Decimal
import hashlib
import json
import os
from pathlib import Path
import sys
import types

SOURCE_SHA = '73a866b21723ecbc28b6d15951b2eb3aa2f2443650ff6df489bf55ac5edceefa'
NEXT_SHA = '1e349fb39b85f7707ee78d39879f9d5d61f4d30f67980bb0bf07bd35b2f8bf30'
SCRIPT_SHA = '143541654f475ae494abf1a6cef39a987963697b1559454e2de7e1a3a737d29b'
LIB_SHA = '79162c0292de87a338ec60fdad2926fa5cee677ffc83815a6bbbb656d909cbb4'
RELEASE = 'cosmos-sdk v0.33.2 (gaia), tendermint v0.31.0-dev0-fix0, iavl v0.12.1'


class Number(str):
    """Retain numeric-token spelling separately from JSON strings."""


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('duplicate JSON object key')
        result[key] = value
    return result


def reject_constant(_):
    raise ValueError('nonfinite JSON token')


def strict_json(raw):
    return json.loads(raw, object_pairs_hook=unique_object, parse_int=Number,
                      parse_float=Number, parse_constant=reject_constant)


def typed(value):
    if value is None:
        return ['null']
    if type(value) is bool:
        return ['boolean', value]
    if type(value) is Number:
        sign, digits, exponent = Decimal(value).as_tuple()
        text = ''.join(str(digit) for digit in digits)
        if not text.strip('0'):
            return ['number', 0, '0', 0]
        compact = text.rstrip('0')
        return ['number', sign, compact, exponent + len(text) - len(compact)]
    if type(value) is str:
        return ['string', value]
    if type(value) is list:
        return ['array', [typed(item) for item in value]]
    if type(value) is dict:
        return ['object', [[key, typed(value[key])] for key in sorted(value)]]
    raise ValueError('unrecognized JSON scalar type')


def tree_sha(value):
    raw = json.dumps(typed(value), ensure_ascii=True, separators=(',', ':')).encode()
    return hashlib.sha256(raw).hexdigest()


def differences(left, right, path=''):
    if type(left) is not type(right):
        return [{'path': path, 'kind': 'type', 'native_tree_sha256': tree_sha(left), 'next_tree_sha256': tree_sha(right)}]
    if type(left) is dict:
        result = []
        for key in sorted(set(left) | set(right)):
            child = path + '/' + key.replace('~', '~0').replace('/', '~1')
            if key not in left or key not in right:
                side = left if key in left else right
                result.append({'path': child, 'kind': 'missing_next' if key in left else 'missing_native', 'subtree_sha256': tree_sha(side[key])})
            else:
                result.extend(differences(left[key], right[key], child))
        return result
    if type(left) is list:
        result = []
        for i in range(max(len(left), len(right))):
            child = path + '/' + str(i)
            if i >= len(left) or i >= len(right):
                side = left if i < len(left) else right
                result.append({'path': child, 'kind': 'missing_next' if i < len(left) else 'missing_native', 'subtree_sha256': tree_sha(side[i])})
            else:
                result.extend(differences(left[i], right[i], child))
        return result
    if typed(left) != typed(right):
        return [{'path': path, 'kind': 'value', 'native_tree_sha256': tree_sha(left), 'next_tree_sha256': tree_sha(right)}]
    return []


def pinned(path, expected):
    raw = Path(path).read_bytes()
    if hashlib.sha256(raw).hexdigest() != expected:
        raise ValueError('pinned public input checksum differs')
    return raw


def migrate(genesis, target, script, library):
    module = types.ModuleType('lib')
    exec(compile(library, '<pinned-sdk-migration-lib>', 'exec'), module.__dict__)
    previous = sys.modules.get('lib')
    sys.modules['lib'] = module
    try:
        namespace = {'__name__': 'pinned_sdk_migration'}
        exec(compile(script, '<pinned-sdk-migration>', 'exec'), namespace)
        args = types.SimpleNamespace(chain_id=target['chain_id'], start_time=target['genesis_time'])
        return namespace['process_raw_genesis'](genesis, args)
    finally:
        if previous is None:
            sys.modules.pop('lib', None)
        else:
            sys.modules['lib'] = previous


def compare(args):
    source_raw = pinned(args.source_genesis, SOURCE_SHA)
    next_raw = pinned(args.next_genesis, NEXT_SHA)
    native_raw = Path(args.native_export).read_bytes()
    source, target, native = map(strict_json, (source_raw, next_raw, native_raw))
    required = {'release', 'source_height', 'source_app_hash', 'for_zero_height', 'app_state', 'validators'}
    if type(native) is not dict or set(native) != required:
        raise ValueError('unexpected complete native export envelope')
    if native['release'] != RELEASE or type(native['source_height']) is not Number or native['source_height'] != '500000' or native['for_zero_height'] is not True:
        raise ValueError('native source provenance differs')
    proof_raw = Path(args.checkpoint_proof).read_bytes()
    proof = strict_json(proof_raw)
    if type(proof) is not dict or proof.get('kind') != 'checkpoint_next_header_only' or proof.get('release') != RELEASE or proof.get('chain') != 'cosmoshub-1':
        raise ValueError('independent checkpoint proof identity differs')
    for key, expected in [('checkpoint_height', '500000'), ('state_hash_verified_through', '500000'), ('next_header_height', '500001')]:
        if type(proof.get(key)) is not Number or proof[key] != expected:
            raise ValueError('independent checkpoint proof boundary differs')
    source_hash = native['source_app_hash']
    if type(source_hash) is not str or len(source_hash) != 64 or any(c not in '0123456789abcdef' for c in source_hash) or source_hash != proof.get('application_hash'):
        raise ValueError('native export does not bind to the proven checkpoint root')
    if type(native['app_state']) is not dict or type(native['validators']) is not list:
        raise ValueError('native export state or validator array is absent')
    if Path(args.receipt).parent.resolve() != Path(args.native_export).parent.resolve():
        raise ValueError('private receipt must be beside the native export')
    script = pinned(args.migration_script, SCRIPT_SHA)
    library = pinned(args.migration_lib, LIB_SHA)
    candidate = copy.deepcopy(source)
    candidate['app_state'] = native['app_state']
    candidate['validators'] = native['validators']
    migrated = migrate(candidate, target, script, library)
    delta = differences(migrated, target)
    receipt = {
        'component': 'complete-native-hub1-export-to-hub2-genesis',
        'source_genesis_sha256': SOURCE_SHA, 'next_genesis_sha256': NEXT_SHA,
        'native_export_sha256': hashlib.sha256(native_raw).hexdigest(),
        'checkpoint_proof_sha256': hashlib.sha256(proof_raw).hexdigest(),
        'source_app_hash': source_hash,
        'migration_script_sha256': SCRIPT_SHA, 'migration_lib_sha256': LIB_SHA,
        'migrated_native_typed_tree_sha256': tree_sha(migrated),
        'next_genesis_typed_tree_sha256': tree_sha(target),
        'all_fields_compared': True, 'array_positions_exact': True,
        'scalar_types_and_exact_numeric_values': True,
        'duplicate_keys_rejected': True, 'full_genesis_tree_equal': not delta,
        'difference_count': len(delta), 'differences': delta,
        'accounting_or_tax_acceptance': False,
    }
    fd = os.open(args.receipt, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w') as handle:
        json.dump(receipt, handle, sort_keys=True, separators=(',', ':'))
        handle.write('\n')
        handle.flush()
        os.fsync(handle.fileno())
    print(json.dumps({'full_genesis_tree_equal': not delta, 'difference_count': len(delta), 'accounting_or_tax_acceptance': False}))
    return 0 if not delta else 1


def main():
    default = Path(__file__).resolve().parent / 'testdata' / 'hub1-migration'
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-genesis', required=True)
    parser.add_argument('--next-genesis', required=True)
    parser.add_argument('--native-export', required=True)
    parser.add_argument('--checkpoint-proof', required=True)
    parser.add_argument('--receipt', required=True)
    parser.add_argument('--migration-script', default=str(default / 'v0.33.x-to-v0.34.0.py'))
    parser.add_argument('--migration-lib', default=str(default / 'lib.py'))
    try:
        return compare(parser.parse_args())
    except (OSError, ValueError, TypeError, KeyError, ArithmeticError):
        print('boundary comparison stopped; preserved inputs and failed artifacts are not acceptance', file=sys.stderr)
        return 2


if __name__ == '__main__':
    raise SystemExit(main())
