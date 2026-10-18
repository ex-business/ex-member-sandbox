#!/usr/bin/env python3
# coding: utf-8
"""仅针对已启动的隔离本机预览环境，走会员来源页面接口到佣金冲正；不直接写业务数据库。"""
import argparse,json,secrets,time,urllib.request,urllib.parse,http.cookiejar,uuid
from pathlib import Path
p=argparse.ArgumentParser();p.add_argument('--dir',required=True);a=p.parse_args();root=Path(a.dir);cfg=json.loads((root/'api-runtime.json').read_text());membercfg=json.loads((root/'member.json').read_text());assert cfg['LOCAL_TENANT'].startswith('reversal-preview-') and cfg['HTTP_ADDR'].startswith('127.0.0.1:'),'拒绝非隔离环境'
api='http://'+cfg['HTTP_ADDR'];public=membercfg['PublicURL'].rstrip('/')
def backend(path,data=None,approver=False):
 raw=json.dumps(data).encode() if data is not None else None;headers={'Authorization':'Bearer '+cfg['LOCAL_APPROVER_TOKEN' if approver else 'LOCAL_OPERATOR_TOKEN'],'Content-Type':'application/json','Idempotency-Key':uuid.uuid4().hex}
 with urllib.request.urlopen(urllib.request.Request(api+path,data=raw,headers=headers),timeout=10) as r:return json.load(r).get('data')
if backend('/v1/affiliate/config')['revision']==0:
 condition={'deposit_minor':'100','valid_bet_minor':'100'};config={'enabled':True,'currency':'USD','currency_scale':2,'timezone':'UTC','grade_period':'DAY','grade_condition':condition,'referral_condition':condition,'invitee_condition':condition,'invitee_reward_minor':'50','levels':[{'level':0,'qualified_members':0,'daily_valid_bet_minor':'0','rates_ppm':[2500,1500,500]}],'steps':[{'step':1,'additional_members':1,'reward_minor':'50'}],'venue_type_ids':['SPORTS','LIVE']}
 request=backend('/v1/affiliate/config-requests',dict(config=config,expected_revision=0,reason='隔离冲正联调配置'))
 backend('/v1/approvals/'+request['approval_id']+'/decisions',dict(decision='APPROVE',expected_revision=0,object_hash=request['object_hash'],reason='独立审核联调配置'),True)
class Member:
 def __init__(self):self.opener=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
 def call(self,path,data=None):
  raw=json.dumps(data).encode() if data is not None else None
  with self.opener.open(urllib.request.Request(public+path,data=raw,headers={'Origin':public,'Content-Type':'application/json'}),timeout=10) as r:return json.load(r)
def wait(fn,description):
 for _ in range(100):
  value=fn()
  if value:return value
  time.sleep(.15)
 raise AssertionError('等待超时：'+description)
prefix='reversal-'+uuid.uuid4().hex[:10];password=secrets.token_urlsafe(24)
parent=Member();parent.call('/api/register',dict(email=prefix+'-parent@example.test',display_name='冲正演示推荐人',password=password));wait(lambda:parent.call('/api/me')['delivery_status']=='SUCCEEDED','推荐人注册投递')
invitation=parent.call('/api/invitation',{})['invitation_url'];code=urllib.parse.parse_qs(urllib.parse.urlparse(invitation).query)['ic'][0]
child=Member();child.call('/api/register',dict(email=prefix+'-child@example.test',display_name='冲正演示会员',password=password,invitation_code=code));wait(lambda:child.call('/api/me')['delivery_status']=='SUCCEEDED','受邀注册投递')
account=dict(email=prefix+'-child@example.test',password=password);(root/'account.json').write_text(json.dumps(account));(root/'account.json').chmod(0o600)
for business,kind,amount,valid,venue in [('deposit','DEPOSIT_SUCCEEDED','1.00','0',''),('bet','BET_SETTLED','1.00','1.00','SPORTS')]:child.call('/api/simulation/events',dict(business_id=prefix+'-'+business,kind=kind,amount=amount,valid_bet=valid,venue_type_id=venue))
def delivered(n):
 rows=child.call('/api/simulation/events')['items'];return len(rows)==n and all(r['state']=='DELIVERED' for r in rows)
wait(lambda:delivered(2),'充值投注投递')
initial=backend('/v1/affiliate/rewards')['rows'];assert sum(r['status']=='CLAIMABLE' for r in initial)>=2
child.call('/api/simulation/events',dict(business_id=prefix+'-refund',kind='DEPOSIT_REFUNDED',original_business_id=prefix+'-deposit',amount='1.00',valid_bet='0',reason='充值退款联调'))
wait(lambda:delivered(3),'退款冲正投递')
child.call('/api/simulation/events',dict(business_id=prefix+'-cancel',kind='BET_CANCELLED',original_business_id=prefix+'-bet',amount='1.00',valid_bet='1.00',reason='投注撤单联调'))
wait(lambda:delivered(4),'投注撤单投递')
records=backend('/v1/affiliate/reversals')['items'];assert len(records)>=2 and all(r['debt_added']=='0' for r in records)
final=backend('/v1/affiliate/rewards')['rows'];own=[r for r in final if r['reward_id'] in {i['reward_id'] for i in initial}];assert len(own)>=2 and all(r['status']=='CANCELLED' for r in own)
result={'result':'PASS','prefix':prefix,'checks':['邀请注册','充值投注自动投递与解锁','退款自动投递与冲正','投注撤单自动投递','未领奖励取消','冲正明细可查询'],'preview':public+'/simulation'};(root/'acceptance.json').write_text(json.dumps(result,ensure_ascii=False,indent=2));print(json.dumps(result,ensure_ascii=False))
