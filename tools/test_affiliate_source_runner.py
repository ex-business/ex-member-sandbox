import datetime as dt
import json
import tempfile
import sqlite3
import unittest
from pathlib import Path
from unittest.mock import patch
import affiliate_business_source as source
import affiliate_source_runner as runner


class RunnerTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.config = dict(database=str(Path(self.temp.name)/'source.db'), tenant='t', app='a', source='sandbox', member_database='members')
        self.db = source.connect(self.config)
        self.zone = 'UTC'
        self.fail = False
        self.calls = []
        def request(c, path, body=None, prefix=None):
            if path.endswith('/config'):
                return 200, {'data': {'current': {'config': {'timezone': self.zone}}}}
            self.calls.append(body['business_day'])
            return (503, {}) if self.fail else (200, {})
        self.mock = patch.object(source, 'request', request)
        self.mock.start()
    def tearDown(self):
        self.mock.stop()
        self.db.close()
        self.temp.cleanup()
    def at(self, value):
        return source.instant(value)
    def test_empty_days_restart_and_bounded_catchup(self):
        runner.cycle(self.config, self.db, self.at('2026-10-17T23:59:59Z'))
        self.assertEqual(self.calls, [])
        self.assertEqual(runner.cycle(self.config, self.db, self.at('2026-10-20T00:00:00Z'), 1), ['2026-10-17'])
        self.db.close(); self.db = source.connect(self.config)
        self.assertEqual(runner.cycle(self.config, self.db, self.at('2026-10-20T00:00:00Z')), ['2026-10-18', '2026-10-19'])
        runner.cycle(self.config, self.db, self.at('2026-10-20T01:00:00Z'))
        self.assertEqual(self.calls, ['2026-10-17', '2026-10-18', '2026-10-19'])
    def test_failure_does_not_advance_cursor(self):
        runner.cycle(self.config, self.db, self.at('2026-10-17T12:00:00Z'))
        self.fail = True
        with self.assertRaises(ValueError):
            runner.cycle(self.config, self.db, self.at('2026-10-18T00:00:00Z'))
        self.assertEqual(self.db.execute('SELECT next_day FROM runner_state').fetchone()[0], '2026-10-17')
        self.fail = False
        self.assertEqual(runner.cycle(self.config, self.db, self.at('2026-10-18T00:00:00Z')), ['2026-10-17'])
    def test_blocked_fact_prevents_closure_and_following_days(self):
        event = dict(business_time='2026-10-17T12:00:00Z')
        self.db.execute("INSERT INTO facts(kind,business_id,payload,state) VALUES('BET_SETTLED','b',?,'BLOCKED')", (json.dumps(event),));self.db.commit()
        with self.assertRaisesRegex(ValueError, '禁止宣布'):
            runner.cycle(self.config, self.db, self.at('2026-10-19T00:00:00Z'))
        self.assertEqual(self.calls, [])
    def test_close_holds_write_lock_until_upstream_confirmation(self):
        runner.cycle(self.config, self.db, self.at('2026-10-17T12:00:00Z'))
        original = source.request
        def concurrent_write(config, path, body=None, prefix=None):
            if path.endswith('source-watermarks'):
                # 模拟另一个CLI进程在水位请求期间写入事实，必须被SQLite写锁阻止。
                other = sqlite3.connect(config['database'], timeout=0)
                try:
                    with self.assertRaisesRegex(sqlite3.OperationalError, 'locked'):
                        other.execute("INSERT INTO facts(kind,business_id,payload,state) VALUES('BET_SETTLED','concurrent','{}','PENDING')")
                finally:
                    other.close()
            return original(config, path, body, prefix)
        with patch.object(source, 'request', concurrent_write):
            runner.cycle(self.config, self.db, self.at('2026-10-18T00:00:00Z'))
        other = sqlite3.connect(self.config['database'], timeout=0)
        other.execute("INSERT INTO facts(kind,business_id,payload,state) VALUES('BET_SETTLED','after-close','{}','PENDING')")
        other.commit();other.close()

    def test_dst_uses_local_day_and_rejects_timezone_change(self):
        self.zone = 'America/New_York'
        runner.cycle(self.config, self.db, self.at('2026-11-01T03:59:59Z'))
        self.assertEqual(runner.cycle(self.config, self.db, self.at('2026-11-01T04:00:00Z')), ['2026-10-31'])
        self.assertEqual(runner.cycle(self.config, self.db, self.at('2026-11-02T04:59:59Z')), [])
        self.assertEqual(runner.cycle(self.config, self.db, self.at('2026-11-02T05:00:00Z')), ['2026-11-01'])
        self.zone = 'UTC'
        with self.assertRaisesRegex(ValueError, '时区已变化'):
            runner.cycle(self.config, self.db, self.at('2026-11-03T05:00:00Z'))


if __name__ == '__main__':
    unittest.main()
