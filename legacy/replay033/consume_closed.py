#!/usr/bin/env python3
"""Strict three-profile complete-source consumer. No production admission or replay."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import zlib
from consume_bounded import Consumer, HistoryWindow, NAMES, RECORD_FIELDS, exact, native_numbers, sha
from capture_bounded import PROFILES, manifest
from compare_zero_export import strict_json
from verify_movement_rehearsal import require, state_values

SCHEMA = 'native033-closed-source-capture-v1'
IMAGE = 'ghcr.io/bryanlabs/cosmos-tax-replay033@sha256:ba3fe2827be1057855a40edfa0c2f37bda2b80392e5c0442fa537d33d5abd190'

def closed_lines(root, binding, name):
    """Hash every exact byte and require one complete gzip member, no suffix."""
    spec = binding['streams'][name]
    path = root / (name + '.source-closed.gz')
    require(path.is_file() and not path.is_symlink() and sha(path) == spec['sha256'] and path.stat().st_size == spec['compressed_bytes'], 'changed complete original compressed source')
    decoder = zlib.decompressobj(31)
    pending = b''
    digest = __import__('hashlib').sha256()
    count = length = 0
    last = None
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(65536), b''):
            require(not decoder.eof, 'trailing or concatenated complete source gzip')
            pending += decoder.decompress(chunk)
            require(not decoder.unused_data and len(pending) <= 16 << 20, 'trailing compressed source or unbounded line')
            while b'\n' in pending:
                raw, pending = pending.split(b'\n', 1)
                raw += b'\n'
                length += len(raw)
                count += 1
                digest.update(raw)
                last = native_numbers(strict_json(raw))
                yield last
    require(decoder.eof and not decoder.unused_data and not pending and last is not None, 'complete source lacks gzip close or ends mid-record')
    require(count == spec['records'] and length == spec['raw_bytes'] and digest.hexdigest() == spec['raw_sha256'], 'complete original raw source binding differs')
    require(last['kind'] == spec['completion_kind'] and int(last['height']) == binding['through'], 'complete source lacks exact actual completion marker')

class ClosedHistory(HistoryWindow):
    def __init__(self, root, binding):
        self.rows = iter(closed_lines(root, binding, 'native-history'))
        self.anchor = self.validate(next(self.rows), anchor=True)
        require(self.anchor['height'] == binding['first'] - 1, 'closed opening native height differs')
        self.cache, self.last = {}, self.anchor['height']
        self.target, self.completion = binding['through'], None

    def __getitem__(self, height):
        if height <= self.target:
            return super().__getitem__(height)
        require(height == self.target + 1 and self.last == self.target, 'unproved closed history next-header request')
        if self.completion is None:
            r = next(self.rows)
            require(set(r) == set('schema kind first_height height commit unexecuted_next_header unexecuted_next_block_hash'.split()), 'unknown closed history completion field')
            require(r['schema'] == 'native033-lossless-history-v1' and r['kind'] == 'history_completed' and r['first_height'] == self.anchor['height'] + 1 and r['height'] == self.target, 'closed native history completion identity differs')
            require(exact(r['commit']) == exact(self.cache[self.target]['commit']) and int(r['unexecuted_next_header']['height']) == height and r['unexecuted_next_header']['chain_id'] == 'cosmoshub-1', 'actual terminal native commit or unexecuted next header differs')
            self.completion = r
            require(next(self.rows, None) is None, 'trailing complete native history')
        # This is the original unexecuted next header, NOT an invented height
        # execution, result, or financial state. Native checks the raw cache.
        return {'header': self.completion['unexecuted_next_header']}

class ClosedConsumer(Consumer):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.final_stores, self.closed = set(), False

    def accept(self, r):
        require(set(r) <= RECORD_FIELDS, 'unknown complete source record field')
        require(not self.closed, 'trailing complete financial source')
        if r['kind'] == 'native_state_checkpoint' and r['label'].startswith('segment_final/'):
            self.finish_store()
            self.finish_scope()
            require(self.heights == self.through and int(r['height']) == self.through and r['phase'] == 'segment' and r['status'] == 'committed' and int(r['tx']) == int(r['message']) == -1, 'premature or mislabelled final native checkpoint')
            store = r['label'].split('/')[1].split(':')[1]
            require(store in NAMES and store not in self.final_stores and state_values(r['before']) == self.latest[store], 'complete final raw native store differs')
            self.final_stores.add(store)
            self.journal.accept(r)
            return
        if r['kind'] == 'observation_completed':
            require(self.heights == self.through and self.final_stores == NAMES and not self.rows and int(r['height']) == self.through and r['phase'] == 'segment' and r['status'] == 'committed' and int(r['tx']) == int(r['message']) == -1 and int(r['code']) == 0, 'full financial source completion missing or premature')
            self.closed = True
            return
        require(not self.final_stores, 'historical frame after final source anchor')
        super().accept(r)

def validate_source(root):
    b = json.loads((root / 'capture.private.json').read_bytes())
    require(b['schema'] == SCHEMA and b['first'] in PROFILES, 'explicit closed source capture required')
    target, name, uid = PROFILES[b['first']]
    from source_reader import completed_manifest, validate_reader
    completed = completed_manifest((root/'completed.private.sha256').read_bytes(), b['first'], target)
    for stream in ('movements','native-history'):
        require(b['streams'][stream]['sha256'] == completed[f'/recordings/range-{b["first"]}-{target}/{stream}.ndjson.gz'], 'closed source does not match original producer completion seal')
    require(sha(root/'source-status-before.private.json') == sha(root/'source-status-after.private.json') == completed[f'/recordings/range-{b["first"]}-{target}/home/status.json'] and (root/'recording-complete.private').read_bytes().strip(), 'actual completed producer status or gzip validation seal differs')
    readers = [validate_reader(json.loads((root/f).read_bytes())) for f in ('reader-pod.private.json','reader-pod-after.private.json')]
    require(readers[0]['status']['containerStatuses'][0]['containerID'] == readers[1]['status']['containerStatuses'][0]['containerID'], 'retained reader changed during complete capture')
    binaries = (root/'binaries.private.sha256').read_text().splitlines()
    require([line.split()[0] for line in binaries] == ['7773ca271496eba347385d0bff11b0fcff2376de4cc58c61f568e130e883e485','10cd8b474d95b27a3ac7baf53b5aebed1efdebe3118ec2948e709e3789201f9a'], 'original producer recorded runtime binary pins differ')
    producer_binding = json.loads((root/'source-and-storage-binding.private.json').read_bytes())
    require(producer_binding['source_commit'] == 'e999d6057f3f791ba9ed540616ed0ff7385ea371' and producer_binding['image'] == IMAGE and producer_binding['replay_binary_sha256'] == binaries[0].split()[0] and producer_binding['checkpoint_proof_binary_sha256'] == binaries[1].split()[0], 'actual completed original source/build binding differs')
    require(b['through'] == b['source_target'] == target and b['source_whole_complete'] is True and b['source_shell_failure_marker_absent'] is True and b['financial_admission'] is False, 'full source coverage or admission differs')
    for stream, kind in (('movements', 'observation_completed'), ('native-history', 'history_completed')):
        require(b['streams'][stream]['source_path'] == f'/recordings/range-{b["first"]}-{target}/{stream}.ndjson.gz' and b['streams'][stream]['completion_kind'] == kind, 'actual source completion kind differs')
    for filename in ('source-status-before.private.json', 'source-status-after.private.json'):
        s = json.loads((root / filename).read_bytes())
        require(s['chain'] == 'cosmoshub-1' and s['first_height_this_run'] == b['first'] and s['last_height_committed'] == s['state_hash_verified_through'] == s['target'] == target and s['heights_where_results_differ_from_cache'] == 0 and s['stop_reason'] == 'reached target; verified by unexecuted next header', 'actual producer did not close verified full source')
    pods = [json.loads((root / f).read_bytes()) for f in ('source-pod.private.json', 'source-pod-after.private.json')]
    for p in pods:
        require(p['metadata']['name'] == name and p['metadata']['uid'] == uid and len(p['spec']['containers']) == len(p['status']['containerStatuses']) == 1 and p['spec']['containers'][0]['image'] == IMAGE, 'closed original producer identity differs')
        c = p['status']['containerStatuses'][0]
        require(c['restartCount'] == 0 and (c['state'].get('running') or c['state'].get('terminated', {}).get('exitCode') == 0), 'original producer restarted or failed')
    a, z = [p['status']['containerStatuses'][0] for p in pods]
    require(a['containerID'] == z['containerID'] and a['imageID'] == z['imageID'], 'original closed producer container changed')
    expected_metadata = {'source-status-before.private.json','source-status-after.private.json','source-pod.private.json','source-pod-after.private.json','reader-pod.private.json','reader-pod-after.private.json','completed.private.sha256','binaries.private.sha256','source-and-storage-binding.private.json','recording-complete.private'}
    if b['first'] > 1:
        expected_metadata |= {'source-before.private.sha256','initial-clone.private.sha256','source-after.private.sha256','source-native-proof.private.json'}
    require(set(b['source_metadata_sha256']) == expected_metadata, 'complete producer and reader metadata coverage differs')
    for field in ('cache_members_sha256', 'source_metadata_sha256'):
        for name, pin in b[field].items():
            rel = Path(name)
            require(not rel.is_absolute() and '..' not in rel.parts, 'unsafe complete source member path')
            path = root / ('cache' if field == 'cache_members_sha256' else '') / rel
            require(path.is_file() and not path.is_symlink() and sha(path) == pin, 'changed exact original closed source member')
    expected = {'genesis/genesis.cosmoshub-1.json'}
    for height in range(max(1, b['first'] - 1), target + 2):
        for suffix in ('block', 'block_results'):
            expected.add(f'cosmoshub-1/{height // 10000}/{height}-{suffix}.raw.gz')
    require(set(b['cache_members_sha256']) == expected, 'complete closed raw cache member coverage differs')
    if b['first'] > 1:
        pins = b['opening_checkpoint_members_sha256']
        require(pins and pins == manifest((root / 'source-before.private.sha256').read_bytes()) == manifest((root / 'initial-clone.private.sha256').read_bytes()), 'closed normal opening differs from actual producer clone')
        for name, pin in pins.items():
            require(not Path(name).is_absolute() and '..' not in Path(name).parts, 'unsafe opening member path')
            require(sha(root / 'opening-checkpoint' / name) == pin, 'closed opening original raw bytes changed')
    else:
        require(not b.get('opening_checkpoint_members_sha256'), 'genesis cannot use synthetic opening')
    return b

def verify(root, binary, receipt):
    b = validate_source(root)
    history = ClosedHistory(root, b)
    command = [str(binary), '--cache', str(root / 'cache'), '--receipt', str(receipt.parent / 'native-proof.private.json')]
    if b['first'] > 1:
        command += ['--checkpoint-home', str(root / 'opening-checkpoint'), '--working-home', str(receipt.parent / 'native-home')]
    with (receipt.parent / 'native-consumer.private.log').open('xb') as log:
        p = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=log, stderr=subprocess.STDOUT)
        def send(q):
            p.stdin.write((json.dumps(q, separators=(',', ':')) + '\n').encode())
        c = ClosedConsumer(send, history, b['through'], b['first'])
        send(dict(kind='anchor', anchor=history.anchor, through=b['through']))
        try:
            for r in closed_lines(root, b, 'movements'):
                c.accept(r)
            require(c.closed and c.heights == b['through'] and c.journal.heights == b['through'] - b['first'] + 1 and history.completion is not None, 'complete source financial/journal/next-header closure missing')
            send(dict(kind='bounded_end', height=c.heights))
            p.stdin.close()
            require(p.wait(timeout=1800) == 0, 'independent complete native financial proof refused')
        except BaseException:
            try:
                p.stdin.close()
            except BrokenPipeError:
                pass
            p.wait(timeout=30)
            raise
    ending = history.cache[b['through']]
    result = dict(schema='native033-complete-profile-financial-v1', first=b['first'], through=c.heights,
                  heights=c.journal.heights, transactions=c.transactions, phases=dict(c.counts), journal_counts=dict(c.journal.counts),
                  opening_commit=history.anchor['commit'], opening_store_roots=history.anchor['native_commit_info'],
                  ending_commit=ending['commit'], ending_store_roots=ending['native_commit_info'],
                  ending_block_hash=ending['block_hash'], actual_unexecuted_next_header=history.completion['unexecuted_next_header'],
                  source_whole_complete=True, financial_admission=False, global_books_tax_acceptance=False,
                  capture_sha256=sha(root / 'capture.private.json'), native_proof_sha256=sha(receipt.parent / 'native-proof.private.json'), native_helper_sha256=sha(binary))
    with receipt.open('x') as stream:
        json.dump(result, stream, indent=2, sort_keys=True)
    return result

if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--directory', required=True, type=Path)
    parser.add_argument('--native-helper', required=True, type=Path)
    parser.add_argument('--receipt', required=True, type=Path)
    a = parser.parse_args()
    os.umask(0o077)
    r = verify(a.directory, a.native_helper, a.receipt)
    print(json.dumps({k: r[k] for k in ('first', 'through', 'heights', 'transactions', 'financial_admission')}))
