"""Canonicalize raw event JSON in PostgreSQL's restricted emitted ASCII domain.

Blocks go through actual PostgreSQL, including its timestamp coercion. Event
objects have only fixed integer/text fields and arrays of ASCII text pairs;
there are no arbitrary JSON objects or floating point values to approximate.
Actual private PostgreSQL verifies deterministic samples before admission.
No SQL connection or credential is used by this module.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import sys
import time

from adapter import CHAIN, COLUMNS, MAX_LINE, decode, require


def canonical_event(row):
    require(set(row) == set(COLUMNS['block_events']))
    require(row['chain_id'] == CHAIN)
    for key in ('height', 'phase', 'event_index'):
        require(type(row[key]) is int and 0 <= row[key] <= 2147483647)
    require(row['height'] > 0 and row['phase'] == 1)
    require(type(row['attributes']) is list and isinstance(row['type'], str))
    for pair in row['attributes']:
        require(type(pair) is list and len(pair) == 2)
        for text in pair:
            require(isinstance(text, str) and text.isascii() and '\x00' not in text)
    require(row['type'].isascii() and '\x00' not in row['type'])
    ordered = {k: row[k] for k in sorted(row, key=lambda k: (len(k.encode()), k.encode()))}
    return json.dumps(ordered, ensure_ascii=False, separators=(', ', ': ')).encode('ascii') + b'\n'


def canonicalize(source, output, samples, progress=None):
    started = window = time.monotonic()
    size = os.stat(source).st_size
    read = last_read = n = sample_count = 0
    input_hash, output_hash = hashlib.sha256(), hashlib.sha256()
    kinds = set()
    with open(source, 'rb') as f, open(output, 'xb') as out, open(samples, 'xb') as sample:
        os.chmod(output, 0o600)
        os.chmod(samples, 0o600)
        while True:
            raw = f.readline(MAX_LINE * 2 + 1)
            if not raw:
                break
            require(raw.endswith(b'\n') and len(raw) <= MAX_LINE * 2)
            read += len(raw)
            input_hash.update(raw)
            row = decode(raw)
            canonical = canonical_event(row)
            out.write(canonical)
            output_hash.update(canonical)
            n += 1
            # Cover every event type and component, plus regularly spaced large
            # artifacts. Samples go to a private file, never terminal output.
            kind = (row['type'], row['height'] in (1, 200001, 400001, 500001))
            if n % 4096 == 0 or kind not in kinds:
                sample.write(canonical)
                sample_count += 1
                kinds.add(kind)
            now = time.monotonic()
            if progress and now - window >= 30:
                rate = (read - last_read) / (now - window)
                import datetime
                finish = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=(size - read) / rate)
                with open(progress, 'w') as status:
                    json.dump({'bytes_read': read, 'total': size, 'latest_window_bytes_per_second': rate,
                               'latest_window_seconds': now - window, 'finish_utc': finish.isoformat(),
                               'production_writes': 0}, status)
                window, last_read = now, read
        out.flush()
        os.fsync(out.fileno())
        sample.flush()
        os.fsync(sample.fileno())
    return {'rows': n, 'input_sha256': input_hash.hexdigest(), 'output_sha256': output_hash.hexdigest(),
            'actual_pg_samples': sample_count, 'seconds': time.monotonic() - started,
            'source_bytes': size, 'output_bytes': os.stat(output).st_size,
            'all_input_keys_and_values_preserved': True,
            'canonicalization_domain': 'fixed-root-keys-int32-and-ascii-text-pairs-only',
            'production_writes': 0}


def main():
    p = argparse.ArgumentParser()
    p.add_argument('--source', required=True)
    p.add_argument('--output', required=True)
    p.add_argument('--samples', required=True)
    p.add_argument('--receipt', required=True)
    p.add_argument('--progress')
    args = p.parse_args()
    try:
        require(Path(args.source).resolve() != Path(args.output).resolve())
        for path in [args.output, args.samples, args.receipt] + ([args.progress] if args.progress else []):
            require(Path(path).is_absolute() and not Path(path).exists() and not Path(path).is_symlink())
        r = canonicalize(args.source, args.output, args.samples, args.progress)
        with open(args.receipt, 'x') as f:
            os.chmod(f.name, 0o600)
            json.dump(r, f, sort_keys=True)
            f.write('\n')
        print(json.dumps(r, sort_keys=True))
    except Exception:
        print('offline canonicalization refused; private output and source retained', file=sys.stderr)
        return 2
    return 0


if __name__ == '__main__':
    sys.exit(main())
