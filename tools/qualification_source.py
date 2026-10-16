"""有效会员的独立本地事实适配器：只生成合成成功订单，不操作钱包或改会员服务表结构。"""
import argparse
from datetime import datetime, timezone
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import sqlite3
import urllib.error
import urllib.parse
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # 服务凭据不跟随上游跳转，防止错误配置把签名或身份发往另一目的地。
        return None


def load(path):
    return json.loads(Path(path).expanduser().read_text())


def request(config, path, raw=None, event_id=None):
    headers = {'Authorization': 'Bearer ' + config['Token']}
    if raw is not None:
        headers.update({'Content-Type': 'application/json', 'Idempotency-Key': event_id})
        if path == '/v1/member-qualification-facts':
            prefix = 'qualification-v1\n{Tenant}\n{App}\n{Source}\n'.format(**config).encode()
            headers['X-Qualification-Signature'] = hmac.new(config['Key'].encode(), prefix + raw, hashlib.sha256).hexdigest()
    req = urllib.request.Request(config['URL'] + path, data=raw, headers=headers)
    try:
        with urllib.request.build_opener(NoRedirect()).open(req, timeout=10) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        # 只保留业务错误码，不把响应载荷或凭据输出到日志。
        try:
            code = json.loads(error.read()).get('error', {}).get('code', 'UPSTREAM_REJECTED')
        except (ValueError, AttributeError):
            code = 'INVALID_UPSTREAM_RESPONSE'
        return error.code, {'error_code': code}
    except (urllib.error.URLError, TimeoutError, ValueError):
        return 503, {'error_code': 'DEPENDENCY_UNAVAILABLE'}


def database(config):
    db = sqlite3.connect(config['Database'], timeout=10)
    # 独立事实库，绝不迁移或写入会员数据库；注册模块和原outbox完全不变。
    db.execute('CREATE TABLE IF NOT EXISTS facts(event_id TEXT PRIMARY KEY, scope_hash TEXT NOT NULL, kind TEXT NOT NULL, business_id TEXT NOT NULL, fingerprint TEXT NOT NULL, payload BLOB NOT NULL, status TEXT NOT NULL, result_code TEXT NOT NULL, UNIQUE(scope_hash,kind,business_id))')
    return db


def scope_hash(config):
    values = [config[key] for key in ('Tenant', 'App', 'Source', 'Policy', 'Currency')]
    return hashlib.sha256(json.dumps(values).encode()).hexdigest()


def deliver(config, db, only=None, retry_blocked=False):
    states = ('PENDING', 'BLOCKED') if retry_blocked else ('PENDING',)
    rows = db.execute('SELECT event_id,payload FROM facts WHERE scope_hash=? AND status IN (%s)' % ','.join('?' for _ in states), (scope_hash(config),) + states).fetchall()
    delivered = 0
    for event_id, raw in rows:
        if only and event_id != only:
            continue
        # 原载荷和事件号持久保存，重试不创建新业务，也不补写另一邀请码。
        status, result = request(config, '/v1/member-qualification-facts', raw, event_id)
        state = 'PENDING'
        code = result.get('error_code', 'INVALID_UPSTREAM_RESPONSE')
        if status == 200:
            expected = json.loads(raw)
            data = result.get('data', {})
            if data.get('external_user_id') == expected['external_user_id'] and data.get('policy_id') == config['Policy'] and data.get('source_id') == config['Source'] and data.get('currency') == config['Currency']:
                state, code = 'DELIVERED', ''
                delivered += 1
        elif 400 <= status < 500 and status not in (408, 429):
            state = 'BLOCKED'
        with db:
            db.execute('UPDATE facts SET status=?,result_code=? WHERE event_id=?', (state, code, event_id))
        print(json.dumps({'event_id': event_id, 'delivery_status': state, 'result_code': code}, ensure_ascii=False))
    return delivered


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', default=str(ROOT / '.local/qualification-source.json'))
    commands = parser.add_subparsers(dest='command', required=True)
    init = commands.add_parser('init')
    init.add_argument('--commission-config', required=True)
    init.add_argument('--member-config', required=True)
    init.add_argument('--policy-id', default='demo-effective-v1')
    init.add_argument('--currency', default='USD')
    policy = commands.add_parser('create-policy')
    policy.add_argument('--deposit-minor', required=True)
    policy.add_argument('--valid-bet-minor', required=True)
    policy.add_argument('--venue', action='append', required=True)
    record = commands.add_parser('record')
    record.add_argument('--member-id', required=True)
    record.add_argument('--business-id', required=True)
    record.add_argument('--type', choices=['DEPOSIT_SUCCEEDED', 'BET_SETTLED'], required=True)
    record.add_argument('--amount-minor', required=True)
    record.add_argument('--venue')
    record.add_argument('--business-time')
    retry = commands.add_parser('deliver')
    retry.add_argument('--retry-blocked', action='store_true')
    query = commands.add_parser('query')
    query.add_argument('--member-id', required=True)
    args = parser.parse_args()
    if args.command == 'init':
        upstream, member = load(args.commission_config), load(args.member_config)
        if upstream.get('LOCAL_QUALIFICATION_ENABLED') != 'true':
            raise SystemExit('上游未启用有效会员模块')
        if (member['Tenant'], member['App'], member['Source']) != (upstream['LOCAL_TENANT'], upstream['LOCAL_APP'], 'sandbox'):
            raise SystemExit('会员来源与上游作用域不一致，拒绝生成配置')
        config = {'URL': 'http://' + upstream['HTTP_ADDR'], 'Token': upstream['LOCAL_OPERATOR_TOKEN'], 'Key': upstream['LOCAL_QUALIFICATION_KEY'], 'Tenant': member['Tenant'], 'App': member['App'], 'Source': member['Source'], 'Policy': args.policy_id, 'Currency': args.currency, 'MemberDatabase': member['Database'], 'Database': str(ROOT / '.local/qualification-source.db')}
        path = Path(args.config);path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        with open(path, 'x', opener=lambda path, flags: os.open(path, flags, 0o600)) as handle:
            json.dump(config, handle, ensure_ascii=False, indent=2)
        print('私有事实来源配置已生成，未输出凭据')
        return
    config = load(args.config)
    destination = urllib.parse.urlparse(config['URL'])
    if destination.scheme != 'http' or destination.hostname not in ('127.0.0.1', '::1') or destination.username or destination.query or destination.fragment:
        raise SystemExit('事实适配器仅允许本地回环佣金地址')
    if args.command == 'create-policy':
        payload = {'policy_id': config['Policy'], 'source_id': config['Source'], 'currency': config['Currency'], 'deposit_threshold_minor': args.deposit_minor, 'valid_bet_threshold_minor': args.valid_bet_minor, 'venue_types': args.venue}
        status, result = request(config, '/v1/member-qualification-policies', json.dumps(payload).encode(), uuid.uuid4().hex)
        print(json.dumps({'http_status': status, 'result': result}, ensure_ascii=False))
        if status != 200: raise SystemExit(1)
        return
    if args.command == 'query':
        status, result = request(config, '/v1/member-qualifications?' + urllib.parse.urlencode({'policy_id': config['Policy'], 'source_id': config['Source'], 'external_user_id': args.member_id}))
        print(json.dumps({'http_status': status, 'result': result}, ensure_ascii=False, indent=2))
        if status != 200: raise SystemExit(1)
        return
    with database(config) as db:
        if args.command == 'deliver':
            deliver(config, db, retry_blocked=args.retry_blocked)
            return
        if not re.fullmatch(r'(0|[1-9][0-9]{0,37})', args.amount_minor) or not 1 <= len(args.business_id) <= 128:
            raise SystemExit('金额必须是非负整数最小货币单位，业务号为1至128位')
        if args.type == 'BET_SETTLED' and not args.venue or args.type == 'DEPOSIT_SUCCEEDED' and (args.venue or args.amount_minor == '0'):
            raise SystemExit('有效投注必须提供场馆，成功充值必须为正金额且不带场馆')
        # 只确认外部编号属于现有会员；不读取密码、写会员资料或修改注册事件。
        with sqlite3.connect(Path(config['MemberDatabase']).resolve().as_uri() + '?mode=ro', uri=True) as member_db:
            if not member_db.execute('SELECT id FROM members WHERE id=?', (args.member_id,)).fetchone():
                raise SystemExit('会员不存在，拒绝制造充值/投注事实')
        at = args.business_time or datetime.now(timezone.utc).isoformat(timespec='microseconds').replace('+00:00', 'Z')
        payload = {'schema_version': '1', 'policy_id': config['Policy'], 'source_id': config['Source'], 'event_id': uuid.uuid4().hex, 'business_id': args.business_id, 'event_type': args.type, 'external_user_id': args.member_id, 'business_time': at, 'amount_minor': args.amount_minor, 'currency': config['Currency'], 'venue_type': args.venue}
        fingerprint_data = dict(payload);fingerprint_data.pop('event_id');fingerprint_data.pop('business_time')
        fingerprint = hashlib.sha256(json.dumps(fingerprint_data, sort_keys=True).encode()).hexdigest()
        # 同一订单重复命令沿用已保存的时间、载荷和事件号，参数变化拒绝而非覆盖。
        db.execute('BEGIN IMMEDIATE')
        existing = db.execute('SELECT event_id,fingerprint,payload FROM facts WHERE scope_hash=? AND kind=? AND business_id=?', (scope_hash(config), args.type, args.business_id)).fetchone()
        if existing:
            if existing[1] != fingerprint or args.business_time and json.loads(existing[2])['business_time'] != args.business_time:
                raise SystemExit('该业务订单已保存不同事实，拒绝覆盖')
            event_id = existing[0]
        else:
            event_id = payload['event_id']
            db.execute('INSERT INTO facts VALUES(?,?,?,?,?,?,?,?)', (event_id, scope_hash(config), args.type, args.business_id, fingerprint, json.dumps(payload, separators=(',', ':'), ensure_ascii=False).encode(), 'PENDING', ''))
        db.commit()
        deliver(config, db, only=event_id)


if __name__ == '__main__':
    main()
