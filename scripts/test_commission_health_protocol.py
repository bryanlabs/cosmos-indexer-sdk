import copy
import datetime
import unittest
from commission_health_protocol import adapt, DAY_START, LABELS, policy_verified, GLOBAL_LOCK


def iso(t):
    return datetime.datetime.fromtimestamp(t, datetime.timezone.utc).isoformat()


def fixture(t, quota=16, main=10):
    actors = {}
    for label in ('hub1_main',)+LABELS:
        actors[label] = dict(pod_uid=label, pod_phase='Running', container_status=[dict(restarts=0,state={'running':{}})],
                             native_processes=[] if label=='hub1_main' else [dict(pid=13,birth_ticks=100)],
                             native_progress={'watcher':dict(step='hold',fresh=True)})
    raw = dict(ObservedAt=iso(t), PublicHealthy=True, ReadOnlyDBSelect1=True, PublicHTTP={'http_status':200},
               NodeHeadroom=True, NodeCPU={n:dict(observed_epoch=t,window_seconds=60,used_cores=4,
                   allocatable_cores=16,headroom_cores=12,reserve_cores=2.1,headroom_ok=True)
                   for n in ('worker-1','controller-1')}, Actors=actors,
               Postgres=dict(uid='pg',restarts=0,observed_at=iso(t),kernel_cgroup={'cpu_max':f'{quota*100000} 100000','usage_usec':int(t*4e6)}),
               ReplayRates={k:dict(source_start=t-60,source_end=t,window_seconds=60,source_age_seconds=0,
                   verified_per_second=main if k=='hub2_main' else 6,transactions_per_block=.1,
                   pod_uid_and_pid_birth_match=True,baseline_verified_per_second=7.4)for k in LABELS},
               ReplayHealthy=True, Errors=[])
    for r in raw['ReplayRates'].values():
        r['verified_delta']=int(r['verified_per_second']*60)
        r['transactions_delta']=round(r['verified_delta']*.1)
        r['transactions_per_block']=r['transactions_delta']/r['verified_delta']
    return raw, dict(copy.deepcopy(actors),postgres_uid='pg')


class ProtocolTests(unittest.TestCase):
    def evaluate(self, raw, state=None, now=None, actual=16, policy=True, demand=False):
        expected=fixture(DAY_START)[1]
        return adapt(raw, state if state is not None else {}, now or float(datetime.datetime.fromisoformat(raw['ObservedAt']).timestamp()),actual,expected,policy,demand)

    def bootstrap(self):
        state={}
        for delta in (0,60,120):
            raw,_=fixture(DAY_START+delta)
            out=self.evaluate(raw,state)
        self.assertTrue(out['ReplayHealthy'],out['FailReasons'])
        return state

    def test_current_night10_not_api16(self):
        raw,_=fixture(DAY_START-100,10)
        out=self.evaluate(raw,actual=10)
        self.assertTrue(out['CPUWriterPolicyAuthorized'])
        self.assertFalse(out['ReplayHealthy'])
        self.assertIn('need_comparable_pg_cpu_window',out['FailReasons'])

    def test_unpinned_quiet10_can_establish_baseline_and_admit(self):
        state={}
        for delta in (0,60,120):
            raw,_=fixture(DAY_START-600+delta,10,main=8)
            out=self.evaluate(raw,state,actual=10)
        self.assertTrue(out['ReplayHealthy'],out['FailReasons'])
        self.assertTrue(out['AdmissionGranted'])
        self.assertEqual(out['ActualPGQuotaCores'],10)
        self.assertFalse(out['Historical9FloorRecovered'])

    def test_pinned_quiet10_cannot_admit(self):
        state={}
        for delta in (0,60,120):
            t=DAY_START-600+delta
            raw,_=fixture(t,10)
            raw['Postgres']['kernel_cgroup']['usage_usec']=int(t*9.98e6)
            out=self.evaluate(raw,state,actual=10)
        self.assertFalse(out['AdmissionGranted'])
        self.assertIn('pg_cpu_headroom_not_proven',out['FailReasons'])
        self.assertNotIn('baseline',state)

    def test_actual16_during_quiet_is_not_permission_to_race_policy(self):
        raw,_=fixture(DAY_START-100,16)
        self.assertIn('quiet_policy_requires_actual10_not_override',self.evaluate(raw)['FailReasons'])

    def test_baseline_never_resets_under_new_demand(self):
        state=self.bootstrap();baseline=copy.deepcopy(state['baseline'])
        raw,_=fixture(DAY_START+180,main=9.5)
        self.assertFalse(self.evaluate(raw,state,demand=True)['ReplayHealthy'])
        self.assertEqual(state['baseline'],baseline)

    def test_stale_native_source_window_rejected(self):
        raw,_=fixture(DAY_START)
        raw['ReplayRates']['hub2_main']['source_start']-=60
        raw['ReplayRates']['hub2_main']['source_end']-=60
        self.assertFalse(self.evaluate(raw)['ComparableReplayWindow'])

    def test_api_claim_does_not_replace_actual_kernel(self):
        raw,_=fixture(DAY_START+100,16)
        out=self.evaluate(raw,actual=10)
        self.assertFalse(out['ReplayHealthy'])
        self.assertIn('actual_quota_or_pg_identity_unknown',out['FailReasons'])

    def test_fresh_day16_bootstrap_and_positive_permission(self):
        self.bootstrap()

    def test_seven_point_four_is_not_an_admission_floor(self):
        raw,_=fixture(DAY_START-100,10,main=7.4)
        self.assertFalse(self.evaluate(raw,actual=10)['ReplayHealthy'])

    def test_dense_post_day16_baseline_requires_fresh_CPU_proof_not_historical_rate(self):
        state={}
        for delta in (0,60,120):
            raw,_=fixture(DAY_START+delta,main=6.4)
            raw['ReplayRates']['hub2_main']['transactions_delta']=96
            raw['ReplayRates']['hub2_main']['transactions_per_block']=.25
            out=self.evaluate(raw,state)
        self.assertTrue(out['ReplayHealthy'],out['FailReasons'])
        self.assertFalse(out['Historical9FloorRecovered'])
        self.assertEqual(out['SourceProof']['pg_average_used_cores'],4)
        self.assertIn('NOT a claim density explains historical slowdown',out['BaselineMethod'])

    def test_cpu_capacity_not_invented_from_density(self):
        state={'day_quota_observed_at':DAY_START,'pg_points':[[DAY_START+40,0]]}
        raw,_=fixture(DAY_START+100,main=6.5)
        raw['Postgres']['kernel_cgroup']['usage_usec']=int(15*60e6)
        self.assertIn('pg_cpu_headroom_not_proven',self.evaluate(raw,state)['FailReasons'])

    def test_stale_receipt(self):
        raw,_=fixture(DAY_START)
        self.assertIn('stale_receipt',self.evaluate(raw,now=DAY_START+31)['FailReasons'])

    def test_partial_schema_all_six_fields_present_and_denied(self):
        out=self.evaluate({'ObservedAt':iso(DAY_START)})
        for key in ('PublicHealthy','ReplayHealthy','NodeCPUHeadroom','ComparableReplayWindow','CPUWriterPolicyAuthorized','ReplayBaselinePreserved'):
            self.assertIn(key,out)
        self.assertFalse(out['AdmissionGranted'])

    def test_density_mismatch_cannot_invent_normalized_capacity(self):
        state=self.bootstrap();raw,_=fixture(DAY_START+180)
        raw['ReplayRates']['hub2_main']['transactions_delta']=180
        raw['ReplayRates']['hub2_main']['transactions_per_block']=.3
        out=self.evaluate(raw,state)
        self.assertFalse(out['ReplayHealthy'])
        self.assertIn('density_mismatch_no_source_CPU_normalization_proof',out['FailReasons'])

    def test_regression_after_admission(self):
        state=self.bootstrap();raw,_=fixture(DAY_START+180,main=9.5)
        out=self.evaluate(raw,state,demand=True)
        self.assertFalse(out['ReplayHealthy'])
        self.assertIn('measured_replay_regression',out['FailReasons'])

    def test_no_baseline_bootstrap_under_native_demand(self):
        state={'day_quota_observed_at':DAY_START,'pg_points':[[DAY_START+40,int((DAY_START+40)*4e6)]]}
        raw,_=fixture(DAY_START+100)
        self.assertIn('cannot_bootstrap_baseline_under_new_demand',self.evaluate(raw,state,demand=True)['FailReasons'])

    def test_h1_hold_neutral_not_healthy_zero(self):
        raw,_=fixture(DAY_START-100,10);out=self.evaluate(raw,actual=10)
        self.assertIn('NOT healthy zero-rate',out['H1Status'])
        raw['Actors']['hub1_main']['native_progress']['watcher']['step']='run'
        self.assertIn('hub1_phase_unknown',self.evaluate(raw,actual=10)['FailReasons'])

    def test_actor_birth_change(self):
        raw,_=fixture(DAY_START);raw['Actors']['hub2_main']['native_processes'][0]['birth_ticks']=101
        self.assertIn('actor_identity_changed:hub2_main',self.evaluate(raw)['FailReasons'])

    def test_public200_alone_insufficient(self):
        raw,_=fixture(DAY_START);raw['NodeHeadroom']=False
        self.assertFalse(self.evaluate(raw)['ReplayHealthy'])

    def test_next_stage_needs_full_post_permit_window(self):
        state=self.bootstrap();state['last_permit_at']=DAY_START+120
        raw,_=fixture(DAY_START+150);out=self.evaluate(raw,state,demand=True)
        self.assertTrue(out['ReplayHealthy']);self.assertFalse(out['StageReady'])
        raw,_=fixture(DAY_START+180);self.assertTrue(self.evaluate(raw,state,demand=True)['StageReady'])

    def test_source_rate_arithmetic_must_match_counters(self):
        raw,_=fixture(DAY_START)
        raw['ReplayRates']['hub2_main']['verified_per_second']=99
        self.assertFalse(self.evaluate(raw)['ComparableReplayWindow'])

    def test_consumer_requires_all_six_flags_and_freshness(self):
        from commission_acceleration_sampler import health_admission_allowed
        flags={k:True for k in ('PublicHealthy','ReplayHealthy','NodeCPUHeadroom','ComparableReplayWindow','CPUWriterPolicyAuthorized','ReplayBaselinePreserved')}
        self.assertTrue(health_admission_allowed(flags,True))
        for k in flags:
            missing=dict(flags);missing.pop(k)
            self.assertFalse(health_admission_allowed(missing,True))
        self.assertFalse(health_admission_allowed(flags,False))

    def test_pilot_without_shard_suffix_sorts_first_and_preserves_ten(self):
        from commission_acceleration_sampler import record_order
        names=[f'cosmos-tax-corrected-accel-four-s{i:02d}-20261115-v1' for i in range(1,8)]+[f'cosmos-tax-corrected-accel-later-s{i:02d}-20261115-v1' for i in range(1,4)]
        pilot='cosmos-tax-queryfix-pilot-20261115-v1'
        ordered=[r['job']for r in sorted([{'job':n}for n in names+[pilot]],key=record_order)]
        self.assertEqual(ordered[0],pilot)
        self.assertEqual(ordered[1:7],[names[0],names[7],names[1],names[8],names[2],names[9]])

    def test_completed_pilot_frees_global_slot_and_never_readmits(self):
        from commission_acceleration_sampler import Admission
        a=Admission(1,60)
        self.assertEqual(a.update(['pilot','four'],0,10,4,True),{'pilot'})
        a.mark_terminal('pilot')
        self.assertEqual(a.update(['pilot','four'],60,10,4,True),{'four'})
        self.assertIn('pilot',a.completed)
        self.assertEqual(len(a.jobs),1)

    def test_pilot_and_ten_queues_share_one_global_cap(self):
        from commission_acceleration_sampler import Admission
        a=Admission(10,60);jobs=['pilot']+['queue'+str(i)for i in range(10)]
        for i in range(11):
            a.update(jobs,i*60,16,4,True)
        self.assertEqual(len(a.jobs),10)
        self.assertIn('pilot',a.jobs)
        a.mark_terminal('pilot')
        self.assertEqual(a.update(jobs,11*60,16,4,True),set(jobs[1:]))
        self.assertEqual(len(a.jobs),10)

    def test_pending_pilot_no_pod_uid_valid_and_reserves_quiet_only(self):
        import tempfile,pathlib,json
        from commission_acceleration_sampler import load_records,quiet_pilot_reservation
        pilot={'job':'cosmos-tax-queryfix-pilot-20261115-v1','job_uid':'exact-job','native_binary_sha256':'8ab0b0a9cafdc564a3399c42e05805ba922409e59eb9226398e218f68f0e99a5','image':'pinned@sha256:2c9eaa53d21b104df307924c1d06275f5cf4535496ae71594fe7012090d75a55','intent':'private single-root compatibility pilot','pilot_state':'pending'}
        with tempfile.TemporaryDirectory()as temp:
            path=pathlib.Path(temp)/'inventory.json';path.write_text(json.dumps({'records':[pilot]}))
            records=load_records(path)
        self.assertTrue(quiet_pilot_reservation(records,DAY_START-1,set()))
        self.assertFalse(quiet_pilot_reservation(records,DAY_START,set()))
        self.assertFalse(quiet_pilot_reservation(records,DAY_START-1,{pilot['job']}))
        pilot['pilot_state']='failed'
        self.assertFalse(quiet_pilot_reservation([pilot],DAY_START-1,set()))

    def test_ready_pilot_requires_pod_uid(self):
        import tempfile,pathlib,json
        from commission_acceleration_sampler import load_records
        pilot={'job':'cosmos-tax-queryfix-pilot-20261115-v1','job_uid':'exact-job','native_binary_sha256':'8ab0b0a9cafdc564a3399c42e05805ba922409e59eb9226398e218f68f0e99a5','image':'pinned@sha256:2c9eaa53d21b104df307924c1d06275f5cf4535496ae71594fe7012090d75a55','intent':'private pilot','pilot_state':'ready'}
        with tempfile.TemporaryDirectory()as temp:
            path=pathlib.Path(temp)/'inventory.json';path.write_text(json.dumps({'records':[pilot]}))
            with self.assertRaises(ValueError):load_records(path)

    def test_quota_change_rebaselines_only_without_new_demand(self):
        state=self.bootstrap();baseline=copy.deepcopy(state['baseline'])
        raw,_=fixture(DAY_START+180,24)
        self.evaluate(raw,state,actual=24,demand=True)
        self.assertEqual(state['baseline'],baseline)
        state=self.bootstrap();self.evaluate(raw,state,actual=24,demand=False)
        self.assertNotIn('baseline',state)

    def test_scoped_single_pilot_budget_and_default_restoration(self):
        from commission_acceleration_sampler import scoped_pg_reserve
        p={'job':'cosmos-tax-queryfix-pilot-20261115-v1','job_uid':'exact-job','native_binary_sha256':'8ab0b0a9cafdc564a3399c42e05805ba922409e59eb9226398e218f68f0e99a5','image':'pinned@sha256:2c9eaa53d21b104df307924c1d06275f5cf4535496ae71594fe7012090d75a55','intent':'private pilot','pilot_state':'pending'}
        budget=lambda admitted=(),now=DAY_START-600,quota=10,done=set():scoped_pg_reserve([p],admitted,now,quota,done)
        self.assertEqual(budget(),1.1)
        self.assertEqual(budget(['other']),2.1)
        self.assertEqual(budget(now=DAY_START),2.1)
        self.assertEqual(budget(quota=16),2.1)
        self.assertEqual(budget(done={p['job']}),2.1)
        p.update(pilot_state='ready',pod_uid='exact-pod')
        self.assertEqual(budget([p['job']]),.1)
        self.assertEqual(budget([p['job'],'other']),2.1)
        p['native_binary_sha256']='wrong'
        self.assertEqual(budget(),2.1)

    def test_single_pilot_bootstrap_two_spare_positive_one_spare_negative(self):
        for used,allowed in ((8,True),(9,False)):
            state={}
            for delta in (0,60,120):
                t=DAY_START-600+delta;raw,_=fixture(t,10,main=8)
                raw['Postgres']['kernel_cgroup']['usage_usec']=int(t*used*1e6)
                out=adapt(raw,state,t,10,fixture(t)[1],True,pg_reserve=1.1)
            self.assertEqual(out['AdmissionGranted'],allowed,out['FailReasons'])

    def test_active_pilot_pinned_and_regression_still_deny(self):
        state=self.bootstrap();raw,_=fixture(DAY_START+180,main=9.5)
        out=adapt(raw,state,DAY_START+180,16,fixture(DAY_START)[1],True,demand_started=True,pg_reserve=.1)
        self.assertFalse(out['ReplayHealthy'])
        state={'day_quota_observed_at':DAY_START,'pg_points':[[DAY_START+40,0]]}
        raw,_=fixture(DAY_START+100,10)
        raw['Postgres']['kernel_cgroup']['usage_usec']=int(10*60e6)
        out=adapt(raw,state,DAY_START+100,10,fixture(DAY_START)[1],True,demand_started=True,pg_reserve=.1)
        self.assertIn('pg_cpu_headroom_not_proven',out['FailReasons'])

    def test_protocol_check_never_writes_canonical_output(self):
        import tempfile,pathlib,json,sys,io,contextlib
        from unittest.mock import patch
        import commission_acceleration_sampler as sampler
        with tempfile.TemporaryDirectory() as temp:
            jobs=pathlib.Path(temp)/'jobs.json';health=pathlib.Path(temp)/'health.json'
            jobs.write_text(json.dumps({'records':[{'job':'cosmos-tax-corrected-accel-four-s01-20261115-v1'}]}))
            health.write_text('{}');original=pathlib.Path.read_text
            def read(path,*args,**kwargs):
                if str(path).endswith('pg-stage-replay-before.safe.json'):
                    return json.dumps({'after':{'actors':{},'postgres':{'uid':'pg'}}})
                return original(path,*args,**kwargs)
            with patch.object(pathlib.Path,'read_text',read),patch.object(sys,'argv',['sampler','--jobs',str(jobs),'--health-receipt',str(health),'--protocol-check','--max-admitted','1']),patch.object(sampler,'cgroup',return_value={'cpu.max':'1000000 100000'}),patch.object(sampler,'live_policy',return_value=True),patch.object(sampler,'adapt',return_value={'ReplayHealthy':False}),patch.object(sampler,'atomic_json') as write,contextlib.redirect_stdout(io.StringIO()) as output:
                sampler.main()
                write.assert_not_called()
                self.assertEqual(json.loads(output.getvalue()),{'ReplayHealthy':False})

    def test_global_lock_absolute(self):
        self.assertEqual(GLOBAL_LOCK,'/Users/danb/.cache/corrected324-acceleration-20261115/resource-sampler.lock')

    def test_missing_policy_cannot_authorize(self):
        self.assertFalse(policy_verified({},{}))
        raw,_=fixture(DAY_START)
        self.assertFalse(self.evaluate(raw,policy=False)['CPUWriterPolicyAuthorized'])


if __name__=='__main__':
    unittest.main()
