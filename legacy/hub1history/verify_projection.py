"""Read-only round-trip check of the adapter's entire private NDJSON output."""
import argparse
import base64
import hashlib
import json
import sys
from pathlib import Path

from adapter import CHAIN, COLUMNS, MAX_LINE, PROFILE, decode, digest, require


def verify(manifest, directory):
    directory = Path(directory)
    receipt = decode((directory / 'projection.safe.json').read_bytes())
    require(receipt['profile'] == PROFILE and receipt['production_admitted'] is False and
            receipt['full_chain_books_tax_acceptance'] is False and
            receipt['sql_inclusion_projection_complete'] is False)
    require(receipt['source_manifest_sha256'] == hashlib.sha256(
        json.dumps(manifest, sort_keys=True, separators=(',', ':')).encode()).hexdigest())
    require(len(receipt['tables']) == 2 and
            {t['table'] for t in receipt['tables']} == set(COLUMNS))
    components = manifest['components']
    first, last = components[0]['first'], components[-1]['last']
    block_count = txs = event_count = artifacts = raw_lines = 0
    history_hashes = [hashlib.sha256() for c in components]
    history_bytes = [0 for c in components]
    artifact_index = chunk_index = artifact_bytes = 0
    artifact_hash = hashlib.sha256()
    component_index = -1
    previous = (-1, -1, -1)
    for table in receipt['tables']:
        require(table['columns'] == COLUMNS[table['table']])
        path = directory / (table['table'] + '.ndjson')
        require(str(path) == table['file']['path'])
        file_hash = hashlib.sha256()
        n = 0
        with open(path, 'rb') as f:
            while True:
                line = f.readline(MAX_LINE * 2 + 1)
                if not line:
                    break
                require(line.endswith(b'\n') and len(line) <= MAX_LINE * 2)
                file_hash.update(line)
                row = decode(line)
                require(set(row) == set(COLUMNS[table['table']]) and row['chain_id'] == CHAIN)
                n += 1
                if table['table'] == 'blocks':
                    require(row['height'] == first + block_count)
                    block_count += 1
                    require(type(row['tx_count']) is int and row['tx_count'] >= 0)
                    txs += row['tx_count']
                    continue
                key = (row['height'], row['phase'], row['event_index'])
                require(key > previous and first <= row['height'] <= last and row['phase'] == 1)
                if key[:2] == previous[:2]:
                    require(key[2] == previous[2] + 1)
                else:
                    require(key[2] == 0)
                previous = key
                event_count += 1
                attrs = row['attributes']
                require(len({pair[0] for pair in attrs}) == len(attrs))
                attrs = dict(attrs)
                kind = row['type']
                if kind == 'hub1_native033_raw_history':
                    raw = base64.b64decode(attrs['raw'], validate=True)
                    x = decode(raw)
                    require(attrs['encoding'] == 'base64-exact-jsonl-line' and x['kind'] == attrs['kind'])
                    if x['kind'] == 'source_anchor':
                        if component_index >= 0:
                            require(artifact_index == len(components[component_index]['artifacts']) and chunk_index == 0)
                        component_index += 1
                        require(component_index < len(components))
                        require(row['height'] == components[component_index]['first'])
                        artifact_index = 0
                    require(component_index >= 0)
                    history_hashes[component_index].update(raw)
                    history_bytes[component_index] += len(raw)
                    raw_lines += 1
                elif kind == 'hub1_source_artifact_chunk':
                    require(component_index >= 0 and row['height'] == components[component_index]['first'])
                    require(int(attrs['artifact']) == artifact_index and int(attrs['chunk']) == chunk_index)
                    require(attrs['encoding'] == 'base64-exact-source-bytes')
                    raw = base64.b64decode(attrs['raw'], validate=True)
                    require(len(raw) > 0)
                    artifact_hash.update(raw)
                    artifact_bytes += len(raw)
                    chunk_index += 1
                elif kind == 'hub1_source_artifact_complete':
                    require(component_index >= 0 and row['height'] == components[component_index]['first'])
                    expected = components[component_index]['artifacts'][artifact_index]
                    require(int(attrs['artifact']) == artifact_index and int(attrs['chunks']) == chunk_index)
                    require(int(attrs['bytes']) == artifact_bytes and attrs['sha256'] == artifact_hash.hexdigest())
                    require(attrs['sha256'] == expected['sha256'] and attrs['source_path'] == expected['path'])
                    # Source originals must still have their exact declared bytes.
                    require(digest(expected['path']) == expected['sha256'])
                    artifacts += 1
                    artifact_index += 1
                    chunk_index = artifact_bytes = 0
                    artifact_hash = hashlib.sha256()
                elif kind == 'hub1_raw_history_status':
                    require(attrs['profile'] == PROFILE and attrs['derived_inclusions'] == 'deferred' and
                            attrs['derived_tax_rows'] == 'deferred' and attrs['tax_complete'] == 'false')
                else:
                    require(False)
        require(n == table['rows'] and file_hash.hexdigest() == table['file']['sha256'])
    require(block_count == last - first + 1)
    require(txs == sum(c['transactions_preserved'] for c in receipt['components']))
    require(component_index == len(components) - 1 and
            artifact_index == len(components[-1]['artifacts']) and chunk_index == 0)
    require(raw_lines == block_count + 2 * len(components))
    for i, summary in enumerate(receipt['components']):
        require(history_hashes[i].hexdigest() == summary['raw_history_sha256'] and
                history_bytes[i] == summary['raw_history_bytes'])
    return {'profile': PROFILE, 'first': first, 'last': last, 'blocks': block_count,
            'signed_transactions_retained': txs, 'raw_history_lines': raw_lines,
            'source_artifacts_roundtripped': artifacts, 'block_events': event_count,
            'all_raw_bytes_roundtripped': True, 'sql_fixture_tested': False,
            'production_admitted': False, 'production_writes': 0}


def main():
    p = argparse.ArgumentParser()
    p.add_argument('--manifest', required=True)
    p.add_argument('--directory', required=True)
    p.add_argument('--receipt', required=True)
    args = p.parse_args()
    try:
        manifest = decode(Path(args.manifest).read_bytes())
        result = verify(manifest, args.directory)
        with open(args.receipt, 'x') as f:
            import os
            os.chmod(f.name, 0o600)
            json.dump(result, f, sort_keys=True)
            f.write('\n')
        print(json.dumps(result, sort_keys=True))
    except Exception:
        print('offline raw projection roundtrip refused; source and output retained', file=sys.stderr)
        return 2
    return 0


if __name__ == '__main__':
    sys.exit(main())
