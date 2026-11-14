#!/usr/bin/env python3
"""Validate the unchanged retained real Hub2 input with a new signed consumer.

Offline only. Never contacts Kubernetes, replays a native block or writes a DB.
The original failed component is evidence, not overwritten or relabeled success.
"""
import argparse
import base64
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import subprocess
import hub2_sealed_window as original


def verify_inputs(source, profile):
    original.require(source.is_dir() and not source.is_symlink())
    original.require(profile.lstat().st_mode & 0o777 == 0o600 and not profile.is_symlink())
    original.require(original.file_sha(profile) == original.PROFILE)
    original.require(original.file_sha(source/'sealed-window.json') ==
        'bec88dd3747d011d5f037144e6371aded75a7935f3b740e1e640a47c85c2f68d')
    original.require(original.file_sha(source/'component-result.json') ==
        'f3dedab6353c09b5f01624500985d6b1953231ccd76fbe2f12fdf428ac111a05')
    seal = original.decode((source/'sealed-window.json').read_bytes())
    failed = original.decode((source/'component-result.json').read_bytes())
    original.require(failed['component_validated'] is False and failed['exit_code'] == 1)
    original.require(failed['sealed_window_receipt_sha256'] == original.file_sha(source/'sealed-window.json'))
    original.require(seal['source_job_uid'] == original.UID and seal['source_commit'] == original.SOURCE
                     and seal['source_image'] == original.IMAGE and seal['original_target'] == 2902000
                     and seal['from'] == original.FIRST and seal['through'] == original.LAST
                     and seal['whole_job_complete'] is False)
    hashes = {}
    names = set(original.NAMES.values()) | {'window.movements.ndjson', 'window.differences.txt',
        'proof.json', 'sealed-window.json', 'component-result.json', 'cli.binding.private.json'}
    names |= set(seal['artifact_sha256'])
    for name in names:
        original.require(Path(name).name == name)
        path = source/name
        original.require(path.is_file() and not path.is_symlink() and path.stat().st_mode & 0o777 == 0o600)
        hashes[name] = original.file_sha(path)
    for kind, entry in seal['files'].items():
        original.require(entry['name'] == original.NAMES[kind] and hashes[entry['name']] == entry['sha256']
            == entry['remote_prefix_sha256_before'] == entry['remote_prefix_sha256_after'])
    for name, digest in seal['artifact_sha256'].items():
        original.require(hashes[name] == digest)
    binding = original.decode((source/'cache.binding.private.json').read_bytes())
    manifest = binding['all_raw_members']
    remote = {}
    for line in (source/'cache.remote.sha256').read_text().splitlines():
        digest, name = line.split('  ', 1)
        original.require(name not in remote)
        remote[name] = digest
    original.require(manifest == remote and len(manifest) == 164184)
    for height in range(original.FIRST-1, original.LAST+2):
        for kind in ('block', 'block_results'):
            name = f'cosmoshub-2/{height//10000}/{height}-{kind}.raw.gz'
            path = source/'cache'/name
            original.require(path.is_file() and not path.is_symlink()
                and path.stat().st_mode & 0o777 == 0o600 and original.file_sha(path) == manifest[name])
    raw = (source/'window.differences.txt').read_bytes()
    exact = original.decode(profile.read_bytes())
    original.require(raw == base64.b64decode(exact['original_marker_bytes'])
                     and original.sha(raw) == original.MARKERS and raw.count(b'\n') == 3387)
    count, records, expected = 0, 0, original.FIRST
    with (source/'window.movements.ndjson').open('rb') as f:
        for line in f:
            original.require(line.endswith(b'\n'))
            record = original.decode(line)
            original.require(record['height'] == expected)
            records += 1
            if record['kind'] == 'height_completed':
                count += 1
                expected += 1
    original.require(count == 82090 and expected == original.LAST+1 and records == 375524
        and (source/'window.movements.ndjson').stat().st_size == 66454626
        and hashes['window.movements.ndjson'] ==
        'a0d72f037d9407f923c99d34729e17aad712816ccd9d838d41767395a46bf7a0')
    proof = original.decode((source/'proof.json').read_bytes())
    original.require(proof['records_sha256'] == hashes['window.movements.ndjson'])
    return dict(original_artifact_sha256=hashes, complete_input_heights=count,
        input_record_count=records, input_record_bytes=(source/'window.movements.ndjson').stat().st_size,
        raw_cache_members=164184, raw_cache_pairs=82092, profile_sha256=original.PROFILE,
        marker_sha256=original.MARKERS, marker_count=3387,
        original_source_directory=str(source.resolve()), original_failed_result_unchanged=True,
        provenance='retained original source attestations, no new live runtime attestation',
        descriptor_kind='hub2-retained-actual-input-v1')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', required=True, type=Path)
    parser.add_argument('--out', required=True, type=Path)
    parser.add_argument('--profile', required=True, type=Path)
    parser.add_argument('--cli', required=True, type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    args.out.mkdir(mode=0o700)
    descriptor = verify_inputs(args.source, args.profile)
    original.bind_cli(args.cli, args.out)
    descriptor['consumer_binding_sha256'] = original.file_sha(args.out/'cli.binding.private.json')
    descriptor['retained_runner_sha256'] = original.file_sha(Path(__file__))
    descriptor['observed'] = datetime.now(timezone.utc).isoformat()
    original.save_json(args.out/'retained-input-descriptor.json', descriptor)
    cmd = [str(args.cli), '--cache', str(args.source/'cache'),
        '--records', str(args.source/'window.movements.ndjson'), '--proof', str(args.source/'proof.json'),
        '--status', str(args.source/'status.original.json'),
        '--result-differences', str(args.source/'window.differences.txt'),
        '--approved-gas-profile', str(args.profile), '--sealed-window-receipt', str(args.source/'sealed-window.json')]
    original.save_json(args.out/'command.json', dict(argv=cmd, validation_only=True,
        no_database=True, no_source_recapture=True))
    with original.create(args.out/'core-validation.private.log') as log:
        result = subprocess.run(cmd, stdout=log, stderr=subprocess.STDOUT, timeout=1800)
    output = (args.out/'core-validation.private.log').read_text()
    match = re.search(r'^rehearsal validated heights=(\d+) held=(\d+) transactions=(\d+) rows=(\d+) written=false$', output, re.M)
    counts = dict(zip(('validated_heights', 'held', 'transactions', 'rows'), map(int, match.groups()))) if match else {}
    # Full source input hashes are checked again after consumption. This is not
    # a second financial run and never changes any source artifact.
    unchanged = verify_inputs(args.source, args.profile)
    original.require(unchanged['original_artifact_sha256'] == descriptor['original_artifact_sha256'])
    passed = result.returncode == 0 and counts.get('validated_heights') == 82090 and counts.get('held') == 0
    original.save_json(args.out/'component-result.json', dict(exit_code=result.returncode,
        component_validated=passed, **counts, complete_input_heights=descriptor['complete_input_heights'],
        input_record_count=descriptor['input_record_count'], input_record_bytes=descriptor['input_record_bytes'],
        descriptor_sha256=original.file_sha(args.out/'retained-input-descriptor.json'),
        consumer_binding_sha256=descriptor['consumer_binding_sha256'], original_failed_result_unchanged=True,
        source_job_target=2902000, source_job_complete=False, full_chain_books_tax_acceptance=False,
        production_write=False, gas_used_overwritten=False, native_replay=False, source_recapture=False))
    original.require(passed)
    print(json.dumps(dict(component_validated=True, **counts, full_chain_books_tax_acceptance=False,
                          production_write=False)))


if __name__ == '__main__':
    try:
        main()
    except (original.Refused, KeyError, ValueError, OSError, subprocess.SubprocessError):
        print('retained component refused; private artifacts retained', file=__import__('sys').stderr)
        raise SystemExit(1)
