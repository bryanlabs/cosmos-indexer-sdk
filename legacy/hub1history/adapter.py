"""Offline lossless Hub1 evidence projection, never a promoter or SQL client.

Produces the existing atomic importer's NDJSON table format. Raw evidence and
blocks are distinct from deferred derived inclusions/tax rows. The ordinary
complete-financial import profile MUST NOT be used for this projection.
"""
import argparse
import base64
import datetime
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import sys
import time

CHAIN = 'cosmoshub-1'
RELEASE = 'cosmos-sdk v0.33.2 (gaia), tendermint v0.31.0-dev0-fix0, iavl v0.12.1'
SCHEMA = 'native033-lossless-history-v1'
PROFILE = 'hub1-lossless-raw-history-deferred-financial-v1'
COLUMNS = {
    'blocks': ['chain_id', 'height', 'time', 'block_hash', 'proposer', 'tx_count'],
    'block_events': ['chain_id', 'height', 'phase', 'event_index', 'type', 'attributes'],
}
STORES = {'main', 'acc', 'staking', 'slashing', 'mint', 'distr', 'gov', 'fee', 'params'}
CHUNK = 192 * 1024
MAX_LINE = 64 * 1024 * 1024


class Refused(Exception):
    pass


def require(ok):
    if not ok:
        raise Refused('offline history projection refused; source retained')


def unique(pairs):
    result = {}
    for k, v in pairs:
        require(k not in result)
        result[k] = v
    return result


def decode(raw):
    return json.loads(raw, object_pairs_hook=unique,
                      parse_constant=lambda _: require(False))


def digest(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b''):
            h.update(chunk)
    return h.hexdigest()


def pin(p):
    require(set(p) == {'path', 'sha256'})
    require(Path(p['path']).is_absolute() and re.fullmatch('[a-f0-9]{64}', p['sha256']))
    require(Path(p['path']).is_file() and digest(p['path']) == p['sha256'])


def hash_bytes(value, size=32):
    require(isinstance(value, str))
    raw = base64.b64decode(value, validate=True)
    require(len(raw) == size)
    return raw.hex().upper()


def roots(row, height):
    commit = row['commit']
    require(commit['Version'] == height)
    hash_bytes(commit['Hash'])
    info = row['native_commit_info']
    require(info['Version'] == height and len(info['StoreInfos']) == len(STORES))
    require({s['Name'] for s in info['StoreInfos']} == STORES)
    for s in info['StoreInfos']:
        require(s['Core']['CommitID']['Version'] == height)
        # Empty stores can have an empty root. Retain their exact native value.
        require(isinstance(s['Core']['CommitID']['Hash'], str))
    return commit


class DigestReader(io.RawIOBase):
    def __init__(self, source):
        self.source = source
        self.hash = hashlib.sha256()

    def readable(self):
        return True

    def readinto(self, buffer):
        n = self.source.readinto(buffer)
        if n:
            self.hash.update(memoryview(buffer)[:n])
        return n


def scan(component, visit=None):
    """Validate complete gzip including trailer before a second projection pass.

    This validates continuity, NOT a replacement for the original signed native
    root proof. Every unknown field survives in the exact raw-line evidence.
    """
    first, last = component['first'], component['last']
    require(type(first) is int and type(last) is int and 1 <= first <= last <= 500015)
    closed = first == 500001 and last == 500015
    require(last <= 500000 or closed)
    terminal_proof = None
    if closed:
        pin(component['terminal_proof'])
        with open(component['terminal_proof']['path'], 'rb') as f:
            terminal_proof = decode(f.read())
    anchor = terminal = None
    previous_hash = previous_commit = None
    count = txs = raw_bytes = 0
    raw_hash = hashlib.sha256()
    require(component.get('compression', 'gzip') in ('gzip', 'none'))
    source = open(component['history']['path'], 'rb')
    reader = DigestReader(source)
    buffered = io.BufferedReader(reader)
    f = gzip.GzipFile(fileobj=buffered) if component.get('compression', 'gzip') == 'gzip' else buffered
    try:
        while True:
            line = f.readline(MAX_LINE + 1)
            if not line:
                break
            require(len(line) <= MAX_LINE and line.endswith(b'\n'))
            raw_hash.update(line)
            raw_bytes += len(line)
            x = decode(line)
            require(isinstance(x, dict) and terminal is None)
            kind = x.get('kind')
            require(x.get('schema') == SCHEMA or
                    (closed and kind == 'closed_terminal_history_completed' and
                     x.get('schema') == 'native033-closed-terminal-v1'))
            if anchor is None:
                require(kind == 'source_anchor' and x['height'] == first - 1)
                require(x['chain'] == CHAIN and x['release'] == RELEASE)
                require(x['genesis_sha256'] == component['genesis_sha256'])
                if first > 1:
                    previous_commit = roots(x, first - 1)
                anchor = x
            elif kind == 'height':
                height = first + count
                require(x['height'] == height <= last and x['chain'] == CHAIN and x['release'] == RELEASE)
                header = x['header']
                require(header['chain_id'] == CHAIN and header['height'] == height)
                require(isinstance(header['time'], str))
                when = datetime.datetime.fromisoformat(header['time'].replace('Z', '+00:00'))
                require(when.utcoffset() == datetime.timedelta(0))
                require(re.fullmatch('[0-9A-F]{40}', header['proposer_address']))
                block_hash = hash_bytes(x['block_hash'])
                require(header['num_txs'] == len(x['transactions']) == len(x['deliver'] or []))
                if closed:
                    require(x['transactions'] == [] and x['deliver'] is None)
                for tx in x['transactions']:
                    require(len(base64.b64decode(tx, validate=True)) > 0)
                if previous_hash is not None:
                    require(header['last_block_id']['hash'] == previous_hash)
                if previous_commit is not None:
                    require(header['app_hash'] == hash_bytes(previous_commit['Hash']))
                previous_commit = roots(x, height)
                previous_hash = block_hash
                count += 1
                txs += len(x['transactions'])
            elif kind == 'closed_terminal_history_completed':
                require(closed and x == terminal_proof and count == 15)
                require(x['first_height'] == first and x['height'] == last)
                require(x['chain'] == CHAIN and x['release'] == RELEASE and
                        x['proof_mode'] == 'closed_pinned_native_reference' and
                        x['next_header_proof'] is False and x['financial_admission'] is False and
                        'unexecuted_next_header' not in x)
                require(x['source_manifest_sha256'] == '417424c8075ea3dcfd166eddaad8f5012c2510797abeca7f4f0864d399f0fd86')
                ref = x['closed_reference']
                require(ref['commit'] == previous_commit and hash_bytes(ref['block_hash']) == previous_hash)
                require(ref['original_status']['state_hash_verified_through'] == last - 1 and
                        ref['original_status']['last_height_committed'] == last and
                        ref['original_tail']['height'] == last)
                terminal = x
            elif kind == 'history_completed':
                require(not closed)
                require(count == last - first + 1 and x['first_height'] == first and x['height'] == last)
                require(x['commit'] == previous_commit)
                next_header = x['unexecuted_next_header']
                require(next_header['chain_id'] == CHAIN and next_header['height'] == last + 1)
                require(next_header['app_hash'] == hash_bytes(previous_commit['Hash']))
                require(next_header['last_block_id']['hash'] == previous_hash)
                hash_bytes(x['unexecuted_next_block_hash'])
                terminal = x
            else:
                require(False)
            if visit is not None:
                visit(x, line)
        require(reader.hash.hexdigest() == component['history']['sha256'])
    finally:
        f.close()
        buffered.close()
        source.close()
    require(anchor is not None and terminal is not None)
    return {'heights': count, 'transactions_preserved': txs,
            'raw_history_bytes': raw_bytes, 'raw_history_sha256': raw_hash.hexdigest(),
            'opening_commit': anchor['commit'], 'closing_commit': previous_commit,
            'terminal_mode': 'closed_pinned_native_reference' if closed else 'unexecuted_next_header'}


class Table:
    def __init__(self, directory, name):
        self.name = name
        self.path = directory / (name + '.ndjson')
        self.f = open(self.path, 'xb')
        os.chmod(self.path, 0o600)
        self.h = hashlib.sha256()
        self.rows = 0

    def put(self, row):
        require(set(row) == set(COLUMNS[self.name]))
        raw = json.dumps(row, separators=(',', ':'), ensure_ascii=True).encode() + b'\n'
        self.f.write(raw)
        self.h.update(raw)
        self.rows += 1

    def close(self):
        self.f.flush()
        os.fsync(self.f.fileno())
        self.f.close()
        return {'table': self.name, 'columns': COLUMNS[self.name], 'rows': self.rows,
                'file': {'path': str(self.path), 'sha256': self.h.hexdigest()}}


def project(manifest, destination, progress_path=None):
    started = time.monotonic()
    require(manifest['version'] == 1 and manifest['chain'] == CHAIN)
    components = manifest['components']
    require(len(components) > 0)
    next_height = components[0]['first']
    summaries = []
    # Nothing is emitted until every source pin and every history closure passes.
    for c in components:
        require(c['first'] == next_height)
        next_height = c['last'] + 1
        pin(c['history'])
        require(len(c['artifacts']) > 0)
        require(len({a['path'] for a in c['artifacts']}) == len(c['artifacts']))
        for a in c['artifacts']:
            pin(a)
        summaries.append(scan(c))
    for left, right in zip(summaries, summaries[1:]):
        require(left['closing_commit'] == right['opening_commit'])
    destination = Path(destination)
    require(destination.is_absolute())
    resolved = destination.resolve()
    for c in components:
        for a in [c['history']] + c['artifacts']:
            source = Path(a['path']).resolve()
            require(resolved != source and resolved not in source.parents and
                    source.parent not in resolved.parents and resolved != source.parent)
    if progress_path is not None:
        pp = Path(progress_path)
        require(pp.is_absolute() and not pp.exists() and not pp.is_symlink())
        require(pp.resolve() not in [destination / name for name in
                                    ('blocks.ndjson', 'block_events.ndjson', 'projection.safe.json')])
        for c in components:
            for a in [c['history']] + c['artifacts']:
                require(Path(a['path']).resolve().parent not in pp.resolve().parents)
        with open(pp, 'x') as f:
            os.chmod(f.name, 0o600)
            f.write('{"projection_finished":false,"production_writes":0}\n')
    destination.mkdir(mode=0o700)
    blocks, events = Table(destination, 'blocks'), Table(destination, 'block_events')
    total_bytes = sum(s['raw_history_bytes'] for s in summaries) + sum(
        os.stat(a['path']).st_size for c in components for a in c['artifacts'])
    completed_bytes = 0
    window_time = time.monotonic()
    window_bytes = 0

    def progress(done=0, final=False):
        nonlocal completed_bytes, window_bytes, window_time
        completed_bytes += done
        now = time.monotonic()
        if progress_path is None or (not final and now - window_time < 30):
            return
        rate = (completed_bytes - window_bytes) / max(now - window_time, 0.001)
        remaining = max(0, total_bytes - completed_bytes)
        finish = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=remaining / max(rate, 1))
        state = {'profile': PROFILE, 'done_input_bytes': completed_bytes,
                 'total_input_bytes': total_bytes, 'latest_window_seconds': now - window_time,
                 'latest_window_input_bytes_per_second': rate,
                 'project_finish_utc': finish.isoformat(), 'projection_finished': final,
                 'production_admitted': False, 'production_writes': 0}
        path = Path(progress_path)
        tmp = path.with_name(path.name + '.tmp')
        with open(tmp, 'x') as f:
            os.chmod(f.name, 0o600)
            json.dump(state, f, sort_keys=True)
            f.write('\n')
        os.replace(tmp, path)
        window_bytes, window_time = completed_bytes, now

    try:
        for c in components:
            indices = {}

            def event(height, kind, attrs):
                i = indices.get(height, 0)
                indices[height] = i + 1
                events.put({'chain_id': CHAIN, 'height': height, 'phase': 1,
                            'event_index': i, 'type': kind, 'attributes': attrs})

            def visit(x, line):
                kind = x['kind']
                height = max(c['first'], x['height'])
                if kind == 'closed_terminal_history_completed':
                    height = c['last']
                progress(len(line))
                event(height, 'hub1_native033_raw_history', [
                    ['encoding', 'base64-exact-jsonl-line'], ['kind', kind],
                    ['raw', base64.b64encode(line).decode('ascii')]])
                if kind == 'source_anchor':
                    event(height, 'hub1_raw_history_status', [
                        ['profile', PROFILE], ['derived_inclusions', 'deferred'],
                        ['derived_tax_rows', 'deferred'], ['tax_complete', 'false'],
                        ['canonical_height', '500000'], ['historical_tail', '500001..500015'],
                        ['intermediate_zero_height_payouts', 'validation-only-not-income']])
                    for artifact_no, a in enumerate(c['artifacts']):
                        h = hashlib.sha256()
                        size = 0
                        chunks = 0
                        with open(a['path'], 'rb') as f:
                            for raw in iter(lambda: f.read(CHUNK), b''):
                                h.update(raw)
                                size += len(raw)
                                event(height, 'hub1_source_artifact_chunk', [
                                    ['artifact', str(artifact_no)], ['chunk', str(chunks)],
                                    ['encoding', 'base64-exact-source-bytes'],
                                    ['raw', base64.b64encode(raw).decode('ascii')]])
                                chunks += 1
                                progress(len(raw))
                        require(h.hexdigest() == a['sha256'])
                        event(height, 'hub1_source_artifact_complete', [
                            ['artifact', str(artifact_no)], ['source_path', a['path']],
                            ['sha256', a['sha256']], ['bytes', str(size)], ['chunks', str(chunks)]])
                elif kind == 'height':
                    hdr = x['header']
                    blocks.put({'chain_id': CHAIN, 'height': height, 'time': hdr['time'],
                                'block_hash': hash_bytes(x['block_hash']),
                                'proposer': hdr['proposer_address'], 'tx_count': len(x['transactions'])})
                # Bound bookkeeping to one native height, not the whole chain.
                if kind == 'height' and height > c['first']:
                    indices.pop(height - 1, None)

            scan(c, visit)
            # Detect changes during both passes, including artifact replacement.
            pin(c['history'])
            for a in c['artifacts']:
                pin(a)
        projections = [blocks.close(), events.close()]
        progress(final=True)
        receipt = {
            'version': 1, 'profile': PROFILE, 'chain': CHAIN,
            'first': components[0]['first'], 'last': components[-1]['last'],
            'source_manifest_sha256': hashlib.sha256(json.dumps(manifest, sort_keys=True, separators=(',', ':')).encode()).hexdigest(),
            'tables': projections, 'components': summaries,
            'raw_projection_complete_for_declared_components': True,
            'full_hub1_history_complete': components[0]['first'] == 1 and components[-1]['last'] == 500015,
            'sql_inclusion_projection_complete': False,
            'full_chain_books_tax_acceptance': False,
            'derived_inclusions': 'deferred: exact signed bytes and complete results retained in raw history',
            'derived_financial_rows': 'deferred: native033 shares require native exchange context; no zero or invented amounts',
            'production_admitted': False, 'production_writes': 0,
            'seconds': time.monotonic() - started,
        }
        with open(destination / 'projection.safe.json', 'x') as f:
            os.chmod(f.name, 0o600)
            json.dump(receipt, f, sort_keys=True)
            f.write('\n')
            f.flush()
            os.fsync(f.fileno())
        return receipt
    finally:
        for table in (blocks, events):
            if not table.f.closed:
                table.f.close()


def main():
    p = argparse.ArgumentParser()
    p.add_argument('--manifest', required=True)
    p.add_argument('--output', required=True)
    p.add_argument('--progress-safe')
    a = p.parse_args()
    try:
        with open(a.manifest, 'rb') as f:
            manifest = decode(f.read())
        r = project(manifest, a.output, a.progress_safe)
        print(json.dumps({'profile': PROFILE, 'first': r['first'], 'last': r['last'],
                          'tables': {t['table']: t['rows'] for t in r['tables']},
                          'seconds': r['seconds'], 'production_admitted': False}))
    except Exception:
        print('offline history projection refused; private details and failed output retained', file=sys.stderr)
        return 2
    return 0


if __name__ == '__main__':
    sys.exit(main())
