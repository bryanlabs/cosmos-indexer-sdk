import base64
import copy
import json
from pathlib import Path
import tempfile
import unittest
from compare_zero_export import RELEASE
from verify_movement_rehearsal import audit_compare, commitment, verify_stream

NAMES = ['main','acc','staking','slashing','mint','distr','gov','fee','params']


def encode(value):
    return base64.b64encode(value).decode()


def pairs(state):
    return [{'key':encode(key),'value':state[key]} for key in sorted(state)]


def row(kind,label,phase='block',**extra):
    result = dict(kind=kind,label=label,phase=phase,status='committed',height=400001,tx=-1,message=-1)
    result.update(extra)
    return result


def scope(records,phase,before,after,**extra):
    for name in NAMES:
        label = 'store:'+name+':native'
        records.append(row('native_state_hash',label,phase,unit='native_store_sha256',
                           before=commitment(before[name]),after=commitment(after[name]),**extra))
        for key in sorted(set(before[name])|set(after[name])):
            b,a = before[name].get(key),after[name].get(key)
            if b != a:
                records.append(row('native_state',label+'/key:'+encode(key),phase,
                                   unit='native_store_bytes',before=b,after=a,**extra))


class CompleteMovementEvidence(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)

    def tearDown(self):
        self.temp.cleanup()

    def write(self,name,records):
        path = self.root/name
        path.write_text(''.join(json.dumps(r,separators=(',',':'))+'\n' for r in records))
        return path

    def stream(self):
        self.before = {name:{b'\x01':'',b'\x02':encode(b'[3,1,2]')} for name in NAMES}
        after = copy.deepcopy(self.before)
        after['acc'][b'\x02'] = encode(b'[2,3,1]')
        records = [row('native_state_checkpoint','segment_baseline/store:'+name+':native',before=pairs(self.before[name])) for name in NAMES]
        scope(records,'begin_block',self.before,self.before)
        scope(records,'end_block',self.before,after)
        scope(records,'block',self.before,after)
        records.append(row('height_completed',''))
        records.extend(row('native_state_checkpoint','segment_final/store:'+name+':native',before=pairs(after[name])) for name in NAMES)
        records.append(row('observation_completed','',phase='segment'))
        return records,after

    def check(self,records,after):
        return verify_stream(self.write('stream',records),{400001:after},{},baseline=self.before)

    def test_complete_changed_unchanged_state_and_real_predecessors(self):
        records,after = self.stream()
        result = self.check(records,after)
        self.assertEqual(result['records']['native_state'],2)
        self.assertEqual(result['records']['native_state_hash'],27)

    def test_missing_changed_key_fails_not_hash_only_acceptance(self):
        records,after = self.stream()
        records = [r for r in records if r['kind'] != 'native_state']
        with self.assertRaisesRegex(ValueError,'commitment differs'):
            self.check(records,after)

    def test_changed_array_bytes_wrong_units_unknown_predecessor_and_incomplete_end(self):
        records,after = self.stream()
        for change in ('array','unit','unknown','completion'):
            bad = copy.deepcopy(records)
            if change == 'array':next(r for r in bad if r['kind']=='native_state')['after']=encode(b'[1,2,3]')
            elif change == 'unit':next(r for r in bad if r['kind']=='native_state')['unit']='tokens'
            elif change == 'unknown':next(r for r in bad if r['kind']=='native_state_hash')['before']['sha256']='0'*64
            else:bad.pop()
            with self.subTest(change=change),self.assertRaises(ValueError):self.check(bad,after)

    def native_audit(self):
        return dict(release=RELEASE,chain='cosmoshub-1',height=400001,transactions=[],deliver=None,
                    all_persistent_stores=[dict(name=name,partitions=[dict(code=0,height='400001',key=encode(bytes([p])),value='AA==') for p in range(256)]) for name in NAMES],
                    native_extra={'all_values':[3,1,2],'scalar':9})

    def test_complete_typed_comparison_keeps_every_field_and_position(self):
        native = self.native_audit()
        left = self.write('native',[native])
        self.assertEqual(audit_compare(left,self.write('observed',[dict(reversed(list(native.items())))]),400001,400001),{})
        for kind in ('field','array','type','null'):
            bad = copy.deepcopy(native)
            if kind=='field':del bad['native_extra']
            elif kind=='array':bad['native_extra']['all_values'].reverse()
            elif kind=='type':bad['native_extra']['scalar']='9'
            else:bad['deliver']=[]
            with self.subTest(kind=kind),self.assertRaisesRegex(ValueError,'whole native response/state audit differs'):
                audit_compare(left,self.write('observed',[bad]),400001,400001)

    def test_identical_but_structurally_incomplete_native_audits_refuse(self):
        for kind in ('prefix','store','query_identity','chain','height_type'):
            bad = self.native_audit()
            if kind=='prefix':bad['all_persistent_stores'][0]['partitions'].pop()
            elif kind=='store':bad['all_persistent_stores'].pop()
            elif kind=='query_identity':bad['all_persistent_stores'][0]['partitions'][0]['key']=encode(b'\xff')
            elif kind=='chain':bad['chain']='wrong-chain'
            else:bad['all_persistent_stores'][0]['partitions'][0]['height']=400001
            with self.subTest(kind=kind),self.assertRaises(ValueError):
                audit_compare(self.write('native',[bad]),self.write('observed',[bad]),400001,400001)

    def test_checkpoint_cannot_inject_unrelated_state(self):
        records,after=self.stream()
        bad=row('native_state_checkpoint','unknown/store:acc:native',before=pairs({b'\x01':encode(b'arbitrary')}))
        records.insert(9,bad)
        with self.assertRaisesRegex(ValueError,'unsupported native checkpoint'):
            self.check(records,after)

    def test_valid_previously_seen_state_is_not_actual_phase_predecessor(self):
        records,after=self.stream()
        records=records[:next(i for i,r in enumerate(records) if r['kind']=='native_state_checkpoint' and r['label'].startswith('segment_final/'))]
        scope(records,'begin_block',after,after,height=400002)
        scope(records,'end_block',after,after,height=400002)
        scope(records,'block',after,after,height=400002)
        records.append(row('height_completed','',height=400002))
        records.extend(row('native_state_checkpoint','segment_final/store:'+name+':native',height=400002,before=pairs(after[name])) for name in NAMES)
        records.append(row('observation_completed','',height=400002))
        states={400001:after,400002:after}
        verify_stream(self.write('stream',records),states,{},baseline=self.before)
        index=next(i for i,r in enumerate(records) if r['kind']=='native_state_hash' and r['phase']=='block' and r['height']==400002 and r['label']=='store:acc:native')
        records[index]['before']=commitment(self.before['acc'])
        records.insert(index+1,row('native_state','store:acc:native/key:'+encode(b'\x02'),height=400002,unit='native_store_bytes',before=self.before['acc'][b'\x02'],after=after['acc'][b'\x02']))
        with self.assertRaisesRegex(ValueError,'commitment differs'):
            verify_stream(self.write('stream',records),states,{},baseline=self.before)

    def test_forged_classification_and_positive_fee_refuse(self):
        records,after=self.stream()
        for label,classification,amount in [('ante_fee','delegator_reward_payout','5000000'),('bank/send/bank.send','validator_commission_payout','1'),('ante_fee','signed_ante_fee','7')]:
            bad=copy.deepcopy(records)
            bad.insert(9,row('movement',label,'ante',tx=0,message=-1,classification=classification,amount=amount,unit='native_bank_tokens',before='0',after='1'))
            with self.subTest(label=label,classification=classification),self.assertRaises(ValueError):self.check(bad,after)

    def test_aborted_bank_attempt_never_counts_as_payout(self):
        records,after=self.stream()
        records.insert(9,row('movement','bank/send/bank.send','message',status='aborted',classification='bank_transfer',amount='1',unit='native_bank_tokens',before='1',after='2'))
        result=self.check(records,after)
        self.assertEqual(result['classifications'],{})
        self.assertEqual(result['aborted_attempt_classifications'],{'bank_transfer':1})

    def test_export_anchors_every_row_and_baseline_to_native_source(self):
        records,after=self.stream()
        export=[row('native_state_checkpoint','export_baseline/store:'+name+':native',before=pairs(self.before[name])) for name in NAMES]
        scope(export,'zero_height_export',self.before,after,validator='synthetic-validator')
        scope(export,'zero_height_export',self.before,after)
        export.extend(row('native_state_checkpoint','export_final/store:'+name+':native',before=pairs(after[name])) for name in NAMES)
        export.append(row('export_completed','','zero_height_export'))
        for r in export:r['status']='exported'
        self.assertTrue(verify_stream(self.write('export',export),exported=True,baseline=self.before,export_height=400001))
        for kind in ('height','baseline'):
            bad=copy.deepcopy(export)
            if kind=='height':bad[0]['height']=400002
            else:bad[0]['before'][0]['value']=encode(b'wrong checkpoint')
            with self.subTest(kind=kind),self.assertRaises(ValueError):
                verify_stream(self.write('export',bad),exported=True,baseline=self.before,export_height=400001)

    def test_output_after_completion_refuses(self):
        records,after=self.stream();records.append(row('accounting',''))
        with self.assertRaisesRegex(ValueError,'data follows native completion'):self.check(records,after)


if __name__=='__main__':unittest.main()
