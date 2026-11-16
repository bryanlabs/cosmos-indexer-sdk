"""Read-only commands through an EXISTING retained Pod, not a completed producer."""
import json
from pathlib import PurePosixPath
import subprocess
from capture_bounded import BASE
from verify_movement_rehearsal import require

EXISTS_SCRIPT = 'if test -f "$1"; then printf "present\\n"; else printf "absent\\n"; fi'
POD = 'cosmos-tax-legacy-replay-hub1-pttnh'
UID = '1282eed2-35c4-43e9-b9d0-850283596887'
IMAGE = 'ghcr.io/bryanlabs/cosmos-tax-sdk:ffc9515-legacy-replay033@sha256:89f105fbe5a764ebbd1154171ad77f64c0ac9bd885079979c130f13a2ed8e79b'


def validate_reader(pod):
    require(pod['metadata']['name'] == POD and pod['metadata']['uid'] == UID and pod['status']['phase'] == 'Running', 'retained read-only-command carrier identity differs')
    containers = pod['spec']['containers']
    require(len(containers) == 1 and containers[0]['image'] == IMAGE, 'retained reader image differs')
    status = pod['status']['containerStatuses']
    require(len(status) == 1 and status[0]['ready'] and status[0]['restartCount'] == 0 and status[0]['state'].get('running'), 'retained reader restarted or is not ready')
    volumes = {v['name']: v.get('persistentVolumeClaim', {}).get('claimName') for v in pod['spec']['volumes']}
    mounts = {m['mountPath']: m for m in containers[0]['volumeMounts']}
    require(volumes[mounts['/replay']['name']] == 'cosmos-tax-legacy-replay' and not mounts['/replay'].get('subPath') and volumes[mounts['/cache']['name']] == 'cosmos-hub4-archive-cache' and mounts['/cache'].get('readOnly') is True and not mounts['/cache'].get('subPath'), 'retained reader does not mount exact original shared claims')
    return pod


class Reader:
    def __init__(self):
        self.before = validate_reader(json.loads(subprocess.check_output(BASE + ['get','pod',POD,'-o','json'],timeout=30)))
        self.base = BASE + ['exec','-i',POD,'--']

    @staticmethod
    def path(value):
        if value.startswith('/'):
            require('..' not in PurePosixPath(value).parts and any(value == root or value.startswith(root + '/') for root in ('/recordings','/sources','/cache')), 'readback path outside original claims')
        for original, actual in (('/recordings','/replay/hub1-financial-e999d60-v1'),('/sources','/replay/cosmoshub-1/checkpoints')):
            if value == original or value.startswith(original + '/'):
                return actual + value[len(original):]
        return value

    def args(self, args):
        require(args and args[0] in ('dd','stat','sha256sum','tar','xargs','sh'), 'non-read-only source command refused')
        if args[0] == 'sh':
            require(len(args) == 5 and args[:4] == ['sh','-c',EXISTS_SCRIPT,'readback-exists'] and args[4].startswith('/'), 'only fixed read-only existence probe shell allowed')
        if args[0] == 'dd':
            require(all(arg.startswith(('if=','bs=','count=')) for arg in args[1:]) and any(arg.startswith('if=') for arg in args[1:]), 'source dd output or unknown option refused')
        if args[0] == 'tar':
            require('-cf' in args and args[args.index('-cf') + 1] == '-' and not any(arg.startswith(('-x','--extract','--delete')) for arg in args[1:]), 'source tar must write only stdout')
        if args[0] == 'xargs':
            require(args == ['xargs','-0','-r','-n','256','sha256sum'], 'source batch may only hash files')
        result = []
        for arg in args:
            result.append('if=' + self.path(arg[3:]) if arg.startswith('if=') else self.path(arg))
        return result

    def command(self, args, timeout=60, input=None):
        return subprocess.check_output(self.base + self.args(args),timeout=timeout,input=input)

    def exists(self, path):
        out = self.command(['sh','-c',EXISTS_SCRIPT,'readback-exists',path]).decode().strip()
        require(out in ('present','absent'), 'unexpected read-only existence probe response')
        return out == 'present'

    def after(self):
        after = validate_reader(json.loads(subprocess.check_output(BASE + ['get','pod',POD,'-o','json'],timeout=30)))
        a, z = self.before['status']['containerStatuses'][0], after['status']['containerStatuses'][0]
        require(a['containerID'] == z['containerID'] and a['imageID'] == z['imageID'] and a['state']['running']['startedAt'] == z['state']['running']['startedAt'], 'retained read-only-command carrier changed during readback')
        return after


def completed_manifest(raw, first, target):
    prefix = f'/recordings/range-{first}-{target}/'
    expected = {'movements.ndjson.gz','native-history.ndjson.gz','home/status.json','home/tail.json','home/validator-updates.jsonl','home/result-differences.txt'}
    pins = {}
    for line in raw.decode().splitlines():
        digest, name = line.split(None,1)
        require(len(digest) == 64 and all(c in '0123456789abcdef' for c in digest) and name.startswith(prefix) and name[len(prefix):] in expected and name not in pins, 'invalid original completed-source manifest')
        pins[name] = digest
    require(len(pins) == len(expected), 'original completed-source manifest coverage differs')
    return pins
