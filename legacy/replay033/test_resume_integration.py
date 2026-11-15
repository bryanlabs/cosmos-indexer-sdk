"""Real scratch checker kill/restart equivalence, no financial checker mocks."""
import argparse
import json
import os
from pathlib import Path
import signal
import shutil
import subprocess
import sys
import time
from unittest.mock import patch

import consume_closed
import durable_resume
from resume_fixture import bind_fixture, provenance_adapters


def run_child(args):
    os.environ['GOMAXPROCS'] = '2'
    source, anchor = provenance_adapters(args.fixture)
    with source, anchor:
        original = durable_resume.save_checkpoint
        def barrier(*values, **kwargs):
            result = original(*values, **kwargs)
            if result['height'] != args.interrupt_height:
                return result
            marker = args.output / 'checkpoint-ready.json'
            durable_resume.put(marker, durable_resume.canonical(result))
            durable_resume.sync_dir(args.output)
            while True:
                signal.pause()
        args.output.mkdir(mode=0o700, parents=True, exist_ok=True)
        def forbidden_prefix(*values, **kwargs):
            raise AssertionError('resume attempted original gzip prefix replay')
        with patch.object(durable_resume, 'save_checkpoint', barrier if args.mode == 'interrupted' else original), patch.object(consume_closed, 'closed_lines', forbidden_prefix if args.mode == 'resumed' else consume_closed.closed_lines):
            consume_closed.verify(args.fixture, args.binary, args.output / 'receipt.json',
                                  checkpoint_directory=args.checkpoints,
                                  seekable_source=args.sources,
                                  resume=args.mode == 'resumed',
                                  checkpoint_seconds=0.000001 if args.mode == 'interrupted' else 99999)


def main(args):
    args.output.mkdir(mode=0o700)
    if not (args.fixture / 'capture.private.json').exists():
        bind_fixture(args.fixture)
    binding = json.loads((args.fixture / 'capture.private.json').read_bytes())
    start = time.monotonic()
    durable_resume.prepare_sources(args.fixture, binding, args.sources, consume_closed.closed_lines, chunk_bytes=8192)
    preparation = time.monotonic() - start
    checkpoints = args.output / 'checkpoints'
    def command(mode, output):
        return [sys.executable, '-B', str(Path(__file__).resolve()), '--mode', mode,
                '--fixture', str(args.fixture), '--binary', str(args.binary), '--output', str(output),
                '--interrupt-height', str(args.interrupt_height),
                '--sources', str(args.sources), '--checkpoints', str(checkpoints)]
    baseline = args.output / 'baseline'
    start = time.monotonic()
    with (args.output / 'baseline.log').open('xb') as log:
        subprocess.run(command('baseline', baseline), check=True, stdout=log, stderr=log)
    baseline_seconds = time.monotonic() - start
    interrupted = args.output / 'interrupted'
    with (args.output / 'interrupted.log').open('xb') as log:
        child = subprocess.Popen(command('interrupted', interrupted), stdout=log, stderr=log, start_new_session=True)
        deadline = time.monotonic() + 120
        marker = interrupted / 'checkpoint-ready.json'
        while not marker.exists():
            if child.poll() is not None:
                raise RuntimeError('actual interrupted checker failed before checkpoint')
            if time.monotonic() > deadline:
                os.killpg(child.pid, signal.SIGKILL)
                child.wait()
                raise RuntimeError('scratch checkpoint timed out')
            time.sleep(0.005)
        boundary = json.loads(marker.read_bytes())
        os.killpg(child.pid, signal.SIGKILL)
        child.wait(timeout=10)
    resumed = args.output / 'resumed'
    start = time.monotonic()
    with (args.output / 'resumed.log').open('xb') as log:
        subprocess.run(command('resumed', resumed), check=True, stdout=log, stderr=log)
    restore_and_finish = time.monotonic() - start
    proof_files = ['receipt.json', 'native-proof.private.json', 'native-stake-reductions.private.jsonl']
    hashes = {}
    for name in proof_files:
        left, right = baseline / name, resumed / name
        assert left.exists() == right.exists(), 'resumed output presence differs'
        if left.exists():
            assert left.read_bytes() == right.read_bytes(), 'resumed final receipt/facts differ'
            hashes[name] = durable_resume.sha(left)
    rejected = {}
    for case in ('python-tamper', 'native-tamper', 'identity-mismatch', 'torn-pointer'):
        isolated = args.output / ('negative-' + case)
        shutil.copytree(checkpoints, isolated)
        pointer = json.loads((isolated / 'CURRENT').read_bytes())
        generation = isolated / pointer['generation']
        if case in ('python-tamper', 'native-tamper'):
            target = generation / ('python.json' if case == 'python-tamper' else 'native/phase.raw.jsonl')
            with target.open('ab') as stream:
                stream.write(b' ')
        elif case == 'identity-mismatch':
            target = generation / 'seal.json'
            seal = json.loads(target.read_bytes())
            seal['identity']['config']['through'] += 1
            target.write_bytes(durable_resume.canonical(seal))
            pointer['seal_sha256'] = durable_resume.sha(target)
            (isolated / 'CURRENT').write_bytes(durable_resume.canonical(pointer))
        else:
            pointer['generation'] = 'missing-generation'
            (isolated / 'CURRENT').write_bytes(durable_resume.canonical(pointer))
        out = args.output / ('refused-' + case)
        cmd = command('resumed', out)
        cmd[-1] = str(isolated)
        with (args.output / ('negative-' + case + '.log')).open('xb') as log:
            negative = subprocess.run(cmd, stdout=log, stderr=log)
        assert negative.returncode != 0 and not (out / 'receipt.json').exists(), 'corrupt integrated checkpoint accepted'
        rejected[case] = True
    result = dict(schema='native033-actual-checker-resume-test-v1', passed=True, negative_controls=rejected,
                  synthetic_fixture=True, actual_python_consumer=True, actual_journal=True,
                  actual_native_subprocess=True, scratch_process_group_killed=True,
                  resume_original_gzip_reader_forbidden=True,
                  test_only_provenance_adapters=['source-producer-identity', 'historical-genesis-pin'],
                  checkpoint_height=boundary['height'], checkpoint_write_seconds=boundary['write_seconds'],
                  baseline_seconds=baseline_seconds, restore_and_finish_seconds=restore_and_finish,
                  derivative_prepare_seconds=preparation, final_output_hashes=hashes,
                  financial_admission=False, original_processes_touched=False)
    durable_resume.put(args.output / 'result.safe.json', durable_resume.canonical(result))
    print(json.dumps(result, sort_keys=True))


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--mode', choices=['test', 'baseline', 'interrupted', 'resumed'], default='test')
    parser.add_argument('--fixture', type=Path, required=True)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--sources', type=Path, required=True)
    parser.add_argument('--checkpoints', type=Path)
    parser.add_argument('--interrupt-height', type=int, choices=[1, 2, 3], default=1)
    args = parser.parse_args()
    for key in ('fixture', 'binary', 'output', 'sources'):
        setattr(args, key, getattr(args, key).resolve())
    if args.mode == 'test':
        main(args)
    else:
        run_child(args)
