#!/usr/bin/env python3
"""Compare complete retained native captures without reading a database.

Raw captures stay private. Only field paths, digests and match booleans leave
this module. Different snapshots never become a claim of identical inputs.
"""
import copy
import gzip
import hashlib
import json
import pathlib

MAX_RAW = 64 * 1024 * 1024
FIELDS = {'run', 'rows', 'evidence'}


def digest(data):
    return hashlib.sha256(data).hexdigest()


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=False).encode()


def load_capture(directory, expected_manifest_sha=None, manifest_path=None, field_suffix='.json.gz'):
    directory = pathlib.Path(directory)
    path = pathlib.Path(manifest_path) if manifest_path else directory / 'manifest.json'
    assert path.is_file() and not path.is_symlink() and path.stat().st_size < 65536
    raw = path.read_bytes()
    if expected_manifest_sha is not None:
        assert digest(raw) == expected_manifest_sha, 'manifest digest mismatch'
    manifest = json.loads(raw)
    assert len(manifest['Fields']) == 3 and {f['Field'] for f in manifest['Fields']} == FIELDS
    payloads = {}
    checks = {}
    for field in manifest['Fields']:
        name = field['Field']
        assert field['Codec'] == 'gzip-json-bytes-v1'
        assert 0 <= field['RawBytes'] <= MAX_RAW and 0 <= field['EncodedBytes'] <= MAX_RAW
        assert field_suffix in {'.json.gz', '.private.json.gz'}
        path = directory / (name + field_suffix)
        assert path.is_file() and not path.is_symlink()
        assert path.stat().st_size == field['EncodedBytes']
        encoded = path.read_bytes()
        assert digest(encoded) == field['EncodedSHA256'], 'encoded field digest mismatch'
        with gzip.open(path, 'rb') as stream:
            decoded = stream.read(MAX_RAW + 1)
        assert len(decoded) == field['RawBytes'] and len(decoded) <= MAX_RAW
        assert digest(decoded) == field['RawSHA256'], 'decoded field digest mismatch'
        payloads[name] = json.loads(decoded)
        checks[name] = dict(raw_bytes=len(decoded), raw_sha256=digest(decoded), encoded_bytes=len(encoded), encoded_sha256=digest(encoded))
    return manifest, payloads, checks


def safe_key(key):
    key = str(key)
    if key.startswith(('cosmos1', 'cosmosvaloper1', 'cosmosvalcons1')) or len(key) > 80:
        return '<identity-sha256:' + digest(key.encode())[:12] + '>'
    return key


def difference_paths(left, right, path='', limit=100):
    if type(left) is not type(right):
        return [path or '/']
    if isinstance(left, dict):
        paths = []
        for key in sorted(set(left) | set(right)):
            child = path + '/' + safe_key(key)
            if key not in left or key not in right:
                paths.append(child)
            else:
                paths.extend(difference_paths(left[key], right[key], child, limit))
            if len(paths) >= limit:
                break
        return paths[:limit]
    if isinstance(left, list):
        if len(left) != len(right):
            return [path + '/length']
        paths = []
        for index, (a, b) in enumerate(zip(left, right)):
            paths.extend(difference_paths(a, b, path + '/' + str(index), limit))
            if len(paths) >= limit:
                break
        return paths[:limit]
    return [] if left == right else [path or '/']


def compare(old_manifest, old, new_manifest, new, expected):
    assert set(old) == FIELDS and set(new) == FIELDS, 'every native output field is required'
    for manifest in [old_manifest, new_manifest]:
        assert manifest['Chain'] == expected['chain']
        assert digest(manifest['Validator'].encode()) == expected['root_sha256']
    assert old_manifest['Build'] == expected['old_build'] and new_manifest['Build'] == expected['new_build']
    assert old_manifest['Version'] == expected['old_version'] and new_manifest['Version'] == expected['new_version']
    assert old_manifest['Version'] != new_manifest['Version'] and old_manifest['Build'] != new_manifest['Build']
    for outputs in [old, new]:
        run = outputs['run']
        assert run['Chain'] == expected['chain'] and digest(run['Validator'].encode()) == expected['root_sha256']
        assert run['FromDay'] == expected['from_day'] and run['AsOf'] == expected['as_of_inclusive']
        assert outputs['evidence']['PrivateProfile']['ProfileSHA256'] == expected['profile_sha256']
        assert outputs['evidence']['SourceCorrectionProfile']['ProfileSHA256'] == expected['corrections_sha256']
        assert isinstance(outputs['rows'], list) and isinstance(outputs['evidence']['Wallets'], list)
        assert 'ComputedAt' in run
    a, b = copy.deepcopy(old), copy.deepcopy(new)
    computed = [a['run'].pop('ComputedAt'), b['run'].pop('ComputedAt')]
    a['rows'].sort(key=canonical)
    b['rows'].sort(key=canonical)
    # Only these documented receipt IDs are capture metadata. Every receipt's
    # scope, coverage, refusal, effect and source digest remains compared.
    snapshots = []
    for outputs in [a, b]:
        receipt_ids = []
        for name in ['PrivateProjections', 'SourceCorrectionReads']:
            for receipt in outputs['evidence'].get(name, []):
                assert 'Snapshot' in receipt, 'snapshot-bearing receipt is incomplete'
                receipt_ids.append(dict(kind=name, snapshot_sha256=digest(receipt['Snapshot'].encode())))
                receipt['Snapshot'] = '<distinct-source-snapshot-metadata>'
        snapshots.append(receipt_ids)
    matches = {name: canonical(a[name]) == canonical(b[name]) for name in sorted(FIELDS)}
    complete = new['run']['Status'] in {'ok', 'reconciled', 'drift'}
    if not complete:
        result = 'pilot_failed'
    elif matches['run'] and matches['rows'] and matches['evidence']:
        result = 'match_with_distinct_capture_metadata'
    elif matches['run'] and matches['rows']:
        result = 'economic_match_evidence_changed'
    else:
        result = 'economic_mismatch'
    return dict(result=result, complete_native_result=complete, root_sha256=expected['root_sha256'], matches=matches,
                full_fields_examined=sorted(FIELDS), field_digests=[{k: digest(canonical(v)) for k, v in outputs.items()} for outputs in [a, b]],
                differences={name: difference_paths(a[name], b[name]) for name in sorted(FIELDS) if not matches[name]},
                distinct_envelopes=[{k: manifest[k] for k in ['Version', 'Build', 'Chain']} for manifest in [old_manifest, new_manifest]],
                computed_at=[str(x) for x in computed], snapshot_metadata=snapshots,
                same_input_equivalence_proven=False, financial_acceptance=False, public_promotion=False,
                rollout_authorized=False)
