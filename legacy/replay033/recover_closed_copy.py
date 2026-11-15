#!/usr/bin/env python3
"""Exclusive file reconstruction, never a mutable financial-store resume.

The retained incomplete gzip is only an untrusted byte prefix. The complete new
file must match the original producer's SHA256, then pass full gzip and every
JSON-record validation. Original actors and failed artifacts remain untouched.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import capture_closed as closed
import capture_bounded as bounded


def reconstruct(dest, name, target, completion, reader, completed, prefix, scanner):
    remote = bounded.ROOT + '/' + name + '.ndjson.gz'
    expected = int(reader.command(['stat', '-c', '%s', remote]))
    path = dest / (name + '.source-closed.gz')
    offset = prefix.stat().st_size if prefix else 0
    if not 0 <= offset < expected:
        raise ValueError('retained prefix length invalid')
    with path.open('xb') as out:
        os.chmod(path, 0o600)
        if prefix:
            with prefix.open('rb') as source:
                shutil.copyfileobj(source, out, 1 << 20)
        # Keep slow validation off the remote stdout path. The lower transport
        # cause remains unproven; exact length and hash still detect every loss.
        args = ['dd', 'if=' + remote, 'bs=1048576', 'skip=' + str(offset), 'iflag=skip_bytes']
        with tempfile.TemporaryFile() as errors:
            result = subprocess.run(reader.base + reader.args(args), stdout=out, stderr=errors, timeout=1800)
        out.flush(); os.fsync(out.fileno())
    size = path.stat().st_size
    actual = bounded.sha(path)
    final = int(reader.command(['stat', '-c', '%s', remote]))
    failures = []
    if result.returncode: failures.append('remote_exit')
    if size != expected: failures.append('copy_length')
    if actual != completed[remote]: failures.append('checksum')
    if final != expected: failures.append('source_size_changed')
    if failures:
        closed._stream_failure(dest, name, dict(schema='closed-stream-failure-v1', failures=failures,
            expected_compressed_bytes=expected, compressed_bytes=size, final_source_bytes=final,
            remote_exit_code=result.returncode, actual_sha256=actual, expected_sha256=completed[remote]))
    # Scanner emits only counts, digests and the completion kind/height.
    result = subprocess.run([str(scanner), str(path)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=1800)
    if result.returncode:
        closed._stream_failure(dest, name, dict(schema='closed-stream-failure-v1', failures=['full_record_gzip_scan'], remote_exit_code=result.returncode))
    metadata = json.loads(result.stdout)
    if metadata.pop('height') != target or metadata['completion_kind'] != completion:
        closed._stream_failure(dest, name, dict(schema='closed-stream-failure-v1', failures=['completion_target_or_kind']))
    return dict(source_path=remote, compressed_bytes=size, sha256=actual, **metadata)


def capture(dest, first, prefix, scanner):
    original = closed.closed_stream
    def stream(dest, name, target, completion, reader, completed):
        return reconstruct(dest, name, target, completion, reader, completed,
                           prefix if name == 'movements' else None, scanner)
    closed.closed_stream = stream
    try:
        return closed.capture(dest, first)
    finally:
        closed.closed_stream = original


if __name__ == '__main__':
    p = argparse.ArgumentParser()
    p.add_argument('--directory', type=Path, required=True)
    p.add_argument('--prefix', type=Path, required=True)
    p.add_argument('--scanner', type=Path, required=True)
    a = p.parse_args()
    os.umask(0o077)
    capture(a.directory, 200001, a.prefix, a.scanner)
