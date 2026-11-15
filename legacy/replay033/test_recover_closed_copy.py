import gzip, hashlib, json, subprocess, tempfile, unittest
from pathlib import Path
from unittest.mock import patch
import recover_closed_copy as r
class Reader:
 base=['synthetic']
 def __init__(self,size):self.size=size
 def command(self,args):return str(self.size).encode()
 def args(self,args):return args
class Recovery(unittest.TestCase):
 scanner=Path('/Users/danb/.cache/cosmos-tax-hub1-recording-recovery-20261115/scan-closed')
 def case(self,corrupt=False,short=False,existing=False):
  with tempfile.TemporaryDirectory() as tmp:
   root=Path(tmp);dest=root/'new';dest.mkdir();data=gzip.compress(b'{"kind":"history_completed","height":42}\n',mtime=0)
   prefix=root/'failed.gz';prefix.write_bytes(data[:10] if not corrupt else b'wrongbytes')
   if existing:(dest/'native-history.source-closed.gz').write_bytes(b'older')
   calls=[];real=subprocess.run
   def run(args,**kw):
    if args[0]=='synthetic':
     calls.append(args);kw['stdout'].write(data[10:-1] if short else data[10:]);return subprocess.CompletedProcess(args,0)
    return real(args,**kw)
   old=prefix.read_bytes();remote=r.bounded.ROOT+'/native-history.ndjson.gz'
   with patch.object(r.subprocess,'run',side_effect=run):
    try:
     result=r.reconstruct(dest,'native-history',42,'history_completed',Reader(len(data)),{remote:hashlib.sha256(data).hexdigest()},prefix,self.scanner)
     self.assertEqual(result['records'],1)
    finally:self.assertEqual(prefix.read_bytes(),old)
   self.assertIn('skip=10',calls[0])
 def test_exact_prefix_plus_suffix(self):self.case()
 def test_corrupt_prefix_never_accepted(self):
  with self.assertRaises(r.closed.ClosedStreamError):self.case(corrupt=True)
 def test_zero_exit_short_tail_never_accepted(self):
  with self.assertRaises(r.closed.ClosedStreamError):self.case(short=True)
 def test_existing_attempt_never_replaced(self):
  with self.assertRaises(FileExistsError):self.case(existing=True)
if __name__=='__main__':unittest.main()
