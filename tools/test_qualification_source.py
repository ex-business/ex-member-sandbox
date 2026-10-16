"""事实适配器的幂等与断线恢复测试，不连接真实会员或佣金系统。"""
from contextlib import redirect_stdout
import importlib.util
import io
import json
from pathlib import Path
import sqlite3
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('qualification_source', Path(__file__).with_name('qualification_source.py'))
source = importlib.util.module_from_spec(spec)
spec.loader.exec_module(source)


class QualificationSourceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        member_db = root / 'members.db'
        with sqlite3.connect(member_db) as db:
            db.execute('CREATE TABLE members(id TEXT PRIMARY KEY)')
            db.execute("INSERT INTO members VALUES('member-one')")
        self.config = {'URL': 'http://127.0.0.1:18084', 'Token': 'test-token', 'Key': 'q' * 32, 'Tenant': 'test', 'App': 'app', 'Source': 'sandbox', 'Policy': 'test-policy', 'Currency': 'USD', 'MemberDatabase': str(member_db), 'Database': str(root / 'source.db')}
        self.file = root / 'config.json'
        self.file.write_text(json.dumps(self.config))

    def run_tool(self, args):
        with patch.object(sys, 'argv', ['qualification_source.py', '--config', str(self.file)] + args), redirect_stdout(io.StringIO()):
            source.main()

    def record(self, **changes):
        args = {'--member-id': 'member-one', '--business-id': 'deposit-one', '--type': 'DEPOSIT_SUCCEEDED', '--amount-minor': '5000'}
        args.update(changes)
        self.run_tool(['record'] + [value for pair in args.items() for value in pair])

    def response(self, config, path, raw=None, event_id=None):
        fact = json.loads(raw)
        self.assertEqual(event_id, fact['event_id'])
        return 200, {'data': {'external_user_id': fact['external_user_id'], 'policy_id': config['Policy'], 'source_id': config['Source'], 'currency': config['Currency']}}

    def test_failed_delivery_persists_original_and_retries(self):
        with patch.object(source, 'request', return_value=(503, {'error_code': 'DEPENDENCY_UNAVAILABLE'})):
            self.record()
        with sqlite3.connect(self.config['Database']) as db:
            before = db.execute('SELECT event_id,payload,status FROM facts').fetchone()
            self.assertEqual(before[2], 'PENDING')
        with patch.object(source, 'request', side_effect=self.response):
            self.run_tool(['deliver'])
            self.record()
        with sqlite3.connect(self.config['Database']) as db:
            after = db.execute('SELECT event_id,payload,status FROM facts').fetchone()
            self.assertEqual(before[:2], after[:2])
            self.assertEqual(after[2], 'DELIVERED')
            self.assertEqual(db.execute('SELECT COUNT(*) FROM facts').fetchone()[0], 1)

    def test_different_fact_cannot_overwrite_business(self):
        with patch.object(source, 'request', side_effect=self.response):
            self.record()
            with self.assertRaises(SystemExit):
                self.record(**{'--amount-minor': '6000'})

    def test_unknown_member_not_manufactured(self):
        with patch.object(source, 'request') as request:
            with self.assertRaises(SystemExit):
                self.record(**{'--member-id': 'missing-member'})
            request.assert_not_called()

    def test_rejected_fact_retained_for_explicit_retry(self):
        with patch.object(source, 'request', return_value=(404, {'error_code': 'NOT_FOUND'})):
            self.record()
        with patch.object(source, 'request', side_effect=self.response) as request:
            self.run_tool(['deliver'])
            request.assert_not_called()
            self.run_tool(['deliver', '--retry-blocked'])
            self.assertEqual(request.call_count, 1)

    def test_other_scope_does_not_deliver_existing_facts(self):
        with patch.object(source, 'request', return_value=(503, {'error_code': 'DEPENDENCY_UNAVAILABLE'})):
            self.record()
        self.config['Tenant'] = 'another-site'
        self.file.write_text(json.dumps(self.config))
        with patch.object(source, 'request') as request:
            self.run_tool(['deliver'])
            request.assert_not_called()


if __name__ == '__main__':
    unittest.main()
