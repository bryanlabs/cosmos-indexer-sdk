import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from source_reader import Reader, UID, POD, IMAGE, validate_reader, completed_manifest
from seal_closed import CODE, validate_start
from consume_closed import ClosedConsumer

class RetainedReader(unittest.TestCase):
    def pod(self):
        return dict(metadata=dict(name=POD,uid=UID),spec=dict(containers=[dict(image=IMAGE,volumeMounts=[dict(name='replay',mountPath='/replay'),dict(name='cache',mountPath='/cache',readOnly=True)])],volumes=[dict(name='replay',persistentVolumeClaim=dict(claimName='cosmos-tax-legacy-replay')),dict(name='cache',persistentVolumeClaim=dict(claimName='cosmos-hub4-archive-cache'))]),status=dict(phase='Running',containerStatuses=[dict(ready=True,restartCount=0,state=dict(running=dict(startedAt='original')),containerID='original',imageID='original-image')]))

    def test_actual_claim_layout_and_identity_required(self):
        p=self.pod();validate_reader(p)
        for value in ('uid','restart','claim','subpath'):
            q=copy.deepcopy(p)
            if value=='uid':q['metadata']['uid']='another'
            elif value=='restart':q['status']['containerStatuses'][0]['restartCount']=1
            elif value=='claim':q['spec']['volumes'][0]['persistentVolumeClaim']['claimName']='another'
            else:q['spec']['containers'][0]['volumeMounts'][0]['subPath']='wrong'
            with self.subTest(value=value):
                with self.assertRaises(ValueError):validate_reader(q)

    def test_completed_producer_never_needed_for_exec(self):
        r=Reader.__new__(Reader)
        self.assertEqual(r.args(['dd','if=/recordings/range-1-200000/home/status.json','bs=65536','count=1'])[1],'if=/replay/hub1-financial-e999d60-v1/range-1-200000/home/status.json')
        self.assertEqual(Reader.path('/sources/200000'),'/replay/cosmoshub-1/checkpoints/200000')
        self.assertEqual(Reader.path('/cache/genesis/genesis.cosmoshub-1.json'),'/cache/genesis/genesis.cosmoshub-1.json')
        for command in (['dd','if=/cache/test','of=/recordings/overwrite'],['tar','-xf','-'],['xargs','rm'],['rm','file'],['sha256sum','/outside/file'],['stat','/cache/../outside']):
            with self.subTest(command=command):
                with self.assertRaises(ValueError):r.args(command)

    def test_completed_manifest_exact_six_original_files(self):
        first,target=200001,400000
        names=('movements.ndjson.gz','native-history.ndjson.gz','home/status.json','home/tail.json','home/validator-updates.jsonl','home/result-differences.txt')
        raw=''.join('a'*64+f'  /recordings/range-{first}-{target}/{n}\n' for n in names).encode()
        self.assertEqual(len(completed_manifest(raw,first,target)),6)
        for changed in (raw.splitlines(keepends=True)[0],raw+raw.splitlines(keepends=True)[0],raw.replace(b'/recordings/',b'/another/')):
            with self.assertRaises(ValueError):completed_manifest(changed,first,target)

class RunStart(unittest.TestCase):
    def test_later_head_or_changed_python_cannot_name_executed_source(self):
        with tempfile.TemporaryDirectory() as d:
            repo=Path(d); pins={}; data=b'pass\n'
            for name in CODE:
                if name.endswith('.py'):
                    p=repo/name;p.parent.mkdir(parents=True,exist_ok=True);p.write_bytes(data);pins[name]=hashlib.sha256(data).hexdigest()
            record=repo/'start.json';record.write_text(json.dumps(dict(signed_source_commit='original',python_source_pins=pins)))
            def git(args,**kwargs):return 'original\n' if args[-1]=='HEAD' else data
            with patch('seal_closed.subprocess.check_output',side_effect=git):
                self.assertEqual(validate_start(repo,record),'original')
                (repo/next(iter(pins))).write_text('changed')
                with self.assertRaises(ValueError):validate_start(repo,record)
            with patch('seal_closed.subprocess.check_output',return_value='later-signed-head\n'):
                with self.assertRaises(ValueError):validate_start(repo,record)

    def test_final_marker_unknown_field_refused_before_special_case(self):
        c=ClosedConsumer(lambda q:None,{},200000)
        with self.assertRaisesRegex(ValueError,'unknown complete source record field'):
            c.accept(dict(kind='observation_completed',height=200000,unknown_field=True))

class Supervisor(unittest.TestCase):
    def test_only_genuine_source_closure_can_start_financial_consumer(self):
        from run_whole import source_ready
        from unittest.mock import Mock
        reader=Mock()
        status=dict(chain='cosmoshub-1',first_height_this_run=400001,target=500000,heights_where_results_differ_from_cache=0,last_height_committed=499999,state_hash_verified_through=499998)
        reader.command.return_value=json.dumps(status).encode()
        self.assertFalse(source_ready(reader,400001))
        status.update(last_height_committed=500000,state_hash_verified_through=500000,stop_reason='reached target; verified by unexecuted next header')
        reader.command.return_value=json.dumps(status).encode()
        self.assertTrue(source_ready(reader,400001))
        status['stop_reason']='height failed'
        reader.command.return_value=json.dumps(status).encode()
        with self.assertRaises(ValueError):source_ready(reader,400001)

    def test_adoption_requires_actual_owned_pid_command_not_pid_alone(self):
        from run_whole import alive_profile
        repo=Path('/frozen');attempt=Path('/private/profile-400001')
        self.assertFalse(alive_profile(0,400001,attempt,repo))
        args=['python3','-B',str(repo/'legacy/replay033/run_closed_profile.py'),'--first','400001','--directory',str(attempt),'--repo',str(repo)]
        with patch('run_whole.Path.read_bytes',return_value=('\0'.join(args)+'\0').encode()):
            self.assertTrue(alive_profile(123,400001,attempt,repo))
            self.assertFalse(alive_profile(123,200001,attempt,repo))
            self.assertFalse(alive_profile(123,400001,Path('/other-attempt'),repo))

class RestartAndWorkspace(unittest.TestCase):
    def test_transport_unknown_retries_but_source_content_failure_refuses(self):
        from run_whole import probe_source
        from unittest.mock import Mock
        import subprocess
        reader=Mock();reader.exists.side_effect=subprocess.TimeoutExpired('kubectl',20)
        self.assertIsNone(probe_source(reader,400001)['ready'])
        reader.exists.side_effect=None;reader.exists.return_value=False
        status=dict(chain='cosmoshub-1',first_height_this_run=400001,target=500000,heights_where_results_differ_from_cache=0,last_height_committed=499999,state_hash_verified_through=499998)
        reader.command.return_value=json.dumps(status).encode()
        self.assertFalse(probe_source(reader,400001)['ready'])
        reader.exists.return_value=True
        with self.assertRaisesRegex(ValueError,'shell failed'):probe_source(reader,400001)
        reader.exists.return_value=False;status['target']=500001
        reader.command.return_value=json.dumps(status).encode()
        with self.assertRaises(ValueError):probe_source(reader,400001)

    def test_native_target_without_shell_seal_waits_not_success(self):
        from run_whole import probe_source
        from unittest.mock import Mock
        reader=Mock()
        status=dict(chain='cosmoshub-1',first_height_this_run=400001,target=500000,heights_where_results_differ_from_cache=0,last_height_committed=500000,state_hash_verified_through=500000,stop_reason='reached target; verified by unexecuted next header')
        reader.exists.side_effect=[False,False];reader.command.return_value=json.dumps(status).encode()
        self.assertEqual(probe_source(reader,400001)['stage'],'waiting_for_original_shell_integrity_seal')
        reader.exists.side_effect=[False,True];reader.command.side_effect=[json.dumps(status).encode(),b'actual-shell-complete\n',b'100',b'200']
        result=probe_source(reader,400001)
        self.assertTrue(result['ready']);self.assertEqual(result['storage_reservation_bytes'],600+(75<<30))

    def test_sibling_reservations_count_future_not_already_allocated_bytes(self):
        from run_whole import remaining_reservation
        with tempfile.TemporaryDirectory() as d:
            p=Path(d);(p/'source').mkdir();file=p/'source/movements.source-closed.gz';file.write_bytes(b'x'*4096)
            item=dict(stage='running',storage_reservation_bytes=100000)
            self.assertEqual(remaining_reservation(item,p),max(0,100000-file.stat().st_blocks*512))
            item['stage']='complete';self.assertEqual(remaining_reservation(item,p),0)

    def test_matching_config_reused_and_success_adopted_without_overwrite(self):
        from run_whole import save_config,config_digest,adopt_completed
        with tempfile.TemporaryDirectory() as d:
            p=Path(d);old=dict(profiles=[],canonical_boundary='/boundary',terminal_seal='/wrong')
            first=save_config(p,old);original=first.read_bytes()
            self.assertEqual(save_config(p,old),first)
            new=dict(old,terminal_seal='/tail');second=save_config(p,new)
            self.assertNotEqual(first,second);self.assertEqual(first.read_bytes(),original)
            result=dict(input_config_sha256=config_digest(new),all_historical_sources_and_coinflows_reconciled=True)
            (p/'whole-reconciliation.private.json').write_text(json.dumps(result));(p/'exit.private.json').write_text('{"exit":0}')
            with patch('run_whole.reconcile',return_value=result):
                self.assertTrue(adopt_completed(p,p,Path('/boundary'),Path('/tail')))
            self.assertEqual((p/'exit.private.json').read_text(),'{"exit":0}')
            with patch('run_whole.reconcile',side_effect=ValueError('changed artifact')):
                with self.assertRaises(ValueError):adopt_completed(p,p,Path('/boundary'),Path('/tail'))
            self.assertEqual((p/'exit.private.json').read_text(),'{"exit":0}')

if __name__=='__main__':unittest.main()
