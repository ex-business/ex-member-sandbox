import contextlib, importlib.util, io, json, sqlite3, tempfile, unittest, urllib.error
from pathlib import Path
spec=importlib.util.spec_from_file_location('affiliate_source',Path(__file__).with_name('affiliate_business_source.py'));source=importlib.util.module_from_spec(spec);spec.loader.exec_module(source)

class BusinessSource(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.root=Path(self.temp.name);members=self.root/'members.db'
        with sqlite3.connect(members) as db:db.executescript("CREATE TABLE members(id TEXT);CREATE TABLE registration_outbox(member_id TEXT,status TEXT);INSERT INTO members VALUES('member');INSERT INTO registration_outbox VALUES('member','SUCCEEDED');")
        self.cfg=dict(url='http://127.0.0.1:18086',token='private-test-token',key='k'*32,tenant='tenant',app='app',source='sandbox',member_database=str(members),database=str(self.root/'facts.db'))
        self.path=self.root/'source.json';self.path.write_text(json.dumps(self.cfg));self.previous=source.request;self.calls=[]
        def request(c,path,body=None,prefix=None):
            self.calls.append((path,body,prefix))
            if path=='/v1/affiliate/config':return 200,{'data':{'current':{'config':{'currency':'USD','timezone':'UTC'}}}}
            if path=='/v1/affiliate/business-facts':return 200,{'data':{'business_id':body['business_id'],'event_id':body['event_id'],'accepted':True,'late_adjustment':False}}
            return 200,{'data':{}}
        source.request=request
    def tearDown(self):source.request=self.previous;self.temp.cleanup()
    def run_tool(self,*args):
        with contextlib.redirect_stdout(io.StringIO()):source.main(['--config',str(self.path),*args])
    def test_order_is_immutable_and_retry_keeps_original_event_and_time(self):
        args=['record','--member-id','member','--business-id','deposit1','--kind','DEPOSIT_SUCCEEDED','--amount-minor','100']
        successful=source.request
        def unavailable(c,path,body=None,prefix=None):
            if path=='/v1/affiliate/business-facts':raise urllib.error.URLError('temporary')
            return successful(c,path,body,prefix)
        source.request=unavailable;self.run_tool(*args)
        with source.connect(self.cfg) as db:old=dict(db.execute('SELECT * FROM facts').fetchone());self.assertEqual(old['state'],'PENDING')
        source.request=successful;self.run_tool(*args)
        with source.connect(self.cfg) as db:new=dict(db.execute('SELECT * FROM facts').fetchone());self.assertEqual(new['state'],'DELIVERED');self.assertEqual(old['payload'],new['payload']);self.assertEqual(db.execute('SELECT COUNT(*) FROM facts').fetchone()[0],1)
        with self.assertRaisesRegex(ValueError,'不可覆盖'):self.run_tool(*args[:-1],'200')
    def test_risk_identity_is_hashed_and_bound_to_immutable_order(self):
        args=['record','--member-id','member','--business-id','risk1','--kind','BET_SETTLED','--amount-minor','1000','--valid-bet-minor','1000','--venue','SPORTS','--ip','203.0.113.9','--device-id','private-device','--shared-account','private-account']
        self.run_tool(*args)
        with source.connect(self.cfg) as db:
            raw=db.execute('SELECT payload FROM facts').fetchone()[0];event=json.loads(raw)
        self.assertNotIn('203.0.113.9',raw);self.assertNotIn('private-device',raw);self.assertNotIn('private-account',raw)
        self.assertEqual(set(event['risk_identity']),{'ip_hash','device_hash','account_hash'})
        self.assertTrue(all(len(v)==64 for v in event['risk_identity'].values()))
        baseline=source.business_hash(event);event['risk_identity']['device_hash']='f'*64
        self.assertNotEqual(baseline,source.business_hash(event))
        with self.assertRaisesRegex(ValueError,'不可覆盖'):self.run_tool(*args[:-1],'other-account')
    def test_unknown_member_and_invalid_money_rejected(self):
        with self.assertRaisesRegex(ValueError,'真实沙箱会员'):self.run_tool('record','--member-id','unknown','--business-id','x','--kind','DEPOSIT_SUCCEEDED','--amount-minor','100')
        for v in ['-1','1.5','01','1e3','9'*39]:
            with self.subTest(v=v),self.assertRaises(ValueError):source.money(v,True)
    def test_pending_fact_blocks_source_watermark(self):
        with source.connect(self.cfg) as db:
            event=dict(source_id='sandbox',event_id='e',business_id='b',external_user_id='member',kind='DEPOSIT_SUCCEEDED',currency='USD',amount_minor='100',valid_bet_minor='0',venue_type_id='',business_time='2026-10-18T12:00:00Z')
            db.execute("INSERT INTO facts(kind,business_id,payload,state)VALUES(?,?,?,'PENDING')",('DEPOSIT_SUCCEEDED','b',source.dump(event)));db.commit()
        with self.assertRaisesRegex(ValueError,'禁止宣布来源完整'):self.run_tool('close-day','--day','2026-10-18')
    def test_scope_change_does_not_reuse_old_outbox(self):
        with source.connect(self.cfg):pass
        self.cfg['tenant']='another'
        with self.assertRaisesRegex(ValueError,'来源作用域已变化'):source.connect(self.cfg)
    def test_canonical_business_hash_ignores_delivery_id_and_timestamp_zeroes(self):
        e=dict(source_id='sandbox',event_id='delivery1',business_id='dep-001',external_user_id='member-1',kind='DEPOSIT_SUCCEEDED',currency='USD',amount_minor='100',valid_bet_minor='0',venue_type_id='',business_time='2026-10-18T12:00:00.100000Z')
        first=source.business_hash(e);e['event_id']='delivery2';e['business_time']='2026-10-18T12:00:00.1Z';self.assertEqual(first,source.business_hash(e))
        self.assertEqual(source.rfc(source.instant('2026-10-18T12:00:00.000001Z')),'2026-10-18T12:00:00.000001Z')
        self.assertEqual(source.rfc(source.instant('2026-10-18T12:00:00Z')),'2026-10-18T12:00:00Z')
if __name__=='__main__':unittest.main()
