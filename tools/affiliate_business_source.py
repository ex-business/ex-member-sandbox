#!/usr/bin/env python3
"""独立模拟来源：可靠投递成功充值/提现、已结算业务量与日结清单；不修改会员钱包或原注册表。"""
import argparse, datetime as dt, hashlib, hmac, ipaddress, json, os, re, secrets, sqlite3, sys, urllib.request, urllib.error, urllib.parse
from pathlib import Path
from zoneinfo import ZoneInfo

os.umask(0o077)
ROOT=Path(__file__).resolve().parents[1]
def dump(v): return json.dumps(v,ensure_ascii=False,separators=(',',':'))
def gojson(v):
    return dump(v).replace('&','\\u0026').replace('<','\\u003c').replace('>','\\u003e').replace('\u2028','\\u2028').replace('\u2029','\\u2029')
def digest(parts):return hashlib.sha256(gojson(parts).encode()).hexdigest()
def instant(value):
    if not re.fullmatch(r'\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,6})?(?:Z|[+-]\d{2}:\d{2})',value):raise ValueError('业务时间必须是微秒精度的完整ISO时间')
    value=re.sub(r'(\.\d{1,6})(?=Z|[+-]\d{2}:\d{2}$)',lambda m:m.group(1).ljust(7,'0'),value)
    parsed=dt.datetime.fromisoformat(value.replace('Z','+00:00'))
    if parsed.tzinfo is None:raise ValueError('业务时间必须包含时区')
    return parsed.astimezone(dt.timezone.utc)
def rfc(value):
    stamp=value.astimezone(dt.timezone.utc)
    fraction=('%06d'%stamp.microsecond).rstrip('0')
    return stamp.strftime('%Y-%m-%dT%H:%M:%S')+('.'+fraction if fraction else '')+'Z'
def business_hash(e):
    parts=[e['source_id'],e['kind'],e['business_id'],e['external_user_id'],e['currency'],e['amount_minor'],e['valid_bet_minor'],e['venue_type_id'],rfc(instant(e['business_time']))]
    if 'risk_identity' in e:
        r=e['risk_identity'];parts+=['risk-v1',r.get('ip_hash',''),r.get('device_hash',''),r.get('account_hash','')]
    return digest(parts)
def money(v,positive=False):
    if not isinstance(v,str) or not v.isascii() or not v.isdigit() or len(v)>38 or (len(v)>1 and v[0]=='0') or (positive and int(v)==0):raise ValueError('金额必须为最多38位的最小货币单位整数字符串')
    return v

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self,req,fp,code,msg,headers,newurl):return None

def request(c,path,body=None,prefix=None):
    target=urllib.parse.urlsplit(c['url'])
    try:local=ipaddress.ip_address(target.hostname).is_loopback
    except ValueError:local=False
    if target.username or target.password or target.query or target.fragment or target.path or (target.scheme!='https' and not(target.scheme=='http' and local)):raise ValueError('来源上游仅支持HTTPS或明确本地回环地址')
    raw=None if body is None else dump(body).encode()
    headers={'Authorization':'Bearer '+c['token'],'Content-Type':'application/json','Idempotency-Key':secrets.token_hex(24)}
    if prefix:headers['X-Affiliate-Signature']=hmac.new(c['key'].encode(),(prefix+'\n'+c['tenant']+'\n'+c['app']+'\n'+c['source']+'\n').encode()+raw,hashlib.sha256).hexdigest()
    req=urllib.request.Request(c['url']+path,data=raw,headers=headers)
    try:
        with urllib.request.build_opener(NoRedirect()).open(req,timeout=15) as response:
            raw=response.read(1048577)
            if len(raw)>1048576:raise ValueError("来源响应过大")
            return response.status,json.loads(raw)
    except urllib.error.HTTPError as e:
        try:body=json.loads(e.read())
        except Exception:body={}
        return e.code,body

def connect(c):
    db=sqlite3.connect(c['database']);db.row_factory=sqlite3.Row
    db.executescript('''PRAGMA journal_mode=WAL;
    CREATE TABLE IF NOT EXISTS meta(fingerprint TEXT NOT NULL);
    CREATE TABLE IF NOT EXISTS facts(kind TEXT NOT NULL,business_id TEXT NOT NULL,payload TEXT NOT NULL,state TEXT NOT NULL,attempts INTEGER NOT NULL DEFAULT 0,last_code TEXT NOT NULL DEFAULT '',PRIMARY KEY(kind,business_id));
    CREATE TABLE IF NOT EXISTS watermarks(day TEXT PRIMARY KEY,payload TEXT NOT NULL,state TEXT NOT NULL);''')
    fp=digest([c['tenant'],c['app'],c['source'],c['member_database']]);old=db.execute('SELECT fingerprint FROM meta').fetchone()
    if old and old[0]!=fp:raise ValueError('来源作用域已变化，请建立独立来源库，禁止复用旧outbox')
    if not old:db.execute('INSERT INTO meta VALUES(?)',(fp,));db.commit()
    return db

def deliver(c,db,retry_blocked=False):
    states=['PENDING']+(['BLOCKED'] if retry_blocked else [])
    rows=db.execute('SELECT * FROM facts WHERE state IN ('+','.join('?' for _ in states)+') ORDER BY rowid',states).fetchall()
    for row in rows:
        e=json.loads(row['payload'])
        try:status,body=request(c,'/v1/affiliate/business-facts',e,'affiliate-business-v1')
        except (OSError,ValueError):
            db.execute("UPDATE facts SET attempts=attempts+1,last_code='DEPENDENCY_UNAVAILABLE' WHERE kind=? AND business_id=?",(row['kind'],row['business_id']));db.commit();continue
        state='PENDING';code=str(status)
        if status==200:
            data=body.get('data',{})
            if data.get('business_id')==e['business_id'] and data.get('event_id')==e['event_id']:
                state='LATE_ADJUSTMENT' if data.get('late_adjustment') else 'DELIVERED'
            else:code='INVALID_UPSTREAM_RESPONSE'
        elif 400<=status<500 and status not in [408,429]:state='BLOCKED';code=body.get('error',{}).get('code',str(status))
        db.execute('UPDATE facts SET state=?,attempts=attempts+1,last_code=? WHERE kind=? AND business_id=?',(state,code,row['kind'],row['business_id']));db.commit()

def close_business_day(c,db,day,zone):
    """仅在全部事实已确认后提交不可变清单；失败保留本地状态供重试。"""
    # 关账与CLI写入共享SQLite写锁，清单读取至远端确认之间禁止插入未确认事实。
    # 提交后的新事实由后端按迟到调整处理；网络失败则回滚，允许原日重试。
    db.execute('BEGIN IMMEDIATE')
    try:
        date=dt.date.fromisoformat(day);parts=[]
        for row in db.execute('SELECT * FROM facts'):
            e=json.loads(row['payload'])
            if instant(e['business_time']).astimezone(zone).date()!=date:continue
            if row['state'] not in ['DELIVERED','LATE_ADJUSTMENT']:raise ValueError('该日仍有未送达或被拒绝事实，禁止宣布来源完整')
            if row['state']=='DELIVERED':parts.append(e['kind']+'\n'+e['business_id']+'\n'+business_hash(e))
        parts.sort();watermark=dict(source_id=c['source'],business_day=day,fact_count=len(parts),facts_digest=digest(parts))
        old=db.execute('SELECT payload FROM watermarks WHERE day=?',(day,)).fetchone()
        if old and json.loads(old[0])!=watermark:raise ValueError('已提交来源日结清单不可改写')
        status,body=request(c,'/v1/affiliate/source-watermarks',watermark,'affiliate-watermark-v1')
        if status!=200:raise ValueError('来源日结未被接受：'+body.get('error',{}).get('code',str(status)))
        db.execute("INSERT INTO watermarks VALUES(?,?,'SOURCE_CLOSED') ON CONFLICT(day) DO NOTHING",(day,dump(watermark)));db.commit();print(dump({'来源':c['source'],'业务日':day,'事实数':len(parts),'处理结果':'来源关账成功，等待日终结算'}),flush=True)
    except Exception:
        db.rollback()
        raise


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--config',default=str(ROOT/'.local/affiliate-source.json'))
    sub=parser.add_subparsers(dest='command',required=True)
    init=sub.add_parser('init');init.add_argument('--commission-config',required=True);init.add_argument('--member-config',required=True)
    record=sub.add_parser('record');record.add_argument('--member-id',required=True);record.add_argument('--business-id',required=True);record.add_argument('--kind',choices=['DEPOSIT_SUCCEEDED','WITHDRAWAL_SUCCEEDED','BET_SETTLED'],required=True);record.add_argument('--amount-minor',required=True);record.add_argument('--valid-bet-minor',default='0');record.add_argument('--venue',default='');record.add_argument('--at');record.add_argument('--ip');record.add_argument('--device-id');record.add_argument('--shared-account')
    send=sub.add_parser('deliver');send.add_argument('--retry-blocked',action='store_true')
    close=sub.add_parser('close-day');close.add_argument('--day',required=True)
    sub.add_parser('status')
    args=parser.parse_args(argv);path=Path(args.config)
    if args.command=='init':
        back=json.loads(Path(args.commission_config).read_text());member=json.loads(Path(args.member_config).read_text())
        if back.get('LOCAL_AFFILIATE_FLOW_ENABLED')!='true' or len(back.get('LOCAL_AFFILIATE_FLOW_KEY',''))<32:raise ValueError('佣金独立代理业务模块尚未启用')
        if member['Tenant']!=back['LOCAL_TENANT'] or member['App']!=back['LOCAL_APP']:raise ValueError('会员与佣金作用域不一致')
        c=dict(url=member['CommissionURL'],token=back['LOCAL_OPERATOR_TOKEN'],key=back['LOCAL_AFFILIATE_FLOW_KEY'],tenant=member['Tenant'],app=member['App'],source=member['Source'],member_database=member['Database'],database=str(path.with_suffix('.db')))
        path.parent.mkdir(parents=True,exist_ok=True,mode=0o700);path.write_text(json.dumps(c,ensure_ascii=False,indent=2));path.chmod(0o600)
        with connect(c):pass
        print('独立代理事实来源已初始化，凭据仅保存在私有配置。');return
    c=json.loads(path.read_text())
    with connect(c) as db:
        if args.command=='record':
            if not args.business_id or len(args.business_id)>128:raise ValueError('业务订单号无效')
            money(args.amount_minor,True);money(args.valid_bet_minor)
            if args.kind=='BET_SETTLED':
                if not args.venue or int(args.valid_bet_minor)>int(args.amount_minor):raise ValueError('已结算业务必须提供场馆及不超过订单金额的有效量')
            elif args.valid_bet_minor!='0' or args.venue:raise ValueError('成功充值/提现不含场馆或有效业务量')
            with sqlite3.connect(Path(c['member_database']).resolve().as_uri()+'?mode=ro',uri=True) as members:
                found=members.execute("SELECT 1 FROM members m JOIN registration_outbox o ON o.member_id=m.id WHERE m.id=? AND o.status='SUCCEEDED'",(args.member_id,)).fetchone()
            if not found:raise ValueError('仅能为已确认注册同步的真实沙箱会员创建模拟事实')
            status,profile=request(c,'/v1/affiliate/config')
            if status!=200 or not profile.get('data',{}).get('current'):raise ValueError('请先通过独立审批发布站点推荐返佣配置')
            currency=profile['data']['current']['config']['currency']
            old=db.execute('SELECT * FROM facts WHERE kind=? AND business_id=?',(args.kind,args.business_id)).fetchone()
            e=dict(source_id=c['source'],event_id=secrets.token_hex(24),business_id=args.business_id,external_user_id=args.member_id,kind=args.kind,currency=currency,amount_minor=args.amount_minor,valid_bet_minor=args.valid_bet_minor,venue_type_id=args.venue,business_time=rfc(instant(args.at)) if args.at else rfc(dt.datetime.now(dt.timezone.utc)))
            # 模拟受信来源生成作用域隔离摘要；不保存原始IP、设备或共享账户值。
            identity={}
            for field,value in [('ip_hash',args.ip),('device_hash',args.device_id),('account_hash',args.shared_account)]:
                if value:
                    if field=='ip_hash':value=str(ipaddress.ip_address(value))
                    if len(value)>512:raise ValueError('识别标识过长')
                    identity[field]=digest([c['tenant'],c['app'],field,value])
            if identity:e['risk_identity']=identity
            if old:
                previous=json.loads(old['payload']);e['event_id']=previous['event_id']
                if not args.at:e['business_time']=previous['business_time']
                if e!=previous:raise ValueError('同一业务订单不可覆盖会员、金额、场馆或业务时间')
            else:db.execute('INSERT INTO facts(kind,business_id,payload,state) VALUES(?,?,?,?)',(args.kind,args.business_id,dump(e),'PENDING'));db.commit()
            deliver(c,db)
        elif args.command=='deliver':deliver(c,db,args.retry_blocked)
        elif args.command=='close-day':
            status,profile=request(c,'/v1/affiliate/config')
            if status!=200 or not profile.get('data',{}).get('current'):raise ValueError('站点尚无生效配置')
            close_business_day(c,db,args.day,ZoneInfo(profile['data']['current']['config']['timezone']))
        states={row[0]:row[1] for row in db.execute('SELECT state,COUNT(*) FROM facts GROUP BY state')};print(json.dumps(states,ensure_ascii=False))
if __name__=='__main__':
    try:main()
    except (ValueError,OSError,sqlite3.Error) as e:print('操作未完成：'+str(e),file=sys.stderr);sys.exit(1)
