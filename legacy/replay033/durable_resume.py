"""Private, fail-closed height-boundary snapshots and seekable sealed derivatives.

No pickle, original DB access, source mutation, or financial admission. Preparing a
source is a separate explicit operation, never an implicit resume prefix replay.
"""
import base64
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
import time

from consume_bounded import native_numbers, sha
from compare_zero_export import strict_json
from verify_movement_rehearsal import require


def sync_dir(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def put(path, data):
    with path.open('xb') as f:
        f.write(data)
        f.flush()
        os.fsync(f.fileno())


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':')).encode()


def members(root):
    result = {}
    for path in sorted(root.rglob('*')):
        require(not path.is_symlink(), 'checkpoint symlink refused')
        if path.is_file():
            result[str(path.relative_to(root))] = dict(bytes=path.stat().st_size, sha256=sha(path))
        else:
            require(path.is_dir(), 'checkpoint nonregular member refused')
    return result


def graph_dump(value):
    """Typed JSON object graph, retaining map aliases without executable objects."""
    memo, nodes = {}, []
    def pack(v):
        if type(v) in (str, int, bool, type(None)):
            return ['scalar', v]
        if isinstance(v, bytes):
            return ['bytes', base64.b64encode(v).decode()]
        ident = id(v)
        if ident in memo:
            return ['ref', memo[ident]]
        index = len(nodes)
        memo[ident] = index
        nodes.append(None)
        if isinstance(v, dict):
            node = ['counter' if isinstance(v, Counter) else 'dict', [[pack(k), pack(w)] for k, w in v.items()]]
        elif isinstance(v, (list, tuple, set)):
            node = [type(v).__name__, [pack(w) for w in v]]
        else:
            raise ValueError('unsupported checkpoint state type')
        nodes[index] = node
        return ['ref', index]
    root = pack(value)
    return canonical(dict(root=root, nodes=nodes))


def graph_load(data):
    spec = json.loads(data)
    memo, active = {}, set()
    def unpack(p):
        tag, value = p
        if tag == 'scalar':
            require(type(value) in (str, int, bool, type(None)), 'invalid checkpoint scalar')
            return value
        if tag == 'bytes':
            return base64.b64decode(value, validate=True)
        require(tag == 'ref' and type(value) is int and 0 <= value < len(spec['nodes']), 'invalid checkpoint graph reference')
        if value in memo:
            return memo[value]
        require(value not in active, 'cyclic checkpoint refused')
        active.add(value)
        kind, content = spec['nodes'][value]
        if kind in ('dict', 'counter'):
            result = Counter() if kind == 'counter' else {}
            for k, v in content:
                key = unpack(k)
                require(key not in result, 'duplicate checkpoint map key')
                result[key] = unpack(v)
        else:
            require(kind in ('list', 'tuple', 'set'), 'unknown checkpoint graph type')
            result = {'list': list, 'tuple': tuple, 'set': set}[kind](unpack(v) for v in content)
        memo[value] = result
        active.remove(value)
        return result
    return unpack(spec['root'])


def prepare_sources(root, binding, destination, lines, chunk_bytes=8 << 20):
    """Explicit one-time strict gzip validation into line-aligned hashed chunks."""
    destination.mkdir(mode=0o700)
    manifest = dict(schema='native033-seekable-source-v1', capture_sha256=sha(root / 'capture.private.json'), streams={})
    for name in ('movements', 'native-history'):
        chunks, pending, records = [], bytearray(), 0
        def flush():
            nonlocal pending, records
            filename = f'{name}.{len(chunks):08d}.jsonl'
            data = bytes(pending)
            put(destination / filename, data)
            chunks.append(dict(file=filename, bytes=len(data), records=records, sha256=hashlib.sha256(data).hexdigest()))
            pending, records = bytearray(), 0
        for raw, row in lines(root, binding, name, with_raw=True):
            pending.extend(raw)
            records += 1
            if len(pending) >= chunk_bytes:
                flush()
        if pending:
            flush()
        manifest['streams'][name] = dict(original=binding['streams'][name], chunks=chunks)
    put(destination / 'manifest.json', canonical(manifest))
    sync_dir(destination)
    return manifest


def validate_derivative(root, manifest):
    """Once before a fresh run, bind ALL derivative bytes to original raw seal.

    Resume instead pins this exact manifest in the durable checkpoint, and checks
    each chunk before use, so it never replays or hashes an old gzip prefix.
    """
    for spec in manifest['streams'].values():
        digest = hashlib.sha256()
        length = count = 0
        for chunk in spec['chunks']:
            require(Path(chunk['file']).name == chunk['file'], 'unsafe source chunk path')
            path = root / chunk['file']
            require(not path.is_symlink() and path.stat().st_size == chunk['bytes'] and sha(path) == chunk['sha256'], 'changed seekable source chunk')
            with path.open('rb') as stream:
                for raw in stream:
                    require(raw.endswith(b'\n') and len(raw) <= 16 << 20, 'invalid derivative source line')
                    digest.update(raw)
                    length += len(raw)
                    count += 1
        original = spec['original']
        require(digest.hexdigest() == original['raw_sha256'] and length == original['raw_bytes'] and count == original['records'], 'derivative does not reproduce exact original source')


class SeekableRows:
    def __init__(self, root, manifest, name, cursor=None):
        self.root, self.chunks = root, manifest['streams'][name]['chunks']
        self.chunk, self.offset = cursor or (0, 0)
        self.stream = None
        require(0 <= self.chunk <= len(self.chunks) and self.offset >= 0, 'invalid source cursor')

    def __iter__(self):
        return self

    def __next__(self):
        while self.chunk < len(self.chunks):
            spec = self.chunks[self.chunk]
            path = self.root / spec['file']
            require(Path(spec['file']).name == spec['file'], 'unsafe source chunk path')
            if self.stream is None:
                require(not path.is_symlink() and path.stat().st_size == spec['bytes'] and sha(path) == spec['sha256'], 'changed seekable source chunk')
                require(self.offset <= spec['bytes'], 'source offset outside chunk')
                self.stream = path.open('rb')
                if self.offset:
                    self.stream.seek(self.offset - 1)
                    require(self.stream.read(1) == b'\n', 'source cursor not at record boundary')
                self.stream.seek(self.offset)
            raw = self.stream.readline((16 << 20) + 1)
            if not raw:
                self.stream.close()
                self.stream = None
                self.chunk += 1
                self.offset = 0
                continue
            require(raw.endswith(b'\n') and len(raw) <= 16 << 20, 'invalid seekable source record')
            self.offset = self.stream.tell()
            return native_numbers(strict_json(raw))
        raise StopIteration

    def close(self):
        if self.stream is not None:
            self.stream.close()
            self.stream = None

    def __del__(self):
        self.close()

    def cursor(self):
        return [self.chunk, self.offset]


def code_identity(binary, config, source_manifest):
    base = Path(__file__).resolve().parent
    paths = sorted(base.glob('*.py')) + sorted((base / 'cmd' / 'consume-native').glob('*.go')) + [base / 'go.mod', base / 'go.sum']
    return dict(schema='native033-integrated-resume-v1', binary_sha256=sha(binary),
                code={str(p.relative_to(base)): sha(p) for p in paths}, config=config,
                source_manifest_sha256=sha(source_manifest))


def save_checkpoint(directory, identity, consumer, history, movements, checkpoint_native):
    require(consumer.heights >= consumer.first and not consumer.pending and not consumer.scope and not consumer.rows, 'checkpoint requires finished financial height')
    require(not consumer.journal.frames and not consumer.journal.tips and not consumer.journal.block, 'checkpoint requires finished journal height')
    started = time.monotonic()
    directory.mkdir(mode=0o700, exist_ok=True)
    generation = f'height-{consumer.heights:012d}-{time.time_ns()}'
    stage = directory / (generation + '.partial')
    stage.mkdir(mode=0o700)
    checkpoint_native(stage / 'native', consumer.heights)
    state = {k: v for k, v in consumer.__dict__.items() if k not in ('send', 'history', 'journal', 'state_commitments')}
    hstate = {k: v for k, v in history.__dict__.items() if k != 'rows'}
    put(stage / 'python.json', graph_dump(dict(consumer=state, journal=consumer.journal.__dict__, history=hstate,
                                             movement_cursor=movements.cursor(), history_cursor=history.rows.cursor())))
    seal = dict(schema='native033-durable-boundary-v1', height=consumer.heights, identity=identity, members=members(stage))
    put(stage / 'seal.json', canonical(seal))
    sync_dir(stage)
    final = directory / generation
    os.rename(stage, final)
    sync_dir(directory)
    current = canonical(dict(generation=generation, seal_sha256=sha(final / 'seal.json')))
    pointer = directory / (generation + '.pointer')
    put(pointer, current)
    os.replace(pointer, directory / 'CURRENT')
    sync_dir(directory)
    return dict(height=consumer.heights, write_seconds=time.monotonic() - started, generation=generation)


def load_checkpoint(directory, identity):
    started = time.monotonic()
    pointer = json.loads((directory / 'CURRENT').read_bytes())
    name = pointer['generation']
    require(Path(name).name == name and not name.endswith('.partial'), 'unsafe checkpoint generation')
    generation = directory / name
    require(not generation.is_symlink() and sha(generation / 'seal.json') == pointer['seal_sha256'], 'torn or changed checkpoint seal')
    seal = json.loads((generation / 'seal.json').read_bytes())
    require(seal['schema'] == 'native033-durable-boundary-v1' and seal['identity'] == identity, 'source/code/config checkpoint identity differs')
    actual = members(generation)
    actual.pop('seal.json')
    require(actual == seal['members'], 'torn or tampered checkpoint members')
    state = graph_load((generation / 'python.json').read_bytes())
    require(state['consumer']['heights'] == seal['height'], 'checkpoint height mismatch')
    return generation, state, dict(height=seal['height'], restore_seconds=time.monotonic() - started)
