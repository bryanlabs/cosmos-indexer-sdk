import copy
import json
import unittest
from compare_zero_export import strict_json, RELEASE
from verify_terminal_fifteen import proof_check, record_check, typed_export, MANIFEST, GENESIS, BLOCK, RESULTS


def native(value):
    return strict_json(json.dumps(value).encode())


class ClosedTerminalProof(unittest.TestCase):
    def fixture(self):
        audit = dict(commit={'Version':500015,'Hash':'AQ=='}, all_persistent_stores=[{'name':'acc','key':'/w==','value':''}],
                     begin={}, deliver=None, end={}, block_hash='Ag==')
        ref = copy.deepcopy(audit)
        ref.update(original_status={'state_hash_verified_through':500014,'last_height_committed':500015},
                   original_tail={'height':500015})
        proof = dict(schema='native033-closed-terminal-v1',kind='closed_terminal_history_completed',
                     release=RELEASE,chain='cosmoshub-1',proof_mode='closed_pinned_native_reference',
                     source_manifest_sha256=MANIFEST,genesis_sha256=GENESIS,
                     terminal_block_capture_sha256=BLOCK,terminal_results_capture_sha256=RESULTS,
                     first_height=500001,height=500015,next_header_proof=False,financial_admission=False,
                     closed_reference=ref)
        return native(proof), native(audit)

    def test_positive_and_every_terminal_binding(self):
        proof,audit = self.fixture()
        proof_check(proof,audit)
        for kind in ('missing_last','wrong_root','wrong_result','wrong_header','raw_key','unknown_prefix',
                     'original_bytes','wrong_phase','fake_next','false_income','overstated_verified'):
            bad = copy.deepcopy(proof)
            if kind=='missing_last':bad['height']=native(500014)
            elif kind=='wrong_root':bad['closed_reference']['commit']['Hash']='Aw=='
            elif kind=='wrong_result':bad['closed_reference']['end']={'code':native(1)}
            elif kind=='wrong_header':bad['closed_reference']['block_hash']='Aw=='
            elif kind=='raw_key':bad['closed_reference']['all_persistent_stores'][0]['key']='AA=='
            elif kind=='unknown_prefix':bad['closed_reference']['all_persistent_stores'][0]['value']='AQ=='
            elif kind=='original_bytes':bad['source_manifest_sha256']='0'*64
            elif kind=='wrong_phase':bad['kind']='history_completed'
            elif kind=='fake_next':bad['unexecuted_next_header']={}
            elif kind=='false_income':bad['financial_admission']=True
            else:bad['closed_reference']['original_status']['state_hash_verified_through']=native(500015)
            with self.subTest(kind=kind),self.assertRaises(ValueError):proof_check(bad,audit)

    def test_no_invented_signed_fee_principal_shares_or_income_in_actual_empty_tail(self):
        good=dict(kind='accounting',phase='block',height=500015,label='',tx=-1,message=-1,status='committed')
        record_check(native(good),False)
        for kind in ('movement','principal','principal_request','staking_shares','share_request'):
            bad=dict(good,kind=kind,label='ante_fee',classification='delegator_reward_payout',amount='1')
            with self.subTest(kind=kind),self.assertRaises(ValueError):record_check(native(bad),False)
        for phase in ('ante','message','transaction','invented_phase'):
            with self.subTest(phase=phase),self.assertRaises(ValueError):record_check(native(dict(good,phase=phase)),False)
        with self.assertRaises(ValueError):record_check(native(dict(good,kind='native_write',phase='end_block')),False)
        with self.assertRaises(ValueError):record_check(native(dict(good,kind='native_cache_write',phase='begin_block')),False)
        with self.assertRaises(ValueError):record_check(native(dict(good,height=500016)),False)
        with self.assertRaises(ValueError):record_check(native(dict(good,kind='observation_completed')),False)
        sealed=native(dict(good,kind='observation_closed_terminal_completed',phase='segment_closed_pinned_native_reference'))
        self.assertEqual(record_check(sealed,False)['kind'],'observation_completed')

    def test_whole_typed_export_keeps_types_keys_positions_and_binding(self):
        good=native(dict(release=RELEASE,source_height=500015,for_zero_height=True,
                         terminal_proof_mode='closed_pinned_native_reference',closed_source_manifest_sha256=MANIFEST,
                         source_app_hash='01',app_state={'unknown':[1,2,3],'shares':'1.000000000000000001'},validators=[]))
        root={'Hash':'AQ=='}
        typed_export(good,copy.deepcopy(good),root)
        for kind in ('value','array','type','unknown','root','height','mode','share'):
            bad=copy.deepcopy(good)
            if kind=='value':bad['app_state']['unknown'][0]=native(7)
            elif kind=='array':bad['app_state']['unknown'].reverse()
            elif kind=='type':bad['app_state']['unknown'][0]='1'
            elif kind=='unknown':del bad['app_state']['unknown']
            elif kind=='root':bad['source_app_hash']='02'
            elif kind=='height':bad['source_height']=native(500014)
            elif kind=='mode':bad['terminal_proof_mode']='next_header'
            else:bad['app_state']['shares']='1.0'
            with self.subTest(kind=kind),self.assertRaises(ValueError):typed_export(good,bad,root)


if __name__=='__main__':unittest.main()
