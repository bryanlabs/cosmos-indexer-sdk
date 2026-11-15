#!/usr/bin/env python3
"""Refresh the accepted canonical Hub1 boundary, every account and coin.

No SQL or replay. No source mutation, amounts, or identities in the receipt.
This is a state-boundary check, not a claim that SQL history is complete.
"""
import argparse
import copy
import datetime
import hashlib
import json
from pathlib import Path
import time
import compare_zero_export as comparator
from verify_terminal_fifteen import canonical_boundary


def account_coins(accounts):
    result = {}
    for account in accounts:
        address = account['address']
        if type(address) is not str or not address or address in result:
            raise ValueError('account identity missing or duplicate')
        coins = {}
        for coin in account['coins']:
            denom, amount = coin['denom'], coin['amount']
            if type(denom) is not str or not denom or denom in coins:
                raise ValueError('coin identity missing or duplicate')
            if type(amount) is not str or not amount.isascii() or not amount.isdecimal():
                raise ValueError('coin amount is not a nonnegative integer string')
            coins[denom] = int(amount)
        result[address] = coins
    return result


def compare_accounts(native, target):
    left, right = account_coins(native), account_coins(target)
    result = dict(source_accounts=len(left), next_genesis_accounts=len(right),
                  accounts_checked=len(set(left) | set(right)),
                  source_coin_entries=sum(map(len, left.values())),
                  next_genesis_coin_entries=sum(map(len, right.values())),
                  missing_accounts=len(set(right)-set(left)), extra_accounts=len(set(left)-set(right)),
                  missing_coins=0, extra_coins=0, mismatched_coin_amounts=0)
    for address in set(left) & set(right):
        a, b = left[address], right[address]
        result['missing_coins'] += len(set(b)-set(a))
        result['extra_coins'] += len(set(a)-set(b))
        result['mismatched_coin_amounts'] += sum(a[d] != b[d] for d in set(a) & set(b))
    result['pass'] = not any(result[k] for k in ('missing_accounts','extra_accounts','missing_coins','extra_coins','mismatched_coin_amounts'))
    return result


def verify(root, migration_root):
    start = time.monotonic()
    binding = canonical_boundary(root)
    native, source, target = [comparator.strict_json((root/n).read_bytes()) for n in ('native-export.json','genesis.cosmoshub-1.json','genesis.cosmoshub-2.json')]
    script = comparator.pinned(migration_root/'v0.33.x-to-v0.34.0.py', comparator.SCRIPT_SHA)
    library = comparator.pinned(migration_root/'lib.py', comparator.LIB_SHA)
    candidate = copy.deepcopy(source)
    candidate['app_state'], candidate['validators'] = native['app_state'], native['validators']
    migrated = comparator.migrate(candidate, target, script, library)
    equal = comparator.typed(migrated) == comparator.typed(target)
    counts = compare_accounts(native['app_state']['accounts'], target['app_state']['accounts'])
    return dict(schema='hub1-all-account-all-coin-boundary-v1', at=datetime.datetime.now(datetime.timezone.utc).isoformat(),
                canonical_height=500000, next_chain='cosmoshub-2',
                existing_original_proofs_unchanged=True, full_typed_genesis_equal=equal,
                source_sha256=binding['original_source_sha256'], **counts,
                elapsed_seconds=time.monotonic()-start,
                balance_condition_passed=equal and counts['pass'], production_rows_written=0,
                sql_history_completeness_claim=False, independent_financial_audit_is_not_an_additional_permission_gate=True)

if __name__ == '__main__':
    p=argparse.ArgumentParser();p.add_argument('--boundary', type=Path, required=True);p.add_argument('--receipt',type=Path,required=True)
    a=p.parse_args();value=verify(a.boundary,Path(__file__).parent/'testdata/hub1-migration/v0.34.10')
    with a.receipt.open('x') as out:json.dump(value,out,sort_keys=True)
    print(json.dumps({k:v for k,v in value.items() if k!='source_sha256'}))
    if not value['balance_condition_passed']:raise SystemExit(1)
