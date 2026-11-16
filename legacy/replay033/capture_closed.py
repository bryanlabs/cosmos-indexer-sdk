#!/usr/bin/env python3
"""Read-only exact captures of original CLOSED profiles, no job dispatch or replay."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import subprocess
import threading
import zlib
import capture_bounded as bounded
from consume_closed import SCHEMA, validate_source
from source_reader import Reader, completed_manifest


class ClosedStreamError(ValueError):
    """Safe predicate metadata only; source records and exception text stay private."""

    def __init__(self, diagnostic):
        self.diagnostic = diagnostic
        super().__init__('closed stream failed: ' + ', '.join(diagnostic['failures']))


def _stream_failure(dest, name, diagnostic):
    receipt = dest / (name + '.capture-failure.private.json')
    try:
        with receipt.open('x') as out:
            os.chmod(receipt, 0o600)
            json.dump(diagnostic, out, sort_keys=True)
    except OSError:
        # Never replace prior evidence or mask the original failed predicates
        # if a full disk or permissions also prevent writing the receipt.
        diagnostic['failures'].append('failure_receipt_io')
    raise ClosedStreamError(diagnostic) from None


class _StderrSummary:
    """Drain the pipe without deadlock, retaining no arbitrary remote output."""

    LIMIT = 4096
    SIGNATURES = (b'permission denied', b'no such file', b'connection reset',
                  b'connection refused', b'unexpected eof', b'error executing command')

    def __init__(self, stream):
        self.stream = stream
        self.bytes = 0
        self.sample = bytearray()
        self.complete = False

    def drain(self):
        try:
            for chunk in iter(lambda: self.stream.read(4096), b''):
                self.bytes += len(chunk)
                self.sample.extend(chunk[:max(0, self.LIMIT - len(self.sample))])
            self.complete = True
        except OSError:
            pass
        finally:
            self.stream.close()

    def metadata(self):
        sample = bytes(self.sample).lower()
        return dict(bytes_observed=self.bytes, sample_limit=self.LIMIT,
                    sample_truncated=self.bytes > self.LIMIT, drain_complete=self.complete,
                    categories=[s.decode().replace(' ', '_') for s in self.SIGNATURES if s in sample])


def closed_stream(dest, name, target, completion, reader, completed):
    remote = bounded.ROOT + '/' + name + '.ndjson.gz'
    source_size = int(reader.command(['stat', '-c', '%s', remote]))
    source_hash = completed[remote]
    path = dest / (name + '.source-closed.gz')
    decoder, digest = zlib.decompressobj(31), hashlib.sha256()
    pending = b''
    count = length = compressed = 0
    last = None
    failures = []

    def fail(reason):
        if reason not in failures:
            failures.append(reason)

    # Exclusive creation precedes transport so an existing failed copy is never
    # replaced, appended to, or accompanied by another remote capture attempt.
    with path.open('xb') as out:
        os.chmod(path, 0o600)
        try:
            p = subprocess.Popen(reader.base + reader.args(['dd', 'if=' + remote, 'bs=1048576']), stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        except OSError as exc:
            _stream_failure(dest, name, dict(schema='closed-stream-failure-v1',
                            failures=['transport_start'], remote_exit_code=None,
                            expected_compressed_bytes=source_size, compressed_bytes=0,
                            transport_errno=exc.errno))
        stderr = _StderrSummary(p.stderr)
        drain = threading.Thread(target=stderr.drain, daemon=True)
        drain.start()
        parsing = True
        try:
            for chunk in iter(lambda: p.stdout.read(65536), b''):
                # Keep every received byte even if gzip/record validation fails.
                out.write(chunk)
                compressed += len(chunk)
                if not parsing:
                    continue
                if decoder.eof:
                    fail('gzip_trailing_data')
                    parsing = False
                    continue
                try:
                    pending += decoder.decompress(chunk)
                except zlib.error:
                    fail('gzip_decode_or_footer')
                    parsing = False
                    continue
                if decoder.unused_data:
                    fail('gzip_trailing_data')
                    parsing = False
                    continue
                if len(pending) > 16 << 20:
                    fail('record_buffer_limit')
                    parsing = False
                    continue
                while b'\n' in pending:
                    line, pending = pending.split(b'\n', 1)
                    digest.update(line + b'\n')
                    length += len(line) + 1
                    count += 1
                    try:
                        last = json.loads(line)
                    except (ValueError, UnicodeError, RecursionError):
                        fail('record_json')
                        parsing = False
                        break
            out.flush()
            os.fsync(out.fileno())
        except OSError:
            fail('copy_io')
        finally:
            p.stdout.close()
            try:
                code = p.wait(timeout=30)
            except subprocess.TimeoutExpired:
                code = None
                fail('remote_wait_timeout')
            drain.join(timeout=1)
        if code != 0:
            fail('remote_exit')
    if compressed != source_size:
        fail('copy_length')
    try:
        actual_hash = bounded.sha(path)
    except OSError:
        actual_hash = None
        fail('checksum_io')
    if actual_hash != source_hash:
        fail('checksum')
    not_evaluated = []
    if not parsing:
        if not decoder.eof:
            not_evaluated.append('gzip_footer')
        not_evaluated.extend(['record_termination', 'completion_trailer', 'completion_target'])
    else:
        if not decoder.eof:
            fail('gzip_footer_missing')
        if pending:
            fail('record_unterminated')
        if not isinstance(last, dict) or last.get('kind') != completion:
            fail('completion_trailer')
        try:
            target_matches = isinstance(last, dict) and int(last['height']) == target
        except (KeyError, ValueError, TypeError, OverflowError):
            target_matches = False
        if not target_matches:
            fail('completion_target')
    if decoder.unused_data:
        fail('gzip_trailing_data')
    # A second stat remains mandatory, including when another predicate failed.
    try:
        final_size = int(reader.command(['stat', '-c', '%s', remote]))
        if final_size != source_size:
            fail('source_size_changed')
    except (OSError, ValueError, subprocess.SubprocessError):
        final_size = None
        fail('source_restat')
    if failures:
        diagnostic = dict(schema='closed-stream-failure-v1', failures=failures,
                          not_evaluated=not_evaluated,
                          expected_compressed_bytes=source_size, compressed_bytes=compressed,
                          final_source_bytes=final_size, remote_exit_code=code,
                          expected_sha256=source_hash, actual_sha256=actual_hash,
                          gzip_eof=decoder.eof, records_seen=count,
                          raw_bytes_seen=length, stderr=stderr.metadata())
        _stream_failure(dest, name, diagnostic)
    return dict(source_path=remote, compressed_bytes=compressed, sha256=source_hash, records=count, raw_bytes=length, raw_sha256=digest.hexdigest(), completion_kind=completion)


def capture(dest, first, reserved_other_bytes=0):
    if type(reserved_other_bytes) is not int or reserved_other_bytes < 0:
        raise ValueError('invalid sibling private workspace reservation')
    with Path('/Users/danb/.cache/cosmos-tax-hub1-capture-owner.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        return capture_locked(dest, first, reserved_other_bytes)


def capture_locked(dest, first, reserved_other_bytes):
    target, bounded.POD, uid = bounded.PROFILES[first]
    bounded.ROOT = f'/recordings/range-{first}-{target}'
    # Refuse BEFORE copying any large source unless original native status is
    # genuinely complete. A target height without verified next-header is not it.
    reader = Reader()
    if reader.exists(bounded.ROOT + '/failed.private'):
        raise ValueError('original shell failed after native recording')
    before = reader.command(['dd', 'if=' + bounded.ROOT + '/home/status.json', 'bs=65536', 'count=1'])
    s = json.loads(before)
    if not (s['chain'] == 'cosmoshub-1' and s['target'] == s['last_height_committed'] == s['state_hash_verified_through'] == target and s['first_height_this_run'] == first and s['heights_where_results_differ_from_cache'] == 0 and s['stop_reason'] == 'reached target; verified by unexecuted next header'):
        raise ValueError('original source is not actually closed, no capture started')
    completed_raw = reader.command(['dd', 'if=' + bounded.ROOT + '/completed.private.sha256', 'bs=65536', 'count=1'])
    completed = completed_manifest(completed_raw, first, target)
    completion_stamp = reader.command(['dd', 'if=' + bounded.ROOT + '/recording-complete.private', 'bs=65536', 'count=1'])
    if not completion_stamp.strip():
        raise ValueError('original shell did not finish gzip validation')
    compressed = sum(int(reader.command(['stat', '-c', '%s', bounded.ROOT + '/' + name + '.ndjson.gz'])) for name in ('movements', 'native-history'))
    # Preserve the existing 55 GiB private-output allowance and 20 GiB reserve,
    # in addition to measured closed streams and a second copy's headroom.
    required = reserved_other_bytes + 2 * compressed + (75 << 30)
    fs = os.statvfs(dest.parent)
    if fs.f_bavail * fs.f_frsize < required:
        raise ValueError('measured closed-source copies plus preserved output/reserve do not fit')
    dest.mkdir(mode=0o700)
    (dest / 'source-status-before.private.json').write_bytes(before)
    pod = subprocess.check_output(bounded.BASE + ['get', 'pod', bounded.POD, '-o', 'json'], timeout=30)
    if json.loads(pod)['metadata']['uid'] != uid:
        raise ValueError('original closed producer identity differs')
    (dest / 'source-pod.private.json').write_bytes(pod)
    (dest / 'reader-pod.private.json').write_text(json.dumps(reader.before))
    (dest / 'completed.private.sha256').write_bytes(completed_raw)
    (dest / 'recording-complete.private').write_bytes(completion_stamp)
    for name in ('binaries.private.sha256', 'source-and-storage-binding.private.json'):
        (dest / name).write_bytes(reader.command(['dd', 'if=' + bounded.ROOT + '/' + name, 'bs=1048576', 'count=1']))
    opening = None
    if first > 1:
        for name in ('source-before.private.sha256', 'initial-clone.private.sha256', 'source-after.private.sha256', 'source-native-proof.private.json'):
            (dest / name).write_bytes(reader.command(['dd', 'if=' + bounded.ROOT + '/' + name, 'bs=1048576', 'count=1']))
        original = (dest / 'source-before.private.sha256').read_bytes()
        if original != (dest / 'initial-clone.private.sha256').read_bytes() or original != (dest / 'source-after.private.sha256').read_bytes():
            raise ValueError('original completed normal opening changed')
        with (dest / 'opening-checkpoint.private.tar').open('xb') as out:
            subprocess.run(reader.base + reader.args(['tar', '-C', f'/sources/{first - 1}', '-cf', '-', '.']), stdout=out, stderr=subprocess.PIPE, timeout=600, check=True)
        opening = bounded.extract_private(dest / 'opening-checkpoint.private.tar', dest / 'opening-checkpoint', bounded.manifest(original))
    streams = {name: closed_stream(dest, name, target, completion, reader, completed) for name, completion in [('movements', 'observation_completed'), ('native-history', 'history_completed')]}
    files = ['genesis/genesis.cosmoshub-1.json']
    for height in range(max(1, first - 1), target + 2):
        for kind in ('block', 'block_results'):
            files.append(f'cosmoshub-1/{height // 10000}/{height}-{kind}.raw.gz')
    with (dest / 'cache-source.private.tar').open('xb') as out:
        subprocess.run(reader.base + reader.args(['tar', '-C', '/cache', '-cf', '-', '-T', '-']), input=('\n'.join(files) + '\n').encode(), stdout=out, stderr=subprocess.PIPE, timeout=3600, check=True)
    pins = bounded.extract_private(dest / 'cache-source.private.tar', dest / 'cache')
    if set(pins) != set(files):
        raise ValueError('closed raw cache scope differs')
    after = reader.command(['dd', 'if=' + bounded.ROOT + '/home/status.json', 'bs=65536', 'count=1'])
    if before != after:
        raise ValueError('original closed native status changed')
    if reader.exists(bounded.ROOT + '/failed.private'):
        raise ValueError('original shell failure marker appeared during capture')
    if reader.command(['dd', 'if=' + bounded.ROOT + '/completed.private.sha256', 'bs=65536', 'count=1']) != completed_raw:
        raise ValueError('original complete-source seal changed during capture')
    (dest / 'reader-pod-after.private.json').write_text(json.dumps(reader.after()))
    (dest / 'source-status-after.private.json').write_bytes(after)
    (dest / 'source-pod-after.private.json').write_bytes(subprocess.check_output(bounded.BASE + ['get', 'pod', bounded.POD, '-o', 'json'], timeout=30))
    metadata = {p.name: bounded.sha(p) for p in dest.iterdir() if p.is_file() and p.name not in ('movements.source-closed.gz', 'native-history.source-closed.gz', 'cache-source.private.tar', 'opening-checkpoint.private.tar')}
    result = dict(schema=SCHEMA, first=first, through=target, source_target=target, source_whole_complete=True, source_shell_failure_marker_absent=True, financial_admission=False, source_job='cosmos-tax-hub1-financial-e999d60-v1', streams=streams, opening_checkpoint_members_sha256=opening, cache_members_sha256=pins, source_metadata_sha256=metadata)
    with (dest / 'capture.private.json').open('x') as out:
        json.dump(result, out, indent=2, sort_keys=True)
    validate_source(dest)
    return dict(first=first, through=target, original_closed_source_captured=True, financial_admission=False)

if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--directory', type=Path, required=True)
    parser.add_argument('--first', type=int, choices=sorted(bounded.PROFILES), required=True)
    parser.add_argument('--reserved-other-bytes', type=int, default=0)
    a = parser.parse_args()
    os.umask(0o077)
    print(json.dumps(capture(a.directory.resolve(), a.first, a.reserved_other_bytes)))
