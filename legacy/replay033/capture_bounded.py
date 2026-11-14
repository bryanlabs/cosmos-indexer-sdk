#!/usr/bin/env python3
"""Read-only bounded capture of two growing gzip streams, never a completion adapter."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tarfile
import zlib

POD = 'cosmos-tax-hub1-financial-e999d60-v1-0-4r29d'
ROOT = '/recordings/range-1-200000'
BASE = ['kubectl', '--context', 'bryanlabs', '-n', 'apps']
PROFILES = {
    1: (200000, 'cosmos-tax-hub1-financial-e999d60-v1-0-4r29d', 'ae26e6f8-4ee7-4ece-96a0-ff6d523829d5'),
    200001: (400000, 'cosmos-tax-hub1-financial-e999d60-v1-1-bbd4l', '4ca733fe-e5e2-4255-884a-0fd890565845'),
    400001: (500000, 'cosmos-tax-hub1-financial-e999d60-v1-2-hfkfd', '68129186-a3e9-4d23-a812-d7f2052d96f3'),
}

def manifest(raw):
    result = {}
    for line in raw.decode().splitlines():
        digest, name = line.split('  ', 1)
        path = Path(name)
        if len(digest) != 64 or any(c not in '0123456789abcdef' for c in digest) or path.is_absolute() or '..' in path.parts or str(path) in result:
            raise ValueError('invalid frozen checkpoint manifest')
        result[str(path)] = digest
    return result

def extract_private(archive_path, destination, expected=None):
    destination.mkdir(mode=0o700)
    found = set()
    with tarfile.open(archive_path) as archive:
        for member in archive:
            path = Path(member.name)
            if path.is_absolute() or '..' in path.parts or member.issym() or member.islnk():
                raise ValueError('unsafe original archive member')
            if member.isdir():
                continue
            if not member.isfile() or member.size > 256 << 20 or str(path) in found:
                raise ValueError('invalid or duplicate original archive member')
            found.add(str(path))
            out_path = destination / path
            out_path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            with archive.extractfile(member) as source, out_path.open('xb') as out:
                for chunk in iter(lambda: source.read(1 << 20), b''):
                    out.write(chunk)
    if expected is not None:
        if found != set(expected) or any(sha(destination / name) != digest for name, digest in expected.items()):
            raise ValueError('original frozen checkpoint byte identity differs')
    return {name: sha(destination / name) for name in sorted(found)}

def checkpoint_capture(dest, first):
    for name in ('source-before.private.sha256', 'initial-clone.private.sha256', 'source-native-proof.private.json'):
        (dest / name).write_bytes(command(['dd', 'if=' + ROOT + '/' + name, 'bs=1048576', 'count=1']))
    original = (dest / 'source-before.private.sha256').read_bytes()
    if original != (dest / 'initial-clone.private.sha256').read_bytes():
        raise ValueError('producer opening was not exact normal checkpoint clone')
    expected = manifest(original)
    with (dest / 'opening-checkpoint.private.tar').open('xb') as out:
        subprocess.run(BASE + ['exec', POD, '--', 'tar', '-C', f'/sources/{first - 1}', '-cf', '-', '.'],
                       stdout=out, stderr=subprocess.PIPE, timeout=120, check=True)
    return extract_private(dest / 'opening-checkpoint.private.tar', dest / 'opening-checkpoint', expected)

def sha(path):
    h = hashlib.sha256()
    with path.open('rb') as f:
        for b in iter(lambda: f.read(1 << 20), b''):
            h.update(b)
    return h.hexdigest()

def command(args):
    return subprocess.check_output(BASE + ['exec', POD, '--'] + args, timeout=30)

def capture(dest, name, target, marker, limit=96 << 20):
    """Retain original compressed chunks and only complete original JSON lines.

    The fragment may have no gzip trailer and can end inside a later height.
    Its selected byte length/digest ends ONLY at the exact requested marker.
    Revalidation must derive that same boundary, never trust EOF as completion.
    """
    process = subprocess.Popen(BASE + ['exec', POD, '--', 'dd',
        'if=' + ROOT + '/' + name + '.ndjson.gz', 'bs=65536',
        'count=' + str(limit // 65536)], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    decoder = zlib.decompressobj(31)
    pending = b''
    raw_hash = hashlib.sha256()
    raw_bytes = records = compressed_bytes = 0
    found = False
    try:
        with (dest / (name + '.source-fragment.gz')).open('xb') as out:
            while not found:
                chunk = process.stdout.read(65536)
                if not chunk:
                    raise ValueError('requested complete boundary absent in bounded source bytes')
                compressed_bytes += len(chunk)
                if compressed_bytes > limit:
                    raise ValueError('capture byte budget exceeded')
                out.write(chunk)
                pending += decoder.decompress(chunk)
                if len(pending) > 16 << 20:
                    raise ValueError('unbounded source line')
                while b'\n' in pending:
                    line, pending = pending.split(b'\n', 1)
                    row = json.loads(line)
                    raw_hash.update(line + b'\n')
                    raw_bytes += len(line) + 1
                    records += 1
                    if row.get('kind') == marker and row.get('height') == target:
                        found = True
                        break
            out.flush()
            os.fsync(out.fileno())
    finally:
        process.stdout.close()
        process.wait(timeout=30)
    return dict(source_path=ROOT + '/' + name + '.ndjson.gz',
                fragment_sha256=sha(dest / (name + '.source-fragment.gz')),
                compressed_fragment_bytes=compressed_bytes, selected_raw_bytes=raw_bytes,
                selected_raw_sha256=raw_hash.hexdigest(), selected_records=records,
                boundary_height=target, boundary_kind=marker,
                source_gzip_whole_closed=False)

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--directory', required=True)
    parser.add_argument('--through', type=int, default=1000)
    parser.add_argument('--first', type=int, choices=sorted(PROFILES), default=1)
    args = parser.parse_args()
    global POD, ROOT
    target, POD, uid = PROFILES[args.first]
    ROOT = f'/recordings/range-{args.first}-{target}'
    if not args.first <= args.through < target:
        raise ValueError('bounded window must remain inside actual producer range')
    os.umask(0o077)
    dest = Path(args.directory)
    dest.mkdir(mode=0o700)
    before = command(['dd', 'if=' + ROOT + '/home/status.json', 'bs=65536', 'count=1'])
    status = json.loads(before)
    if not (status['target'] == target and status['first_height_this_run'] == args.first and status['last_height_committed'] > args.through + 1
            and status['state_hash_verified_through'] >= args.through + 1
            and status['heights_where_results_differ_from_cache'] == 0):
        raise ValueError('actual source status does not cover selected bounded range')
    (dest / 'source-status-before.private.json').write_bytes(before)
    pod = subprocess.check_output(BASE + ['get', 'pod', POD, '-o', 'json'], timeout=30)
    (dest / 'source-pod.private.json').write_bytes(pod)
    if json.loads(pod)['metadata']['uid'] != uid:
        raise ValueError('original producer UID differs')
    opening = checkpoint_capture(dest, args.first) if args.first > 1 else None
    captures = {name: capture(dest, name, target, marker) for name, target, marker in [
        ('movements', args.through, 'height_completed'),
        ('native-history', args.through + 1, 'height')]}
    files = ['genesis/genesis.cosmoshub-1.json']
    for height in range(max(1, args.first - 1), args.through + 2):
        for kind in ('block', 'block_results'):
            files.append(f'cosmoshub-1/{height // 10000}/{height}-{kind}.raw.gz')
    with (dest / 'cache-source.private.tar').open('xb') as out:
        subprocess.run(BASE + ['exec', '-i', POD, '--', 'tar', '-C', '/cache', '-cf', '-', '-T', '-'],
            input=('\n'.join(files) + '\n').encode(), stdout=out, stderr=subprocess.PIPE,
            timeout=90, check=True)
    cache = dest / 'cache'
    cache_pins = extract_private(dest / 'cache-source.private.tar', cache)
    if set(cache_pins) != set(files):
        raise ValueError('exact bounded cache member scope differs')
    after = command(['dd', 'if=' + ROOT + '/home/status.json', 'bs=65536', 'count=1'])
    (dest / 'source-status-after.private.json').write_bytes(after)
    after_status = json.loads(after)
    if not (after_status['target'] == status['target'] == target and
            after_status['last_height_committed'] >= status['last_height_committed'] and
            after_status['state_hash_verified_through'] >= status['state_hash_verified_through'] and
            after_status['heights_where_results_differ_from_cache'] == 0 and
            not after_status.get('stop_reason')):
        raise ValueError('source status changed incompatibly during bounded capture')
    pod_after = subprocess.check_output(BASE + ['get', 'pod', POD, '-o', 'json'], timeout=30)
    first, last = json.loads(pod), json.loads(pod_after)
    if not (first['metadata']['uid'] == last['metadata']['uid'] and
            first['status']['containerStatuses'] == last['status']['containerStatuses'] and
            last['status']['phase'] == 'Running'):
        raise ValueError('source native runtime changed during bounded capture')
    (dest / 'source-pod-after.private.json').write_bytes(pod_after)
    receipt = dict(schema='native033-bounded-in-progress-capture-v1', first=args.first, through=args.through,
        source_target=target, source_job='cosmos-tax-hub1-financial-e999d60-v1',
        source_whole_complete=False, financial_admission=False, streams=captures,
        opening_checkpoint_members_sha256=opening,
        cache_members_sha256=cache_pins,
        source_metadata_sha256={p.name: sha(p) for p in dest.iterdir() if p.is_file()})
    with (dest / 'capture.private.json').open('x') as out:
        json.dump(receipt, out, indent=2, sort_keys=True)
    print(json.dumps(dict(captured_through=args.through, source_whole_complete=False,
        compressed_bytes=sum(c['compressed_fragment_bytes'] for c in captures.values()))))

if __name__ == '__main__':
    main()
