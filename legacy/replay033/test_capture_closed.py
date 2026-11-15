"""Synthetic-only closed-stream diagnostics. No original readers or data."""
import gzip
import hashlib
import io
import json
import os
import random
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import capture_closed as capture


class FakeReader:
    base = ['synthetic-reader']

    def __init__(self, size, final_size=None):
        self.sizes = iter([size, size if final_size is None else final_size])
        self.commands = []

    def command(self, args):
        self.commands.append(args)
        value = next(self.sizes)
        if isinstance(value, Exception):
            raise value
        return str(value).encode()

    def args(self, args):
        return args


class FakeProcess:
    def __init__(self, data, code=0, stderr=b''):
        self.stdout = io.BytesIO(data)
        self.stderr = io.BytesIO(stderr)
        self.code = code

    def wait(self, timeout):
        if isinstance(self.code, Exception):
            raise self.code
        return self.code


class ClosedStreamDiagnostics(unittest.TestCase):
    def stream(self, records=None):
        if records is None:
            records = [{'kind': 'synthetic'}, {'kind': 'history_completed', 'height': 42}]
        return gzip.compress(b''.join(json.dumps(r).encode() + b'\n' for r in records), mtime=0)

    def run_stream(self, data, *, size=None, source_hash=None, code=0, stderr=b'', final_size=None):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        dest = Path(directory.name)
        reader = FakeReader(len(data) if size is None else size, final_size)
        remote = capture.bounded.ROOT + '/native-history.ndjson.gz'
        process = FakeProcess(data, code, stderr)
        with patch.object(capture.subprocess, 'Popen', return_value=process) as launch:
            try:
                result = capture.closed_stream(dest, 'native-history', 42, 'history_completed', reader,
                                               {remote: source_hash or hashlib.sha256(data).hexdigest()})
                error = None
            except capture.ClosedStreamError as exc:
                result, error = None, exc
        self.assertEqual((dest / 'native-history.source-closed.gz').read_bytes(), data)
        self.assertEqual(os.stat(dest / 'native-history.source-closed.gz').st_mode & 0o777, 0o600)
        self.assertEqual(len(reader.commands), 2)
        launch.assert_called_once()
        self.assertIs(launch.call_args.kwargs['stderr'], subprocess.PIPE)
        receipt = dest / 'native-history.capture-failure.private.json'
        if error:
            self.assertEqual(json.loads(receipt.read_text()), error.diagnostic)
            self.assertEqual(os.stat(receipt).st_mode & 0o777, 0o600)
            self.assertLess(receipt.stat().st_size, 4096)
        else:
            self.assertFalse(receipt.exists())
        return result, error, dest

    def test_zero_exit_with_more_than_pipe_buffer_pending_is_fully_drained(self):
        noise = random.Random(2547221).randbytes(256000).hex()
        data = self.stream([{'kind': 'synthetic', 'noise': noise},
                            {'kind': 'history_completed', 'height': 42}])
        self.assertGreater(len(data), 128 << 10)
        # The process reports exit0 before stdout is consumed. Pending stdout
        # remains authoritative through EOF, not a poll/exit-code shortcut.
        with patch.object(FakeProcess, 'poll', return_value=0, create=True):
            result, error, _ = self.run_stream(data, code=0)
        self.assertIsNone(error)
        self.assertEqual(result['compressed_bytes'], len(data))
        self.assertEqual(result['records'], 2)

    def test_success_preserves_existing_receipt_contract(self):
        result, error, _ = self.run_stream(self.stream())
        self.assertIsNone(error)
        self.assertEqual(result['records'], 2)
        self.assertEqual(result['completion_kind'], 'history_completed')
        self.assertEqual(set(result), {'source_path', 'compressed_bytes', 'sha256', 'records',
                                      'raw_bytes', 'raw_sha256', 'completion_kind'})

    def test_remote_exit_is_not_lost_with_exact_copy(self):
        _, error, _ = self.run_stream(self.stream(), code=17)
        self.assertEqual(error.diagnostic['failures'], ['remote_exit'])
        self.assertEqual(error.diagnostic['remote_exit_code'], 17)

    def test_short_copy_reports_size_checksum_footer_and_exit(self):
        full = self.stream()
        _, error, _ = self.run_stream(full[:-5], size=len(full), source_hash=hashlib.sha256(full).hexdigest(), code=1)
        self.assertTrue({'copy_length', 'checksum', 'gzip_footer_missing', 'remote_exit'} <= set(error.diagnostic['failures']))
        self.assertEqual(error.diagnostic['expected_compressed_bytes'], len(full))
        self.assertEqual(error.diagnostic['compressed_bytes'], len(full) - 5)

    def test_checksum_failure_independent_of_valid_gzip(self):
        _, error, _ = self.run_stream(self.stream(), source_hash='0' * 64)
        self.assertEqual(error.diagnostic['failures'], ['checksum'])

    def test_gzip_crc_and_header_failures_keep_entire_copy(self):
        full = self.stream()
        bad_crc = full[:-8] + bytes([full[-8] ^ 1]) + full[-7:]
        for data in (bad_crc, b'not gzip' + b'x' * 150000):
            with self.subTest(size=len(data)):
                _, error, _ = self.run_stream(data)
                self.assertIn('gzip_decode_or_footer', error.diagnostic['failures'])

    def test_trailing_and_concatenated_gzip_are_never_accepted(self):
        full = self.stream()
        for suffix in (b'trailing', self.stream(), b'x' * 150000):
            with self.subTest(size=len(suffix)):
                _, error, _ = self.run_stream(full + suffix)
                self.assertIn('gzip_trailing_data', error.diagnostic['failures'])

    def test_bad_records_trailer_target_and_pending_are_distinct(self):
        cases = [
            (gzip.compress(b'not-json\n' + b'x' * 150000), 'record_json'),
            (gzip.compress(b'[' * 2000 + b']' * 2000 + b'\n'), 'record_json'),
            (gzip.compress(b'{"kind":"history_completed","height":42}'), 'record_unterminated'),
            (self.stream([]), 'completion_trailer'),
            (self.stream([None]), 'completion_trailer'),
            (self.stream([{'kind': 'other', 'height': 42}]), 'completion_trailer'),
            (self.stream([{'kind': 'history_completed', 'height': 43}]), 'completion_target'),
            (self.stream([{'kind': 'history_completed'}]), 'completion_target'),
            (self.stream([{'kind': 'history_completed', 'height': {'private': 'record'}}]), 'completion_target'),
        ]
        for data, predicate in cases:
            with self.subTest(predicate=predicate):
                _, error, _ = self.run_stream(data)
                self.assertIn(predicate, error.diagnostic['failures'])

    def test_multichunk_parse_failure_does_not_invent_unchecked_failures(self):
        noise = random.Random(1236).randbytes(150000).hex()
        raw = (b'not-json\n' + json.dumps({'kind': 'synthetic', 'noise': noise}).encode()
               + b'\n{"kind":"history_completed","height":42}\n')
        data = gzip.compress(raw, mtime=0)
        self.assertGreater(len(data), 65536)
        _, error, _ = self.run_stream(data)
        self.assertEqual(error.diagnostic['failures'], ['record_json'])
        self.assertEqual(error.diagnostic['not_evaluated'],
                         ['gzip_footer', 'record_termination', 'completion_trailer', 'completion_target'])
        self.assertFalse(error.diagnostic['gzip_eof'])

    def test_known_footer_is_not_reported_as_unevaluated_after_trailing_data(self):
        _, error, _ = self.run_stream(self.stream() + b'trailing')
        self.assertEqual(error.diagnostic['failures'], ['gzip_trailing_data'])
        self.assertTrue(error.diagnostic['gzip_eof'])
        self.assertNotIn('gzip_footer', error.diagnostic['not_evaluated'])

    def test_record_limit_still_rejects_and_drains(self):
        _, error, _ = self.run_stream(gzip.compress(b'x' * ((16 << 20) + 1)))
        self.assertIn('record_buffer_limit', error.diagnostic['failures'])

    def test_source_restat_still_required(self):
        data = self.stream()
        _, error, _ = self.run_stream(data, final_size=len(data) + 1)
        self.assertEqual(error.diagnostic['failures'], ['source_size_changed'])

    def test_source_restat_transport_error_is_safe_and_fails(self):
        _, error, _ = self.run_stream(self.stream(), final_size=subprocess.CalledProcessError(1, 'synthetic', stderr=b'PRIVATE'))
        self.assertEqual(error.diagnostic['failures'], ['source_restat'])
        self.assertIsNone(error.diagnostic['final_source_bytes'])
        self.assertNotIn('PRIVATE', str(error.diagnostic))

    def test_stderr_storage_is_bounded_while_pipe_is_fully_drained(self):
        summary = capture._StderrSummary(io.BytesIO(b'x' * 200000))
        summary.drain()
        self.assertEqual(len(summary.sample), summary.LIMIT)
        self.assertEqual(summary.bytes, 200000)
        self.assertTrue(summary.complete)

    def test_stderr_has_bounded_metadata_and_no_arbitrary_text(self):
        private = b'PRIVATE_SYNTHETIC_RECORD_VALUE'
        stderr = b'permission denied ' + private + b'x' * 200000
        _, error, dest = self.run_stream(self.stream(), code=1, stderr=stderr)
        summary = error.diagnostic['stderr']
        self.assertEqual(summary['bytes_observed'], len(stderr))
        self.assertEqual(summary['sample_limit'], 4096)
        self.assertTrue(summary['sample_truncated'])
        self.assertTrue(summary['drain_complete'])
        self.assertEqual(summary['categories'], ['permission_denied'])
        self.assertNotIn(private.decode(), str(error))
        self.assertNotIn(private, (dest / 'native-history.capture-failure.private.json').read_bytes())

    def test_wait_timeout_is_not_turned_into_success_or_signal(self):
        _, error, _ = self.run_stream(self.stream(), code=subprocess.TimeoutExpired('synthetic', 30))
        self.assertIn('remote_wait_timeout', error.diagnostic['failures'])
        self.assertIsNone(error.diagnostic['remote_exit_code'])

    def test_launch_error_keeps_empty_copy_and_safe_receipt(self):
        with tempfile.TemporaryDirectory() as directory:
            dest = Path(directory)
            remote = capture.bounded.ROOT + '/native-history.ndjson.gz'
            with patch.object(capture.subprocess, 'Popen', side_effect=OSError(2, 'PRIVATE')):
                with self.assertRaises(capture.ClosedStreamError) as caught:
                    capture.closed_stream(dest, 'native-history', 42, 'history_completed', FakeReader(1), {remote: '0' * 64})
            self.assertEqual(caught.exception.diagnostic['failures'], ['transport_start'])
            self.assertEqual(caught.exception.diagnostic['transport_errno'], 2)
            self.assertNotIn('PRIVATE', str(caught.exception.diagnostic))
            self.assertEqual((dest / 'native-history.source-closed.gz').read_bytes(), b'')
            self.assertTrue((dest / 'native-history.capture-failure.private.json').exists())

    def test_receipt_failure_cannot_mask_or_replace_failed_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            dest = Path(directory)
            receipt = dest / 'native-history.capture-failure.private.json'
            receipt.write_bytes(b'older synthetic evidence')
            with self.assertRaises(capture.ClosedStreamError) as caught:
                capture._stream_failure(dest, 'native-history', {'failures': ['remote_exit']})
            self.assertEqual(caught.exception.diagnostic['failures'], ['remote_exit', 'failure_receipt_io'])
            self.assertEqual(receipt.read_bytes(), b'older synthetic evidence')

    def test_existing_copy_cannot_be_replaced_or_recaptured(self):
        with tempfile.TemporaryDirectory() as directory:
            dest = Path(directory)
            path = dest / 'native-history.source-closed.gz'
            path.write_bytes(b'failed synthetic evidence')
            remote = capture.bounded.ROOT + '/native-history.ndjson.gz'
            with patch.object(capture.subprocess, 'Popen') as launch:
                with self.assertRaises(FileExistsError):
                    capture.closed_stream(dest, 'native-history', 42, 'history_completed', FakeReader(1), {remote: '0' * 64})
            launch.assert_not_called()
            self.assertEqual(path.read_bytes(), b'failed synthetic evidence')


if __name__ == '__main__':
    unittest.main()
