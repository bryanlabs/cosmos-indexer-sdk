import copy
import gzip
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from consume_closed import ClosedConsumer, closed_lines, validate_source
from reconcile_whole import check_profiles

NAMES = ('main','acc','staking','slashing','mint','distr','gov','fee','params')

class ClosedBytes(unittest.TestCase):
    def fixture(self, directory, rows):
        raw = b''.join(json.dumps(r).encode() + b'\n' for r in rows)
        path = directory / 'movements.source-closed.gz'
        path.write_bytes(gzip.compress(raw, mtime=0))
        b = dict(through=200000, streams={'movements':dict(sha256=hashlib.sha256(path.read_bytes()).hexdigest(), compressed_bytes=path.stat().st_size, records=len(rows), raw_bytes=len(raw), raw_sha256=hashlib.sha256(raw).hexdigest(), completion_kind='observation_completed')})
        return b

    def test_footer_and_exact_marker_both_required(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            b = self.fixture(root, [dict(kind='height_completed',height=200000)])
            with self.assertRaises(ValueError):
                list(closed_lines(root,b,'movements'))
            b = self.fixture(root,[dict(kind='observation_completed',height=200000)])
            self.assertEqual(len(list(closed_lines(root,b,'movements'))),1)
            p=root/'movements.source-closed.gz'
            p.write_bytes(p.read_bytes()[:-1])
            b['streams']['movements']['sha256']=hashlib.sha256(p.read_bytes()).hexdigest()
            b['streams']['movements']['compressed_bytes']=p.stat().st_size
            with self.assertRaises(ValueError):
                list(closed_lines(root,b,'movements'))

    def test_trailing_member_and_record_refused(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d)
            b=self.fixture(root,[dict(kind='observation_completed',height=200000),dict(kind='height_completed',height=200001)])
            with self.assertRaises(ValueError):
                list(closed_lines(root,b,'movements'))
            b=self.fixture(root,[dict(kind='observation_completed',height=200000)])
            p=root/'movements.source-closed.gz'
            p.write_bytes(p.read_bytes()+gzip.compress(b'{}\n'))
            b['streams']['movements']['sha256']=hashlib.sha256(p.read_bytes()).hexdigest()
            b['streams']['movements']['compressed_bytes']=p.stat().st_size
            with self.assertRaises(ValueError):
                list(closed_lines(root,b,'movements'))

    def test_bounded_schema_cannot_be_adapted(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d)
            (root/'capture.private.json').write_text(json.dumps(dict(schema='native033-bounded-in-progress-capture-v1',first=1)))
            with self.assertRaises(ValueError):
                validate_source(root)

class FinalAnchors(unittest.TestCase):
    def test_final_marker_needs_all_nine_actual_final_states(self):
        c=ClosedConsumer(lambda q:None,{},200000,1)
        c.heights=200000
        c.latest={name:{} for name in NAMES}
        c.journal.canonical={'store:'+name+':native':{} for name in NAMES}
        marker=dict(kind='observation_completed',height=200000,tx=-1,message=-1,phase='segment',status='committed',code=0)
        with self.assertRaises(ValueError):c.accept(marker)
        for name in NAMES:
            c.accept(dict(kind='native_state_checkpoint',height=200000,tx=-1,message=-1,phase='segment',status='committed',label='segment_final/store:'+name+':native',before=[]))
        c.accept(marker)
        self.assertTrue(c.closed)
        with self.assertRaises(ValueError):c.accept(marker)

class Seams(unittest.TestCase):
    def profiles(self):
        def commit(height):return dict(Version=height,Hash=str(height))
        def roots(height):return dict(Version=height,StoreInfos=[dict(Name=name,Core=dict(CommitID=commit(height))) for name in NAMES])
        return [dict(first=first,through=last,heights=last-first+1,transactions=0,phases=dict(begin_block=last-first+1,end_block=last-first+1,block=last-first+1),source_whole_complete=True,opening_commit=commit(first-1),ending_commit=commit(last),opening_store_roots=roots(first-1),ending_store_roots=roots(last)) for first,last in ((1,200000),(200001,400000),(400001,500000))]

    def test_exact_three_profiles_all_nine_seams(self):
        p=self.profiles()
        self.assertEqual(check_profiles(p)['distinct_prefix_heights'],500000)
        for index,field in ((1,'opening_commit'),(1,'opening_store_roots'),(2,'ending_store_roots')):
            q=copy.deepcopy(p)
            if field=='opening_commit':q[index][field]['Hash']='changed'
            elif field=='opening_store_roots':q[index][field]['StoreInfos'][0]['Core']['CommitID']['Hash']='changed'
            else:q[index][field]['StoreInfos'].pop()
            with self.subTest(index=index,field=field):
                with self.assertRaises(ValueError):check_profiles(q)

    def test_random_native_store_info_order_is_not_a_financial_difference(self):
        p=self.profiles()
        for item in p:
            item['opening_store_roots']['StoreInfos'].reverse()
        self.assertEqual(check_profiles(p)['distinct_prefix_heights'],500000)
        for change in ('duplicate','version'):
            q=copy.deepcopy(p)
            info=q[1]['opening_store_roots']['StoreInfos']
            if change=='duplicate':info[0]=copy.deepcopy(info[1])
            else:info[0]['Core']['CommitID']['Version']-=1
            with self.assertRaises(ValueError):check_profiles(q)

    def test_no_bounded_gap_duplicate_or_tail_as_canonical(self):
        p=self.profiles()
        for index,field,value in ((0,'through',1000),(1,'first',200002),(2,'through',500015),(1,'source_whole_complete',False)):
            q=copy.deepcopy(p);q[index][field]=value
            with self.subTest(index=index,field=field):
                with self.assertRaises(ValueError):check_profiles(q)
        with self.assertRaises(ValueError):check_profiles(p+p[:1])
        with self.assertRaises(ValueError):check_profiles(p[1:])

if __name__=='__main__':unittest.main()
