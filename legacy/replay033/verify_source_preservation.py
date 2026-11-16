#!/usr/bin/env python3
"""Post-proof original byte readback, only hashes/status, no live DB opens."""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import subprocess
from capture_bounded import PROFILES, BASE
from consume_bounded import sha
from verify_movement_rehearsal import require


def verify(root, receipt):
    b=json.loads((root/'capture.private.json').read_bytes())
    target,pod,uid=PROFILES[b['first']]
    command=BASE+['exec','-i',pod,'--']
    checked={}
    closed=b['schema']=='native033-closed-source-capture-v1'
    reader=None
    if closed:
        from source_reader import Reader
        reader=Reader()
        command=reader.base
        old_reader=json.loads((root/'reader-pod.private.json').read_bytes())
        require(reader.before['status']['containerStatuses'][0]['containerID']==old_reader['status']['containerStatuses'][0]['containerID'],'retained source reader changed after capture')
    for name,spec in b['streams'].items():
        if closed:
            size=int(reader.command(['stat','-c','%s',spec['source_path']]))
            out=reader.command(['sha256sum',spec['source_path']],timeout=max(600,size//(8<<20)+300)).decode().split()[0]
            require(out==spec['sha256'],'original complete compressed source changed')
            checked[name]=dict(sha256=out)
        else:
            n=spec['compressed_fragment_bytes']
            require(n%65536==0,'exact original compressed chunk alignment differs')
            p=subprocess.Popen(command+['dd','if='+spec['source_path'],'bs=65536','count='+str(n//65536)],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
            digest=hashlib.sha256();count=0
            for chunk in iter(lambda:p.stdout.read(65536),b''):
                digest.update(chunk);count+=len(chunk)
            p.stdout.close()
            require(p.wait(timeout=300)==0 and count==n and digest.hexdigest()==spec['fragment_sha256'],'original bounded compressed prefix changed')
            checked[name]=dict(bytes=count,sha256=digest.hexdigest())
    def remote_pins(prefix,pins,label):
        for name in pins:
            rel=Path(name)
            require(not rel.is_absolute() and '..' not in rel.parts and '\n' not in name,'unsafe original readback member')
        prefix=reader.path(prefix) if closed else prefix
        names=[prefix+'/'+name for name in sorted(pins)]
        raw=subprocess.check_output(command+['xargs','-0','-r','-n','256','sha256sum'],input=('\0'.join(names)+'\0').encode(),timeout=max(600,len(names)//100+300))
        actual={}
        for line in raw.decode().splitlines():
            digest,name=line.split(None,1)
            require(name.startswith(prefix+'/'),'original hash readback escaped source')
            rel=name[len(prefix)+1:]
            require(rel not in actual,'duplicate original hash member')
            actual[rel]=digest
        require(actual==pins,'original '+label+' members changed')
        with (receipt.parent/(label+'-source-after.private.sha256')).open('xb') as out:out.write(raw)
    remote_pins('/cache',b['cache_members_sha256'],'cache')
    if b['first']>1:
        remote_pins('/sources/'+str(b['first']-1),b['opening_checkpoint_members_sha256'],'opening')
    if closed:
        remote=f'/recordings/range-{b["first"]}-{target}'
        require(not reader.exists(remote+'/failed.private') and reader.exists(remote+'/recording-complete.private'),'original complete source shell failure or missing closure stamp')
        raw=reader.command(['dd','if='+remote+'/binaries.private.sha256','bs=65536','count=1'])
        require(raw==(root/'binaries.private.sha256').read_bytes(),'original recorded binary evidence changed')
        require(reader.command(['dd','if='+remote+'/completed.private.sha256','bs=65536','count=1'])==(root/'completed.private.sha256').read_bytes(),'original complete-source seal changed after proof')
        reader.after()
    else:
        raw=subprocess.check_output(command+['sha256sum','/usr/local/bin/replay033','/usr/local/bin/checkpoint-proof'],timeout=30)
    require([line.split()[0] for line in raw.decode().splitlines()]==['7773ca271496eba347385d0bff11b0fcff2376de4cc58c61f568e130e883e485','10cd8b474d95b27a3ac7baf53b5aebed1efdebe3118ec2948e709e3789201f9a'],'original runtime binaries differ')
    current=json.loads(subprocess.check_output(BASE+['get','pod',pod,'-o','json'],timeout=30))
    before=json.loads((root/'source-pod.private.json').read_bytes())
    a,z=before['status']['containerStatuses'][0],current['status']['containerStatuses'][0]
    require(current['metadata']['uid']==uid and a['containerID']==z['containerID'] and a['imageID']==z['imageID'] and z['restartCount']==0 and (z['state'].get('running') or z['state'].get('terminated',{}).get('exitCode')==0),'original producer runtime changed or failed')
    result=dict(schema='native033-source-after-proof-preservation-v1',first=b['first'],through=b['through'],source_target=target,
                source_capture_sha256=sha(root/'capture.private.json'),source_preserved_after_proof=True,
                compressed_original_sources=checked,raw_cache_members=len(b['cache_members_sha256']),
                original_normal_opening_members=len(b.get('opening_checkpoint_members_sha256') or {}),
                original_runtime_binary_pins_equal=True,original_pod_uid=uid,original_restarts=0,
                checked_utc=datetime.datetime.now(datetime.timezone.utc).isoformat(),financial_admission=False)
    with receipt.open('x') as out:json.dump(result,out,indent=2,sort_keys=True)
    return result

if __name__=='__main__':
    p=argparse.ArgumentParser();p.add_argument('--capture',type=Path,required=True);p.add_argument('--receipt',type=Path,required=True);a=p.parse_args()
    os.umask(0o077)
    r=verify(a.capture,a.receipt)
    print(json.dumps(dict(first=r['first'],through=r['through'],source_preserved_after_proof=True,raw_cache_members=r['raw_cache_members'])))
