import unittest
from check_all_account_coins import compare_accounts
class AllCoins(unittest.TestCase):
 def account(self,coins,address='synthetic-account'):
  return dict(address=address,coins=[dict(denom=d,amount=a) for d,a in coins])
 def test_all_denominations_and_zero_preserved(self):
  a=[self.account([('first','3'),('second','0')])]
  self.assertTrue(compare_accounts(a,a)['pass'])
  b=[self.account([('first','3'),('second','1')])]
  self.assertEqual(compare_accounts(a,b)['mismatched_coin_amounts'],1)
 def test_missing_extra_account_not_omitted(self):
  a=[self.account([])];b=[self.account([],address='other-synthetic-account')]
  r=compare_accounts(a,b);self.assertEqual(r['missing_accounts'],1);self.assertEqual(r['extra_accounts'],1);self.assertFalse(r['pass'])
 def test_absent_zero_coin_is_not_equal(self):
  a=[self.account([('zero','0')])];b=[self.account([])]
  self.assertEqual(compare_accounts(a,b)['extra_coins'],1)
  self.assertEqual(compare_accounts(b,a)['missing_coins'],1)
 def test_duplicate_account_or_denom_refused(self):
  a=self.account([('one','1')])
  for accounts in [[a,a],[self.account([('one','1'),('one','1')])]]:
   with self.assertRaises(ValueError):compare_accounts(accounts,accounts)
 def test_unsupported_numeric_value_refused(self):
  for amount in ['-1','1.0','nan',1]:
   a=[self.account([('one',amount)])]
   with self.assertRaises(ValueError):compare_accounts(a,a)
if __name__=='__main__':unittest.main()
