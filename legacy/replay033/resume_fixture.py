#!/usr/bin/env python3
"""Synthetic native033 fixture adapter, never original-source admission.

The source producer identity and historical genesis pin are the ONLY test-only
provenance adapters. Journal, phase, raw-cache, native genesis and financial
checks execute normally. All fixture members are content bound.
"""
import argparse
import gzip
import hashlib
import json
from pathlib import Path
from unittest.mock import patch

import consume_closed


def digest(data):
    return hashlib.sha256(data).hexdigest()


def bind_fixture(root):
    assert (root / 'SYNTHETIC-FIXTURE').is_file()
    binding = dict(schema='synthetic-native033-resume-fixture-v1', first=1,
                   through=3, source_target=3, source_whole_complete=True,
                   financial_admission=False, streams={}, cache_members_sha256={})
    for name, completion in [('movements', 'observation_completed'),
                             ('native-history', 'history_completed')]:
        compressed = (root / (name + '.source-closed.gz')).read_bytes()
        raw = gzip.decompress(compressed)
        binding['streams'][name] = dict(sha256=digest(compressed),
            compressed_bytes=len(compressed), records=len(raw.splitlines()),
            raw_bytes=len(raw), raw_sha256=digest(raw), completion_kind=completion)
    for member in sorted((root / 'cache').rglob('*')):
        if member.is_file():
            binding['cache_members_sha256'][str(member.relative_to(root / 'cache'))] = digest(member.read_bytes())
    path = root / 'capture.private.json'
    with path.open('x') as stream:
        json.dump(binding, stream, sort_keys=True, indent=2)
    return binding


def fixture_source(root):
    assert (root / 'SYNTHETIC-FIXTURE').is_file()
    binding = json.loads((root / 'capture.private.json').read_bytes())
    assert binding['schema'] == 'synthetic-native033-resume-fixture-v1'
    assert (binding['first'], binding['through']) == (1, 3)
    expected = {'genesis/genesis.cosmoshub-1.json'} | {
        f'cosmoshub-1/0/{height}-{suffix}.raw.gz'
        for height in range(1, 5) for suffix in ('block', 'block_results')}
    assert set(binding['cache_members_sha256']) == expected
    for name, pin in binding['cache_members_sha256'].items():
        member = root / 'cache' / name
        assert not member.is_symlink() and digest(member.read_bytes()) == pin
    return binding


def provenance_adapters(root):
    """Return two narrow test patches, with the original schema checks retained."""
    original = consume_closed.HistoryWindow.validate
    genesis_pin = digest((root / 'cache/genesis/genesis.cosmoshub-1.json').read_bytes())

    def validate(row, anchor=False):
        if not anchor:
            return original(row, anchor=False)
        assert row['height'] == 0 and row['genesis_sha256'] == genesis_pin
        checked = dict(row, genesis_sha256='73a866b21723ecbc28b6d15951b2eb3aa2f2443650ff6df489bf55ac5edceefa')
        original(checked, anchor=True)
        return row

    return (patch.object(consume_closed, 'validate_source', fixture_source),
            patch.object(consume_closed.HistoryWindow, 'validate', staticmethod(validate)))


def run_fixture(root, binary, receipt):
    receipt.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    source, anchor = provenance_adapters(root)
    with source, anchor:
        return consume_closed.verify(root, binary, receipt)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--directory', type=Path, required=True)
    parser.add_argument('--bind', action='store_true')
    parser.add_argument('--native-helper', type=Path)
    parser.add_argument('--receipt', type=Path)
    args = parser.parse_args()
    if args.bind:
        bind_fixture(args.directory)
    if args.native_helper:
        assert args.receipt
        result = run_fixture(args.directory, args.native_helper, args.receipt)
        print(json.dumps({k: result[k] for k in ('first', 'through', 'heights', 'transactions', 'financial_admission')}))
