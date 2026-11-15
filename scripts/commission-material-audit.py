#!/usr/bin/env python3
"""Offline, private cash/source and route audit. Never mutates source or accepts finances."""
import argparse
import datetime
import importlib.util
import os
from collections import defaultdict, deque
from decimal import Decimal
import hashlib
import json
from pathlib import Path
import re

ZERO = Decimal(0)
NATIVE = 'uatom'
_spec = importlib.util.spec_from_file_location('aggregate_proof', Path(__file__).with_name('commission-proof-audit.py'))
_proof = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_proof)

def read(path):
    return json.loads(path.read_text())

def lines(path):
    return [json.loads(x) for x in path.read_text().splitlines() if x] if path.exists() else []

def coins(value):
    result = defaultdict(Decimal)
    for part in value.split(','):
        match = re.fullmatch(r'([0-9]+)([^0-9,][^,]*)', part.strip())
        if not match:
            raise ValueError('invalid complete native coin string')
        result[match[2]] += Decimal(match[1])
    return dict(result)

def utc_day(value):
    return datetime.datetime.fromisoformat(value.replace('Z', '+00:00')).astimezone(datetime.timezone.utc).date().isoformat()

def native_cells(rows, address, source):
    events = defaultdict(list)
    for row in rows:
        events[row['EventID']].append(row)
    result = defaultdict(Decimal)
    messages = {}
    for event_id, attrs in events.items():
        head = attrs[0]
        kind = head['EventType']
        key = 'receiver' if kind == 'coin_received' else 'spender' if kind == 'coin_spent' else None
        if key is None:
            raise ValueError('unexpected native bank event')
        actors = [a['Value'] for a in attrs if a['Key'] == key]
        values = [a['Value'] for a in attrs if a['Key'] == 'amount']
        if actors != [address] or len(values) != 1:
            raise ValueError('native actor/amount uniqueness not established')
        day = utc_day(head['Timestamp'])
        message = head.get('MessageID', 0)
        identity = (source, message if message else event_id)
        messages[identity] = head.get('MessageType', 'block:' + kind)
        for denom, amount in coins(values[0]).items():
            result[(day, source, identity[1], denom)] += amount if kind == 'coin_received' else -amount
    return dict(result), messages

def flow_cash(events, address):
    """Independent liquid projection, not the attribution/pool engine."""
    result = defaultdict(Decimal)
    categories = defaultdict(set)
    bids = defaultdict(Decimal)
    for event in events:
        category, amount = event['Category'], Decimal(event['Amount'])
        incoming = event['Dir'] != 'out'
        delta = amount if incoming else -amount
        if category == 'transfer' and event['From'] == event['To']:
            delta = ZERO
        elif category in ('undelegate', 'redelegate', 'cancel_unbonding', 'tokenize_shares', 'redeem_shares', 'gov_deposit_burn'):
            delta = ZERO
        elif category == 'delegate':
            delta = -amount
        elif category == 'unbonding_complete':
            delta = amount
        elif category in ('protocol_withdrawal', 'gov_deposit_refund'):
            delta = amount
            if event.get('Trigger') == 'auction_refund' and event.get('Lot'):
                bids[event['Lot']] = max(ZERO, bids[event['Lot']] - amount)
        elif category in ('protocol_deposit', 'gov_deposit'):
            if not incoming and event.get('Trigger') == 'auction_bid' and event.get('Lot'):
                bids[event['Lot']] += amount
        elif category == 'nft_sale' and not incoming:
            try:
                detail = json.loads(event.get('Detail') or '{}')
            except json.JSONDecodeError:
                detail = {}
            if detail.get('sale') == 'auction' and detail.get('protocol') == 'Stargaze':
                paid = min(amount, bids[event.get('Lot', '')])
                bids[event.get('Lot', '')] -= paid
                delta = -(amount - paid)
        key = (utc_day(event['Key']['Time']), 'message' if event['Key']['Message'] else 'block', event['Key']['Message'] or event['Key']['Height'], NATIVE)
        if category == 'fee':
            key = (key[0], 'fee', 0, NATIVE)
            delta = -amount
        result[key] += delta
        categories[key].add(category)
    return dict(result), categories

def route_paths(directory, targets):
    identity = read(directory / 'identity.private.json')
    edges = lines(directory / 'tagged-routes.private.jsonl')
    # Each path carries the SAME actual origin and allocation rule on every hop.
    graphs = defaultdict(lambda: defaultdict(list))
    for index, edge in enumerate(edges):
        if edge['error'] != '<nil>' or not edge['destination_loaded']:
            continue
        for rule, parts in enumerate(edge['parts']):
            for part in parts or []:
                if part['Origin'] and Decimal(part['Amount']) > ZERO:
                    graphs[(rule, part['Origin'].removesuffix('+unbonded'))][edge['from']].append((edge['event']['To'], index))
    def event_key(event):
        key = event['Key']
        return (datetime.datetime.fromisoformat(key['Time'].replace('Z', '+00:00')), key.get('Height', 0), key.get('Message', 0), key.get('Sub', 0))
    seeds = defaultdict(dict)
    for root in identity['roots']:
        path = directory / (hashlib.sha256(root.encode()).hexdigest() + '.events.private.json')
        if not path.exists():
            continue
        for event in read(path):
            if event['Category'] == 'commission' and event['Validator'] == identity['validator'] and Decimal(event['Amount']) > ZERO:
                month = event['Key']['Time'][:7]
                key = event_key(event)
                if root not in seeds[month] or key < seeds[month][root]:
                    seeds[month][root] = key
    proofs = {}
    for label, target in targets.items():
        found = []
        for (rule, origin), graph in graphs.items():
            queue = deque((root, [], key) for root, key in seeds[origin].items())
            seen = dict(seeds[origin])
            while queue:
                node, path, earliest = queue.popleft()
                if node == target:
                    found.append({'rule': rule, 'origin': origin, 'edge_indexes': path})
                    break
                for dest, index in graph[node]:
                    key = event_key(edges[index]['event'])
                    if key >= earliest and (dest not in seen or key < seen[dest]):
                        seen[dest] = key
                        queue.append((dest, path + [index], key))
        proofs[label] = {'member_proven': bool(found), 'paths': found}
    return {'edges': len(edges), 'targets': proofs, 'financially_accepted': False}

def audit(root, targets=None):
    findings = []
    wallets = []
    source_hashes = {}
    native_coin_totals = defaultdict(Decimal)
    routes = {}
    comparisons = {}
    for path in sorted(root.glob('archive-compare-*/*.archive-compare.private.json')):
        comparison = read(path)
        comparisons[comparison['address']] = (comparison, path)
    for directory in sorted(root.glob('validator-*')):
        openings = {x['Address']: x for x in lines(directory / 'openings.private.jsonl')}
        identity = read(directory / 'identity.private.json')
        archive_by_hash = defaultdict(list)
        evidence_path = directory / 'evidence.private.json'
        if evidence_path.exists():
            for receipt in read(evidence_path).get('Archive') or []:
                archive_by_hash[receipt['SHA256']].append(receipt)
        native_paths = sorted(directory.glob('*.native-cash.private.json'))
        native_addresses = {read(path)['address'] for path in native_paths}
        for address in sorted(set(openings) - native_addresses):
            findings.append({'wallet': hashlib.sha256(address.encode()).hexdigest(), 'kind': 'wallet_native_capture_pending_or_failed'})
        if not (directory / 'trace-result.private.json').exists():
            findings.append({'validator': directory.name, 'kind': 'trace_still_running'})
        elif read(directory / 'trace-result.private.json')['error'] != '<nil>':
            findings.append({'validator': directory.name, 'kind': 'trace_execution_failed'})
        if targets:
            routes[directory.name] = route_paths(directory, targets)
        for path in native_paths:
            native = read(path)
            address = native['address']
            if native['asof_exclusive'] != identity['asof_exclusive']:
                raise ValueError('native and flow end windows differ')
            opening_day = datetime.datetime.fromisoformat(openings[address]['Day'].replace('Z', '+00:00'))
            if datetime.datetime.fromisoformat(native['since'].replace('Z', '+00:00')) != opening_day + datetime.timedelta(days=1):
                raise ValueError('native and opening windows differ')
            stem = path.name.removesuffix('.native-cash.private.json')
            flow_path = directory / (stem + '.events.private.json')
            input_path = directory / (stem + '.input.private.json')
            if native['source_error'] != '<nil>' or not native.get('closing_known') or native.get('closing_error') != '<nil>':
                findings.append({'wallet': stem, 'kind': 'native_source_or_closing_unavailable'})
                continue
            bank, types = native_cells(native['message_attributes'] or [], address, 'message')
            blocks, block_types = native_cells(native['block_attributes'] or [], address, 'block')
            # Block events have a native source event identity, while projected
            # block categories use height. Aggregate only after retaining both.
            bank.update(blocks)
            for key, value in bank.items():
                native_coin_totals[key[3]] += value
            events = read(flow_path)
            projected, categories = flow_cash(events, address)
            raw = read(input_path)['Wallets'][0]
            fees = sum((Decimal(f['Amount']) for f in raw.get('Fees') or [] if f['Denom'] == NATIVE), ZERO)
            opening = openings[address]['Holdings']
            closing = native['closing']
            native_net = sum((v for k, v in bank.items() if k[3] == NATIVE), ZERO) - fees
            cash_residual = Decimal(opening['Liquid']) + native_net - Decimal(closing['Liquid'])
            projection_net = sum(projected.values(), ZERO)
            projection_residual = projection_net - native_net
            daily = defaultdict(lambda: {'native': ZERO, 'projected': ZERO})
            for key, value in bank.items():
                if key[3] == NATIVE:
                    daily[key[0]]['native'] += value
            for f in raw.get('Fees') or []:
                if f['Denom'] == NATIVE:
                    daily[utc_day(f['Ts'])]['native'] -= Decimal(f['Amount'])
            for key, value in projected.items():
                daily[key[0]]['projected'] += value
            native_messages = defaultdict(Decimal)
            for key, value in bank.items():
                if key[3] == NATIVE and key[1] == 'message':
                    native_messages[(key[0], key[2])] += value
            projected_messages = defaultdict(Decimal)
            for key, value in projected.items():
                if key[1] == 'message':
                    projected_messages[(key[0], key[2])] += value
            discrepancies = []
            for day, message in sorted(set(native_messages) | set(projected_messages)):
                n, p = native_messages[(day, message)], projected_messages[(day, message)]
                if n != p:
                    discrepancies.append({'day': day, 'message': message, 'message_type': types.get(('message', message), 'no_native_bank_event'), 'native_base': str(n), 'projected_base': str(p), 'difference_base': str(p-n), 'projected_categories': sorted(categories.get((day, 'message', message, NATIVE), set()))})
            receipts = defaultdict(list, {k: list(v) for k, v in archive_by_hash.items()})
            for receipt in native.get('archive') or []:
                if hashlib.sha256(receipt['Response'].encode()).hexdigest() != receipt['SHA256']:
                    raise ValueError('archive receipt hash mismatch')
                receipts[receipt['SHA256']].append(receipt)
            closing_day = (datetime.datetime.fromisoformat(native['asof_exclusive'].replace('Z', '+00:00')) - datetime.timedelta(days=1)).date().isoformat()
            if not _proof.exact_source(opening['Source'], address, opening_day.date().isoformat(), receipts):
                findings.append({'wallet': stem, 'kind': 'opening_provenance_not_yet_verified'})
            if not _proof.exact_source(closing['Source'], address, closing_day, receipts):
                findings.append({'wallet': stem, 'kind': 'closing_provenance_invalid'})
            record = {'validator': directory.name, 'wallet': stem, 'address': address, 'opening_source': opening['Source'], 'closing_source': closing['Source'], 'native_cash_residual_base': str(cash_residual), 'projection_minus_native_base': str(projection_residual), 'daily': {d: {k: str(v) for k, v in x.items()} for d, x in sorted(daily.items())}, 'all_message_discrepancies': discrepancies, 'all_native_coin_cells': [{'day': k[0], 'source': k[1], 'source_identity': k[2], 'coin': k[3], 'net_base': str(v)} for k, v in sorted(bank.items())]}
            if address in comparisons:
                comparison, comparison_path = comparisons[address]
                source_hashes[str(comparison_path.relative_to(root))] = hashlib.sha256(comparison_path.read_bytes()).hexdigest()
                if comparison['opening_error'] != '<nil>' or comparison['closing_error'] != '<nil>':
                    findings.append({'wallet': stem, 'kind': 'archive_comparison_unavailable'})
                else:
                    proof_receipts = defaultdict(list)
                    for receipt in comparison['archive']:
                        proof_receipts[receipt['SHA256']].append(receipt)
                    for name, holding_day in [('opening', opening_day.date().isoformat()), ('closing', closing_day)]:
                        holding = comparison[name]
                        if not _proof.exact_source(holding['Source'], address, holding_day, proof_receipts):
                            raise ValueError('archive comparison source proof invalid')
                        baseline = opening if name == 'opening' else closing
                        for bucket in ['Liquid', 'Staked', 'Unbonding']:
                            if bucket == 'Liquid':
                                differs = Decimal(holding[bucket]) != Decimal(baseline[bucket])
                            else:
                                left, right = holding[bucket] or {}, baseline[bucket] or {}
                                differs = any(Decimal(left.get(k, '0')) != Decimal(right.get(k, '0')) for k in set(left) | set(right))
                            if differs:
                                findings.append({'wallet': stem, 'kind': 'ledger_' + name + '_' + bucket.lower() + '_differs_exact_archive'})
                    archive_residual = Decimal(comparison['opening']['Liquid']) + native_net - Decimal(comparison['closing']['Liquid'])
                    record['exact_archive_native_cash_residual_base'] = str(archive_residual)
                    if archive_residual:
                        findings.append({'wallet': stem, 'kind': 'native_source_bank_cash_still_unexplained_after_archive'})
            elif cash_residual or projection_residual:
                findings.append({'wallet': stem, 'kind': 'material_archive_comparison_pending'})
            wallets.append(record)
            if cash_residual:
                findings.append({'wallet': stem, 'kind': 'native_cash_not_conserved'})
            if projection_residual:
                findings.append({'wallet': stem, 'kind': 'source_projection_gap', 'message_cells': len(discrepancies)})
            for source in [path, flow_path, input_path, directory / 'openings.private.jsonl']:
                source_hashes[str(source.relative_to(root))] = hashlib.sha256(source.read_bytes()).hexdigest()
    return {'wallets': wallets, 'findings': findings, 'source_sha256': source_hashes, 'native_coin_totals': {k: str(v) for k, v in sorted(native_coin_totals.items())}, 'routes': routes, 'financially_accepted': False, 'note': 'Cash/source evidence only. Full cohort, pool conservation, provenance and every material residual still require review. No accounting acceptance is inferred.'}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('root', type=Path)
    parser.add_argument('--targets', type=Path)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    result = audit(args.root, read(args.targets) if args.targets else None)
    descriptor = os.open(args.output, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    with os.fdopen(descriptor, 'w') as handle:
        json.dump(result, handle)
    print(json.dumps({'audited_wallets': len(result['wallets']), 'findings': len(result['findings']), 'source_files_hashed': len(result['source_sha256']), 'financially_accepted': False}))

if __name__ == '__main__':
    main()
