# INF-748: merge the per-unit extraction lines into one daily issuance file, and
# check it: every height counted once, no missing or unreadable blocks, every
# block minted, and each day's supply change against the day's mint.
import argparse, json, sys
from decimal import Decimal, ROUND_HALF_EVEN, getcontext

getcontext().prec = 60

# Community tax by height: the param change runs in the end block of the
# tallying height H, so the mint of H itself is still split at the old rate.
TAX_PROPOSALS = {'88': Decimal('0.10'), '996': Decimal('0.02')}
GENESIS_TAX = Decimal('0.02')
FIRST_BLOCK_TIME = '2021-02-18T12:23:53Z'


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--bounds', required=True)
    ap.add_argument('--split', help='jsonl of extraction runs split at tax change heights')
    ap.add_argument('--out', required=True)
    ap.add_argument('inputs', nargs='+')
    a = ap.parse_args()
    bounds = json.load(open(a.bounds))
    day_start = dict((k, h) for k, h in bounds['days'])
    order = [k for k, _ in bounds['days']]
    days, seen, problems = {}, [], []
    names = set()
    for path in a.inputs:
        for line in open(path):
            u = json.loads(line)
            # Two runs may meet in the middle of a range; a unit counted twice is the same unit.
            if u['unit'] in names:
                continue
            names.add(u['unit'])
            seen.append((u['start'], u['end']))
            for kind in ('missing', 'no_mint', 'bad'):
                if u[kind]:
                    problems.append((u['unit'], kind, u[kind][:5]))
            for k, d in u['days'].items():
                t = days.setdefault(k, {'blocks': 0, 'minted': 0, 'rewards': Decimal(0), 'commission': Decimal(0),
                                        'first': None, 'last': None, 'infl_sum': 0.0, 'bonded_sum': 0.0, 'proposals': []})
                t['blocks'] += d['blocks']
                t['minted'] += d['minted']
                t['rewards'] += Decimal(d['rewards'])
                t['commission'] += Decimal(d['commission'])
                t['infl_sum'] += d['infl_sum']
                t['bonded_sum'] += d['bonded_sum']
                t['proposals'] += d['proposals']
                if d['first'] and (t['first'] is None or d['first'][0] < t['first'][0]):
                    t['first'] = d['first']
                if d['last'] and (t['last'] is None or d['last'][0] > t['last'][0]):
                    t['last'] = d['last']
    seen.sort()
    gaps, overlaps = [], []
    for (s0, e0), (s1, e1) in zip(seen, seen[1:]):
        if s1 > e0 + 1:
            gaps.append((e0 + 1, s1 - 1))
        if s1 <= e0:
            overlaps.append((s1, e0))
    first_h, last_h = seen[0][0], seen[-1][1]
    covered = sum(e - s + 1 for s, e in seen)

    # Community tax per height from the tallies the extraction saw.
    passed = sorted((h, pid) for d in days.values() for h, pid, res in d['proposals']
                    if res in ('proposal_passed', 'passed') and pid in TAX_PROPOSALS)
    tax_steps = [(first_h, GENESIS_TAX, None)] + [(h + 1, TAX_PROPOSALS[pid], pid) for h, pid in passed]

    def tax_at(h):
        t = GENESIS_TAX
        for sh, rate, _ in tax_steps:
            if h >= sh:
                t = rate
        return t

    # Minted before and after a tax change on the day it happened, from the split runs.
    split = {}
    if a.split:
        for line in open(a.split):
            u = json.loads(line)
            for k, d in u['days'].items():
                split[(k, u['start'], u['end'])] = d['minted']

    rows, supply_checks = [], []
    keys = [k for k in order if k in days]
    for i, k in enumerate(keys):
        d = days[k]
        start = day_start[k]
        end = day_start[keys[i + 1]] - 1 if i + 1 < len(keys) else last_h
        if d['blocks'] != end - start + 1:
            problems.append((k, 'blocks', [d['blocks'], end - start + 1]))
        f = d['first']
        supply = (Decimal(f[3]) / Decimal(f[1])).to_integral_value(ROUND_HALF_EVEN)
        steps_today = [s for s in tax_steps[1:] if start < s[0] <= end]
        if steps_today:
            sh = steps_today[0][0]
            before = split.get((k, start, sh - 1))
            if before is None:
                problems.append((k, 'tax change day needs a split run', [start, sh - 1, sh, end]))
                before = 0
            pool = Decimal(before) * tax_at(start) + Decimal(d['minted'] - before) * tax_at(sh)
        else:
            pool = Decimal(d['minted']) * tax_at(start)
        to_stakers = Decimal(d['minted']) - pool
        com_share = d['commission'] / d['rewards'] if d['rewards'] else Decimal(0)
        rows.append({
            'date': k, 'first_height': start, 'last_height': end, 'blocks': d['blocks'],
            'minted_uatom': d['minted'],
            'community_pool_uatom': int(pool.to_integral_value(ROUND_HALF_EVEN)),
            'stakers_uatom': int(to_stakers.to_integral_value(ROUND_HALF_EVEN)),
            'commission_uatom': int((to_stakers * com_share).to_integral_value(ROUND_HALF_EVEN)),
            'rewards_events_uatom': int(d['rewards']),
            'commission_events_uatom': int(d['commission']),
            'supply_start_uatom': int(supply),
            'inflation_param_start': f[1],
            'inflation_param_avg': round(d['infl_sum'] / d['blocks'], 8),
            'bonded_ratio_avg': round(d['bonded_sum'] / d['blocks'], 8),
            'community_tax_start': str(tax_at(start)),
            'proposals': [[h, pid, res] for h, pid, res in d['proposals'] if res in ('proposal_passed', 'passed')],
        })
    for r0, r1 in zip(rows, rows[1:]):
        diff = r1['supply_start_uatom'] - r0['supply_start_uatom'] - r0['minted_uatom']
        supply_checks.append((r0['date'], diff))
    other = [c for c in supply_checks if c[1] != 0]
    report = {
        'first_height': first_h, 'last_height': last_h, 'heights_covered': covered,
        'heights_expected': last_h - first_h + 1, 'gaps': gaps, 'overlaps': overlaps,
        'problems': problems[:50], 'problem_count': len(problems),
        'days': len(rows), 'minted_uatom': sum(r['minted_uatom'] for r in rows),
        'tax_steps': [[h, str(r), p] for h, r, p in tax_steps],
        'supply_days_off_mint': len(other), 'supply_off_total_uatom': sum(c[1] for c in other),
        'supply_off_positive': [c for c in other if c[1] > 0][:20],
        'supply_off_largest': sorted(other, key=lambda c: c[1])[:10],
    }
    json.dump({'rows': rows, 'report': report, 'first_block_time': FIRST_BLOCK_TIME}, open(a.out, 'w'))
    print(json.dumps(report, indent=1))


if __name__ == '__main__':
    main()


def export(merged_path, out_path, as_of):
    """The compact file the API embeds: one array per UTC day."""
    m = json.load(open(merged_path))
    cols = ['date', 'first_height', 'last_height', 'blocks', 'minted_uatom', 'community_pool_uatom',
            'stakers_uatom', 'commission_uatom', 'rewards_events_uatom', 'commission_events_uatom',
            'supply_start_uatom', 'inflation_param_avg', 'bonded_ratio_avg', 'community_tax_start']
    rep = m['report']
    doc = {
        'chain': 'cosmoshub-4', 'as_of': as_of, 'first_block_time': m['first_block_time'],
        'first_height': rep['first_height'], 'last_height': rep['last_height'],
        'blocks_counted': rep['heights_covered'], 'blocks_expected': rep['heights_expected'],
        'proposal_heights': {pid: h for r in m['rows'] for h, pid, _ in r['proposals']},
        'columns': cols,
        'days': [[r[c] for c in cols] for r in m['rows']],
    }
    json.dump(doc, open(out_path, 'w'), separators=(',', ':'))
