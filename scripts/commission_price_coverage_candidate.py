#!/usr/bin/env python3
"""Read-only candidate, never installed into a live collector or monitor.

Keep physical storage, reusable native evidence and financially usable priced
coverage separate. An unlisted or changed case is never silently accepted.
"""
import re


def key(row):
    return row['version'], row['chain'], row['root_sha256']


def price_guard(decision):
    """Only accept an explicit, identity-bound completeness proof, never USD zero."""
    proof = decision.get('price_completeness', {})
    counters = ['required_nonzero', 'priced_nonzero', 'missing', 'fallback',
                'lookup_errors', 'unknown', 'zero_or_non_economic_proved']
    if not all(type(proof.get(k)) is int and proof[k] >= 0 for k in counters):
        return False
    if proof['required_nonzero'] != proof['priced_nonzero']:
        return False
    if any(proof[k] for k in ('missing', 'fallback', 'lookup_errors', 'unknown')):
        return False
    if (proof.get('source_provenance_complete') is not True or
            proof.get('cache_context_verified') is not True or
            proof.get('requirements_inventory_complete') is not True):
        return False
    if proof['required_nonzero'] == 0 and proof.get('no_pricing_required_reason') not in (
            'zero_asset_quantity_proved', 'non_economic_proved', 'empty_case_proved'):
        return False
    if (proof.get('input_row_sha256') != decision.get('row_sha256') or
            proof.get('native_artifact_sha256') != decision.get('capture_manifest_sha256')):
        return False
    return all(isinstance(proof.get(k), str) and re.fullmatch(r'[0-9a-f]{64}', proof[k])
               for k in ('source_receipt_sha256', 'derivation_config_sha256', 'native_artifact_sha256'))


def coverage(rows, manifest):
    if manifest.get('schema') != 'commission-private-price-quarantine/v1':
        raise ValueError('unsupported quarantine schema')
    members = manifest['canonical_membership']['root_sha256']
    if len(members) != 324 or len(set(members)) != 324:
        raise ValueError('canonical membership must retain all 324 members')
    permitted = set(members)
    decisions = {}
    for decision in manifest['cases']:
        k = key(decision)
        if k in decisions or decision['root_sha256'] not in permitted:
            raise ValueError('duplicate or noncanonical decision')
        decisions[k] = decision
    seen = set()
    accepted_roots = set()
    counts = dict(physical_saved=0, reusable_native=0, usable_price_clean=0,
                  financial_quarantined=0, unaudited_or_changed=0)
    for row in rows:
        k = key(row)
        if k in seen or row['root_sha256'] not in permitted:
            raise ValueError('duplicate or noncanonical stored row')
        seen.add(k)
        counts['physical_saved'] += 1
        d = decisions.get(k)
        same = d is not None and all(row.get(f) == d.get(f) for f in
                   ('build', 'private_profile_sha256', 'row_sha256', 'status'))
        if not same:
            counts['unaudited_or_changed'] += 1
            counts['financial_quarantined'] += 1
            continue
        if d.get('native_price_independence_proved') is True:
            counts['reusable_native'] += 1
        # Neither no stored errors, zero USD, zero commission origin nor an
        # independent native capture proves complete economic valuations.
        accepted = (d.get('financial_coverage_quarantined') is False and
                    row['version'] in manifest.get('accepted_valuation_versions', []) and
                    row.get('status') in ('ok', 'reconciled') and
                    d.get('valuation_status') == 'verified_price_complete' and
                    d.get('price_completeness_guard_passed') is True and
                    isinstance(d.get('valuation_proof_sha256'), str) and
                    re.fullmatch(r'[0-9a-f]{64}', d['valuation_proof_sha256']) is not None and price_guard(d))
        if accepted:
            if row['root_sha256'] in accepted_roots:
                raise ValueError('ambiguous priced generations for one canonical member')
            accepted_roots.add(row['root_sha256'])
            counts['usable_price_clean'] += 1
        else:
            counts['financial_quarantined'] += 1
    return counts


def eligible_rows(rows, manifest):
    """Additive join/filter, no DB writes, no output-version reuse."""
    decisions = {key(d): d for d in manifest['cases']}
    # Run all membership/duplicate checks before returning any selected row.
    coverage(rows, manifest)
    selected = []
    for row in rows:
        if key(row) not in decisions:
            continue
        if coverage([row], manifest)['usable_price_clean'] == 1:
            selected.append(row)
    return selected
