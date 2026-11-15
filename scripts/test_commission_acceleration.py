import copy
import datetime as dt
import importlib.util
from pathlib import Path
import unittest

HERE=Path(__file__).parent

def load(name):
    spec=importlib.util.spec_from_file_location(name,HERE/(name+'.py'))
    module=importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module

P=load('commission-acceleration-prepare')
G=load('commission-acceleration-guard')


class PartitionTests(unittest.TestCase):
    def setUp(self):
        self.expected=['cosmosvaloper1%04d'%i for i in range(324)]
        self.rows=[{'validator':v,'chain':'mainnet','build':'sha256:'+P.DIGEST,'from_day':'2024-03-21','as_of':'2026-11-12','status':'reconciled'} for v in self.expected[:29]]

    def test_disjoint_complete_and_reserved(self):
        parts=P.partition(self.expected,self.rows)
        self.assertEqual(parts['reserve_existing_two'],self.expected[29:31])
        union=parts['stored']+parts['reserve_existing_two']+parts['stage_four']+parts['stage_later_two']
        self.assertCountEqual(union,self.expected)
        self.assertEqual(len(set(union)),324)

    def test_unsaved_failure_is_not_an_active_root(self):
        parts=P.partition(self.expected,self.rows,terminal_unsaved=[self.expected[29]])
        self.assertEqual(parts['reserve_existing_two'],self.expected[30:32])
        self.assertNotIn(self.expected[29],parts['stage_four'])
        self.assertEqual(len(parts['stage_four']),196)
        self.assertEqual(len(parts['stage_later_two']),96)

    def test_failed_snapshot_retained_not_retried(self):
        self.rows[0]['status']='error'
        parts=P.partition(self.expected,self.rows)
        self.assertEqual(parts['immutable_failed'],[self.expected[0]])
        self.assertNotIn(self.expected[0],parts['stage_four'])

    def test_reject_bad_inputs(self):
        for field,value in [('chain','other'),('build','wrong'),('from_day','2024-03-22'),('as_of','2026-11-13'),('status','pending'),('validator','foreign')]:
            with self.subTest(field=field):
                rows=copy.deepcopy(self.rows)
                rows[0][field]=value
                with self.assertRaises(ValueError):
                    P.partition(self.expected,rows)
        with self.assertRaises(ValueError):
            P.partition(self.expected,self.rows+[self.rows[0]])
        with self.assertRaises(ValueError):
            P.partition(self.expected[:-1],self.rows)
        with self.assertRaises(ValueError):
            P.partition(self.expected,self.rows,workers=4)
        with self.assertRaises(ValueError):
            P.partition(self.expected,self.rows,terminal_unsaved=[self.expected[0]])

    def test_native_job_immutable_profile_and_network_identity(self):
        original={'spec':{'selector':{'matchLabels':{'controller-uid':'old'}},'manualSelector':False,'backoffLimit':1,'ttlSecondsAfterFinished':10,
            'template':{'metadata':{'labels':{'app':'cosmos-tax-sdk-2025-backfill','job-name':'old','batch.kubernetes.io/controller-uid':'old','cosmos-tax.bryanlabs.net/job':'commission-flows'}},
            'spec':{'containers':[{'image':'ghcr.io/bryanlabs/cosmos-tax-sdk:tag@sha256:'+P.DIGEST,'command':['/usr/local/bin/taxledger','--commission-flows','--chain=mainnet','--commission-flows-from=2024-03-21','--commission-as-of=2026-11-13','--commission-workers=2','--commission-read-snapshot-timeout=60s','--commission-output-version='+P.VERSION,'--commission-build=sha256:'+P.DIGEST,'--commission-archive-config=/archive/probe.toml'],
            'env':[{'name':'PGAPPNAME','valueFrom': {'secretKeyRef': {'name': 'preservation-credentials-required', 'key': 'password'}}},{'name':'DB_PASS','valueFrom':{'secretKeyRef':{'name':'database','key':'password'}}}],
            'resources':{'limits':{'cpu':'4','memory':'8Gi'},'requests':{'cpu':'500m','memory':'1Gi'}}}], 'restartPolicy':'Never'}}}}
        before=copy.deepcopy(original)
        new=P.make_job(original,self.expected[:196],4,'four')
        self.assertEqual(original,before)
        self.assertNotIn('selector',new['spec'])
        self.assertNotIn('ttlSecondsAfterFinished',new['spec'])
        self.assertEqual(new['spec']['backoffLimit'],0)
        labels=new['spec']['template']['metadata']['labels']
        self.assertEqual(labels['app'],'cosmos-tax-sdk-2025-backfill')
        self.assertEqual(labels['cosmos-tax.bryanlabs.net/job'],'commission-flows')
        self.assertNotIn('job-name',labels)
        a=original['spec']['template']['spec']['containers'][0]
        b=new['spec']['template']['spec']['containers'][0]
        self.assertEqual(a['resources'],b['resources'])
        self.assertEqual(a['env'][1],b['env'][1])
        self.assertEqual(a['image'],b['image'])
        self.assertIn('--commission-workers=4',b['command'])


class GuardTests(unittest.TestCase):
    def setUp(self):
        self.now=dt.datetime(2026,11,14,15,0,tzinfo=dt.timezone.utc)
        public=[{'http_status':200,'elapsed_ms':60,'body_sha256':'public-original','coverage':{'traced':48,'expected':324,'complete':False}} for _ in range(5)]
        job={'uid':'original','spec_sha256':'spec','pods':[{'uid':'pod','name':'pod','phase':'Running','states':[{'restartCount':0}]}], 'native':{'terminal_failures':[]}}
        self.base={'at':self.now.isoformat(),'errors':[],'public':{'samples':public},'sessions':[{'blocked':0}],
            'pg_cgroup':{'anon_plus_shmem_bytes':5*G.GIB,'memory_full_avg60':.5,'oom_kill':0,'cpu.stat':'usage_usec 100000000','memory.max':str(24*G.GIB),'cpu.max':'1600000 100000'},
            'output_row_identity':[{'version':'old','key_hash':'key','xmin':'10'}],
            'job_01bb':copy.deepcopy(job),'job_8704':copy.deepcopy(job),'job_6754':copy.deepcopy(job)}
        self.after=copy.deepcopy(self.base)
        self.after['pg_cgroup']['cpu.stat']='usage_usec 200000000'

    def assess(self):
        return G.assess(self.base,self.base,self.after,10,self.now)

    def test_healthy_and_no_actions(self):
        result=self.assess()
        self.assertTrue(result['safe_for_next_reviewed_stage'])
        self.assertEqual(result['measured']['pg_cpu_cores'],10)
        self.assertEqual(result['actions_performed'],[])
        self.assertFalse(result['execution_authorized'])
        self.assertTrue(result['resource_guard_only'])

    def test_review_thresholds_fail_closed(self):
        for key,value in [('anon_plus_shmem_bytes',12*G.GIB),('memory_full_avg60',2),('oom_kill',1),('memory_full_avg60',float('nan')),('cpu.stat','usage_usec 240000000'),('memory.max',str(8*G.GIB)),('cpu.max','400000 100000')]:
            with self.subTest(metric=key,value=value):
                original=copy.deepcopy(self.after)
                self.after['pg_cgroup'][key]=value
                self.assertFalse(self.assess()['safe_for_next_reviewed_stage'])
                self.after=original

    def test_public_unknown_not_zero(self):
        for sample in ({'error':'HTTPError'},{'http_status':200,'elapsed_ms':float('inf')},{'http_status':200,'elapsed_ms':251,'coverage':{}}):
            original=copy.deepcopy(self.after)
            self.after['public']['samples'][0]=sample
            self.assertFalse(self.assess()['safe_for_next_reviewed_stage'])
            self.after=original

    def test_original_outputs_subset_not_new_append(self):
        self.after['output_row_identity'].append({'version':'old','key_hash':'new-key','xmin':'11'})
        self.assertTrue(self.assess()['safe_for_next_reviewed_stage'])
        self.after['output_row_identity'][0]['xmin']='12'
        self.assertFalse(self.assess()['safe_for_next_reviewed_stage'])

    def test_job_uid_template_pod_preserved(self):
        for key,value in [('uid','changed'),('spec_sha256','changed'),('pods',[])]:
            original=copy.deepcopy(self.after)
            self.after['job_6754'][key]=value
            self.assertFalse(self.assess()['safe_for_next_reviewed_stage'])
            self.after=original

    def test_new_native_failure_cannot_hide_behind_stored_count(self):
        self.after['job_6754']['native']['terminal_failures']=[{'class':'jsonb_array_256MiB','validator_hash':'hash'}]
        self.assertFalse(self.assess()['safe_for_next_reviewed_stage'])

    def test_missing_failure_capture_unknown(self):
        del self.after['job_6754']['native']['terminal_failures']
        self.assertFalse(self.assess()['safe_for_next_reviewed_stage'])

    def test_stale_or_incomplete(self):
        self.after['at']=(self.now-dt.timedelta(minutes=3)).isoformat()
        self.assertFalse(self.assess()['safe_for_next_reviewed_stage'])
        self.after['at']=self.now.isoformat()
        self.after['errors']=['query failed']
        self.assertFalse(self.assess()['safe_for_next_reviewed_stage'])

    def test_no_rows_or_sessions_missing_pass(self):
        del self.after['sessions']
        self.assertFalse(self.assess()['safe_for_next_reviewed_stage'])

    def test_new_stage_cannot_advance_without_actual_runtime(self):
        self.after['expected_stage_jobs']=['four']
        self.after['job_four']={'pods':[], 'native':{'terminal_failures':[]},'status':{}}
        self.assertFalse(self.assess()['safe_for_next_reviewed_stage'])

    def test_original_empty_container_state_is_unknown(self):
        self.after['job_6754']['pods'][0]['states']=[]
        self.assertFalse(self.assess()['safe_for_next_reviewed_stage'])


if __name__=='__main__':
    unittest.main()
