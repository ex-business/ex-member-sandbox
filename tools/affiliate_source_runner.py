#!/usr/bin/env python3
"""模拟业务来源常驻任务：可靠投递并按站点自然日自动关账。"""
import argparse
import datetime as dt
import fcntl
import json
import signal
import sqlite3
import threading
from pathlib import Path
from zoneinfo import ZoneInfo
import affiliate_business_source as source


def cycle(config, db, now=None, limit=31):
    # 先重试事实再关账，任何未确认事实都会阻止该日及后续日期推进。
    source.deliver(config, db)
    status, body = source.request(config, '/v1/affiliate/config')
    current = body.get('data', {}).get('current')
    if status != 200 or not current:
        raise ValueError('站点尚无生效配置')
    zone = ZoneInfo(current['config']['timezone'])
    today = (now or dt.datetime.now(dt.timezone.utc)).astimezone(zone).date()
    db.execute('CREATE TABLE IF NOT EXISTS runner_state(id INTEGER PRIMARY KEY CHECK(id=1),next_day TEXT NOT NULL,timezone TEXT NOT NULL)')
    state = db.execute('SELECT next_day,timezone FROM runner_state WHERE id=1').fetchone()
    if state and state[1] != str(zone):
        raise ValueError('站点时区已变化，需核对历史业务日后迁移来源游标')
    if state:
        day = dt.date.fromisoformat(state[0])
    else:
        # 首次运行从已有事实最早日开始；无事实从今日开始，避免宣布启动前未知日期完整。
        dates = [source.instant(json.loads(r[0])['business_time']).astimezone(zone).date()
                 for r in db.execute('SELECT payload FROM facts')]
        day = min(dates + [today])
        db.execute('INSERT INTO runner_state VALUES(1,?,?)', (day.isoformat(), str(zone)))
        db.commit()
    closed = []
    for _ in range(limit):
        if day >= today:
            break
        if not db.execute('SELECT 1 FROM watermarks WHERE day=?', (day.isoformat(),)).fetchone():
            source.close_business_day(config, db, day.isoformat(), zone)
        closed.append(day.isoformat())
        day += dt.timedelta(days=1)
        # 水位成功后才推进；崩溃在两次提交之间时可从已保存水位恢复，避免重复产生资金。
        db.execute('UPDATE runner_state SET next_day=? WHERE id=1', (day.isoformat(),))
        db.commit()
    return closed


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', required=True)
    parser.add_argument('--interval', type=float, default=5)
    parser.add_argument('--once', action='store_true')
    args = parser.parse_args()
    if args.interval < 1:
        parser.error('轮询间隔至少一秒')
    config = json.loads(Path(args.config).read_text())
    stop = threading.Event()
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: stop.set())
    # 同一来源库只允许一个常驻任务；不干预会员进程或其他租户。
    with open(config['database'] + '.runner.lock', 'a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise SystemExit('该来源自动任务已运行')
        with source.connect(config) as db:
            previous_error = None
            while not stop.is_set():
                try:
                    closed = cycle(config, db)
                    if closed:
                        print(source.dump({'来源': config['source'], '已关账日期': closed}), flush=True)
                    if previous_error:
                        print('来源自动任务已恢复', flush=True)
                    previous_error = None
                except (ValueError, OSError, sqlite3.Error) as error:
                    # 同一失败仅记录一次，保留状态下轮重试，不打印配置和鉴权信息。
                    message = str(error)
                    if message != previous_error:
                        print('来源自动任务等待重试：' + message, flush=True)
                    previous_error = message
                    if args.once:
                        raise SystemExit(1)
                if args.once:
                    return
                stop.wait(args.interval)


if __name__ == '__main__':
    main()
