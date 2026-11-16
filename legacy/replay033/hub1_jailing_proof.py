#!/usr/bin/env python3
"""Write the proof that a Hub 1 block jailed a validator without taking any stake.

The six heights handed to the join were candidates from a validator-set scan
(a validator stopped signing and left the set). One of them, 231614, carries
no penalty fact. This tool reads short scratch recordings made by the
reproducible split recorder from copies of retained native state, and the
store comparisons made by cmd/hub1-store-facts, and writes one pinned file
that says what happened at that block, with two real penalty blocks as
controls that the same method sees a real stake reduction.

Nothing here touches a live run. Only counts and yes or no go into the proof
file; amounts and identities stay in the private comparison files it pins."""
import argparse
import hashlib
import json
from pathlib import Path
from verify_movement_rehearsal import require

SCHEMA = 'hub1-jailing-without-burn-v1'
VERDICT = 'jailed_with_no_stake_moved'
CAUSE = 'operator_self_undelegation_below_minimum_self_delegation_in_a_message'
FACT_SCHEMA = 'hub1-store-facts-v1'
CHECKS = ('block_run_recorded_no_penalty_fact', 'next_block_run_recorded_no_penalty_fact', 'one_validator_newly_jailed', 'validator_tokens_plus_unbonding_unchanged',
          'no_slash_event', 'no_jailed_until_change', 'no_missed_counter_reset', 'pools_not_reduced', 'jailed_validator_tokens_went_down_by_an_undelegation',
          'undelegation_is_the_operator_own_stake', 'undelegated_tokens_equal_the_new_unbonding_balance', 'begin_block_wrote_only_the_staking_pool',
          'neighbour_blocks_jailed_nobody', 'controls_show_stake_taken', 'control_facts_equal_the_live_parts')


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def run_record(directory):
    proof = Path(directory) / 'proof'
    exit_ = json.loads((proof / 'exit.private.json').read_bytes())
    result = json.loads((proof / 'complete-component.private.json').read_bytes())
    launcher = json.loads((Path(directory) / 'launcher.private.txt').read_bytes())
    require(exit_['exit'] == 0 and launcher['checked_before_start'] is True, 'scratch recording did not finish cleanly')
    facts = proof / 'native-stake-reductions.private.jsonl'
    heights = [json.loads(l)['height'] for l in facts.read_bytes().splitlines()] if facts.is_file() else []
    require(len(heights) == result['native_stake_asset_blocks'], 'scratch penalty facts differ from its receipt')
    return dict(directory=str(directory), first=result['first'], through=result['through'], opening_kind=result['opening_kind'], signed_source_commit=launcher['signed_source_commit'],
                helper_sha256=launcher['helper_sha256'], opening_manifest_sha256=launcher['opening_manifest_sha256'], receipt_sha256=sha(proof / 'complete-component.private.json'),
                native_proof_sha256=sha(proof / 'native-proof.private.json'), exit_sha256=sha(proof / 'exit.private.json'), launcher_sha256=sha(Path(directory) / 'launcher.private.txt'),
                penalty_fact_heights=heights, penalty_facts_sha256=sha(facts) if facts.is_file() else '')


def comparison(public, private):
    facts = json.loads(Path(public).read_bytes())
    require(facts['schema'] == FACT_SCHEMA and facts['private_sha256'] == sha(private), 'store comparison and its private file differ')
    return dict(public=facts, public_path=str(public), public_sha256=sha(public), private_path=str(private))


def stake_taken(f):
    return (f['validators']['tokens_went_down'] >= 1 and f['validator_tokens_plus_unbonding_balances_went_down'] and f['distribution_slash_events']['added'] >= 1
            and f['signing_info']['jailed_until_changed'] >= 1 and f['validators']['newly_jailed'] >= 1)


def bech32_bytes(text):
    chars = 'qpzry9x8gf2tvdw0s3jn54khce6mua7l'
    acc = bits = 0
    out = bytearray()
    for v in [chars.index(c) for c in text[text.rindex('1') + 1:]][:-6]:
        acc, bits = (acc << 5) | v, bits + 5
        while bits >= 8:
            bits -= 8
            out.append((acc >> bits) & 255)
    return bytes(out)


def block_details(private, rows, height):
    """Read the private detail and the captured rows of the block. Returns yes or no only."""
    detail = json.loads(Path(private).read_bytes())['private']
    jailed = [v for v in detail['validators'] if v['jailed_after'] and not v['jailed_before']]
    require(len(jailed) == 1, 'expected exactly one newly jailed validator')
    v = jailed[0]
    drop = int(v['tokens_before']) - int(v['tokens_after'])
    records = [json.loads(l) for l in Path(rows).read_bytes().splitlines() if l.strip()]
    require(all(r['height'] == height for r in records) and any(r['kind'] == 'height_completed' for r in records), 'captured rows are not the whole block')
    undelegations = [r for r in records if r['kind'] == 'share_request' and r['label'].startswith('undelegate')]
    principal = [r for r in records if r['kind'] == 'principal' and r['label'] == 'undelegate']
    begin = [r for r in records if r['phase'] == 'begin_block' and r['kind'] == 'native_write' and r['label'].startswith('store:staking')]
    own = len(undelegations) == 1 and bech32_bytes(undelegations[0]['validator']) == bech32_bytes(v['validator']) == bech32_bytes(undelegations[0]['delegator'])
    return dict(jailed_validator_tokens_went_down_by_an_undelegation=drop > 0 and len(principal) == 1 and int(principal[0]['amount']) == drop,
                undelegation_is_the_operator_own_stake=own,
                undelegated_tokens_equal_the_new_unbonding_balance=int(detail['unbonding_balance_change']) == drop and int(detail['validator_tokens_plus_unbonding_change']) == 0,
                begin_block_wrote_only_the_staking_pool=len(begin) == 1 and begin[0]['label'] == 'store:staking:staking/key:AA==')


def build(a):
    block, after = run_record(a.block_run), run_record(a.next_run)
    controls = [run_record(d) for d in a.control_run]
    compared = {name: comparison(p, q) for name, p, q in a.compare}
    f, before, nxt = compared['block']['public'], compared['before']['public'], compared['next']['public']
    require(f['after_height'] == a.height == before['after_height'] + 1 == nxt['before_height'] and block['through'] == a.height and after['through'] == a.height + 1, 'heights do not line up')
    live = {int(h): sha(p) for h, p in a.live_fact}
    control_facts = [compared[n]['public'] for n in compared if n.startswith('control')]
    checks = dict(block_run_recorded_no_penalty_fact=not block['penalty_fact_heights'], next_block_run_recorded_no_penalty_fact=not after['penalty_fact_heights'],
                  one_validator_newly_jailed=f['validators']['newly_jailed'] == 1, validator_tokens_plus_unbonding_unchanged=f['validator_tokens_plus_unbonding_balances_unchanged'],
                  no_slash_event=f['distribution_slash_events']['added'] == 0, no_jailed_until_change=f['signing_info']['jailed_until_changed'] == 0,
                  no_missed_counter_reset=f['signing_info']['missed_counter_reset_to_zero'] == 0, pools_not_reduced=not f['pool_total_went_down'],
                  neighbour_blocks_jailed_nobody=before['validators']['newly_jailed'] == 0 == nxt['validators']['newly_jailed'] and before['validators']['tokens_changed'] == 0 == nxt['validators']['tokens_changed'],
                  controls_show_stake_taken=len(control_facts) == len(controls) >= 2 and all(stake_taken(c) for c in control_facts),
                  control_facts_equal_the_live_parts=sorted(live) == sorted(c['through'] for c in controls) and all(c['penalty_fact_heights'] == [c['through']] and c['penalty_facts_sha256'] == live[c['through']] for c in controls))
    checks.update(block_details(compared['block']['private_path'], a.block_rows, a.height))
    require(set(checks) == set(CHECKS), 'check list differs')
    failed = sorted(k for k, v in checks.items() if v is not True)
    require(not failed, 'the block is not a jailing with no stake moved: ' + ', '.join(failed))
    return dict(schema=SCHEMA, height=a.height, verdict=VERDICT, cause=CAUSE, checks=checks, stake_moved=False,
                recorded_movements_at_height='ordinary undelegation already recorded as principal, share request and staking shares rows',
                origin_of_candidate='validator-set scan of incident 593 (validator stopped signing and left the set); HUB1-HANDOFF.md known downtime candidates',
                reader=dict(commit=a.reader_commit, binary_sha256=sha(a.reader)), block_run=block, next_block_run=after, control_runs=controls,
                comparisons={n: dict(c, private_sha256=sha(c['private_path'])) for n, c in compared.items()}, block_rows_sha256=sha(a.block_rows),
                live_control_facts={str(h): dict(path=p, sha256=live[int(h)]) for h, p in a.live_fact})


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--height', type=int, required=True)
    parser.add_argument('--block-run', type=Path, required=True)
    parser.add_argument('--next-run', type=Path, required=True)
    parser.add_argument('--control-run', type=Path, action='append', required=True)
    parser.add_argument('--compare', nargs=3, action='append', required=True, metavar=('NAME', 'PUBLIC', 'PRIVATE'))
    parser.add_argument('--live-fact', nargs=2, action='append', required=True, metavar=('HEIGHT', 'FILE'))
    parser.add_argument('--block-rows', type=Path, required=True)
    parser.add_argument('--reader', type=Path, required=True)
    parser.add_argument('--reader-commit', required=True)
    parser.add_argument('--out', type=Path, required=True)
    a = parser.parse_args()
    proof = build(a)
    with a.out.open('x') as out:
        json.dump(proof, out, indent=1, sort_keys=True)
        out.write('\n')
    print(json.dumps(dict(proof=str(a.out), sha256=sha(a.out), height=a.height, verdict=VERDICT, checks_passed=len(proof['checks']))))
