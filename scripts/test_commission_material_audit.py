import importlib.util
from pathlib import Path
import unittest
import json
import tempfile
from decimal import Decimal

spec = importlib.util.spec_from_file_location('material', Path(__file__).with_name('commission-material-audit.py'))
a = importlib.util.module_from_spec(spec)
spec.loader.exec_module(a)

class MaterialAuditTests(unittest.TestCase):
    def test_complete_coins_and_no_substrings(self):
        self.assertEqual(a.coins('12uatom,3ibc/ABC,4factory/a/b'), {'uatom': Decimal(12), 'ibc/ABC': Decimal(3), 'factory/a/b': Decimal(4)})
        with self.assertRaises(ValueError):
            a.coins('junk 12uatom')
        with self.assertRaises(ValueError):
            a.coins('1uatom,')

    def attrs(self, actor='wallet', duplicate=False):
        row = {'EventID': 1, 'MessageID': 2, 'Height': 3, 'Timestamp': '2026-11-01T00:00:00Z', 'MessageType': 'withdraw', 'EventType': 'coin_received'}
        result = [dict(row, Key='receiver', Value=actor, Index=0), dict(row, Key='amount', Value='7uatom,2uother', Index=1)]
        if duplicate:
            result.append(dict(row, Key='amount', Value='1uatom', Index=2))
        return result

    def test_native_exact_actor_amount_and_all_coins(self):
        cells, _ = a.native_cells(self.attrs(), 'wallet', 'message')
        self.assertEqual(cells[('2026-11-01', 'message', 2, 'uatom')], Decimal(7))
        self.assertEqual(cells[('2026-11-01', 'message', 2, 'uother')], Decimal(2))
        with self.assertRaises(ValueError):
            a.native_cells(self.attrs(actor='notwallet'), 'wallet', 'message')
        with self.assertRaises(ValueError):
            a.native_cells(self.attrs(duplicate=True), 'wallet', 'message')

    def event(self, category='transfer', direction='out', amount='7', **kw):
        e = {'Category': category, 'Dir': direction, 'Amount': amount, 'From': 'wallet', 'To': 'dest', 'Key': {'Time': '2026-11-01T00:00:00Z', 'Height': 3, 'Message': 2}}
        e.update(kw)
        return e

    def test_bank_projection_and_fees_use_utc_day_not_local_date_prefix(self):
        local = '2024-04-10T23:21:53.762831-04:00'
        self.assertEqual(a.utc_day(local), '2024-04-11')
        self.assertEqual(a.utc_day('2024-04-11T00:30:00+03:00'), '2024-04-10')
        rows = self.attrs()
        for row in rows:
            row['Timestamp'] = local
        cells, _ = a.native_cells(rows, 'wallet', 'message')
        self.assertIn(('2024-04-11', 'message', 2, 'uatom'), cells)
        event = self.event()
        event['Key']['Time'] = local
        projected, _ = a.flow_cash([event], 'wallet')
        self.assertIn(('2024-04-11', 'message', 2, 'uatom'), projected)

    def test_independent_liquid_projection(self):
        for category, delta in [('transfer', -7), ('delegate', -7), ('undelegate', 0), ('redelegate', 0), ('cancel_unbonding', 0), ('tokenize_shares', 0), ('redeem_shares', 0), ('unbonding_complete', 7), ('protocol_withdrawal', 7), ('gov_deposit_refund', 7)]:
            result, _ = a.flow_cash([self.event(category)], 'wallet')
            self.assertEqual(sum(result.values()), Decimal(delta), category)
        result, _ = a.flow_cash([self.event(To='wallet')], 'wallet')
        self.assertEqual(sum(result.values()), 0)

    def test_escrow_bid_and_settlement_not_double_spent(self):
        deposit = self.event('protocol_deposit', Trigger='auction_bid', Lot='nft')
        settlement = self.event('nft_sale', Lot='nft', Detail='{"sale":"auction","protocol":"Stargaze"}')
        result, _ = a.flow_cash([deposit, settlement], 'wallet')
        self.assertEqual(sum(result.values()), -7)
        result, _ = a.flow_cash([settlement], 'wallet')
        self.assertEqual(sum(result.values()), -7)

    def test_fee_not_message_cash_and_nft_direction_is_already_projected(self):
        result, _ = a.flow_cash([self.event('fee')], 'wallet')
        self.assertIn(('2026-11-01', 'fee', 0, 'uatom'), result)
        result, _ = a.flow_cash([self.event('nft_sale', 'in')], 'wallet')
        self.assertEqual(sum(result.values()), 7)

    def test_route_requires_loaded_destination_and_preserves_unbonded_month(self):
        with tempfile.TemporaryDirectory() as name:
            directory = Path(name)
            (directory / 'identity.private.json').write_text(json.dumps({'roots': ['root'], 'validator': 'validator'}))
            seed = self.event('commission', 'in', Validator='validator')
            seed['Key']['Time'] = '2026-03-01T00:00:00Z'
            (directory / (a.hashlib.sha256(b'root').hexdigest() + '.events.private.json')).write_text(json.dumps([seed]))
            edges = [
                {'error': '<nil>', 'destination_loaded': True, 'from': 'root', 'event': {'To': 'hop'}, 'parts': [[{'Origin': '2026-03', 'Amount': '3'}], []]},
                {'error': '<nil>', 'destination_loaded': False, 'from': 'hop', 'event': {'To': 'stopped'}, 'parts': [[{'Origin': '2026-03', 'Amount': '1'}], []]},
                {'error': '<nil>', 'destination_loaded': True, 'from': 'hop', 'event': {'To': 'leaf'}, 'parts': [[{'Origin': '2026-03+unbonded', 'Amount': '2'}], []]},
            ]
            for i, edge in enumerate(edges):
                edge['event']['Key'] = {'Time': '2026-03-02T00:00:00Z', 'Height': 4, 'Message': i+3, 'Sub': 0}
            (directory / 'tagged-routes.private.jsonl').write_text('\n'.join(json.dumps(x) for x in edges))
            result = a.route_paths(directory, {'leaf': 'leaf', 'stopped': 'stopped'})
            self.assertTrue(result['targets']['leaf']['member_proven'])
            self.assertEqual(result['targets']['leaf']['paths'][0]['edge_indexes'], [0, 2])
            self.assertFalse(result['targets']['stopped']['member_proven'])
            self.assertFalse(result['financially_accepted'])

    def test_missing_native_capture_is_a_completeness_finding(self):
        with tempfile.TemporaryDirectory() as name:
            root = Path(name)
            directory = root / 'validator-01'
            directory.mkdir()
            (directory / 'identity.private.json').write_text(json.dumps({'asof_exclusive': '2026-11-13T00:00:00Z'}))
            (directory / 'openings.private.jsonl').write_text(json.dumps({'Address': 'wallet'}))
            result = a.audit(root)
            self.assertIn('wallet_native_capture_pending_or_failed', [x['kind'] for x in result['findings']])
            self.assertIn('trace_still_running', [x['kind'] for x in result['findings']])
            self.assertFalse(result['financially_accepted'])

if __name__ == '__main__':
    unittest.main()
