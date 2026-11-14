#!/usr/bin/env python3
"""Private streaming native033 journal/phase consumer. Bounded, never whole admission."""
import argparse
import base64
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
import subprocess
import zlib
from compare_zero_export import strict_json, typed, Number
from terminal_journal import Journal
from verify_movement_rehearsal import blob, commitment, state_values, require, CLASSES

RECORD_FIELDS = set('native_view native_parent write_operation classification evidence_quality before after unit validator delegator kind code height tx message phase label status address denom amount'.split())

NAMES = {'main', 'acc', 'staking', 'slashing', 'mint', 'distr', 'gov', 'fee', 'params'}

def sha(path):
    h = hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b''):
            h.update(chunk)
    return h.hexdigest()

def native_numbers(value):
    if type(value) is Number:
        require(str(int(value)) == value, 'noncanonical native numeric token')
        return int(value)
    if type(value) is dict:
        return {k: native_numbers(v) for k, v in value.items()}
    if type(value) is list:
        return [native_numbers(v) for v in value]
    return value

def exact(value):
    return typed(strict_json(json.dumps(value, separators=(',', ':'))))

def selected_lines(root, binding, name):
    spec = binding['streams'][name]
    path = root / (name + '.source-fragment.gz')
    require(sha(path) == spec['fragment_sha256'] and path.stat().st_size == spec['compressed_fragment_bytes'], 'changed original compressed source')
    decoder, pending, digest = zlib.decompressobj(31), b'', hashlib.sha256()
    count = length = 0
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(65536), b''):
            pending += decoder.decompress(chunk)
            require(len(pending) <= 16 << 20, 'unbounded or incomplete native line')
            while b'\n' in pending:
                raw, pending = pending.split(b'\n', 1)
                raw += b'\n'
                length += len(raw)
                count += 1
                digest.update(raw)
                row = native_numbers(strict_json(raw))
                if row.get('kind') == spec['boundary_kind'] and int(row['height']) == spec['boundary_height']:
                    require(length == spec['selected_raw_bytes'] and count == spec['selected_records'] and digest.hexdigest() == spec['selected_raw_sha256'], 'complete selected boundary bytes differ')
                    yield row
                    return
                require(row.get('kind') not in ('observation_completed', 'history_completed', 'observation_closed_terminal_completed'), 'fake whole-source completion inside running prefix')
                yield row
    raise ValueError('truncated gzip or absent complete-height boundary')

def patches(before, after, touched=None):
    result = {}
    for name in sorted(after):
        old = before.get(name, {})
        keys = set(old) | set(after[name]) if touched is None else touched.get(name, set())
        result[name] = [{'key': base64.b64encode(key).decode(), 'before': old.get(key), 'value': after[name].get(key)}
                        for key in sorted(keys) if touched is not None or old.get(key) != after[name].get(key)]
    return result

class HistoryWindow:
    """At most the current and next native height, never full history in RAM."""
    def __init__(self, root, binding):
        self.rows = iter(selected_lines(root, binding, 'native-history'))
        self.anchor = self.validate(next(self.rows), anchor=True)
        self.cache, self.last = {}, 0

    @staticmethod
    def validate(row, anchor=False):
        require(row.get('schema') == 'native033-lossless-history-v1' and row.get('chain') == 'cosmoshub-1' and row.get('release') == 'cosmos-sdk v0.33.2 (gaia), tendermint v0.31.0-dev0-fix0, iavl v0.12.1', 'native history schema/release/source chain differs')
        fields = set('schema kind release chain height commit native_commit_info'.split())
        fields |= {'genesis_sha256'} if anchor else set('header block_hash transactions begin deliver end'.split())
        require(set(row) == fields and row['kind'] == ('source_anchor' if anchor else 'height'), 'unknown or missing native history field')
        if anchor:
            require(row['height'] == 0 and row['genesis_sha256'] == '73a866b21723ecbc28b6d15951b2eb3aa2f2443650ff6df489bf55ac5edceefa', 'genuine genesis opening required')
        return row

    def __getitem__(self, height):
        while self.last < height:
            row = self.validate(next(self.rows))
            require(row['height'] == self.last + 1, 'gap/duplicate native history')
            self.last += 1
            self.cache[self.last] = row
            self.cache = {h: r for h, r in self.cache.items() if h >= self.last - 1}
        require(height in self.cache, 'native history cursor cannot use stale height')
        return self.cache[height]

class CommitJournal(Journal):
    def accept(self, record):
        if record['kind'] == 'native_cache_write' and record.get('native_parent', 0):
            store = record['label']
            view, parent = int(record['native_view']), int(record['native_parent'])
            child = self.frame(store, view, parent)
            self.frame(store, parent)['dirty'].update(child['dirty'])
        super().accept(record)

class Consumer:
    def __init__(self, send, history, through):
        self.send, self.history, self.through = send, history, through
        self.journal = CommitJournal()
        self.versions, self.latest, self.helper = {}, {}, {}
        self.pending, self.scope, self.before, self.after, self.rows = None, None, {}, {}, {}
        self.heights, self.counts, self.seen, self.committed_touched = 0, Counter(), set(), {}
        self.phase_order = []
        self.txs_seen = set()
        self.transactions = 0
        self.block_start, self.tx_start, self.ante_tip, self.message_tip = {}, {}, {}, {}
        self.message_index, self.retained_tip = {}, {}
        self.dispositions, self.flushes = {}, {}

    def finish_store(self):
        if self.pending is None:
            return
        record, changes = self.pending
        store = record['label'].split(':')[1]
        require(store in self.latest, 'native scope before complete baseline')
        scope = tuple(record.get(k, '') for k in ('height', 'tx', 'message', 'phase', 'status', 'validator', 'delegator'))
        if scope != self.scope:
            self.finish_scope()
            self.scope = scope
        require(store not in self.before, 'duplicate native store in scope')
        key = store, record['before']['sha256']
        require(key in self.versions, 'missing native before-image anchor')
        before = self.versions[key]
        require(commitment(before) == record['before'], 'complete native before commitment differs')
        after, seen = before.copy(), set()
        for change in changes:
            key = blob(change['label'].split('/key:', 1)[1])
            require(key not in seen and before.get(key) == change['before'], 'duplicate key or changed native before-image')
            seen.add(key)
            require(all(change.get(k) == record.get(k) for k in ('height', 'tx', 'message', 'phase', 'status', 'validator', 'delegator')), 'native effect scope differs')
            if change['after'] is None:
                require(key in after, 'delete missing native before-image')
                del after[key]
            else:
                blob(change['after'])
                after[key] = change['after']
        require(commitment(after) == record['after'], 'complete native after commitment differs')
        self.before[store], self.after[store] = before, after
        self.versions[store, record['after']['sha256']] = after
        self.pending = None

    def finish_scope(self):
        if self.scope is None:
            return
        require(set(self.before) == set(self.after) == NAMES, 'phase lacks all nine complete stores')
        h, tx, msg, phase, status, val, delegator = self.scope
        require(phase in ('begin_block', 'ante', 'message', 'transaction', 'end_block', 'block'), 'unknown native phase')
        require(self.scope not in self.seen, 'overlapping duplicate phase')
        self.seen.add(self.scope)
        self.dispositions[int(h), int(tx), int(msg), phase] = status
        self.phase_order.append(phase)
        identity = int(h), int(tx)
        if phase == 'begin_block':
            require(self.before == self.latest and self.phase_order == ['begin_block'], 'begin block before-image is not actual committed predecessor')
            self.block_start = self.before.copy()
        elif phase == 'ante':
            require(self.before == self.latest and identity not in self.tx_start and int(msg) == -1, 'ante before-image/duplicate transaction differs')
            self.tx_start[identity] = self.before.copy()
            self.ante_tip[identity] = self.after.copy() if status == 'committed' else self.before.copy()
            self.retained_tip[identity] = self.ante_tip[identity]
        elif phase == 'message':
            require(identity in self.ante_tip and int(msg) == self.message_index.get(identity, -1) + 1, 'message lacks ordered native ante predecessor')
            require(self.before == self.message_tip.get(identity, self.ante_tip[identity]), 'message before-image differs from actual prior message cache')
            self.message_tip[identity] = self.after.copy()
            self.message_index[identity] = int(msg)
            if status == 'committed':
                self.retained_tip[identity] = self.after.copy()
        elif phase == 'transaction':
            require(identity in self.tx_start and self.before == self.tx_start[identity] and self.after == self.retained_tip[identity], 'native transaction ante/cache commit/rollback chain differs')
        elif phase == 'end_block':
            require(self.before == self.latest, 'end block before-image differs from committed transaction chain')
        elif phase == 'block':
            require(self.before == self.block_start, 'aggregate block missing original begin predecessor')
        self.send(dict(kind='phase', height=int(h), tx=int(tx), message=int(msg), phase=phase, status=status,
            restore=patches(self.helper, self.before), changes=patches(self.before, self.after), rows=self.rows.pop(self.scope, []),
            header=self.history[int(h)]['header'], transactions=self.history[int(h)]['transactions']))
        self.helper = self.after.copy()
        if phase in ('begin_block', 'transaction', 'end_block'):
            self.latest = self.after.copy()
        if phase == 'block':
            require(self.after == self.latest, 'aggregate block does not equal phase chain')
        self.counts[phase] += 1
        self.before, self.after = {}, {}
        self.scope = None

    def accept(self, r):
        require(set(r) <= RECORD_FIELDS, 'unknown native record field')
        kind, h = r['kind'], int(r['height'])
        require(1 <= h <= self.through, 'native record outside selected full range')
        require(r['status'] in ('committed', 'aborted'), 'invalid phase disposition')
        if kind != 'native_state':
            self.finish_store()
        if kind == 'native_state_checkpoint':
            require(r['label'].startswith('segment_baseline/'), 'unexpected final marker in in-progress prefix')
            store = r['label'].split('/')[1].split(':')[1]
            require(store in NAMES and store not in self.latest, 'unknown or duplicate native baseline store')
            state = state_values(r['before'])
            self.latest[store] = state
            self.versions[store, commitment(state)['sha256']] = state
        elif kind == 'native_state_hash':
            self.pending = r, []
        elif kind == 'native_state':
            require(self.pending is not None and r['label'].split('/key:', 1)[0] == self.pending[0]['label'], 'native key lacks enclosing scope')
            self.pending[1].append(r)
        elif kind in ('movement', 'principal', 'principal_request', 'staking_shares', 'share_request', 'accounting', 'native_execution'):
            row_scope = tuple(r.get(k, '') for k in ('height', 'tx', 'message', 'phase', 'status', 'validator', 'delegator'))
            self.rows.setdefault(row_scope, []).append(r)
            if kind == 'movement':
                require(r['label'] in CLASSES and r.get('classification') == CLASSES[r['label']] and r.get('unit') == 'native_bank_tokens', 'unknown or changed native financial classification')
                require(type(r['amount']) is str and str(int(r['amount'])) == r['amount'], 'noncanonical native movement amount')
                if r['label'] != 'ante_fee':
                    require(type(r['before']) is str and type(r['after']) is str and str(int(r['before'])) == r['before'] and str(int(r['after'])) == r['after'] and min(int(r['before']), int(r['after'])) >= 0 and int(r['after']) - int(r['before']) == int(r['amount']), 'native movement before/after amount differs')
            if kind == 'native_execution' and r['phase'] == 'transaction':
                identity = h, int(r['tx'])
                history = self.history[h]
                require(identity not in self.txs_seen and 0 <= identity[1] < len(history['transactions']), 'duplicate native transaction')
                require(r['before'] == history['transactions'][identity[1]] and exact(r['after']) == exact(history['deliver'][identity[1]]), 'original signed bytes or native ABCI result differs')
                self.txs_seen.add(identity)
        elif kind == 'height_completed':
            self.finish_scope()
            require(h == self.heights + 1 and self.latest == {label.split(':')[1]: {blob(k): v for k, v in state.items()} for label, state in self.journal.canonical.items()}, 'journal and independent phase committed state differ')
            require(self.phase_order[0] == 'begin_block' and self.phase_order[-2:] == ['end_block', 'block'] and not self.rows, 'height phase order or financial row scope incomplete')
            history = self.history[h]
            require({(h, i) for i in range(len(history['transactions']))} == {i for i in self.txs_seen if i[0] == h}, 'missing native transaction execution')
            self.send(dict(kind='commit', height=h, changes=patches({}, self.latest) if h == 1 else patches(self.committed, self.latest, self.committed_touched), history=history, next_header=self.history[h + 1]['header']))
            for (dh, tx, msg, phase), status in self.dispositions.items():
                if dh == h and phase in ('ante', 'message'):
                    flushed = self.flushes.get((h, tx, 'ante' if phase == 'ante' else 'message'), set())
                    require(flushed in (set(), NAMES), 'partial native nine-store cache flush')
                    require(status == ('committed' if flushed else 'aborted'), 'native phase disposition not bound to actual nine-store cache flush')
            self.transactions += len(self.history[h]['transactions'])
            self.txs_seen.clear()
            self.tx_start.clear()
            self.ante_tip.clear()
            self.message_tip.clear()
            self.message_index.clear()
            self.retained_tip.clear()
            self.dispositions.clear()
            self.flushes.clear()
            self.committed = self.latest.copy()
            self.committed_touched = {}
            self.heights = h
            self.versions = {(s, commitment(v)['sha256']): v for s, v in self.latest.items()}
            self.phase_order, self.seen = [], set()
        elif kind not in ('native_write', 'native_cache_write'):
            raise ValueError('unknown financial record kind')
        # Independent Set/Delete/cache-flush reconstruction sees EVERY frame.
        # Commit writes retain touched keys, including repeated/net-zero writes.
        if kind == 'native_cache_write' and r['phase'] == 'transaction':
            require(int(r['tx']) >= 0 and r['status'] == 'committed', 'actual transaction cache flush mislabelled')
            key = h, int(r['tx']), 'ante' if int(r['message']) < 0 else 'message'
            self.flushes.setdefault(key, set()).add(r['label'].split(':')[1])
        if kind == 'native_cache_write' and r['phase'] == 'commit':
            store = r['label'].split(':')[1]
            view, parent = int(r['native_view']), int(r.get('native_parent', 0))
            frame = self.journal.frame(r['label'], view, parent)
            self.committed_touched.setdefault(store, set()).update(blob(k) for k in frame['dirty'])
        self.journal.accept(r)


def verify(root, binary, receipt):
    binding = json.loads((root / 'capture.private.json').read_bytes())
    require(binding['schema'] == 'native033-bounded-in-progress-capture-v1' and binding['first'] == 1 and binding['source_target'] == 200000 and binding['source_whole_complete'] is False, 'capture must preserve actual running source target')
    for name, kind, height in [('movements', 'height_completed', binding['through']), ('native-history', 'height', binding['through'] + 1)]:
        spec = binding['streams'][name]
        require(spec['boundary_kind'] == kind and spec['boundary_height'] == height and spec['source_gzip_whole_closed'] is False, 'bounded stream selection does not bind exact complete common height and next header')
    pod = json.loads((root / 'source-pod.private.json').read_bytes())
    require(pod['metadata']['name'] == 'cosmos-tax-hub1-financial-e999d60-v1-0-4r29d' and pod['metadata']['uid'] == 'ae26e6f8-4ee7-4ece-96a0-ff6d523829d5', 'actual bounded producer identity differs')
    require(pod['spec']['containers'][0]['image'] == 'ghcr.io/bryanlabs/cosmos-tax-replay033@sha256:ba3fe2827be1057855a40edfa0c2f37bda2b80392e5c0442fa537d33d5abd190', 'immutable producer image differs')
    after_pod_path = root / ('source-pod-after.private.json' if (root / 'source-pod-after.private.json').exists() else 'source-pod-continuity.private.json')
    after_pod = json.loads(after_pod_path.read_bytes())
    require(after_pod['metadata']['uid'] == pod['metadata']['uid'] and after_pod['status']['phase'] == 'Running', 'source runtime identity changed after bounded capture')
    require(len(pod['status']['containerStatuses']) == len(after_pod['status']['containerStatuses']) == 1, 'source container coverage differs')
    before_container, after_container = pod['status']['containerStatuses'][0], after_pod['status']['containerStatuses'][0]
    require(before_container['containerID'] == after_container['containerID'] and before_container['imageID'] == after_container['imageID'] and before_container['restartCount'] == after_container['restartCount'] == 0 and before_container['state']['running']['startedAt'] == after_container['state']['running']['startedAt'] and after_container['ready'], 'source container restarted or changed')
    for name in ('source-status-before.private.json', 'source-status-after.private.json'):
        status = json.loads((root / name).read_bytes())
        require(status['chain'] == 'cosmoshub-1' and status['target'] == 200000 and status['first_height_this_run'] == 1 and binding['through'] < status['state_hash_verified_through'] < status['last_height_committed'] < 200000 and status['heights_where_results_differ_from_cache'] == 0 and not status.get('stop_reason'), 'actual whole producer is not the claimed in-progress source')
    for name, expected in binding['cache_members_sha256'].items():
        require(sha(root / 'cache' / name) == expected, 'changed current/next raw source cache')
    for name, expected in binding['source_metadata_sha256'].items():
        require(sha(root / name) == expected, 'changed actual source binding')
    history = HistoryWindow(root, binding)
    anchor = history.anchor
    with (receipt.parent / 'native-consumer.private.log').open('xb') as log:
        process = subprocess.Popen([str(binary), '--cache', str(root / 'cache'), '--receipt', str(receipt.parent / 'native-proof.private.json')], stdin=subprocess.PIPE, stdout=log, stderr=subprocess.STDOUT)
        def send(value):
            process.stdin.write((json.dumps(value, separators=(',', ':')) + '\n').encode())
        consumer = Consumer(send, history, binding['through'])
        send(dict(kind='anchor', anchor=anchor, through=binding['through']))
        try:
            for r in selected_lines(root, binding, 'movements'):
                consumer.accept(r)
            require(consumer.heights == binding['through'] and consumer.journal.heights == binding['through'], 'bounded complete height count differs')
            send(dict(kind='bounded_end', height=consumer.heights))
            process.stdin.close()
            require(process.wait(timeout=1800) == 0, 'independent typed/native financial proof refused')
        except BaseException:
            try:
                process.stdin.close()
            except BrokenPipeError:
                pass
            process.wait(timeout=30)
            raise
    result = dict(schema='native033-bounded-component-v1', first=1, through=consumer.heights,
        phases=dict(consumer.counts), transactions=consumer.transactions, journal_counts=dict(consumer.journal.counts),
        source_target=200000, source_whole_complete=False, financial_admission=False, global_books_tax_acceptance=False,
        original_capture_sha256=sha(root / 'capture.private.json'), source_runtime_after_sha256=sha(after_pod_path), native_proof_sha256=sha(receipt.parent / 'native-proof.private.json'),
        independent_helper_sha256=sha(binary))
    with receipt.open('x') as stream:
        json.dump(result, stream, indent=2, sort_keys=True)
    return result

if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--directory', required=True, type=Path)
    parser.add_argument('--native-helper', required=True, type=Path)
    parser.add_argument('--receipt', required=True, type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    result = verify(args.directory, args.native_helper, args.receipt)
    print(json.dumps({k: result[k] for k in ('first', 'through', 'transactions', 'source_whole_complete', 'financial_admission')}))
