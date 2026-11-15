"""Pin the existing closed local recordings, never contact a producer or actor."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import tarfile
import sys

from adapter import CHAIN, RELEASE, decode, digest, require

CACHE = Path('/Users/danb/.cache')
GENESIS_SHA = '73a866b21723ecbc28b6d15951b2eb3aa2f2443650ff6df489bf55ac5edceefa'
BOUNDARY = {
    'genesis.cosmoshub-1.json': GENESIS_SHA,
    'genesis.cosmoshub-2.json': '1e349fb39b85f7707ee78d39879f9d5d61f4d30f67980bb0bf07bd35b2f8bf30',
    'native-export.json': 'f191855b5291fad621eb1267049fcb29e845bf801fb3035a6c6f0e791e7918ab',
    'checkpoint-proof.json': '321fafeefb17f3b0b650e4c066afb100ad33c3237993968222c77c5d10336bec',
    'complete-genesis-comparison-signed-final.json': '3ccdc373b1af7327dc9e68bf866e9df62082dc3ee2ef537d77f01746ea3ff57b',
}
SOURCES = [
    ('cosmos-tax-tmp/hub1-whole-financial-run-20261114T1823Z/profile-1/source', 1, 200000),
    ('cosmos-tax-tmp/hub1-range2-retry-20261115T1555Z/profile-200001/source', 200001, 400000),
    ('cosmos-tax-tmp/hub1-whole-financial-run-20261114T1823Z/profile-400001/source', 400001, 500000),
]


def pinned(path, expected=None):
    h = digest(path)
    require(expected is None or h == expected)
    return {'path': str(path), 'sha256': h}


def verify_cache(path, expected):
    """No extraction, all members and exact bytes, no traversal or links."""
    seen = set()
    with tarfile.open(path, 'r|') as archive:
        for member in archive:
            name = member.name
            require(member.isfile() and name in expected and name not in seen)
            require(not name.startswith('/') and '..' not in Path(name).parts)
            seen.add(name)
            f = archive.extractfile(member)
            h = hashlib.sha256()
            for part in iter(lambda: f.read(1024 * 1024), b''):
                h.update(part)
            require(h.hexdigest() == expected[name])
    require(seen == set(expected))


def prepare(cache=CACHE):
    boundary = cache / 'cosmos-tax-hub1-native-boundary-aa6b004'
    canonical = [pinned(boundary / name, sha) for name, sha in BOUNDARY.items()]
    components = []
    for relative, first, last in SOURCES:
        root = cache / relative
        capture = decode((root / 'capture.private.json').read_bytes())
        status = decode((root / 'source-status-after.private.json').read_bytes())
        require(capture['schema'] == 'native033-closed-source-capture-v1' and
                capture['first'] == first and capture['through'] == last and
                capture['source_whole_complete'] is True)
        require(status['chain'] == CHAIN and status['release'] == RELEASE and
                status['first_height_this_run'] == first and
                status['last_height_committed'] == last and
                status['state_hash_verified_through'] == last and status['target'] == last and
                status['heights_where_results_differ_from_cache'] == 0)
        completed = {}
        for line in (root / 'completed.private.sha256').read_text().splitlines():
            sha, name = line.split(None, 1)
            require(name not in completed)
            completed[name] = sha
        history = None
        artifacts = []
        for name, label in [('native-history.source-closed.gz', 'native-history'),
                            ('movements.source-closed.gz', 'movements')]:
            evidence = capture['streams'][label]
            require(evidence['source_path'] == f'/recordings/range-{first}-{last}/{label}.ndjson.gz')
            require(evidence['sha256'] == completed[evidence['source_path']])
            p = pinned(root / name, evidence['sha256'])
            require((root / name).stat().st_size == evidence['compressed_bytes'])
            if label == 'native-history':
                history = p
            else:
                artifacts.append(p)
        verify_cache(root / 'cache-source.private.tar', capture['cache_members_sha256'])
        artifacts.append(pinned(root / 'cache-source.private.tar'))
        for name in ['completed.private.sha256', 'source-status-before.private.json',
                     'source-status-after.private.json', 'recording-complete.private',
                     'binaries.private.sha256', 'source-and-storage-binding.private.json']:
            artifacts.append(pinned(root / name, capture['source_metadata_sha256'][name]))
        artifacts.append(pinned(root / 'capture.private.json'))
        if first == 1:
            artifacts.extend(canonical)
        components.append({'first': first, 'last': last, 'genesis_sha256': GENESIS_SHA,
                           'history': history, 'artifacts': artifacts})
    tail = cache / 'cosmos-tax-hub1-terminal-fifteen-attempt1/candidate2'
    proof = decode((tail / 'full-proof-attempt3.private.json').read_bytes())
    require(proof['financial_admission'] is False and proof['tax_acceptance'] is False)
    # Reuse accepted original terminal artifacts. No export or native execution.
    artifacts = [pinned(tail / name) for name in [
        'native-terminal-proof.json', 'observed-terminal-proof.json',
        'native-replay-exit.private.json', 'observed-replay-exit.private.json',
        'full-proof-attempt3.private.json', 'full-proof-attempt3-exit.private.json',
        'observed-movements.ndjson', 'observed-history.ndjson',
        'native-all-store-values.ndjson', 'native-financial-context.ndjson',
    ]]
    artifacts.append(pinned(tail.parent / 'tail-cache.private.tar'))
    components.append({'first': 500001, 'last': 500015, 'compression': 'none',
                       'genesis_sha256': GENESIS_SHA,
                       'history': pinned(tail / 'native-history.ndjson'),
                       'terminal_proof': pinned(tail / 'native-terminal-proof.json'),
                       'artifacts': artifacts})
    return {'version': 1, 'chain': CHAIN, 'canonical_migration_height': 500000,
            'historical_tail_first': 500001, 'historical_tail_last': 500015,
            'approval_already_given': 'yes, load hub one hub two and production when their balance checks pass',
            'balance_condition_already_passed': True,
            'canonical_originals_reused_without_reexecution': canonical,
            'components': components}


def main():
    p = argparse.ArgumentParser()
    p.add_argument('--output', required=True)
    args = p.parse_args()
    try:
        result = prepare()
        with open(args.output, 'x') as f:
            os.chmod(f.name, 0o600)
            json.dump(result, f, sort_keys=True)
            f.write('\n')
        print(json.dumps({'components': len(result['components']), 'first': 1, 'last': 500015,
                          'source_hashes_and_cache_members_checked': True, 'production_writes': 0}))
    except Exception:
        print('offline input pinning refused; private details withheld', file=sys.stderr)
        return 2
    return 0


if __name__ == '__main__':
    sys.exit(main())
