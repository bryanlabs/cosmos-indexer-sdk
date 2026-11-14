#!/usr/bin/env python3
"""Capture a truthful sealed component from the EXISTING long recorder, never a DB.

One bounded attempt, no retry/poll/sleep or Kubernetes writes. All artifacts are
exclusive/private. Failed captures remain evidence. stdout is counts/booleans only.
"""
import argparse
import base64
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import shlex
import subprocess
import tarfile
from datetime import datetime, timezone

FIRST, LAST = 400001, 482090
JOB = 'cosmos-tax-legacy-record-hub2-3e087ab-long1'
UID = 'abef1dde-ab1f-48d7-8b30-518dbc139b5a'
SOURCE = '3e087ab429b75f3b6204ce475908025f6c1bdcfe'
IMAGE = 'sha256:656638fa97f3792c9ba3cddb4b7f11703da5025d51b22163cc0036dd280781bd'
PROFILE = '11b863d30db5b1f799e5d3bdd1b49325e5a2b1637252e5c33df0e44aedf5c236'
MARKERS = '2ff60b284b334e2b29ff2523a9ab7d961a6ed2ec20f29d2976a294bcc6fb36a6'
RELEASE = 'cosmos-sdk v0.34.10 (gaia), tendermint v0.31.11, iavl v0.12.1'
BASE = '/replay/record-hub2-3e087ab-long1'
COMPONENT = 'per-complete-height-financial-reconciliation-not-chain-books-acceptance'
NAMES = {'movements': 'movements.prefix.ndjson', 'validators': 'validators.prefix.ndjson',
         'differences': 'differences.prefix.txt', 'log': 'run.prefix.log', 'status': 'status.original.json'}
REMOTE = {'movements': BASE+'/movements.ndjson', 'validators': BASE+'/home/validator-updates.jsonl',
          'differences': BASE+'/home/result-differences.txt', 'log': BASE+'/run.log', 'status': BASE+'/home/status.json'}

class Refused(Exception):
    pass

def require(ok):
    if not ok:
        raise Refused('sealed component refused; private evidence retained')

def sha(raw):
    return hashlib.sha256(raw).hexdigest()

def unpack(raw):
    with gzip.GzipFile(fileobj=io.BytesIO(raw)) as f:
        body = f.read((64 << 20)+1)
    require(len(body) <= 64 << 20)
    return body


def file_sha(path):
    h = hashlib.sha256()
    with path.open('rb') as f:
        for b in iter(lambda: f.read(1 << 20), b''):
            h.update(b)
    return h.hexdigest()

def create(path):
    return os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'wb')

def save(path, raw):
    with create(path) as f:
        f.write(raw)
        f.flush()
        os.fsync(f.fileno())

def save_json(path, value):
    save(path, (json.dumps(value, indent=2, sort_keys=True)+'\n').encode())

def pairs(items):
    out, seen = {}, set()
    for k, v in items:
        require(k.casefold() not in seen)
        seen.add(k.casefold())
        out[k] = v
    return out

def decode(raw):
    # Telemetry fractions are retained as strings for readiness only; no amount
    # is parsed through floating point or rewritten in any source artifact.
    return json.loads(raw, object_pairs_hook=pairs, parse_float=str,
                      parse_constant=lambda _: (_ for _ in ()).throw(Refused()))

def ready_status(s):
    return (s.get('release') == RELEASE and s.get('chain') == 'cosmoshub-2'
            and s.get('first_height_this_run') == 1 and s.get('target') == 2902000
            and type(s.get('last_height_committed')) is int
            and type(s.get('state_hash_verified_through')) is int
            and s['last_height_committed'] >= LAST+1
            and s['state_hash_verified_through'] == s['last_height_committed']-1
            and not s.get('stop_reason'))

def runtime(job, pods):
    require(job['metadata']['name'] == JOB and job['metadata']['uid'] == UID)
    require(job['spec']['backoffLimit'] == 0 and not job['spec'].get('suspend'))
    require(job['status'].get('active') == 1 and not job['status'].get('failed')
            and not job['status'].get('succeeded'))
    require(len(pods['items']) == 1)
    p = pods['items'][0]
    require(p['metadata']['ownerReferences'][0]['uid'] == UID)
    require(p['status']['phase'] == 'Running')
    require(not p['spec'].get('automountServiceAccountToken', True))
    c = p['status']['containerStatuses']
    require(len(c) == 1 and c[0]['name'] == 'replay' and c[0]['ready']
            and c[0]['restartCount'] == 0 and c[0]['imageID'].endswith(IMAGE))
    spec = p['spec']['containers'][0]
    command = '\n'.join(spec['command'])
    for argument in ['--to 2902000', '--verify-target-next-header', '--sparse-account-checks',
                     '--compare-results=true', '--coin-records "$out/movements.ndjson"']:
        require(argument in command)
    require(any(v['mountPath'] == '/cache' and v.get('readOnly') for v in spec['volumeMounts']))
    return p['metadata']['name'], p['metadata']['uid']

class Remote:
    def __init__(self, pod):
        self.pod = pod

    def command(self, args, output=None, timeout=240):
        cmd = ['kubectl', '--context', 'bryanlabs', '-n', 'apps', 'exec', self.pod,
               '-c', 'replay', '--', *args]
        p = subprocess.run(cmd, stdout=output or subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
        require(p.returncode == 0)
        return p.stdout

    def read(self, path, size):
        return self.command(['dd', 'if='+path, 'bs=65536', 'count='+str(size),
                             'iflag=count_bytes', 'status=none'])

    def stat(self, path):
        size, inode = map(int, self.command(['stat', '-c', '%s %i', path]).split())
        return size, inode

    def digest(self, path, size):
        script = 'set -eu; dd if='+shlex.quote(path)+' bs=65536 count='+str(size)+' iflag=count_bytes status=none | sha256sum'
        return self.command(['sh', '-c', script]).split()[0].decode()

    def movement_offset(self, path, last):
        # awk only counts raw bytes under the C locale. It never emits or
        # reserializes record lines. The local parser independently verifies
        # the exact copied prefix, including its final complete-height marker.
        script = ('set -eu; LC_ALL=C awk -v h='+str(last)+' '
            + shlex.quote(r'''{ n += length($0)+1; if (index($0,"\"kind\":\"height_completed\"") && index($0,"\"height\":" h ",")) { print n; exit } }''')
            + ' '+shlex.quote(path))
        raw = self.command(['sh', '-c', script], timeout=240).strip()
        require(raw.isdigit())
        return int(raw)

    def capture(self, path, size, dest):
        with create(dest) as f:
            self.command(['dd', 'if='+path, 'bs=65536', 'count='+str(size),
                          'iflag=count_bytes', 'status=none'], output=f,
                         timeout=max(240, 60+size//(1 << 20)))
            f.flush()
            os.fsync(f.fileno())
        require(dest.stat().st_size == size)


def select_prefix(source, kind, output, bounded=None, first=FIRST, last=LAST):
    """Byte preserving. Never discard captured partial tails: source stays intact.

    Movements seal only at a complete marker. Sparse streams seal after their
    final ordered record <=last. Their captured tail remains a separate artifact.
    """
    offset, prev, height, found = 0, -1, 1, False
    with source.open('rb') as f, create(output) as out:
        selected = create(bounded) if bounded else None
        try:
            while True:
                line = f.readline((1 << 20)+1)
                if not line:
                    break
                require(len(line) <= 1 << 20)
                # A partial capture is allowed only beyond the required sealed
                # bound; the raw captured file is retained, never called sealed.
                if not line.endswith(b'\n'):
                    require(found)
                    break
                if kind == 'differences':
                    parts = line.split(b' ', 1)
                    require(len(parts) == 2 and parts[0].isdigit() and str(int(parts[0])).encode() == parts[0])
                    h = int(parts[0])
                    require(h > prev)
                    prev = h
                else:
                    r = decode(line)
                    h = r['height' if kind == 'movements' else 'h']
                    require(type(h) is int)
                    if kind == 'validators':
                        require(set(r) == {'h', 'u'} and type(r['u']) is list and r['u'])
                        require(h > prev and (prev >= 0 or h == 0))
                        prev = h
                    else:
                        require(h == height)
                        if r['kind'] == 'height_completed':
                            require(r['phase'] == 'block' and r['status'] == 'committed'
                                    and r['tx'] == -1 and r['message'] == -1 and r['code'] == 0)
                            height += 1
                if h > last:
                    found = True
                    break
                out.write(line)
                offset += len(line)
                if selected and h >= first:
                    selected.write(line)
                if kind == 'movements' and height == last+1:
                    found = True
                    break
            if kind == 'movements':
                require(found and height == last+1)
            elif kind == 'validators':
                require(prev >= 0)
        finally:
            if selected:
                selected.flush()
                os.fsync(selected.fileno())
                selected.close()
        out.flush()
        os.fsync(out.fileno())
    require(offset > 0)
    return offset


def seal(remote, kind, out, bounded=None):
    path = REMOTE[kind]
    size, inode = remote.stat(path)
    captured = out/(kind+'.captured-prefix.raw')
    capture_size = remote.movement_offset(path, LAST) if kind == 'movements' else size
    require(0 < capture_size <= size)
    remote.capture(path, capture_size, captured)
    dest = out/NAMES[kind]
    if kind in ('movements', 'validators', 'differences'):
        offset = select_prefix(captured, kind, dest, bounded, first=FIRST, last=LAST)
    else:
        raw = captured.read_bytes()
        if kind == 'log':
            require(raw.endswith(b'\n'))
        save(dest, raw)
        offset = size
    digest = file_sha(dest)
    before = remote.digest(path, offset)
    after_size, after_inode = remote.stat(path)
    after = remote.digest(path, offset)
    require(inode == after_inode and after_size >= size and before == digest == after)
    return dict(name=dest.name, sha256=digest, bytes=offset, source_inode=inode,
                source_size_before=size, source_size_after=after_size,
                remote_prefix_sha256_before=before, remote_prefix_sha256_after=after)


def capture_cache(remote, out):
    # All context, both sides of every bound and both raw envelopes, no gas-only
    # subset. Original tar and raw gzip members are retained without rewriting.
    archive = out/'cached-window.raw.private.tar'
    script = ('set -eu; for h in $(seq '+str(FIRST-1)+' '+str(LAST+1)+'); do '
              'd=$((h / 10000)); printf "cosmoshub-2/%s/%s-block.raw.gz\\ncosmoshub-2/%s/%s-block_results.raw.gz\\n" "$d" "$h" "$d" "$h"; done | tar -C /cache -cf - -T -')
    with create(archive) as f:
        remote.command(['sh', '-c', script], output=f, timeout=600)
        f.flush()
        os.fsync(f.fileno())
    cache = out/'cache'
    cache.mkdir(mode=0o700)
    manifest, previous = {}, None
    boundaries = {}
    with tarfile.open(archive) as tar:
        for h in range(FIRST-1, LAST+2):
            for kind in ('block', 'block_results'):
                expected = f'cosmoshub-2/{h//10000}/{h}-{kind}.raw.gz'
                m = tar.next()
                require(m is not None and m.name == expected and m.isfile() and m.size <= 64 << 20)
                raw = tar.extractfile(m).read()
                dest = cache/expected
                dest.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
                save(dest, raw)
                manifest[expected] = sha(raw)
                body = unpack(raw)
                doc = decode(body)['result']
                if kind == 'block':
                    header = doc['block']['header']
                    require(header['chain_id'] == 'cosmoshub-2' and int(header['height']) == h)
                    own = doc['block_meta']['block_id']['hash']
                    require(own and header['app_hash'])
                    require(previous is None or header['last_block_id']['hash'] == previous)
                    previous = own
                    if h in (FIRST-1, FIRST, LAST, LAST+1):
                        boundaries[str(h)] = dict(raw_block_sha256=sha(body),
                                                  block_hash=own, header_app_hash=header['app_hash'])
                else:
                    require(int(doc['height']) == h)
        require(tar.next() is None)
    # Verify every copied compressed envelope against its actual remote file,
    # not merely the tar or local write. No data or private path is printed.
    check = ('set -eu; cd /cache; for h in $(seq '+str(FIRST-1)+' '+str(LAST+1)+'); do '
             'd=$((h / 10000)); sha256sum "cosmoshub-2/$d/$h-block.raw.gz" "cosmoshub-2/$d/$h-block_results.raw.gz"; done')
    raw = remote.command(['sh', '-c', check], timeout=600)
    save(out/'cache.remote.sha256', raw)
    observed = {}
    for line in raw.splitlines():
        digest, name = line.decode().split('  ', 1)
        require(name not in observed)
        observed[name] = digest
    require(manifest == observed)
    save_json(out/'cache.binding.private.json', dict(all_raw_members=manifest, boundaries=boundaries,
              native_state_completion='source-attested by retained successful original status/report; no retained per-height native AppHash or full response bytes',
              original_job_executed_482091=True, bounded_component_does_not_execute_any_native_block=True))


def bind_cli(cli, out):
    # Do not let a caller turn /bin/true into an apparent financial proof.
    # Reproduce the host binary from this exact clean, signed source tree and
    # bind both hashes before invoking the existing core gate.
    root = Path(__file__).resolve().parent.parent
    def git(*args):
        p = subprocess.run(['git', '-C', str(root), *args], stdout=subprocess.PIPE,
                           stderr=subprocess.PIPE, timeout=20)
        require(p.returncode == 0)
        return p.stdout.strip()
    require(git('status', '--porcelain') == b'')
    require(git('show', '-s', '--format=%G?', 'HEAD') == b'G')
    commit = git('rev-parse', 'HEAD').decode()
    reproduced = out/'reproduced-validation-cli'
    with create(reproduced):
        pass
    command = ['go', 'build', '-trimpath', '-buildvcs=false', '-o', str(reproduced),
               './cmd/legacy-replay-import']
    env = dict(os.environ, CGO_ENABLED='0')
    with create(out/'cli-build.private.log') as log:
        p = subprocess.run(command, cwd=root, env=env, stdout=log, stderr=subprocess.STDOUT, timeout=600)
    require(p.returncode == 0)
    os.chmod(reproduced, 0o700)
    require(cli.is_file() and not cli.is_symlink() and file_sha(cli) == file_sha(reproduced))
    save_json(out/'cli.binding.private.json', dict(signed_consumer_source=commit, signature='G',
        source_clean=True, cli_sha256=file_sha(cli), reproduced_cli_sha256=file_sha(reproduced),
        runner_sha256=file_sha(Path(__file__)), build_argv=command))


def kubectl_json(args):
    p = subprocess.run(['kubectl', '--context', 'bryanlabs', '-n', 'apps', *args, '-o', 'json'],
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=20)
    require(p.returncode == 0)
    return p.stdout, decode(p.stdout)


def main():
    a = argparse.ArgumentParser(description=__doc__)
    a.add_argument('--out', required=True, type=Path)
    a.add_argument('--profile', required=True, type=Path)
    a.add_argument('--cli', type=Path, help='built signed-source host importer; omitted means readiness only')
    a.add_argument('--readiness-only', action='store_true')
    args = a.parse_args()
    os.umask(0o077)
    args.out.mkdir(mode=0o700)
    require(args.profile.lstat().st_mode & 0o777 == 0o600 and not args.profile.is_symlink()
            and file_sha(args.profile) == PROFILE)
    jobraw, job = kubectl_json(['get', 'job', JOB])
    podraw, pods = kubectl_json(['get', 'pods', '-l', 'job-name='+JOB])
    save(args.out/'job.original.json', jobraw)
    save(args.out/'pod.original.json', podraw)
    pod, poduid = runtime(job, pods)
    remote = Remote(pod)
    size, _ = remote.stat(REMOTE['status'])
    statusraw = remote.read(REMOTE['status'], size)
    save(args.out/'status.readiness.original.json', statusraw)
    status = decode(statusraw)
    available = ready_status(status)
    save_json(args.out/'readiness.json', dict(observed=datetime.now(timezone.utc).isoformat(),
        ready_to_attempt_capture=available, source_job_uid=UID, original_target=2902000,
        committed=status.get('last_height_committed'), verified=status.get('state_hash_verified_through'),
        required_committed_at_least=LAST+1, required_verified_at_least=LAST,
        additional_capture_requirements=['all five raw prefixes captured/read back byte-stable with same inode',
          'all complete movement markers 1..482090, no missing/duplicate/reordered records',
          'all3387 window markers byte-identical to pinned profile',
          'all raw block/result members400000..482091 source-hash bound',
          'unchanged core financial reconciliation every height400001..482090, held0'],
        financial_acceptance=False, full_chain_books_acceptance=False))
    print(json.dumps(dict(ready_to_attempt_capture=available, committed=status.get('last_height_committed'),
                         verified=status.get('state_hash_verified_through'), financial_acceptance=False)))
    if args.readiness_only:
        return
    if not available:
        raise SystemExit(2)
    require(args.cli is not None)
    bind_cli(args.cli, args.out)
    files = {}
    files['movements'] = seal(remote, 'movements', args.out, args.out/'window.movements.ndjson')
    files['validators'] = seal(remote, 'validators', args.out)
    files['differences'] = seal(remote, 'differences', args.out, args.out/'window.differences.txt')
    # Status/report come AFTER the potentially large prefix capture. Their
    # actual original heights/target remain unchanged; never fabricate 482090.
    files['status'] = seal(remote, 'status', args.out)
    files['log'] = seal(remote, 'log', args.out)
    s = decode((args.out/NAMES['status']).read_bytes())
    require(ready_status(s))
    log = (args.out/NAMES['log']).read_bytes()
    require(f" height {s['last_height_committed']} verified {s['state_hash_verified_through']} rate ".encode() in log)
    marker = (args.out/'window.differences.txt').read_bytes()
    profile = decode(args.profile.read_bytes())
    require(sha(marker) == MARKERS and marker == base64.b64decode(profile['original_marker_bytes'])
            and marker.count(b'\n') == 3387)
    # Runtime provenance before/after, including unchanged whole original spec.
    jobafterraw, jobafter = kubectl_json(['get', 'job', JOB])
    podafterraw, podsafter = kubectl_json(['get', 'pods', '-l', 'job-name='+JOB])
    save(args.out/'job.after.json', jobafterraw)
    save(args.out/'pod.after.json', podafterraw)
    require(runtime(jobafter, podsafter) == (pod, poduid) and jobafter['spec'] == job['spec'])
    proof = dict(chain='cosmoshub-2', release=RELEASE, **{'from': FIRST, 'through': LAST},
                 state_hash_verified_through=LAST, records_sha256=file_sha(args.out/'window.movements.ndjson'),
                 result_differences_sha256=MARKERS, financial_recording=profile['financial_recording_lineage'])
    save_json(args.out/'proof.json', proof)
    capture_cache(remote, args.out)
    artifacts = {name: file_sha(args.out/name) for name in
        ('job.original.json', 'job.after.json', 'pod.original.json', 'pod.after.json',
         'cache.binding.private.json', 'cache.remote.sha256')}
    save_json(args.out/'sealed-window.json', dict(version='hub2-sealed-window-v1', source_job=JOB,
         source_job_uid=UID, source_pod_uid=poduid, source_image=IMAGE, source_commit=SOURCE,
         original_target=2902000, whole_job_complete=False, component=COMPONENT,
         **{'from': FIRST, 'through': LAST}, files=files, artifact_sha256=artifacts))
    cmd = [str(args.cli), '--cache', str(args.out/'cache'), '--records', str(args.out/'window.movements.ndjson'),
           '--proof', str(args.out/'proof.json'), '--status', str(args.out/NAMES['status']),
           '--result-differences', str(args.out/'window.differences.txt'), '--approved-gas-profile', str(args.profile),
           '--sealed-window-receipt', str(args.out/'sealed-window.json')]
    save_json(args.out/'command.json', dict(argv=cmd, validation_only=True, no_database=True,
              opening_state='none; per-height native-causal component only, not a ledger rebuild'))
    with create(args.out/'core-validation.private.log') as f:
        p = subprocess.run(cmd, stdout=f, stderr=subprocess.STDOUT, timeout=1800)
    save_json(args.out/'component-result.json', dict(exit_code=p.returncode, component_validated=p.returncode == 0,
        full_chain_books_acceptance=False, production_write=False, gas_used_overwritten=False,
        source_job_complete=False, source_job_target=2902000,
        cli_binding_sha256=file_sha(args.out/'cli.binding.private.json'),
        sealed_window_receipt_sha256=file_sha(args.out/'sealed-window.json')))
    require(p.returncode == 0)
    print(json.dumps(dict(component_validated=True, complete_heights=LAST-FIRST+1,
                         full_chain_books_acceptance=False, production_write=False)))

if __name__ == '__main__':
    try:
        main()
    except (Refused, KeyError, ValueError, OSError, subprocess.SubprocessError, tarfile.TarError):
        print('sealed component refused; private artifacts retained', file=__import__('sys').stderr)
        raise SystemExit(1)
