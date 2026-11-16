#!/usr/bin/env python3
"""Record part of one complete Hub 1 source range. No production admission or replay.

A part is first..through inside the range the closed source covers. A part that
starts after the range start first rebuilds the nine-store state from the same
recorded writes without the native helper, then the helper opens the saved
native checkpoint of that height and refuses unless the rebuilt state equals
it key for key. From there every record is checked exactly as a whole run.
"""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import subprocess
import time
import zlib
from consume_bounded import CommitJournal, HistoryWindow, native_numbers, sha
from consume_closed import ClosedConsumer, ClosedHistory, closed_lines, validate_native_assets, validate_source
from compare_zero_export import strict_json
from verify_movement_rehearsal import require

SCHEMA = 'native033-split-range-financial-v1'
SPLITS = {1: (100000,), 200001: (300000,), 400001: ()}
WRITE, CACHE, DONE, BASE, HASH = (('"kind":"%s"' % kind).encode() for kind in ('native_write', 'native_cache_write', 'height_completed', 'native_state_checkpoint', 'native_state_hash'))


def closed_raw_lines(root, binding, name):
    """Every exact byte of one complete gzip member, as lines without the newline."""
    spec = binding['streams'][name]
    path = root / (name + '.source-closed.gz')
    require(path.is_file() and not path.is_symlink() and sha(path) == spec['sha256'] and path.stat().st_size == spec['compressed_bytes'], 'changed complete original compressed source')
    decoder, digest, pending = zlib.decompressobj(31), hashlib.sha256(), b''
    count = length = 0
    last = None
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(1 << 18), b''):
            require(not decoder.eof, 'trailing or concatenated complete source gzip')
            data = decoder.decompress(chunk)
            require(not decoder.unused_data, 'trailing compressed source')
            digest.update(data)
            length += len(data)
            lines = (pending + data).split(b'\n')
            pending = lines.pop()
            require(len(pending) <= 16 << 20, 'unbounded source line')
            for last in lines:
                count += 1
                yield last
    require(decoder.eof and not decoder.unused_data and not pending and last is not None, 'complete source lacks gzip close or ends mid-record')
    require(count == spec['records'] and length == spec['raw_bytes'] and digest.hexdigest() == spec['raw_sha256'], 'complete original raw source binding differs')
    final = native_numbers(strict_json(last))
    require(final['kind'] == spec['completion_kind'] and int(final['height']) == binding['through'], 'complete source lacks exact actual completion marker')


def parse(raw):
    return native_numbers(strict_json(raw))


class FastForwardJournal(CommitJournal):
    """The same write and cache-flush reconstruction, hashed only where asked."""
    def __init__(self, verify_height):
        super().__init__()
        self.verify_height = verify_height

    def accept(self, record):
        if record['kind'] == 'height_completed' and int(record['height']) != self.verify_height:
            self.frames.clear()
            self.tips.clear()
            self.block.clear()
            self.heights += 1
            return
        super().accept(record)


def fast_forward(lines, base_first, through):
    """Rebuild committed state at `through` from recorded writes. Returns baseline records and the phase count."""
    journal = FastForwardJournal(through)
    started = time.monotonic()
    height, hashes = base_first - 1, 0
    for raw in lines:
        if WRITE in raw or CACHE in raw:
            record = parse(raw)
            if record['kind'] in ('native_write', 'native_cache_write'):
                require(int(record['height']) == height + 1, 'recorded write outside the height being rebuilt')
                journal.accept(record)
            continue
        if HASH in raw:
            hashes += 1
            if height + 1 == through:
                record = parse(raw)
                require(record['kind'] == 'native_state_hash' and int(record['height']) == through, 'state hash outside the checked height')
                journal.accept(record)
            continue
        if DONE in raw or BASE in raw:
            record = parse(raw)
            if record['kind'] == 'native_state_checkpoint':
                require(height == base_first - 1 and not hashes and record['label'].startswith('segment_baseline/'), 'unexpected baseline or final marker while rebuilding')
                journal.accept(record)
            elif record['kind'] == 'height_completed':
                require(int(record['height']) == height + 1 and len(journal.canonical) == 9, 'height gap while rebuilding')
                journal.accept(record)
                height += 1
                if height % 10000 == 0:
                    print(json.dumps(dict(rebuilt_height=height, seconds=round(time.monotonic() - started, 1))), flush=True)
                if height == through:
                    break
    require(height == through and journal.heights == through - base_first + 1 and hashes % 9 == 0, 'recorded source ended before the rebuilt height')
    baseline = []
    for label, state in journal.canonical.items():
        pairs = [{'key': key, 'value': state[key]} if state[key] else {'key': key} for key in sorted(state, key=lambda k: base64.b64decode(k, validate=True))]
        baseline.append(dict(kind='native_state_checkpoint', label='segment_baseline/' + label, height=through + 1, tx=-1, message=-1,
                             phase='block', status='committed', code=0, before=pairs))
    return baseline, hashes // 9


class SplitHistory(ClosedHistory):
    def __init__(self, root, binding, first):
        self.rows = iter(closed_lines(root, binding, 'native-history'))
        source = self.validate(next(self.rows), anchor=True)
        require(source['height'] == binding['first'] - 1, 'closed opening native height differs')
        self.cache, self.last = {}, source['height']
        self.target, self.completion = binding['through'], None
        self.anchor = source
        if first > binding['first']:
            row = HistoryWindow.__getitem__(self, first - 1)
            self.anchor = dict(schema=row['schema'], kind='source_anchor', release=row['release'], chain=row['chain'], height=row['height'],
                               commit=row['commit'], native_commit_info=row['native_commit_info'], block_hash=row['block_hash'],
                               genesis_sha256=source['genesis_sha256'])


def opening_manifest(directory, manifest):
    pins = {}
    for line in manifest.read_text().splitlines():
        digest, name = line.split(None, 1)
        name = name.lstrip('*').removeprefix('./')
        rel = Path(name)
        require(len(digest) == 64 and not rel.is_absolute() and '..' not in rel.parts and name not in pins, 'unsafe opening checkpoint manifest')
        pins[name] = digest
    actual = {str(p.relative_to(directory)) for p in directory.rglob('*') if not p.is_dir()}
    require(pins and actual == set(pins), 'opening checkpoint member coverage differs from its manifest')
    for name, digest in pins.items():
        path = directory / name
        require(path.is_file() and not path.is_symlink() and sha(path) == digest, 'opening checkpoint bytes differ from its manifest')
    return sha(manifest)


def verify(root, binary, receipt, first, through, opening=None, manifest=None, resume=False, savepoint_every=0, validator_history=None, validator_history_sha256='', evidence_cache=None, evidence_capture_sha256=''):
    b = validate_source(root)
    prefix, closed = first > b['first'], through == b['through']
    require(b['first'] <= first <= through <= b['through'], 'part lies outside the complete source range')
    require(not prefix or first - 1 in SPLITS[b['first']] or resume or os.environ.get('HUB1_SPLIT_PROOF_SLICE') == '1', 'part must open at a saved native checkpoint height')
    require(not resume or prefix, 'only a part that opens mid-range can resume retained working state')
    require(prefix == (opening is not None) == (manifest is not None), 'a later part needs its saved opening checkpoint and manifest, the range start must not have one')
    manifest_sha = opening_manifest(opening, manifest) if prefix else ''
    history = SplitHistory(root, b, first)
    lines = closed_raw_lines(root, b, 'movements')
    forward = dict(heights=0, typed_phases=0, seconds=0.0)
    baseline = []
    if prefix:
        started = time.monotonic()
        baseline, phases = fast_forward(lines, b['first'], first - 1)
        forward = dict(heights=first - b['first'], typed_phases=phases, seconds=round(time.monotonic() - started, 3))
    command = [str(binary), '--cache', str(root / 'cache'), '--receipt', str(receipt.parent / 'native-proof.private.json'), '--working-home', str(receipt.parent / 'native-home')]
    if prefix:
        command += ['--checkpoint-home', str(opening), '--progress-phase-base', str(forward['typed_phases'])] + (['--resume-opening'] if resume else [])
    elif b['first'] > 1:
        command += ['--checkpoint-home', str(root / 'opening-checkpoint')]
    if validator_history is not None:
        require(validator_history_sha256 and sha(validator_history) == validator_history_sha256, 'supplemental native validator history pin differs')
        command += ['--validator-history', str(validator_history), '--validator-history-sha256', validator_history_sha256]
    if evidence_cache is not None:
        require(evidence_capture_sha256 and sha(evidence_cache.parent / 'capture.private.json') == evidence_capture_sha256, 'supplemental evidence capture pin differs')
        command += ['--evidence-cache', str(evidence_cache), '--evidence-capture-sha256', evidence_capture_sha256]
    with (receipt.parent / 'native-consumer.private.log').open('xb') as log:
        p = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=log, stderr=subprocess.STDOUT)
        def send(q):
            p.stdin.write((json.dumps(q, separators=(',', ':')) + '\n').encode())
        c = ClosedConsumer(send, history, through, first)

        def part(directory, height, next_header, source_closed, extra=()):
            asset_blocks, asset_rows, asset_hash = validate_native_assets(directory)
            ending = history.cache[height]
            return dict(schema=SCHEMA, first=first, through=height, source_range=[b['first'], b['through']],
                        heights=height - first + 1, transactions=c.transactions, phases=dict(c.counts), journal_counts=dict(c.journal.counts),
                        opening_commit=history.anchor['commit'], opening_store_roots=history.anchor['native_commit_info'],
                        ending_commit=ending['commit'], ending_store_roots=ending['native_commit_info'],
                        ending_block_hash=ending['block_hash'], next_header=next_header, next_header_is_source_closure=source_closed,
                        source_closed_at_through=source_closed, fast_forward=forward, progress_phase_base=forward['typed_phases'],
                        opening_checkpoint_manifest_sha256=manifest_sha, opening_kind=('interrupted_run_state' if resume else 'saved_checkpoint') if prefix else 'source_range_start',
                        financial_admission=False, global_books_tax_acceptance=False,
                        native_stake_asset_blocks=asset_blocks, native_stake_asset_fact_rows=asset_rows,
                        native_stake_asset_facts_sha256=asset_hash, stake_asset_tax_classification='unclassified',
                        capture_sha256=sha(root / 'capture.private.json'), native_proof_sha256=sha(directory / 'native-proof.private.json'), native_helper_sha256=sha(binary), **dict(extra))

        def savepoint(height):
            # The helper copies its committed state and writes its own receipt
            # for the heights proved so far. This side then binds the same
            # counts. A later run can open from that copy after a kill.
            send(dict(kind='savepoint', height=height))
            p.stdin.flush()
            directory = receipt.parent / 'savepoints' / str(height)
            deadline = time.monotonic() + 3600
            while not directory.is_dir():
                require(p.poll() is None and time.monotonic() < deadline, 'native helper did not complete the savepoint')
                time.sleep(0.2)
            require(c.journal.heights == height - first + 1, 'savepoint financial/journal height count differs')
            record = part(directory, height, history[height + 1]['header'], False, dict(savepoint=True, saved_utc=time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())))
            with (directory / 'part.private.json').open('x') as stream:
                json.dump(record, stream, indent=2, sort_keys=True)
        send(dict(kind='anchor', anchor=history.anchor, through=through))
        try:
            for record in baseline:
                c.accept(record)
            baseline = None
            for raw in lines:
                before = c.heights
                c.accept(parse(raw))
                if not closed and c.heights == through:
                    break
                if savepoint_every and c.heights != before and c.heights % savepoint_every == 0 and c.heights < through:
                    savepoint(c.heights)
            require(c.heights == through and c.journal.heights == through - first + 1, 'part financial/journal height count differs')
            if closed:
                require(c.closed and history.completion is not None, 'complete source financial/journal/next-header closure missing')
                next_header = history.completion['unexecuted_next_header']
            else:
                require(not c.closed and not c.final_stores, 'bounded part consumed the source closure')
                next_header = history[through + 1]['header']
            send(dict(kind='bounded_end', height=c.heights))
            p.stdin.close()
            require(p.wait(timeout=1800) == 0, 'independent native financial proof refused')
        except BaseException:
            try:
                p.stdin.close()
            except BrokenPipeError:
                pass
            p.wait(timeout=30)
            raise
    result = part(receipt.parent, c.heights, next_header, closed)
    with receipt.open('x') as stream:
        json.dump(result, stream, indent=2, sort_keys=True)
    return result


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--directory', required=True, type=Path)
    parser.add_argument('--native-helper', required=True, type=Path)
    parser.add_argument('--receipt', required=True, type=Path)
    parser.add_argument('--first', required=True, type=int)
    parser.add_argument('--through', required=True, type=int)
    parser.add_argument('--opening-checkpoint', type=Path)
    parser.add_argument('--opening-manifest', type=Path)
    parser.add_argument('--resume-opening', action='store_true')
    parser.add_argument('--savepoint-every', type=int, default=0)
    parser.add_argument('--validator-history', type=Path)
    parser.add_argument('--validator-history-sha256', default='')
    parser.add_argument('--evidence-cache', type=Path)
    parser.add_argument('--evidence-capture-sha256', default='')
    a = parser.parse_args()
    os.umask(0o077)
    r = verify(a.directory, a.native_helper, a.receipt, a.first, a.through, a.opening_checkpoint, a.opening_manifest, a.resume_opening, a.savepoint_every,
               a.validator_history, a.validator_history_sha256, a.evidence_cache, a.evidence_capture_sha256)
    print(json.dumps({k: r[k] for k in ('first', 'through', 'heights', 'transactions', 'fast_forward', 'financial_admission')}))
